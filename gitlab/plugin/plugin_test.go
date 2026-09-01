package plugin

import (
	"context"
	"fmt"
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
	calls         int
}

func (f *oneShot) Page(_ context.Context, state string, page int) ([]todos.Todo, bool, error) {
	f.calls++
	if page > 1 {
		return nil, false, nil
	}
	if state == todos.StateDone {
		return f.done, false, nil
	}
	return f.pending, false, nil
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
	if info.Kind != Kind || info.RootContext != todos.RootContext {
		t.Fatalf("info = %v", info)
	}
	root, err := p.List(ctx, &pluginv1.ListRequest{Context: todos.RootContext})
	if err != nil {
		t.Fatal(err)
	}
	if root.Authoritative || len(root.Entries) != 2 || root.Entries[0].Key != "week:2026-08-24" || root.Entries[1].Label != "2026-08-17 · 1 open · 1 done" {
		t.Fatalf("root = %v", root.Entries)
	}
	if src.calls != 2 {
		t.Fatalf("outset walk made %d calls, want 2 (pending + done)", src.calls)
	}
	// The root walk covered every week: a descent inside the window is
	// answered from memory, no GitLab round trip.
	wk, err := p.List(ctx, &pluginv1.ListRequest{Context: "week:2026-08-17"})
	if err != nil {
		t.Fatal(err)
	}
	if src.calls != 2 {
		t.Errorf("a fresh week re-walked GitLab (%d calls)", src.calls)
	}
	if len(wk.Entries) != 2 || wk.Entries[0].ServesPage || wk.Entries[0].Kind != "text" || wk.Entries[0].Key != "todo:1" {
		t.Fatalf("week = %v", wk.Entries)
	}
	if h := wk.Entries[0].PlacementHint; h == nil || h.X != todos.TodoTileW || h.W != todos.TodoTileW {
		t.Errorf("Tuesday hint = %v", wk.Entries[0].PlacementHint)
	}
	// Todo 1 is marked done in GitLab. Inside the window nothing moves;
	// past it, the descent's targeted walk flips the label.
	src.pending = src.pending[1:]
	wk, _ = p.List(ctx, &pluginv1.ListRequest{Context: "week:2026-08-17"})
	if strings.HasPrefix(wk.Entries[0].Label, "✓") {
		t.Error("state changed inside the refresh window")
	}
	clock = clock.Add(DefaultRefresh + time.Second)
	wk, _ = p.List(ctx, &pluginv1.ListRequest{Context: "week:2026-08-17"})
	if src.calls != 4 || !strings.HasPrefix(wk.Entries[0].Label, "✓ !1 ") || wk.Entries[0].StatusDetail != todos.StateDone {
		t.Errorf("after the window: calls=%d entry=%v", src.calls, wk.Entries[0])
	}
	// The todo did not go away.
	if len(wk.Entries) != 2 {
		t.Errorf("a done todo left the listing: %v", wk.Entries)
	}
	root, _ = p.List(ctx, &pluginv1.ListRequest{Context: todos.RootContext})
	if root.Entries[1].Label != "2026-08-17 · 0 open · 2 done" {
		t.Errorf("root after flip = %q", root.Entries[1].Label)
	}
}

// TestReadContentBeforeFirstWalkIsUnavailable: a fresh process asked for a
// todo it has not yet seen must answer "not right now", never a Gone body.
// The node serves reads from its cache while the first walk runs, so this
// read can arrive before the walk lands — and a Gone body would be a live
// success the cache stores over the remembered markdown.
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
	if cold.calls != 0 {
		t.Errorf("the restart walked GitLab %d times; the last walk is still inside the refresh window", cold.calls)
	}
	if len(root.Entries) != 2 || root.Entries[1].Label != "2026-08-17 · 1 open · 1 done" {
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

// The restored walk time is not a licence to stop walking: past the refresh
// window, and for a stamp a stepped-back clock puts in the future, the restart
// walks GitLab like any other read.
func TestARestoredWalkTimeStillExpires(t *testing.T) {
	dir := t.TempDir()
	clock := at("2026-08-25T12:00:00Z")
	src := &oneShot{pending: []todos.Todo{mk(1, "2026-08-18T10:00:00Z", "pending")}}
	first := New(src, Options{StateDir: dir, Now: func() time.Time { return clock }})
	if _, err := first.List(context.Background(), &pluginv1.ListRequest{Context: todos.RootContext}); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		now  time.Time
	}{
		{"past the window", clock.Add(DefaultRefresh + time.Second)},
		{"a clock stepped back", clock.Add(-time.Hour)},
	} {
		next := &oneShot{pending: src.pending}
		p := New(next, Options{StateDir: dir, Now: func() time.Time { return tc.now }})
		if _, err := p.List(context.Background(), &pluginv1.ListRequest{Context: todos.RootContext}); err != nil {
			t.Fatal(err)
		}
		if next.calls == 0 {
			t.Errorf("%s: the restart served a stale memory instead of walking", tc.name)
		}
	}
}

