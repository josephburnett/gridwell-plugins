package plugin

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/josephburnett/gridwell-plugins/hey/heycli"
	"github.com/josephburnett/gridwell-plugins/hey/mail"
	pluginv1 "github.com/josephburnett/gridwell/api/gen/plugin/v1"
	"github.com/josephburnett/gridwell/api/rpc"
)

func at(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return t
}

func th(id int64, subject, created string) mail.Thread {
	return mail.Thread{
		TopicID: id, PostingID: id * 10, Subject: subject,
		Summary: "about " + subject, FromName: "Alice", FromEmail: "alice@example.com",
		CreatedAt: at(created),
	}
}

// fakeHEY answers boxes from a map and counts what was asked for.
type fakeHEY struct {
	mu    sync.Mutex
	boxes map[string][]mail.Thread
	whole map[string]bool // absent means whole
	html  map[int64]string
	err   error
	calls map[string]int
	block chan struct{} // when non-nil, Box waits on it
}

func newFake() *fakeHEY {
	return &fakeHEY{boxes: map[string][]mail.Thread{}, whole: map[string]bool{},
		html: map[int64]string{}, calls: map[string]int{}}
}

func (f *fakeHEY) Box(_ context.Context, box string) ([]mail.Thread, bool, error) {
	if f.block != nil {
		<-f.block
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls[box]++
	if f.err != nil {
		return nil, false, f.err
	}
	whole := true
	if w, ok := f.whole[box]; ok {
		whole = w
	}
	return f.boxes[box], whole, nil
}

func (f *fakeHEY) ThreadHTML(_ context.Context, id int64) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls["thread"]++
	if f.err != nil {
		return nil, f.err
	}
	return []byte(f.html[id]), nil
}

func (f *fakeHEY) count(k string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[k]
}

// reader collects a ReadContent stream.
type reader struct {
	pluginv1.Plugin_ReadContentServer
	chunks []*pluginv1.ContentChunk
}

func (r *reader) Send(c *pluginv1.ContentChunk) error { r.chunks = append(r.chunks, c); return nil }
func (r *reader) Context() context.Context            { return context.Background() }

// server collects a ServeContent stream.
type server struct {
	pluginv1.Plugin_ServeContentServer
	chunks []*pluginv1.ServeContentChunk
}

func (s *server) Send(c *pluginv1.ServeContentChunk) error {
	s.chunks = append(s.chunks, c)
	return nil
}
func (s *server) Context() context.Context { return context.Background() }

// stable is a plugin whose clock does not move, so nothing refreshes behind a
// test's back.
func stable(src Source, o Options) *Plugin {
	if o.Now == nil {
		now := at("2026-01-06T12:00:00Z")
		o.Now = func() time.Time { return now }
	}
	if o.FirstAnswer == 0 {
		o.FirstAnswer = 5 * time.Second
	}
	if o.Logf == nil {
		o.Logf = func(string, ...any) {}
	}
	return New(src, o)
}

