package memo

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

// serving is one Watch stream being served.
type serving struct {
	stream *fakeStream
	done   chan error
}

func serve(t *testing.T, c *Changes, scope []string, stream *fakeStream) *serving {
	t.Helper()
	s := &serving{stream: stream, done: make(chan error, 1)}
	go func() { s.done <- c.Serve(scope, stream) }()
	eventually(t, "the header", stream.hasHeader)
	return s
}

// hangUp ends the stream as the node does and waits for Serve to return.
func (s *serving) hangUp(t *testing.T) {
	t.Helper()
	s.stream.cancel()
	select {
	case err := <-s.done:
		if err != nil && !errors.Is(err, context.Canceled) {
			t.Errorf("Serve ended with %v after the node hung up", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not return after the node hung up")
	}
}

// entered waits until the stream has begun n sends.
func (s *serving) entered(t *testing.T, n int) {
	t.Helper()
	for range n {
		select {
		case <-s.stream.entered:
		case <-time.After(5 * time.Second):
			t.Fatal("the stream never sent")
		}
	}
}

// release lets n held sends through.
func (s *serving) release(n int) {
	for range n {
		s.stream.gate <- struct{}{}
	}
}

func (s *serving) want(t *testing.T, want ...string) {
	t.Helper()
	eventually(t, fmt.Sprintf("sends %v", want), func() bool { return len(s.stream.got()) >= len(want) })
	if got := s.stream.got(); !reflect.DeepEqual(got, want) {
		t.Errorf("sent %v, want %v", got, want)
	}
}

// TestChangesFanOut: what one stream is sent, for each way changes can
// arrive.
func TestChangesFanOut(t *testing.T) {
	for _, tc := range []struct {
		name string
		o    ChangeOptions
		run  func(t *testing.T, c *Changes)
	}{
		{"the header goes before any change, on accept", ChangeOptions{}, func(t *testing.T, c *Changes) {
			s := serve(t, c, []string{"inbox"}, newFakeStream())
			c.Publish("inbox")
			s.want(t, "inbox")
			if s.stream.beforeHeader != 0 {
				t.Errorf("%d changes went before the header", s.stream.beforeHeader)
			}
			s.hangUp(t)
		}},
		{"a burst is one change per context", ChangeOptions{}, func(t *testing.T, c *Changes) {
			s := serve(t, c, []string{"inbox"}, held())
			c.Publish("inbox")
			s.entered(t, 1)
			for range 5 {
				c.Publish("inbox", "starred")
			}
			c.Publish("starred")
			s.release(3)
			s.want(t, "inbox", "inbox", "starred")
			s.hangUp(t)
		}},
		{"a stream that falls behind is told its whole scope", ChangeOptions{Buffer: 2}, func(t *testing.T, c *Changes) {
			s := serve(t, c, []string{"todos", "week:1"}, held())
			c.Publish("a")
			s.entered(t, 1)
			c.Publish("b", "c", "d")
			s.release(4)
			s.want(t, "a", "todos", "week:1", "d")
			s.hangUp(t)
		}},
		{"a slow stream never holds up a publish", ChangeOptions{}, func(t *testing.T, c *Changes) {
			s := serve(t, c, []string{"inbox"}, held())
			c.Publish("first")
			s.entered(t, 1)
			published := make(chan struct{})
			go func() {
				for i := range 10 * DefaultBuffer {
					c.Publish(fmt.Sprintf("week:%d", i))
				}
				close(published)
			}()
			select {
			case <-published:
			case <-time.After(5 * time.Second):
				t.Fatal("Publish waited on a stream that was not reading")
			}
			s.hangUp(t)
		}},
		{"every stream hears every change", ChangeOptions{}, func(t *testing.T, c *Changes) {
			a := serve(t, c, []string{"inbox"}, newFakeStream())
			b := serve(t, c, []string{"starred"}, newFakeStream())
			c.Publish("everything")
			a.want(t, "everything")
			b.want(t, "everything")
			a.hangUp(t)
			b.hangUp(t)
		}},
		{"a stream naming no scope watches Unscoped", ChangeOptions{Buffer: 1, Unscoped: []string{"imbox", "feed"}}, func(t *testing.T, c *Changes) {
			s := serve(t, c, nil, held())
			if !c.Watching("imbox") || !c.Watching("feed") {
				t.Error("a stream naming no scope started no work")
			}
			c.Publish("x")
			s.entered(t, 1)
			c.Publish("y", "z")
			s.release(4)
			s.want(t, "x", "imbox", "feed", "z")
			s.hangUp(t)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			life := NewLife()
			t.Cleanup(life.End)
			tc.run(t, NewChanges(life, tc.o))
		})
	}
}

// worklog records which units ran, and when each stopped.
type worklog struct {
	mu      sync.Mutex
	started []string
	running map[string]bool
	polls   int
}

func (w *worklog) do(ctx context.Context, unit string) {
	w.mu.Lock()
	w.started = append(w.started, unit)
	w.running[unit] = true
	w.mu.Unlock()
	<-ctx.Done()
	w.mu.Lock()
	w.running[unit] = false
	w.mu.Unlock()
}

func (w *worklog) is(unit string) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.running[unit]
}

func (w *worklog) starts() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]string(nil), w.started...)
}

