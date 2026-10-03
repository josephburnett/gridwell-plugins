package memo

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// walker is a source whose walks block until the test releases them.
type walker struct {
	mu      sync.Mutex
	calls   []string
	ctxs    []context.Context
	release chan error
}

func (w *walker) Walk(ctx context.Context, key string) error {
	w.mu.Lock()
	w.calls = append(w.calls, key)
	w.ctxs = append(w.ctxs, ctx)
	w.mu.Unlock()
	select {
	case err := <-w.release:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (w *walker) count() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return len(w.calls)
}

func (w *walker) awaitCalls(t *testing.T, n int) {
	t.Helper()
	eventually(t, "walks to start", func() bool { return w.count() >= n })
}

type flightHarness struct {
	clock  *fakeClock
	walk   *walker
	f      *Flights
	life   *Life
	logs   *logs
	landed chan string
}

const window = time.Minute

func newFlightHarness(t *testing.T, o FlightOptions) *flightHarness {
	h := &flightHarness{
		clock:  newFakeClock(),
		walk:   &walker{release: make(chan error)},
		life:   NewLife(),
		logs:   &logs{},
		landed: make(chan string, 16),
	}
	o.Name = "test"
	o.Walk = h.walk.Walk
	if o.Window == 0 {
		o.Window = window
	}
	o.FirstAnswer = time.Second
	o.Clock = h.clock
	o.Logf = h.logs.logf
	o.Landed = func(key string, _ error) { h.landed <- key }
	h.f = NewFlights(h.life, o)
	t.Cleanup(h.life.End)
	return h
}

// land releases one walk with err and waits until its outcome is recorded.
func (h *flightHarness) land(t *testing.T, err error) {
	t.Helper()
	h.walk.release <- err
	select {
	case <-h.landed:
	case <-time.After(5 * time.Second):
		t.Fatal("the walk never landed")
	}
}

type readResult struct {
	reason string
	err    error
}

func (h *flightHarness) readAsync(key string, warm bool) chan readResult {
	ch := make(chan readResult, 1)
	go func() {
		r, err := h.f.Read(context.Background(), key, warm)
		ch <- readResult{r, err}
	}()
	return ch
}

func await(t *testing.T, ch chan readResult) readResult {
	t.Helper()
	select {
	case r := <-ch:
		return r
	case <-time.After(5 * time.Second):
		t.Fatal("Read never answered")
		return readResult{}
	}
}

var errDown = status.Error(codes.Unavailable, "gitlab.example is not answering")

// TestFlights: how one read answers, for each state memory and the source
// can be in.
func TestFlights(t *testing.T) {
	for _, tc := range []struct {
		name string
		run  func(t *testing.T, h *flightHarness)
	}{
		{"a cold read waits for its walk", func(t *testing.T, h *flightHarness) {
			r := h.readAsync("inbox", false)
			h.walk.awaitCalls(t, 1)
			h.land(t, nil)
			if got := await(t, r); got != (readResult{}) {
				t.Errorf("Read = %+v, want a plain answer", got)
			}
		}},
		{"a cold read whose walk failed answers the failure", func(t *testing.T, h *flightHarness) {
			r := h.readAsync("inbox", false)
			h.walk.awaitCalls(t, 1)
			h.land(t, errDown)
			if got := await(t, r); !errors.Is(got.err, errDown) {
				t.Errorf("Read err = %v, want the walk's", got.err)
			}
		}},
		{"a cold read answers memory so far after the first-answer bound", func(t *testing.T, h *flightHarness) {
			r := h.readAsync("inbox", false)
			h.walk.awaitCalls(t, 1)
			h.clock.awaitWaiters(t, 1)
			h.clock.Advance(time.Second)
			if got := await(t, r); got != (readResult{}) {
				t.Errorf("Read = %+v, want memory so far", got)
			}
			if !h.f.Busy("inbox") {
				t.Error("the walk stopped when its reader stopped waiting")
			}
		}},
		{"a warm read answers at once and the walk runs on", func(t *testing.T, h *flightHarness) {
			if got := await(t, h.readAsync("inbox", true)); got != (readResult{}) {
				t.Errorf("Read = %+v, want a plain answer", got)
			}
			h.walk.awaitCalls(t, 1)
			if !h.f.Busy("inbox") {
				t.Error("a warm read past the window started no walk")
			}
		}},
		{"a warm read after a failed walk answers from memory, unreachable", func(t *testing.T, h *flightHarness) {
			await(t, h.readAsync("inbox", true))
			h.land(t, errDown)
			got := await(t, h.readAsync("inbox", true))
			if got.err != nil || got.reason != "gitlab.example is not answering" {
				t.Errorf("Read = %+v, want no error and the reason", got)
			}
			h.land(t, nil)
			h.clock.Advance(window) // that walk's freshness has passed too
			if got := await(t, h.readAsync("inbox", true)); got.reason != "" {
				t.Errorf("after a walk landed, unreachable = %q, want none", got.reason)
			}
		}},
		{"a fresh read walks nothing until the window passes", func(t *testing.T, h *flightHarness) {
			await(t, h.readAsync("inbox", true))
			h.land(t, nil)
			h.clock.Advance(window - time.Second)
			await(t, h.readAsync("inbox", true))
			if h.walk.count() != 1 {
				t.Fatalf("a read inside the window walked: %d walks", h.walk.count())
			}
			h.clock.Advance(time.Second)
			await(t, h.readAsync("inbox", true))
			h.walk.awaitCalls(t, 2)
		}},
		{"a burst of readers costs one walk", func(t *testing.T, h *flightHarness) {
			var rs []chan readResult
			for range 10 {
				rs = append(rs, h.readAsync("inbox", false))
			}
			h.walk.awaitCalls(t, 1)
			eventually(t, "every reader on the clock", func() bool { return h.clock.pending() == 10 })
			h.land(t, nil)
			for _, r := range rs {
				await(t, r)
			}
			if h.walk.count() != 1 {
				t.Errorf("%d walks for one burst, want 1", h.walk.count())
			}
		}},
		{"rewalk during a walk walks once more after it", func(t *testing.T, h *flightHarness) {
			h.f.Rewalk("inbox")
			h.walk.awaitCalls(t, 1)
			h.f.Rewalk("inbox")
			h.f.Rewalk("inbox")
			h.land(t, nil)
			h.walk.awaitCalls(t, 2)
			h.land(t, nil)
			if h.walk.count() != 2 {
				t.Errorf("%d walks, want 2", h.walk.count())
			}
		}},
		{"an outcome that is not a walk is what reads answer", func(t *testing.T, h *flightHarness) {
			await(t, h.readAsync("inbox", true))
			h.land(t, nil)
			h.f.Outcome("inbox", errors.New("glance: 502"))
			if got := await(t, h.readAsync("inbox", true)); got.reason != "glance: 502" {
				t.Errorf("unreachable = %q, want the glance's failure", got.reason)
			}
			h.f.Outcome("inbox", nil)
			if got := await(t, h.readAsync("inbox", true)); got.reason != "" {
				t.Errorf("unreachable = %q after a glance landed, want none", got.reason)
			}
			if h.walk.count() != 1 {
				t.Errorf("an outcome stamped no freshness yet %d walks ran", h.walk.count())
			}
		}},
		{"a failure logs once per episode", func(t *testing.T, h *flightHarness) {
			for range 3 {
				h.f.Rewalk("inbox")
				h.land(t, errDown)
			}
			if h.logs.count() != 1 {
				t.Fatalf("three failed walks logged %d lines, want 1", h.logs.count())
			}
			h.f.Rewalk("inbox")
			h.land(t, nil)
			h.f.Rewalk("inbox")
			h.land(t, errDown)
			if h.logs.count() != 2 {
				t.Errorf("a second episode logged %d lines in all, want 2", h.logs.count())
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) { tc.run(t, newFlightHarness(t, FlightOptions{})) })
	}
}

// TestFlightsCovers: a broader walk answers its narrower keys — they join
// it, are fresh under its stamp, answer its failure and are cleared by its
// landing.
func TestFlightsCovers(t *testing.T) {
	covers := func(key string) []string {
		if key == "root" {
			return nil
		}
		return []string{"root"}
	}
	h := newFlightHarness(t, FlightOptions{Covers: covers})
	await(t, h.readAsync("root", true))
	h.walk.awaitCalls(t, 1)
	await(t, h.readAsync("week:2026-09-28", true))
	if h.walk.count() != 1 {
		t.Fatalf("a week read beside a root walk started its own: %d walks", h.walk.count())
	}
	h.land(t, errDown)
	if got := await(t, h.readAsync("week:2026-09-28", true)); got.reason == "" {
		t.Error("a week read did not answer the root walk's failure")
	}
	h.walk.awaitCalls(t, 2) // the week read was past the window: its own walk
	h.land(t, errDown)
	h.f.Rewalk("root")
	h.land(t, nil)
	if got := await(t, h.readAsync("week:2026-09-28", true)); got.reason != "" {
		t.Errorf("a root walk landed and the week still answers %q", got.reason)
	}
	if h.walk.count() != 3 {
		t.Errorf("a week inside the root's window walked: %d walks, want 3", h.walk.count())
	}
}

// TestFlightsFreshOverride: a plugin whose source tells decides freshness
// itself, and the clock is not asked.
func TestFlightsFreshOverride(t *testing.T) {
	var live bool
	var mu sync.Mutex
	h := newFlightHarness(t, FlightOptions{Fresh: func(string, time.Time) bool {
		mu.Lock()
		defer mu.Unlock()
		return live
	}})
	mu.Lock()
	live = true
	mu.Unlock()
	await(t, h.readAsync("imbox", true))
	if h.walk.count() != 0 {
		t.Errorf("a read the feed keeps current walked")
	}
}

// TestFlightsWalkedAtSurvivesARestart: a walk is fresh for its window
// whichever process ran it, and a stamp from the future is not fresh.
func TestFlightsWalkedAtSurvivesARestart(t *testing.T) {
	h := newFlightHarness(t, FlightOptions{})
	h.f.Rewalk("inbox")
	h.land(t, nil)
	saved := h.f.WalkedAt()

	next := newFlightHarness(t, FlightOptions{})
	next.clock = h.clock
	next.f.o.Clock = h.clock
	next.f.Restore(saved)
	if !next.f.Fresh("inbox") {
		t.Error("a restored walk inside its window is not fresh")
	}
	next.f.Restore(map[string]time.Time{"inbox": h.clock.Now().Add(time.Hour)})
	if next.f.Fresh("inbox") {
		t.Error("a walk stamped in the future is fresh")
	}
}

// TestFlightsWalkUnderTheLifetime: a walk outlives the reader that started
// it and ends with the plugin, and a reader waiting on it is released.
func TestFlightsWalkUnderTheLifetime(t *testing.T) {
	h := newFlightHarness(t, FlightOptions{})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := h.f.Read(ctx, "inbox", false)
		done <- err
	}()
	h.walk.awaitCalls(t, 1)
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("a hung-up reader got %v", err)
	}
	h.walk.mu.Lock()
	walkCtx := h.walk.ctxs[0]
	h.walk.mu.Unlock()
	if walkCtx.Err() != nil {
		t.Fatal("the walk ended with its reader")
	}
	h.life.End()
	if walkCtx.Err() == nil {
		t.Error("the walk outlived the plugin")
	}
	if h.f.Busy("inbox") {
		t.Error("a walk the plugin's end stopped is still in flight")
	}
}

// TestReason: an RPC failure reads as its message alone.
func TestReason(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want string
	}{
		{nil, ""},
		{errors.New("dial tcp: refused"), "dial tcp: refused"},
		{status.Error(codes.Unavailable, "hey: not signed in"), "hey: not signed in"},
	} {
		if got := Reason(tc.err); got != tc.want {
			t.Errorf("Reason(%v) = %q, want %q", tc.err, got, tc.want)
		}
	}
}