// The three collections are three contexts and three (+) menu entries. There
// is no wrapper grid above them and no landing among them: a plugin is not a
// place, it contributes doorways, and each collection is one.
func TestInfoDeclaresEveryCollectionAsAMenuEntry(t *testing.T) {
	p := stable(newFake(), Options{})
	info, err := p.Info(context.Background(), &pluginv1.InfoRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if info.Kind != Kind {
		t.Errorf("kind = %q", info.Kind)
	}
	if info.RootContext != "" {
		t.Errorf("root_context = %q; it is retired and the collections are declared", info.RootContext)
	}
	if !info.HostContent {
		t.Error("host_content not declared; these grids project a mail account")
	}
	if info.Writable {
		t.Error("a read-only projection declared itself writable")
	}
	got := map[string]bool{}
	for _, e := range info.MenuEntries {
		got[e.Context] = true
	}
	if len(info.MenuEntries) != 3 || !got[mail.ImboxContext] ||
		!got[mail.ReplyLaterContext] || !got[mail.SetAsideContext] {
		t.Fatalf("menu entries = %+v, want one per collection", info.MenuEntries)
	}
}

func TestListsEachCollectionAndRefreshesOnAWindow(t *testing.T) {
	f := newFake()
	f.boxes["imbox"] = []mail.Thread{th(1, "lunch", "2026-01-05T14:00:00Z")}
	f.boxes["laterbox"] = []mail.Thread{th(2, "invoice", "2026-01-04T09:00:00Z")}
	f.boxes["asidebox"] = []mail.Thread{th(3, "recipe", "2026-01-03T09:00:00Z")}
	clock := at("2026-01-06T12:00:00Z")
	p := stable(f, Options{Refresh: time.Minute, Now: func() time.Time { return clock }})
	ctx := context.Background()

	for _, c := range mail.Collections {
		resp, err := p.List(ctx, &pluginv1.ListRequest{Context: c.Key})
		if err != nil {
			t.Fatalf("%s: %v", c.Key, err)
		}
		if resp.Authoritative {
			t.Errorf("%s listed authoritatively; absence is Probe's answer", c.Key)
		}
		if len(resp.Entries) != 1 {
			t.Fatalf("%s: %d entries", c.Key, len(resp.Entries))
		}
		if !strings.HasPrefix(resp.SourceLabel, c.Label) {
			t.Errorf("%s: source label %q", c.Key, resp.SourceLabel)
		}
	}
	if got := f.count("imbox"); got != 1 {
		t.Fatalf("imbox walked %d times", got)
	}
	// Inside the window: memory answers, HEY is not asked again.
	if _, err := p.List(ctx, &pluginv1.ListRequest{Context: mail.ImboxContext}); err != nil {
		t.Fatal(err)
	}
	if got := f.count("imbox"); got != 1 {
		t.Fatalf("a fresh listing walked again (%d)", got)
	}
	// Past it: one more walk.
	clock = clock.Add(2 * time.Minute)
	if _, err := p.List(ctx, &pluginv1.ListRequest{Context: mail.ImboxContext}); err != nil {
		t.Fatal(err)
	}
	if got := f.count("imbox"); got != 2 {
		t.Fatalf("a stale listing walked %d times", got)
	}
}

func TestUnknownContextIsRefused(t *testing.T) {
	p := stable(newFake(), Options{})
	_, err := p.List(context.Background(), &pluginv1.ListRequest{Context: "box:trailbox"})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("err = %v, want InvalidArgument", err)
	}
}

// A burst of readers costs HEY one CLI run, not one per reader: the node
// lists a context on every GetGrid and GetTile.
func TestOneWalkServesABurst(t *testing.T) {
	f := newFake()
	f.block = make(chan struct{})
	f.boxes["imbox"] = []mail.Thread{th(1, "lunch", "2026-01-05T14:00:00Z")}
	p := stable(f, Options{})
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = p.List(context.Background(), &pluginv1.ListRequest{Context: mail.ImboxContext})
		}()
	}
	time.Sleep(50 * time.Millisecond)
	close(f.block)
	wg.Wait()
	if got := f.count("imbox"); got != 1 {
		t.Fatalf("a burst of 8 readers cost %d walks", got)
	}
}

// A slow walk must not hold the grid: the reader answers with what memory
// holds and the walk runs on behind it.
func TestASlowWalkAnswersFromMemory(t *testing.T) {
	f := newFake()
	f.block = make(chan struct{})
	p := stable(f, Options{FirstAnswer: 20 * time.Millisecond})
	start := time.Now()
	resp, err := p.List(context.Background(), &pluginv1.ListRequest{Context: mail.ImboxContext})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Entries) != 0 {
		t.Fatalf("got %d entries from a memory that has never been walked", len(resp.Entries))
	}
	if d := time.Since(start); d > time.Second {
		t.Fatalf("the reader waited %s for the walk", d)
	}
	close(f.block)
}

