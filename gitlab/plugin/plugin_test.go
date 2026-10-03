package plugin

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/josephburnett/gridwell-plugins/gitlab/todos"
	"github.com/josephburnett/gridwell-plugins/memo/calendar"
	pluginv1 "github.com/josephburnett/gridwell/api/gen/plugin/v1"
)

func at(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return t
}

func mk(id int64, created, state string) todos.Todo {
	var t todos.Todo
	t.ID, t.CreatedAt, t.State = id, at(created), state
	t.TargetType, t.Target.IID, t.Target.Title, t.Body = "MergeRequest", id, "mr "+strings.Repeat("x", int(id)), "please **review**"
	t.TargetURL = "https://gitlab.example/g/p/-/merge_requests/1"
	return t
}

// oneShot serves whole lists in one page and counts calls.
type oneShot struct {
	pending, done []todos.Todo
	calls         atomic.Int32
}

func (f *oneShot) Page(_ context.Context, state string, page int) (todos.Reply, error) {
	f.calls.Add(1)
	if page > 1 {
		return todos.Reply{}, nil
	}
	if state == todos.StateDone {
		return todos.Reply{Todos: f.done}, nil
	}
	return todos.Reply{Todos: f.pending}, nil
}

// reader collects a ReadContent stream.
type reader struct {
	pluginv1.Plugin_ReadContentServer
	chunks []*pluginv1.ContentChunk
}

func (r *reader) Send(c *pluginv1.ContentChunk) error { r.chunks = append(r.chunks, c); return nil }
func (r *reader) Context() context.Context            { return context.Background() }

func TestListsWeeksThenTodosAndRefreshesOnAWindow(t *testing.T) {
	src := &oneShot{
		pending: []todos.Todo{mk(1, "2026-08-18T10:00:00Z", "pending"), mk(2, "2026-08-25T10:00:00Z", "pending")},
		done:    []todos.Todo{mk(3, "2026-08-19T10:00:00Z", "done")},
	}
	clock := at("2026-08-25T12:00:00Z")
	p := New(src, Options{Now: func() time.Time { return clock }})
	ctx := context.Background()

	info, _ := p.Info(ctx, &pluginv1.InfoRequest{})
	if info.Kind != Kind || len(info.MenuEntries) != 1 || info.MenuEntries[0].Context != todos.RootContext {
		t.Fatalf("info = %v", info)
	}
	root, err := p.List(ctx, &pluginv1.ListRequest{Context: todos.RootContext})
	if err != nil {
		t.Fatal(err)
	}
	if root.Authoritative || len(root.Entries) != 2 || root.Entries[0].Key != "week:2026-08-24" || root.Entries[1].Label != "2026-08-17" {
		t.Fatalf("root = %v", root.Entries)
	}
	if src.calls.Load() != 2 {
		t.Fatalf("outset walk made %d calls, want 2 (pending + done)", src.calls.Load())
	}
	// The root walk covered every week: a descent inside the window is
	// answered from memory, no GitLab round trip.
	wk, err := p.List(ctx, &pluginv1.ListRequest{Context: "week:2026-08-17"})
	if err != nil {
		t.Fatal(err)
	}
	if src.calls.Load() != 2 {
		t.Errorf("a fresh week re-walked GitLab (%d calls)", src.calls.Load())
	}
	if len(wk.Entries) != 2 || wk.Entries[0].ServesPage || wk.Entries[0].Kind != "text" || wk.Entries[0].Key != "todo:1" {
		t.Fatalf("week = %v", wk.Entries)
	}
	if x, y := calendar.Cell(at("2026-08-18T10:00:00Z"), todos.TodoTileW); wk.Entries[0].PlacementHint.GetX() != x || wk.Entries[0].PlacementHint.GetY() != y {
		t.Errorf("Tuesday hint = %v", wk.Entries[0].PlacementHint)
	}
	// Todo 1 is marked done in GitLab. Inside the window nothing moves;
	// past it, a read starts the descent's targeted walk, and the status flips
	// when that walk lands.
	src.pending = src.pending[1:]
	wk, _ = p.List(ctx, &pluginv1.ListRequest{Context: "week:2026-08-17"})
	if wk.Entries[0].StatusDetail != "" {
		t.Error("state changed inside the refresh window")
	}
	clock = clock.Add(DefaultFullRefresh + time.Second)
	_, _ = p.List(ctx, &pluginv1.ListRequest{Context: "week:2026-08-17"})
	landed(t, p)
	wk, _ = p.List(ctx, &pluginv1.ListRequest{Context: "week:2026-08-17"})
	if src.calls.Load() != 4 || !strings.HasPrefix(wk.Entries[0].Label, "!1 ") || wk.Entries[0].StatusDetail != todos.DoneMark {
		t.Errorf("after the window: calls=%d entry=%v", src.calls.Load(), wk.Entries[0])
	}
	// The todo did not go away.
	if len(wk.Entries) != 2 {
		t.Errorf("a done todo left the listing: %v", wk.Entries)
	}
	root, _ = p.List(ctx, &pluginv1.ListRequest{Context: todos.RootContext})
	if root.Entries[1].Label != "2026-08-17" {
		t.Errorf("root after flip = %q", root.Entries[1].Label)
	}
}