// TestChangesWork: background work runs exactly while some stream has its
// context in scope, and lingers across the node swapping one stream for
// another.
func TestChangesWork(t *testing.T) {
	todosOnly := func(ctx string) []string {
		if ctx == "todos" || strings.HasPrefix(ctx, "week:") {
			return []string{"todos"}
		}
		return nil
	}
	for _, tc := range []struct {
		name string
		o    ChangeOptions
		run  func(t *testing.T, c *Changes, w *worklog, clock *fakeClock)
	}{
		{"nothing runs with no stream", ChangeOptions{}, func(t *testing.T, c *Changes, w *worklog, _ *fakeClock) {
			if c.Watching("todos") || len(w.starts()) != 0 {
				t.Errorf("work ran with no stream: %v", w.starts())
			}
		}},
		{"a scope's contexts start their units once", ChangeOptions{Work: todosOnly}, func(t *testing.T, c *Changes, w *worklog, _ *fakeClock) {
			s := serve(t, c, []string{"todos", "week:1", "week:2", "elsewhere"}, newFakeStream())
			eventually(t, "the poll", func() bool { return w.is("todos") })
			if got := w.starts(); !reflect.DeepEqual(got, []string{"todos"}) {
				t.Errorf("started %v, want the one unit", got)
			}
			s.hangUp(t)
		}},
		{"work stops a linger after the last stream leaves", ChangeOptions{Linger: time.Minute}, func(t *testing.T, c *Changes, w *worklog, clock *fakeClock) {
			a := serve(t, c, []string{"inbox"}, newFakeStream())
			b := serve(t, c, []string{"inbox"}, newFakeStream())
			eventually(t, "the work", func() bool { return w.is("inbox") })
			a.hangUp(t)
			b.hangUp(t)
			clock.awaitWaiters(t, 1)
			clock.Advance(time.Minute - time.Second)
			if !w.is("inbox") {
				t.Fatal("the work stopped inside the linger")
			}
			clock.Advance(time.Second)
			eventually(t, "the work to stop", func() bool { return !w.is("inbox") })
			if c.Watching("inbox") {
				t.Error("Watching after the work stopped")
			}
		}},
		{"a stream reopened inside the linger keeps the work", ChangeOptions{Linger: time.Minute}, func(t *testing.T, c *Changes, w *worklog, clock *fakeClock) {
			a := serve(t, c, []string{"inbox"}, newFakeStream())
			eventually(t, "the work", func() bool { return w.is("inbox") })
			a.hangUp(t)
			clock.awaitWaiters(t, 1)
			b := serve(t, c, []string{"inbox", "starred"}, newFakeStream())
			clock.Advance(time.Hour)
			eventually(t, "starred's work", func() bool { return w.is("starred") })
			if !w.is("inbox") {
				t.Error("the old stream's linger stopped work the new stream needs")
			}
			if got := w.starts(); len(got) != 2 {
				t.Errorf("started %v, want inbox once and starred once", got)
			}
			b.hangUp(t)
		}},
		{"a negative linger stops at once", ChangeOptions{Linger: -1}, func(t *testing.T, c *Changes, w *worklog, _ *fakeClock) {
			s := serve(t, c, []string{"inbox"}, newFakeStream())
			eventually(t, "the work", func() bool { return w.is("inbox") })
			s.hangUp(t)
			eventually(t, "the work to stop", func() bool { return !w.is("inbox") })
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			life := NewLife()
			t.Cleanup(life.End)
			clock := newFakeClock()
			w := &worklog{running: map[string]bool{}}
			tc.o.Do = w.do
			tc.o.Clock = clock
			tc.run(t, NewChanges(life, tc.o), w, clock)
		})
	}
}

// TestPollOnlyWhileWatched is rule 8's test for a source that cannot tell:
// with no stream the clock passes many windows and the source is never
// asked; with one it is asked every window; after the stream and its linger
// it is not asked again.
func TestPollOnlyWhileWatched(t *testing.T) {
	life := NewLife()
	t.Cleanup(life.End)
	clock := newFakeClock()
	var mu sync.Mutex
	asks := 0
	asked := func() int {
		mu.Lock()
		defer mu.Unlock()
		return asks
	}
	c := NewChanges(life, ChangeOptions{Linger: -1, Clock: clock, Do: func(ctx context.Context, _ string) {
		Poll(ctx, clock, 30*time.Second, func(context.Context) {
			mu.Lock()
			asks++
			mu.Unlock()
		})
	}})
	for range 5 {
		clock.Advance(30 * time.Second)
	}
	if asked() != 0 {
		t.Fatalf("the source was asked %d times with nothing watching", asked())
	}
	s := serve(t, c, []string{"todos"}, newFakeStream())
	for i := 1; i <= 3; i++ {
		clock.awaitWaiters(t, 1)
		clock.Advance(30 * time.Second)
		eventually(t, "a poll", func() bool { return asked() == i })
	}
	s.hangUp(t)
	eventually(t, "the poll to stop", func() bool { return !c.Watching("todos") })
	eventually(t, "the poll's timer to be dropped", func() bool { return clock.pending() <= 1 })
	for range 5 {
		clock.Advance(30 * time.Second)
	}
	if asked() != 3 {
		t.Errorf("the source was asked %d times, want 3: it polled after the stream left", asked())
	}
}

// TestLifeEndStopsWork: the plugin's end stops every unit and waits for it.
func TestLifeEndStopsWork(t *testing.T) {
	life := NewLife()
	w := &worklog{running: map[string]bool{}}
	c := NewChanges(life, ChangeOptions{Do: w.do})
	s := serve(t, c, []string{"inbox"}, newFakeStream())
	eventually(t, "the work", func() bool { return w.is("inbox") })
	life.End()
	if w.is("inbox") {
		t.Error("work outlived the plugin")
	}
	s.hangUp(t)
}