// A thread is a text tile that serves a page. The markdown is the card; the
// page is the email.
func TestReadContentIsTheCardAndServeContentIsTheEmail(t *testing.T) {
	f := newFake()
	f.boxes["imbox"] = []mail.Thread{th(1, "lunch", "2026-01-05T14:00:00Z")}
	f.html[1] = "<!doctype html><html><body><article>lunch</article></body></html>"
	p := stable(f, Options{})
	ctx := context.Background()
	if _, err := p.List(ctx, &pluginv1.ListRequest{Context: mail.ImboxContext}); err != nil {
		t.Fatal(err)
	}

	r := &reader{}
	if err := p.ReadContent(&pluginv1.ReadContentRequest{Key: "thread:1"}, r); err != nil {
		t.Fatal(err)
	}
	if len(r.chunks) != 1 || r.chunks[0].MediaType != "text/markdown" {
		t.Fatalf("chunks = %+v", r.chunks)
	}
	if !strings.Contains(string(r.chunks[0].Data), "# ") || !strings.Contains(string(r.chunks[0].Data), "lunch") {
		t.Errorf("card = %q", r.chunks[0].Data)
	}

	s := &server{}
	if err := p.ServeContent(&pluginv1.ServeContentRequest{Key: "thread:1"}, s); err != nil {
		t.Fatal(err)
	}
	if len(s.chunks) != 1 || s.chunks[0].Status != 200 || !strings.HasPrefix(s.chunks[0].MediaType, "text/html") {
		t.Fatalf("chunks = %+v", s.chunks)
	}
	if string(s.chunks[0].Data) != f.html[1] {
		t.Errorf("page = %q", s.chunks[0].Data)
	}
}

// An email names no relative resources, so any subpath is an ordinary miss —
// and it must not spend a CLI run finding that out.
func TestServeContentAnswers404ForASubpath(t *testing.T) {
	f := newFake()
	p := stable(f, Options{})
	s := &server{}
	if err := p.ServeContent(&pluginv1.ServeContentRequest{Key: "thread:1", Subpath: "logo.png"}, s); err != nil {
		t.Fatal(err)
	}
	if len(s.chunks) != 1 || s.chunks[0].Status != 404 {
		t.Fatalf("chunks = %+v", s.chunks)
	}
	if got := f.count("thread"); got != 0 {
		t.Errorf("a subpath cost %d CLI runs", got)
	}
}

// A thread HEY served no body for gets a page that says so, not a blank one.
func TestServeContentSaysWhenThereIsNoBody(t *testing.T) {
	f := newFake()
	f.boxes["imbox"] = []mail.Thread{th(1, "lunch", "2026-01-05T14:00:00Z")}
	p := stable(f, Options{})
	if _, err := p.List(context.Background(), &pluginv1.ListRequest{Context: mail.ImboxContext}); err != nil {
		t.Fatal(err)
	}
	s := &server{}
	if err := p.ServeContent(&pluginv1.ServeContentRequest{Key: "thread:1"}, s); err != nil {
		t.Fatal(err)
	}
	if len(s.chunks) != 1 || s.chunks[0].Status != 200 {
		t.Fatalf("chunks = %+v", s.chunks)
	}
	body := string(s.chunks[0].Data)
	if !strings.Contains(body, "lunch") || !strings.Contains(body, "no body") {
		t.Errorf("page = %q", body)
	}
}

// A failure to read the email surfaces. A blank page would look like an email
// with nothing in it.
func TestServeContentSurfacesAFailure(t *testing.T) {
	f := newFake()
	f.err = status.Error(codes.PermissionDenied, "Not logged in")
	p := stable(f, Options{})
	err := p.ServeContent(&pluginv1.ServeContentRequest{Key: "thread:1"}, &server{})
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("err = %v", err)
	}
}