// Without a state_dir the plugin behaves exactly as it did: memory only, no
// file, and an unknown todo before the first walk is still Unavailable.
func TestNoStateDirWritesNothingAndStaysCold(t *testing.T) {
	p := New(&oneShot{pending: []todos.Todo{mk(1, "2026-08-18T10:00:00Z", "pending")}}, Options{})
	if p.cache != "" {
		t.Errorf("cache path = %q with no state_dir", p.cache)
	}
	if _, err := p.List(context.Background(), &pluginv1.ListRequest{Context: todos.RootContext}); err != nil {
		t.Fatal(err)
	}
	cold := New(&oneShot{}, Options{StateDir: " "})
	if cold.cache != "" {
		t.Errorf("a blank state_dir became the path %q", cold.cache)
	}
	r := &reader{}
	if err := cold.ReadContent(&pluginv1.ReadContentRequest{Key: "todo:1"}, r); status.Code(err) != codes.Unavailable {
		t.Errorf("cold read = %v, want Unavailable", err)
	}
}

// A cache that cannot be read or written is reported, never swallowed, and
// never fails the read that provoked it.
func TestAnUnusableCacheIsReportedAndTheWalkStillAnswers(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, todos.CacheFile), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	var lines []string
	logf := func(format string, args ...any) { lines = append(lines, fmt.Sprintf(format, args...)) }
	p := New(&oneShot{pending: []todos.Todo{mk(1, "2026-08-18T10:00:00Z", "pending")}}, Options{StateDir: dir, Logf: logf})
	if len(lines) != 1 || !strings.Contains(lines[0], "cache") {
		t.Fatalf("a corrupt cache logged %v, want one report", lines)
	}
	root, err := p.List(context.Background(), &pluginv1.ListRequest{Context: todos.RootContext})
	if err != nil || len(root.Entries) != 1 {
		t.Fatalf("the walk after a corrupt cache = (%v, %v)", root.GetEntries(), err)
	}

	// A directory the plugin cannot write reports on every walk and still
	// answers from GitLab.
	blocked := filepath.Join(t.TempDir(), "blocked")
	if err := os.Mkdir(blocked, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(blocked, 0o700) })
	lines = nil
	q := New(&oneShot{pending: []todos.Todo{mk(1, "2026-08-18T10:00:00Z", "pending")}}, Options{StateDir: filepath.Join(blocked, "gitlab"), Logf: logf})
	root, err = q.List(context.Background(), &pluginv1.ListRequest{Context: todos.RootContext})
	if err != nil || len(root.Entries) != 1 {
		t.Fatalf("the walk with an unwritable cache = (%v, %v)", root.GetEntries(), err)
	}
	if len(lines) == 0 {
		t.Error("an unwritable cache was swallowed")
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

func (g *gated) Page(_ context.Context, state string, page int) ([]todos.Todo, bool, error) {
	g.calls.Add(1)
	<-g.gate
	if page > 1 || state == todos.StateDone {
		return nil, false, nil
	}
	return []todos.Todo{mk(1, "2026-08-18T10:00:00Z", "pending")}, false, nil
}

// A burst of concurrent Lists on one cold context shares ONE walk: the
// node lists a context on every GetGrid/GetTile, and two panes opening
// the same grid must not each page GitLab.
func TestConcurrentListsShareOneWalk(t *testing.T) {
	src := &gated{gate: make(chan struct{})}
	p := New(src, Options{})
	ctx := context.Background()
	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i := range errs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = p.List(ctx, &pluginv1.ListRequest{Context: todos.RootContext})
		}(i)
	}
	// Both goroutines are in List before the walk is released.
	deadline := time.Now().Add(5 * time.Second)
	for src.calls.Load() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("no walk started")
		}
		time.Sleep(time.Millisecond)
	}
	time.Sleep(20 * time.Millisecond) // let the second List reach sync
	close(src.gate)
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Errorf("List %d: %v", i, err)
		}
	}
	if n := src.calls.Load(); n != 2 {
		t.Errorf("GitLab saw %d page calls, want 2 (one pending + one done page: one shared walk)", n)
	}
}

// paged is a source whose pending list is two pages, the second parked
// behind a gate — a slow GitLab mid-walk. Calls are counted at entry.
type paged struct {
	gate  chan struct{}
	calls atomic.Int32
}

func (s *paged) Page(ctx context.Context, state string, page int) ([]todos.Todo, bool, error) {
	s.calls.Add(1)
	if state == todos.StatePending {
		switch page {
		case 1:
			return []todos.Todo{mk(1, "2026-08-18T10:00:00Z", "pending")}, true, nil
		case 2:
			select {
			case <-s.gate:
			case <-ctx.Done():
				return nil, false, ctx.Err()
			}
			return []todos.Todo{mk(2, "2026-08-11T10:00:00Z", "pending")}, false, nil
		}
	}
	return nil, false, nil
}

// TestListStreamsWhileTheWalkRuns: a cold walk over a real history runs
// minutes, and a List that waits for all of it shows the user nothing the
// whole time. The walk detaches from its caller and List answers what memory
// holds after FirstAnswer — GitLab pages newest-first, so the first answer is
// the most recent weeks, and the node's refresh paints the rest in as pages
// land. One walk serves it all: no restarts, no per-reader paging.
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