// TestReadContentBeforeFirstWalkIsUnavailable: a fresh process asked for a
// todo it has not yet seen must answer "not right now", never a Gone body.
// The node keeps its rows across a restart, so this read can arrive before
// the walk lands, and a Gone body would show a live todo as gone.
func TestReadContentBeforeFirstWalkIsUnavailable(t *testing.T) {
	src := &oneShot{pending: []todos.Todo{mk(1, "2026-08-18T10:00:00Z", "pending")}}
	p := New(src, Options{})
	r := &reader{}
	err := p.ReadContent(&pluginv1.ReadContentRequest{Key: "todo:1"}, r)
	if status.Code(err) != codes.Unavailable {
		t.Fatalf("pre-walk unknown todo = (%v, %v), want Unavailable", r.chunks, err)
	}
	// After a completed walk the same guard stands aside: a known todo
	// answers, and an unknown one is honestly gone.
	if _, err := p.List(context.Background(), &pluginv1.ListRequest{Context: todos.RootContext}); err != nil {
		t.Fatal(err)
	}
	r = &reader{}
	if err := p.ReadContent(&pluginv1.ReadContentRequest{Key: "todo:1"}, r); err != nil {
		t.Fatal(err)
	}
	r = &reader{}
	if err := p.ReadContent(&pluginv1.ReadContentRequest{Key: "todo:99"}, r); err != nil || !strings.Contains(string(r.chunks[0].Data), "todo:99") {
		t.Fatalf("post-walk unknown todo = (%s, %v), want the gone body", r.chunks[0].GetData(), err)
	}
}

func TestReadContentAndProbe(t *testing.T) {
	src := &oneShot{pending: []todos.Todo{mk(1, "2026-08-18T10:00:00Z", "pending")}}
	p := New(src, Options{})
	ctx := context.Background()
	if _, err := p.List(ctx, &pluginv1.ListRequest{Context: todos.RootContext}); err != nil {
		t.Fatal(err)
	}
	r := &reader{}
	if err := p.ReadContent(&pluginv1.ReadContentRequest{Key: "todo:1"}, r); err != nil {
		t.Fatal(err)
	}
	if c := r.chunks[0]; c.MediaType != "text/markdown" || !strings.Contains(string(c.Data), "> please **review**") || !strings.Contains(string(c.Data), "# !1 mr x") || !strings.Contains(string(c.Data), "[Open !1 in GitLab](https://gitlab.example/g/p/-/merge_requests/1)") {
		t.Errorf("content = %s %s", c.MediaType, c.Data)
	}
	r = &reader{}
	_ = p.ReadContent(&pluginv1.ReadContentRequest{Key: "todo:99"}, r)
	if !strings.Contains(string(r.chunks[0].Data), "todo:99") {
		t.Errorf("unknown todo = %s", r.chunks[0].Data)
	}
	r = &reader{}
	_ = p.ReadContent(&pluginv1.ReadContentRequest{Key: "week:2026-08-17"}, r)
	if len(r.chunks[0].Data) != 0 {
		t.Error("a week has no body")
	}
	probe := func(key string) pluginv1.ProbeResponse_Presence {
		r, _ := p.Probe(ctx, &pluginv1.ProbeRequest{Key: key})
		return r.Presence
	}
	if probe("todo:1") != pluginv1.ProbeResponse_PRESENCE_PRESENT || probe("todo:99") != pluginv1.ProbeResponse_PRESENCE_UNSPECIFIED ||
		probe("week:2026-08-17") != pluginv1.ProbeResponse_PRESENCE_PRESENT || probe("week:2026-01-05") != pluginv1.ProbeResponse_PRESENCE_UNSPECIFIED ||
		probe("junk") != pluginv1.ProbeResponse_PRESENCE_GONE {
		t.Error("probe verdicts: known PRESENT, unknown UNSPECIFIED (never GONE), malformed GONE")
	}
	res, _ := p.Search(ctx, &pluginv1.SearchRequest{Query: "REVIEW"})
	if len(res.Results) != 1 || res.Results[0].Entry.Key != "todo:1" || strings.Join(res.Results[0].ContextPath, "/") != "todos/week:2026-08-17" {
		t.Errorf("search = %v", res.Results)
	}
}