// Nothing is GONE until every collection has been read to its end: a thread
// missing from the Imbox is usually in Reply Later, and retiring its id would
// cost the user the tile and its placement.
func TestProbeOnlySaysGoneAfterAWholeSweep(t *testing.T) {
	f := newFake()
	f.boxes["imbox"] = []mail.Thread{th(1, "lunch", "2026-01-05T14:00:00Z")}
	p := stable(f, Options{})
	ctx := context.Background()

	// Nothing walked yet: cannot say.
	got, _ := p.Probe(ctx, &pluginv1.ProbeRequest{Key: "thread:9"})
	if got.Presence != pluginv1.ProbeResponse_PRESENCE_UNSPECIFIED {
		t.Fatalf("cold probe = %v", got.Presence)
	}
	// One collection walked: still cannot say about a thread it did not hold.
	if _, err := p.List(ctx, &pluginv1.ListRequest{Context: mail.ImboxContext}); err != nil {
		t.Fatal(err)
	}
	got, _ = p.Probe(ctx, &pluginv1.ProbeRequest{Key: "thread:1"})
	if got.Presence != pluginv1.ProbeResponse_PRESENCE_PRESENT {
		t.Fatalf("a listed thread probed %v", got.Presence)
	}
	got, _ = p.Probe(ctx, &pluginv1.ProbeRequest{Key: "thread:9"})
	if got.Presence != pluginv1.ProbeResponse_PRESENCE_UNSPECIFIED {
		t.Fatalf("half-swept probe = %v", got.Presence)
	}
	// Every collection walked to its end: now absence is an answer.
	for _, c := range mail.Collections[1:] {
		if _, err := p.List(ctx, &pluginv1.ListRequest{Context: c.Key}); err != nil {
			t.Fatal(err)
		}
	}
	got, _ = p.Probe(ctx, &pluginv1.ProbeRequest{Key: "thread:9"})
	if got.Presence != pluginv1.ProbeResponse_PRESENCE_GONE {
		t.Fatalf("swept probe = %v", got.Presence)
	}
	// A key this plugin never mints is not ours at all.
	got, _ = p.Probe(ctx, &pluginv1.ProbeRequest{Key: "box:imbox"})
	if got.Presence != pluginv1.ProbeResponse_PRESENCE_GONE {
		t.Fatalf("foreign key probed %v", got.Presence)
	}
}

// A capped read is not a whole one, so it can never make a thread GONE.
func TestACappedWalkNeverSweeps(t *testing.T) {
	f := newFake()
	f.whole["asidebox"] = false
	p := stable(f, Options{})
	ctx := context.Background()
	for _, c := range mail.Collections {
		if _, err := p.List(ctx, &pluginv1.ListRequest{Context: c.Key}); err != nil {
			t.Fatal(err)
		}
	}
	got, _ := p.Probe(ctx, &pluginv1.ProbeRequest{Key: "thread:9"})
	if got.Presence != pluginv1.ProbeResponse_PRESENCE_UNSPECIFIED {
		t.Fatalf("a capped sweep probed %v", got.Presence)
	}
}

// A thread that moves from the Imbox to Reply Later keeps its key, so the
// node keeps its id and every link to it still resolves.
func TestAThreadKeepsItsKeyAcrossCollections(t *testing.T) {
	f := newFake()
	f.boxes["imbox"] = []mail.Thread{th(1, "lunch", "2026-01-05T14:00:00Z")}
	clock := at("2026-01-06T12:00:00Z")
	p := stable(f, Options{Refresh: time.Minute, Now: func() time.Time { return clock }})
	ctx := context.Background()
	for _, c := range mail.Collections {
		if _, err := p.List(ctx, &pluginv1.ListRequest{Context: c.Key}); err != nil {
			t.Fatal(err)
		}
	}
	f.mu.Lock()
	f.boxes["imbox"] = nil
	f.boxes["laterbox"] = []mail.Thread{th(1, "lunch", "2026-01-05T14:00:00Z")}
	f.mu.Unlock()
	clock = clock.Add(2 * time.Minute)
	for _, c := range mail.Collections {
		if _, err := p.List(ctx, &pluginv1.ListRequest{Context: c.Key}); err != nil {
			t.Fatal(err)
		}
	}
	later, err := p.List(ctx, &pluginv1.ListRequest{Context: mail.ReplyLaterContext})
	if err != nil {
		t.Fatal(err)
	}
	if len(later.Entries) != 1 || later.Entries[0].Key != "thread:1" {
		t.Fatalf("reply later = %+v", later.Entries)
	}
	got, _ := p.Probe(ctx, &pluginv1.ProbeRequest{Key: "thread:1"})
	if got.Presence != pluginv1.ProbeResponse_PRESENCE_PRESENT {
		t.Fatalf("a moved thread probed %v", got.Presence)
	}
}

// A failed walk is the CLI's verdict, unchanged: "not signed in" must reach
// the user rather than becoming an empty grid.
func TestAWalkFailureSurfaces(t *testing.T) {
	f := newFake()
	f.err = status.Error(codes.PermissionDenied, "Not logged in")
	p := stable(f, Options{})
	_, err := p.List(context.Background(), &pluginv1.ListRequest{Context: mail.ImboxContext})
	if status.Code(err) != codes.PermissionDenied || !strings.Contains(err.Error(), "Not logged in") {
		t.Fatalf("err = %v", err)
	}
}

// Before any sweep, a key the memory does not hold is "not yet", not "gone":
// a Gone body stored over the node's remembered one would be a loss.
func TestReadContentWaitsRatherThanDeclaringAThreadGone(t *testing.T) {
	p := stable(newFake(), Options{})
	err := p.ReadContent(&pluginv1.ReadContentRequest{Key: "thread:1"}, &reader{})
	if status.Code(err) != codes.Unavailable {
		t.Fatalf("err = %v, want Unavailable", err)
	}
}

func TestDeleteIsRefusedWithItsReason(t *testing.T) {
	p := stable(newFake(), Options{})
	_, err := p.Delete(context.Background(), &pluginv1.DeleteRequest{Key: "thread:1"})
	if status.Code(err) != codes.Unimplemented {
		t.Fatalf("err = %v", err)
	}
	if !strings.Contains(err.Error(), "read-only") {
		t.Errorf("the refusal did not say why: %v", err)
	}
}

// The cache is the plugin's memory of HEY across a restart: the next process
// answers the same listing without running the CLI at all.
func TestTheCacheSurvivesARestart(t *testing.T) {
	dir := t.TempDir()
	f := newFake()
	f.boxes["imbox"] = []mail.Thread{th(1, "lunch", "2026-01-05T14:00:00Z")}
	clock := at("2026-01-06T12:00:00Z")
	opts := Options{StateDir: dir, Refresh: time.Hour, Now: func() time.Time { return clock },
		Logf: func(string, ...any) {}}
	p := stable(f, opts)
	ctx := context.Background()
	for _, c := range mail.Collections {
		if _, err := p.List(ctx, &pluginv1.ListRequest{Context: c.Key}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, mail.CacheFile)); err != nil {
		t.Fatalf("no cache file: %v", err)
	}

	cold := newFake() // a source that answers nothing: only the cache can
	back := stable(cold, opts)
	resp, err := back.List(ctx, &pluginv1.ListRequest{Context: mail.ImboxContext})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Entries) != 1 || resp.Entries[0].Key != "thread:1" {
		t.Fatalf("restored listing = %+v", resp.Entries)
	}
	if got := cold.count("imbox"); got != 0 {
		t.Errorf("a restart inside the refresh window walked %d times", got)
	}
	// Completeness rides the file too, so a restart can still say GONE.
	got, _ := back.Probe(ctx, &pluginv1.ProbeRequest{Key: "thread:9"})
	if got.Presence != pluginv1.ProbeResponse_PRESENCE_GONE {
		t.Errorf("a restored sweep probed %v", got.Presence)
	}
}

// The state directory is disposable: deleting it must cost a sweep, never a
// start-up.
func TestAnUnreadableCacheStartsColdAndSaysSo(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, mail.CacheFile), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	var lines []string
	f := newFake()
	f.boxes["imbox"] = []mail.Thread{th(1, "lunch", "2026-01-05T14:00:00Z")}
	p := stable(f, Options{StateDir: dir, Logf: func(format string, args ...any) {
		lines = append(lines, format)
	}})
	if len(lines) == 0 {
		t.Fatal("an unreadable cache was swallowed")
	}
	resp, err := p.List(context.Background(), &pluginv1.ListRequest{Context: mail.ImboxContext})
	if err != nil || len(resp.Entries) != 1 {
		t.Fatalf("cold start did not recover: %v %+v", err, resp)
	}
}