// A restart over a kept state_dir answers the first listing from the cache
// file, with no walk at all: the node lists a plugin's root the moment a pane
// opens it, and that must not wait on GitLab for what the last process
// already knew.
func TestRestartAnswersFromTheCacheFileWithoutWalking(t *testing.T) {
	dir := t.TempDir()
	src := &oneShot{
		pending: []todos.Todo{mk(1, "2026-08-18T10:00:00Z", "pending"), mk(2, "2026-08-25T10:00:00Z", "pending")},
		done:    []todos.Todo{mk(3, "2026-08-19T10:00:00Z", "done")},
	}
	first := New(src, Options{StateDir: dir})
	ctx := context.Background()
	if _, err := first.List(ctx, &pluginv1.ListRequest{Context: todos.RootContext}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, todos.CacheFile)); err != nil {
		t.Fatalf("no cache file after a successful walk: %v", err)
	}

	// The restart: a new plugin over the same directory, and a GitLab that
	// answers nothing, so every entry below can only come from the file.
	cold := &oneShot{}
	second := New(cold, Options{StateDir: dir})
	root, err := second.List(ctx, &pluginv1.ListRequest{Context: todos.RootContext})
	if err != nil {
		t.Fatal(err)
	}
	if n := cold.calls.Load(); n != 0 {
		t.Errorf("the restart walked GitLab %d times; the last walk is still inside the refresh window", n)
	}
	if len(root.Entries) != 2 || root.Entries[1].Label != "2026-08-17" {
		t.Fatalf("root after restart = %v", root.Entries)
	}
	wk, err := second.List(ctx, &pluginv1.ListRequest{Context: "week:2026-08-17"})
	if err != nil {
		t.Fatal(err)
	}
	if len(wk.Entries) != 2 || wk.Entries[0].Key != "todo:1" {
		t.Fatalf("week after restart = %v", wk.Entries)
	}
	// The restored high-water mark stands: an unknown todo reads as gone
	// rather than Unavailable, because a walk DID once reach the end.
	r := &reader{}
	if err := second.ReadContent(&pluginv1.ReadContentRequest{Key: "todo:99"}, r); err != nil {
		t.Fatalf("post-restart unknown todo = %v, want the gone body", err)
	}
	r = &reader{}
	if err := second.ReadContent(&pluginv1.ReadContentRequest{Key: "todo:1"}, r); err != nil || !strings.Contains(string(r.chunks[0].Data), "mr x") {
		t.Errorf("a cached todo's body = (%s, %v)", r.chunks[0].GetData(), err)
	}
}

// Without a state_dir the plugin behaves exactly as it did: memory only, no
// file, and an unknown todo before the first walk is still Unavailable.
func TestNoStateDirWritesNothingAndStaysCold(t *testing.T) {
	p := New(&oneShot{pending: []todos.Todo{mk(1, "2026-08-18T10:00:00Z", "pending")}}, Options{})
	if p.file.Path() != "" {
		t.Errorf("cache path = %q with no state_dir", p.file.Path())
	}
	if _, err := p.List(context.Background(), &pluginv1.ListRequest{Context: todos.RootContext}); err != nil {
		t.Fatal(err)
	}
	cold := New(&oneShot{}, Options{StateDir: " "})
	if cold.file.Path() != "" {
		t.Errorf("a blank state_dir became the path %q", cold.file.Path())
	}
	r := &reader{}
	if err := cold.ReadContent(&pluginv1.ReadContentRequest{Key: "todo:1"}, r); status.Code(err) != codes.Unavailable {
		t.Errorf("cold read = %v, want Unavailable", err)
	}
}