func TestSearchReadsMemoryOnly(t *testing.T) {
	f := newFake()
	f.boxes["laterbox"] = []mail.Thread{th(2, "invoice", "2026-01-04T09:00:00Z")}
	p := stable(f, Options{})
	ctx := context.Background()
	if _, err := p.List(ctx, &pluginv1.ListRequest{Context: mail.ReplyLaterContext}); err != nil {
		t.Fatal(err)
	}
	before := f.count("laterbox")
	res, err := p.Search(ctx, &pluginv1.SearchRequest{Query: "invoice"})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Results) != 1 || res.Results[0].Entry.Key != "thread:2" {
		t.Fatalf("results = %+v", res.Results)
	}
	if got := res.Results[0].ContextPath; len(got) != 1 || got[0] != mail.ReplyLaterContext {
		t.Errorf("context path = %v", got)
	}
	if f.count("laterbox") != before {
		t.Error("search walked HEY")
	}
	empty, _ := p.Search(ctx, &pluginv1.SearchRequest{Query: "  "})
	if len(empty.Results) != 0 {
		t.Error("an empty query matched")
	}
}

func TestFromConfigTakesNoRequiredKeysAndRefusesABadRefresh(t *testing.T) {
	if _, err := FromConfig(map[string]string{}); err != nil {
		t.Fatalf("a zero-config launch was refused: %v", err)
	}
	if _, err := FromConfig(map[string]string{"refresh": "soon"}); err == nil {
		t.Fatal("FromConfig accepted a refresh that is not a duration")
	}
	if _, err := FromConfig(map[string]string{"state_dir": t.TempDir(), "refresh": "30s"}); err != nil {
		t.Fatalf("FromConfig: %v", err)
	}
}

// The seam a fake Source cannot cross: the real heycli.Exec, running the
// executable CLI contract, feeding the real memory and entry derivation. A
// unit test on each side of "what the CLI prints" would not catch a change
// to the shape it prints.
func TestOverTheRealCLIContract(t *testing.T) {
	bin, err := filepath.Abs(filepath.Join("..", "heycli", "testdata", "fake-hey"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(bin); err != nil {
		t.Fatalf("the CLI contract is missing: %v", err)
	}
	p := stable(heycli.New(heycli.Exec{Binary: bin}), Options{StateDir: t.TempDir()})
	ctx := context.Background()

	resp, err := p.List(ctx, &pluginv1.ListRequest{Context: mail.ImboxContext})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	// One thread and one bundle came back; a bundle names no thread, so it is
	// not a tile.
	if len(resp.Entries) != 1 {
		t.Fatalf("entries = %+v", resp.Entries)
	}
	e := resp.Entries[0]
	if e.Key != "thread:101" || !e.ServesPage || e.Kind != rpc.KindText {
		t.Fatalf("entry = %+v", e)
	}
	if !strings.Contains(e.Label, "Alice") || !strings.Contains(e.Label, "Lunch plans") {
		t.Errorf("label = %q", e.Label)
	}
	if !strings.HasPrefix(e.Label, mail.UnseenMark) {
		t.Errorf("an unseen thread lost its mark: %q", e.Label)
	}

	s := &server{}
	if err := p.ServeContent(&pluginv1.ServeContentRequest{Key: "thread:101"}, s); err != nil {
		t.Fatalf("ServeContent: %v", err)
	}
	if len(s.chunks) != 1 || s.chunks[0].Status != 200 {
		t.Fatalf("chunks = %+v", s.chunks)
	}
	if !strings.Contains(string(s.chunks[0].Data), "data-entry-id") {
		t.Errorf("the email did not arrive: %q", s.chunks[0].Data)
	}

	// laterbox reports a cursor, so the sweep is never whole and nothing can
	// be declared gone through it.
	for _, c := range mail.Collections[1:] {
		if _, err := p.List(ctx, &pluginv1.ListRequest{Context: c.Key}); err != nil {
			t.Fatalf("%s: %v", c.Key, err)
		}
	}
	got, _ := p.Probe(ctx, &pluginv1.ProbeRequest{Key: "thread:999"})
	if got.Presence != pluginv1.ProbeResponse_PRESENCE_UNSPECIFIED {
		t.Fatalf("a capped sweep probed %v", got.Presence)
	}
}