// The refresher runs only while a Watch stream is open, since the node holds
// one only while one of these grids is shown: with none open, refresh windows
// pass and GitLab hears nothing; with one open, a memory is warm because the
// interval came round, not because a read paid for it, and the cache file is
// warm with it; once the stream ends GitLab hears nothing again.
func TestTheRefresherRunsOnlyWhileWatched(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	src := &gated{gate: make(chan struct{})}
	close(src.gate) // never blocks: this walk is the refresher's own
	p := New(src, Options{StateDir: dir, Refresh: MinRefresherInterval, Linger: -1})
	t.Cleanup(p.Close)
	flat := func(what string) {
		t.Helper()
		time.Sleep(100 * time.Millisecond) // a request already sent lands
		n := src.calls.Load()
		time.Sleep(3 * MinRefresherInterval)
		if got := src.calls.Load(); got != n {
			t.Fatalf("%s: GitLab heard %d requests across three refresh windows", what, got-n)
		}
	}
	flat("no stream open")

	w := watching(t, p, nil)
	deadline := time.Now().Add(5 * time.Second)
	for src.calls.Load() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the refresher never walked while watched")
		}
		time.Sleep(time.Millisecond)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, todos.CacheFile)); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the refresher walked but wrote no cache")
		}
		time.Sleep(time.Millisecond)
	}

	w.cancel()
	unwatched(t, p)
	flat("the stream ended")
}

// The refresher never spins: a `refresh` shorter than a request would leave
// it always asking GitLab. A tiny full-refresh window is a test's way of
// saying "walk on every read", and reads still honour it.
func TestTheRefresherNeverRunsHotterThanItsFloor(t *testing.T) {
	if got := New(&oneShot{}, Options{Refresh: time.Nanosecond}).refresherInterval(); got != MinRefresherInterval {
		t.Errorf("a 1ns window refreshes every %v, want the floor %v", got, MinRefresherInterval)
	}
	if got := New(&oneShot{}, Options{Refresh: time.Hour}).refresherInterval(); got != time.Hour {
		t.Errorf("a 1h window refreshes every %v", got)
	}
	// The floor is the refresher's alone: a read on a tiny full-refresh
	// window still walks.
	p := New(&oneShot{pending: []todos.Todo{mk(1, "2026-08-18T10:00:00Z", "pending")}}, Options{FullRefresh: time.Nanosecond})
	for i := 0; i < 2; i++ {
		if _, err := p.List(context.Background(), &pluginv1.ListRequest{Context: todos.RootContext}); err != nil {
			t.Fatal(err)
		}
		landed(t, p)
	}
	if n := p.src.(retrying).src.(*oneShot).calls.Load(); n != 4 {
		t.Errorf("two reads on a 1ns window made %d page calls, want 4 (both walked)", n)
	}
}

func TestUnknownContextIsAnArgumentError(t *testing.T) {
	p := New(&oneShot{}, Options{})
	if _, err := p.List(context.Background(), &pluginv1.ListRequest{Context: "bogus"}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("unknown context → %v", err)
	}
}

// gated serves one page per state and blocks every Page call on a gate,
// counting the calls that got through — a slow GitLab.
type gated struct {
	gate  chan struct{}
	calls atomic.Int32
}

func (g *gated) Page(_ context.Context, state string, page int) (todos.Reply, error) {
	g.calls.Add(1)
	<-g.gate
	if page > 1 || state == todos.StateDone {
		return todos.Reply{}, nil
	}
	return todos.Reply{Todos: []todos.Todo{mk(1, "2026-08-18T10:00:00Z", "pending")}}, nil
}

// paged is a source whose pending list is two pages, the second parked
// behind a gate — a slow GitLab mid-walk. Calls are counted at entry.
type paged struct {
	gate  chan struct{}
	calls atomic.Int32
}

func (s *paged) Page(ctx context.Context, state string, page int) (todos.Reply, error) {
	s.calls.Add(1)
	if state == todos.StatePending {
		switch page {
		case 1:
			return todos.Reply{Todos: []todos.Todo{mk(1, "2026-08-18T10:00:00Z", "pending")}, More: true}, nil
		case 2:
			select {
			case <-s.gate:
			case <-ctx.Done():
				return todos.Reply{}, ctx.Err()
			}
			return todos.Reply{Todos: []todos.Todo{mk(2, "2026-08-11T10:00:00Z", "pending")}}, nil
		}
	}
	return todos.Reply{}, nil
}

// TestListStreamsWhileTheWalkRuns: a cold walk over a real history runs
// minutes, and a List that waits for all of it shows the user nothing the
// whole time. List answers what memory holds after FirstAnswer — GitLab pages
// newest-first, so the first answer is the most recent weeks, and the walk's
// announcements paint the rest in as pages land. One walk serves it all: no
// restarts, no per-reader paging.
func TestListStreamsWhileTheWalkRuns(t *testing.T) {
	src := &paged{gate: make(chan struct{})}
	p := New(src, Options{FirstAnswer: 20 * time.Millisecond})
	ctx := context.Background()

	// The walk is parked on pending page 2; the first answer is page 1's
	// newest todo, already a visible week.
	first, err := p.List(ctx, &pluginv1.ListRequest{Context: todos.RootContext})
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Entries) != 1 || first.Entries[0].Key != "week:2026-08-17" {
		t.Fatalf("first answer = %v, want the newest week so far", first.Entries)
	}

	// The walk finishes behind; a later read has the full history, and the
	// source was paged exactly once per page — the parked walk kept going,
	// never restarted.
	close(src.gate)
	deadline := time.Now().Add(5 * time.Second)
	for {
		root, err := p.List(ctx, &pluginv1.ListRequest{Context: todos.RootContext})
		if err != nil {
			t.Fatal(err)
		}
		if len(root.Entries) == 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the walk never completed behind the first answer: %v", root.Entries)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if n := src.calls.Load(); n != 3 {
		t.Fatalf("source paged %d times, want 3 (pending 1, pending 2, done 1 — one walk)", n)
	}
}

// landed waits out every walk in flight: the root's, each remembered week's,
// and those of the weeks named. A warm read answers before the walk it
// started has landed.
func landed(t *testing.T, p *Plugin, weeks ...string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		keys := append([]string{todos.RootContext}, weeks...)
		for _, w := range p.mem.Weeks() {
			keys = append(keys, todos.WeekKey(w.Start))
		}
		busy := false
		for _, k := range keys {
			busy = busy || p.flights.Busy(k)
		}
		if !busy {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("a walk never landed")
		}
		time.Sleep(time.Millisecond)
	}
}

// warmOver builds a plugin over src whose memory holds the last process's
// walk of pending todos 1 and 2, stamped past the refresh window: warm, and
// not fresh.
func warmOver(t *testing.T, src todos.Source, firstAnswer time.Duration) *Plugin {
	t.Helper()
	dir := t.TempDir()
	clock := at("2026-08-25T12:00:00Z")
	seed := &oneShot{pending: []todos.Todo{mk(1, "2026-08-18T10:00:00Z", "pending"), mk(2, "2026-08-25T10:00:00Z", "pending")}}
	first := New(seed, Options{StateDir: dir, Now: func() time.Time { return clock }})
	if _, err := first.List(context.Background(), &pluginv1.ListRequest{Context: todos.RootContext}); err != nil {
		t.Fatal(err)
	}
	later := clock.Add(DefaultFullRefresh + time.Second)
	return New(src, Options{StateDir: dir, FirstAnswer: firstAnswer, Now: func() time.Time { return later }})
}

// A read over a memory that has anything to show answers it at once, however
// slow the walk behind it: the refresher walks for longer than its window on
// a real history, so a read that waited on the walk paid the first-answer
// bound on nearly every listing. The walk still runs and lands.
func TestAWarmReadNeverWaitsOnTheWalk(t *testing.T) {
	src := &gated{gate: make(chan struct{})}
	p := warmOver(t, src, time.Hour)
	ctx := context.Background()
	for _, key := range []string{todos.RootContext, "week:2026-08-17"} {
		answered := make(chan *pluginv1.ListResponse, 1)
		go func() {
			resp, err := p.List(ctx, &pluginv1.ListRequest{Context: key})
			if err != nil {
				t.Error(err)
			}
			answered <- resp
		}()
		select {
		case resp := <-answered:
			if len(resp.GetEntries()) == 0 {
				t.Errorf("%s: a warm read answered nothing", key)
			}
		case <-time.After(time.Second):
			t.Fatalf("%s: a warm read waited on a blocked walk", key)
		}
	}
	deadline := time.Now().Add(5 * time.Second)
	for src.calls.Load() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the warm read started no walk")
		}
		time.Sleep(time.Millisecond)
	}
	// gated serves only todo 1, so a landed walk judges todo 2 done.
	close(src.gate)
	landed(t, p)
	root, err := p.List(ctx, &pluginv1.ListRequest{Context: todos.RootContext})
	if err != nil {
		t.Fatal(err)
	}
	if got := root.SourceLabel; got != displayName+" · 1 open · 1 done" {
		t.Errorf("after the walk landed the root says %q", got)
	}
}

// A week the memory holds nothing for is cold, whatever else it holds: the
// read waits up to the first-answer bound, as a cold start does.
func TestAColdReadStillWaitsForTheFirstAnswer(t *testing.T) {
	src := &gated{gate: make(chan struct{})}
	const bound = 50 * time.Millisecond
	ps := []*Plugin{New(src, Options{FirstAnswer: bound}), warmOver(t, src, bound)}
	for _, p := range ps {
		start := time.Now()
		if _, err := p.List(context.Background(), &pluginv1.ListRequest{Context: "week:2026-01-05"}); err != nil {
			t.Fatal(err)
		}
		if took := time.Since(start); took < bound {
			t.Errorf("a cold read answered in %v, before the first-answer bound", took)
		}
	}
	close(src.gate)
	for _, p := range ps {
		landed(t, p, "week:2026-01-05")
	}
}

// failing answers every page with err until err is cleared.
type failing struct {
	mu  sync.Mutex
	err error
}

func (f *failing) Page(context.Context, string, int) (todos.Reply, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return todos.Reply{}, f.err
	}
	return todos.Reply{Todos: []todos.Todo{mk(1, "2026-08-18T10:00:00Z", "pending")}}, nil
}

// A refresh that fails while memory can answer is the source unreachable,
// never a failed read: the remembered entries answer with the walk's reason
// as unreachable, a revoked token's included, and the reason clears once a
// walk lands.
func TestAWarmReadAnswersMemoryWithTheLastWalksFailure(t *testing.T) {
	src := &failing{err: status.Error(codes.PermissionDenied, "gitlab: 401 Unauthorized")}
	p := warmOver(t, src, time.Hour)
	ctx := context.Background()
	if resp, err := p.List(ctx, &pluginv1.ListRequest{Context: todos.RootContext}); err != nil || resp.Unreachable != "" {
		t.Fatalf("the first warm read = (%q, %v); it answers before its walk fails", resp.GetUnreachable(), err)
	}
	landed(t, p)
	for _, key := range []string{todos.RootContext, "week:2026-08-17"} {
		resp, err := p.List(ctx, &pluginv1.ListRequest{Context: key})
		if err != nil {
			t.Fatalf("%s: the read after a refused walk = %v, want memory", key, err)
		}
		if len(resp.Entries) == 0 || resp.Unreachable != "gitlab: 401 Unauthorized" {
			t.Errorf("%s: the read after a refused walk = %d entries, unreachable %q", key, len(resp.Entries), resp.Unreachable)
		}
	}
	src.mu.Lock()
	src.err = nil
	src.mu.Unlock()
	landed(t, p)
	_, _ = p.List(ctx, &pluginv1.ListRequest{Context: todos.RootContext})
	landed(t, p)
	if resp, err := p.List(ctx, &pluginv1.ListRequest{Context: todos.RootContext}); err != nil || resp.Unreachable != "" {
		t.Errorf("the read after a landed walk = (%q, %v)", resp.GetUnreachable(), err)
	}
}
