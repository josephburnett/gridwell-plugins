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

	"github.com/josephburnett/gridwell-plugins/gmail/mailbox"
	pluginv1 "github.com/josephburnett/gridwell/api/gen/plugin/v1"
)

func at(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return t
}

func msg(id, subject, date string) mailbox.Message {
	return mailbox.Message{
		ID: id, ThreadID: "t" + id, Subject: subject, Snippet: "about " + subject,
		FromName: "Alice", FromEmail: "alice@example.com", Date: at(date),
	}
}

// fakeGmail answers labels from a map and counts what was asked for.
type fakeGmail struct {
	mu sync.Mutex
	// labels maps a label intersection ("INBOX", "INBOX+UNREAD") to the ids
	// it holds, newest first.
	labels map[string][]string
	whole  map[string]bool // absent means whole
	recs   map[string]mailbox.Message
	html   map[string]string
	err    error
	// headerErr fails Headers only, which is the one failure a walk survives.
	headerErr error
	calls     map[string]int
	block     chan struct{} // when non-nil, Label waits on it
}

func newFake() *fakeGmail {
	return &fakeGmail{labels: map[string][]string{}, whole: map[string]bool{},
		recs: map[string]mailbox.Message{}, html: map[string]string{}, calls: map[string]int{}}
}

func (f *fakeGmail) hold(collection string, ms ...mailbox.Message) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var ids []string
	for i := len(ms) - 1; i >= 0; i-- { // Gmail lists newest first
		ids = append(ids, ms[i].ID)
		f.recs[ms[i].ID] = ms[i]
	}
	f.labels[collection] = ids
}

func (f *fakeGmail) Label(_ context.Context, labelIDs []string, limit int) ([]string, bool, error) {
	if f.block != nil {
		<-f.block
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	key := strings.Join(labelIDs, "+")
	f.calls[key]++
	if f.err != nil {
		return nil, false, f.err
	}
	ids := f.labels[key]
	if len(ids) > limit {
		return ids[:limit], false, nil
	}
	whole := true
	if w, ok := f.whole[key]; ok {
		whole = w
	}
	return ids, whole, nil
}

func (f *fakeGmail) Headers(_ context.Context, id string) (mailbox.Message, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls["headers"]++
	if f.err != nil {
		return mailbox.Message{}, f.err
	}
	if f.headerErr != nil {
		return mailbox.Message{}, f.headerErr
	}
	m, ok := f.recs[id]
	if !ok {
		return mailbox.Message{}, status.Errorf(codes.NotFound, "no message %s", id)
	}
	return m, nil
}

func (f *fakeGmail) HTML(_ context.Context, id string) ([]byte, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls["html"]++
	if f.err != nil {
		return nil, "", f.err
	}
	return []byte(f.html[id]), "text/html; charset=utf-8", nil
}

func (f *fakeGmail) count(k string) int {
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

func listAll(t *testing.T, p *Plugin) {
	t.Helper()
	for _, c := range mailbox.Collections {
		if _, err := p.List(context.Background(), &pluginv1.ListRequest{Context: c.Key}); err != nil {
			t.Fatalf("%s: %v", c.Key, err)
		}
	}
}

// The two collections are two contexts, and the inbox is the root: the
// plugin's own (+) row lands there, and starred rides beside it. An empty
// root_context would draw that row as a broken plugin.
func TestInfoDeclaresTheInboxRootAndOneMenuEntry(t *testing.T) {
	p := stable(newFake(), Options{})
	info, err := p.Info(context.Background(), &pluginv1.InfoRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if info.Kind != Kind {
		t.Errorf("kind = %q", info.Kind)
	}
	if info.RootContext != mailbox.InboxContext {
		t.Errorf("root_context = %q, want %q", info.RootContext, mailbox.InboxContext)
	}
	if !info.HostContent {
		t.Error("host_content not declared; these grids project a mail account")
	}
	if info.Writable {
		t.Error("a read-only projection declared itself writable")
	}
	if len(info.MenuEntries) != 1 || info.MenuEntries[0].Context != mailbox.StarredContext {
		t.Fatalf("menu entries = %+v; the root context must not be offered twice", info.MenuEntries)
	}
}

func TestListsEachCollectionAndRefreshesOnAWindow(t *testing.T) {
	f := newFake()
	f.hold("INBOX", msg("a", "lunch", "2026-01-05T14:00:00Z"))
	f.hold("STARRED", msg("b", "invoice", "2026-01-04T09:00:00Z"))
	clock := at("2026-01-06T12:00:00Z")
	p := stable(f, Options{Refresh: time.Minute, Now: func() time.Time { return clock }})
	ctx := context.Background()

	for _, c := range mailbox.Collections {
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
	if got := f.count("INBOX"); got != 1 {
		t.Fatalf("the inbox was listed %d times", got)
	}
	// Inside the window: memory answers, Gmail is not asked again.
	if _, err := p.List(ctx, &pluginv1.ListRequest{Context: mailbox.InboxContext}); err != nil {
		t.Fatal(err)
	}
	if got := f.count("INBOX"); got != 1 {
		t.Fatalf("a fresh listing walked again (%d)", got)
	}
	// Past it: one more walk.
	clock = clock.Add(2 * time.Minute)
	if _, err := p.List(ctx, &pluginv1.ListRequest{Context: mailbox.InboxContext}); err != nil {
		t.Fatal(err)
	}
	if got := f.count("INBOX"); got != 2 {
		t.Fatalf("a stale listing walked %d times", got)
	}
}

// The walk is a delta: a message's subject, sender and date do not change
// once Gmail has it, so a second walk reads metadata only for what is new.
// Re-reading the whole mailbox every minute is the thing this plugin must
// never do.
func TestASecondWalkOnlyFetchesWhatIsNew(t *testing.T) {
	f := newFake()
	f.hold("INBOX", msg("a", "one", "2026-01-05T09:00:00Z"), msg("b", "two", "2026-01-05T10:00:00Z"))
	clock := at("2026-01-06T12:00:00Z")
	p := stable(f, Options{Refresh: time.Minute, Now: func() time.Time { return clock }})
	ctx := context.Background()
	if _, err := p.List(ctx, &pluginv1.ListRequest{Context: mailbox.InboxContext}); err != nil {
		t.Fatal(err)
	}
	if got := f.count("headers"); got != 2 {
		t.Fatalf("a cold walk read %d metadata, want 2", got)
	}
	f.hold("INBOX", msg("a", "one", "2026-01-05T09:00:00Z"), msg("b", "two", "2026-01-05T10:00:00Z"),
		msg("c", "three", "2026-01-05T11:00:00Z"))
	clock = clock.Add(2 * time.Minute)
	resp, err := p.List(ctx, &pluginv1.ListRequest{Context: mailbox.InboxContext})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Entries) != 3 {
		t.Fatalf("entries = %d", len(resp.Entries))
	}
	if got := f.count("headers"); got != 3 {
		t.Fatalf("the second walk read %d metadata in total, want 3: only the new message is new", got)
	}
}

// The unread mark stays true without re-reading a message: one extra cheap
// listing of the label intersected with UNREAD is the whole mechanism.
func TestUnreadComesFromASecondListing(t *testing.T) {
	f := newFake()
	f.hold("INBOX", msg("a", "one", "2026-01-05T09:00:00Z"))
	f.labels["INBOX+UNREAD"] = []string{"a"}
	clock := at("2026-01-06T12:00:00Z")
	p := stable(f, Options{Refresh: time.Minute, Now: func() time.Time { return clock }})
	ctx := context.Background()

	resp, err := p.List(ctx, &pluginv1.ListRequest{Context: mailbox.InboxContext})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(resp.Entries[0].Label, mailbox.UnreadMark) || resp.Entries[0].StatusDetail != "unread" {
		t.Fatalf("entry = %+v", resp.Entries[0])
	}
	if !strings.Contains(resp.SourceLabel, "1 unread") {
		t.Errorf("source label = %q", resp.SourceLabel)
	}
	// Read at Gmail: no metadata is re-read, and the mark still clears.
	before := f.count("headers")
	f.mu.Lock()
	f.labels["INBOX+UNREAD"] = nil
	f.mu.Unlock()
	clock = clock.Add(2 * time.Minute)
	resp, err = p.List(ctx, &pluginv1.ListRequest{Context: mailbox.InboxContext})
	if err != nil {
		t.Fatal(err)
	}
	if strings.HasPrefix(resp.Entries[0].Label, mailbox.UnreadMark) {
		t.Errorf("a message read at Gmail kept its mark: %q", resp.Entries[0].Label)
	}
	if f.count("headers") != before {
		t.Error("clearing an unread mark cost a metadata read")
	}
}

// A starred message that is also in the inbox reads the same on both grids.
// One fact, one owner: the state is the memory's, not a copy in each row.
func TestOneMessageReadsTheSameOnBothGrids(t *testing.T) {
	f := newFake()
	m := msg("a", "lunch", "2026-01-05T09:00:00Z")
	f.hold("INBOX", m)
	f.hold("STARRED", m)
	p := stable(f, Options{})
	ctx := context.Background()
	listAll(t, p) // both walked: the star is a memory fact, not a walk's order

	inbox, err := p.List(ctx, &pluginv1.ListRequest{Context: mailbox.InboxContext})
	if err != nil {
		t.Fatal(err)
	}
	starred, err := p.List(ctx, &pluginv1.ListRequest{Context: mailbox.StarredContext})
	if err != nil {
		t.Fatal(err)
	}
	if len(inbox.Entries) != 1 || len(starred.Entries) != 1 {
		t.Fatalf("entries = %d / %d", len(inbox.Entries), len(starred.Entries))
	}
	if inbox.Entries[0].Key != starred.Entries[0].Key {
		t.Fatalf("keys = %q / %q; one message, one key", inbox.Entries[0].Key, starred.Entries[0].Key)
	}
	if inbox.Entries[0].Label != starred.Entries[0].Label {
		t.Errorf("one message read two ways: %q vs %q", inbox.Entries[0].Label, starred.Entries[0].Label)
	}
	if !strings.HasPrefix(inbox.Entries[0].Label, mailbox.StarMark) {
		t.Errorf("a starred message lost its star in the inbox: %q", inbox.Entries[0].Label)
	}
}

func TestUnknownContextIsRefused(t *testing.T) {
	p := stable(newFake(), Options{})
	_, err := p.List(context.Background(), &pluginv1.ListRequest{Context: "label:SPAM"})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("err = %v, want InvalidArgument", err)
	}
}

// A burst of readers costs Gmail one walk, not one per reader: the node lists
// a context on every GetGrid and GetTile.
func TestOneWalkServesABurst(t *testing.T) {
	f := newFake()
	f.block = make(chan struct{})
	f.hold("INBOX", msg("a", "lunch", "2026-01-05T14:00:00Z"))
	p := stable(f, Options{})
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = p.List(context.Background(), &pluginv1.ListRequest{Context: mailbox.InboxContext})
		}()
	}
	time.Sleep(50 * time.Millisecond)
	close(f.block)
	wg.Wait()
	if got := f.count("INBOX"); got != 1 {
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
	resp, err := p.List(context.Background(), &pluginv1.ListRequest{Context: mailbox.InboxContext})
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

// A message is a text tile that serves a page. The markdown is the card; the
// page is the email.
func TestReadContentIsTheCardAndServeContentIsTheEmail(t *testing.T) {
	f := newFake()
	f.hold("INBOX", msg("a", "lunch", "2026-01-05T14:00:00Z"))
	f.html["a"] = "<div>Are you free friday?</div>"
	p := stable(f, Options{})
	ctx := context.Background()
	if _, err := p.List(ctx, &pluginv1.ListRequest{Context: mailbox.InboxContext}); err != nil {
		t.Fatal(err)
	}

	r := &reader{}
	if err := p.ReadContent(&pluginv1.ReadContentRequest{Key: "msg:a"}, r); err != nil {
		t.Fatal(err)
	}
	if len(r.chunks) != 1 || r.chunks[0].MediaType != "text/markdown" {
		t.Fatalf("chunks = %+v", r.chunks)
	}
	if !strings.Contains(string(r.chunks[0].Data), "# ") || !strings.Contains(string(r.chunks[0].Data), "lunch") {
		t.Errorf("card = %q", r.chunks[0].Data)
	}

	s := &server{}
	if err := p.ServeContent(&pluginv1.ServeContentRequest{Key: "msg:a"}, s); err != nil {
		t.Fatal(err)
	}
	if len(s.chunks) != 1 || s.chunks[0].Status != 200 || !strings.HasPrefix(s.chunks[0].MediaType, "text/html") {
		t.Fatalf("chunks = %+v", s.chunks)
	}
	if string(s.chunks[0].Data) != f.html["a"] {
		t.Errorf("page = %q", s.chunks[0].Data)
	}
	// A key that is not one of ours reads as no body at all, not as an error.
	r2 := &reader{}
	if err := p.ReadContent(&pluginv1.ReadContentRequest{Key: "label:INBOX"}, r2); err != nil {
		t.Fatal(err)
	}
	if len(r2.chunks) != 1 || len(r2.chunks[0].Data) != 0 {
		t.Errorf("chunks = %+v", r2.chunks)
	}
}

// An email names no relative resources this plugin serves, so any subpath is
// an ordinary miss — and it must not spend a Gmail call finding that out.
func TestServeContentAnswers404ForASubpath(t *testing.T) {
	f := newFake()
	p := stable(f, Options{})
	s := &server{}
	if err := p.ServeContent(&pluginv1.ServeContentRequest{Key: "msg:a", Subpath: "logo.png"}, s); err != nil {
		t.Fatal(err)
	}
	if len(s.chunks) != 1 || s.chunks[0].Status != 404 {
		t.Fatalf("chunks = %+v", s.chunks)
	}
	if got := f.count("html"); got != 0 {
		t.Errorf("a subpath cost %d Gmail calls", got)
	}
}

// A message with no body gets a page that says so, not a blank one.
func TestServeContentSaysWhenThereIsNoBody(t *testing.T) {
	f := newFake()
	f.hold("INBOX", msg("a", "lunch", "2026-01-05T14:00:00Z"))
	p := stable(f, Options{})
	if _, err := p.List(context.Background(), &pluginv1.ListRequest{Context: mailbox.InboxContext}); err != nil {
		t.Fatal(err)
	}
	s := &server{}
	if err := p.ServeContent(&pluginv1.ServeContentRequest{Key: "msg:a"}, s); err != nil {
		t.Fatal(err)
	}
	if len(s.chunks) != 1 || s.chunks[0].Status != 200 {
		t.Fatalf("chunks = %+v", s.chunks)
	}
	body := string(s.chunks[0].Data)
	if !strings.Contains(body, "lunch") || !strings.Contains(body, "no text or HTML body") {
		t.Errorf("page = %q", body)
	}
}

// A failure to read the email surfaces. A blank page would look like an email
// with nothing in it.
func TestServeContentSurfacesAFailure(t *testing.T) {
	f := newFake()
	f.err = status.Error(codes.PermissionDenied, "the stored token was refused")
	p := stable(f, Options{})
	err := p.ServeContent(&pluginv1.ServeContentRequest{Key: "msg:a"}, &server{})
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("err = %v", err)
	}
}

// Nothing is GONE until every collection has been read: a message missing
// from the inbox is often still starred, and retiring its id would cost the
// user the tile and its placement.
func TestProbeOnlySaysGoneAfterAWholeSweep(t *testing.T) {
	f := newFake()
	f.hold("INBOX", msg("a", "lunch", "2026-01-05T14:00:00Z"))
	p := stable(f, Options{})
	ctx := context.Background()

	// Nothing walked yet: cannot say.
	got, _ := p.Probe(ctx, &pluginv1.ProbeRequest{Key: "msg:z"})
	if got.Presence != pluginv1.ProbeResponse_PRESENCE_UNSPECIFIED {
		t.Fatalf("cold probe = %v", got.Presence)
	}
	if _, err := p.List(ctx, &pluginv1.ListRequest{Context: mailbox.InboxContext}); err != nil {
		t.Fatal(err)
	}
	got, _ = p.Probe(ctx, &pluginv1.ProbeRequest{Key: "msg:a"})
	if got.Presence != pluginv1.ProbeResponse_PRESENCE_PRESENT {
		t.Fatalf("a listed message probed %v", got.Presence)
	}
	got, _ = p.Probe(ctx, &pluginv1.ProbeRequest{Key: "msg:z"})
	if got.Presence != pluginv1.ProbeResponse_PRESENCE_UNSPECIFIED {
		t.Fatalf("half-swept probe = %v", got.Presence)
	}
	if _, err := p.List(ctx, &pluginv1.ListRequest{Context: mailbox.StarredContext}); err != nil {
		t.Fatal(err)
	}
	got, _ = p.Probe(ctx, &pluginv1.ProbeRequest{Key: "msg:z"})
	if got.Presence != pluginv1.ProbeResponse_PRESENCE_GONE {
		t.Fatalf("swept probe = %v", got.Presence)
	}
	// A key this plugin never mints is not ours at all.
	got, _ = p.Probe(ctx, &pluginv1.ProbeRequest{Key: "label:INBOX"})
	if got.Presence != pluginv1.ProbeResponse_PRESENCE_GONE {
		t.Fatalf("foreign key probed %v", got.Presence)
	}
}

// A message starred out of the inbox keeps its key, so the node keeps its id
// and every link to it still resolves.
func TestAMessageKeepsItsKeyAcrossCollections(t *testing.T) {
	f := newFake()
	m := msg("a", "lunch", "2026-01-05T14:00:00Z")
	f.hold("INBOX", m)
	clock := at("2026-01-06T12:00:00Z")
	p := stable(f, Options{Refresh: time.Minute, Now: func() time.Time { return clock }})
	ctx := context.Background()
	listAll(t, p)

	f.hold("INBOX")
	f.hold("STARRED", m)
	clock = clock.Add(2 * time.Minute)
	listAll(t, p)

	starred, err := p.List(ctx, &pluginv1.ListRequest{Context: mailbox.StarredContext})
	if err != nil {
		t.Fatal(err)
	}
	if len(starred.Entries) != 1 || starred.Entries[0].Key != "msg:a" {
		t.Fatalf("starred = %+v", starred.Entries)
	}
	got, _ := p.Probe(ctx, &pluginv1.ProbeRequest{Key: "msg:a"})
	if got.Presence != pluginv1.ProbeResponse_PRESENCE_PRESENT {
		t.Fatalf("an archived-but-starred message probed %v", got.Presence)
	}
}

// A failed walk is Gmail's verdict, unchanged: "this token was refused" must
// reach the user rather than becoming an empty grid.
func TestAWalkFailureSurfaces(t *testing.T) {
	f := newFake()
	f.err = status.Error(codes.PermissionDenied, "the stored token was refused")
	p := stable(f, Options{})
	_, err := p.List(context.Background(), &pluginv1.ListRequest{Context: mailbox.InboxContext})
	if status.Code(err) != codes.PermissionDenied || !strings.Contains(err.Error(), "refused") {
		t.Fatalf("err = %v", err)
	}
}

// One message whose metadata could not be read costs a tile this pass, not
// the whole grid — but it is never silent, and the id stays in the membership
// so nothing calls it gone.
func TestOneUnreadableMessageDoesNotCostTheWalk(t *testing.T) {
	f := newFake()
	f.hold("INBOX", msg("a", "one", "2026-01-05T09:00:00Z"), msg("b", "two", "2026-01-05T10:00:00Z"))
	f.mu.Lock()
	delete(f.recs, "b") // Gmail has the id in the listing but will not answer for it
	f.mu.Unlock()
	var lines []string
	p := stable(f, Options{Logf: func(format string, args ...any) { lines = append(lines, format) }})
	ctx := context.Background()

	resp, err := p.List(ctx, &pluginv1.ListRequest{Context: mailbox.InboxContext})
	if err != nil {
		t.Fatalf("one unreadable message failed the walk: %v", err)
	}
	if len(resp.Entries) != 1 || resp.Entries[0].Key != "msg:a" {
		t.Fatalf("entries = %+v", resp.Entries)
	}
	if len(lines) == 0 {
		t.Error("an unreadable message was swallowed")
	}
	got, _ := p.Probe(ctx, &pluginv1.ProbeRequest{Key: "msg:b"})
	if got.Presence == pluginv1.ProbeResponse_PRESENCE_GONE {
		t.Error("a message whose metadata read failed was declared gone")
	}
}

// Every metadata read failing is not "a message was skipped", it is the walk
// failing, and it must surface with its reason.
func TestEveryMetadataReadFailingFailsTheWalk(t *testing.T) {
	f := newFake()
	f.hold("INBOX", msg("a", "one", "2026-01-05T09:00:00Z"))
	f.headerErr = status.Error(codes.Unavailable, "gmail is down")
	p := stable(f, Options{})
	_, err := p.List(context.Background(), &pluginv1.ListRequest{Context: mailbox.InboxContext})
	if status.Code(err) != codes.Unavailable {
		t.Fatalf("err = %v, want the walk to fail with Gmail's reason", err)
	}
}

// Before any walk, a key the memory does not hold is "not yet", not "gone":
// a Gone body stored over the node's remembered one would be a loss.
func TestReadContentWaitsRatherThanDeclaringAMessageGone(t *testing.T) {
	p := stable(newFake(), Options{})
	err := p.ReadContent(&pluginv1.ReadContentRequest{Key: "msg:a"}, &reader{})
	if status.Code(err) != codes.Unavailable {
		t.Fatalf("err = %v, want Unavailable", err)
	}
}

func TestDeleteIsRefusedWithItsReason(t *testing.T) {
	p := stable(newFake(), Options{})
	_, err := p.Delete(context.Background(), &pluginv1.DeleteRequest{Key: "msg:a"})
	if status.Code(err) != codes.Unimplemented {
		t.Fatalf("err = %v", err)
	}
	if !strings.Contains(err.Error(), "read-only") {
		t.Errorf("the refusal did not say why: %v", err)
	}
}

// The grid is bounded, and a bounded read is not a whole read: the messages
// below the cap keep their tiles rather than being retired by a read that
// never reached them.
func TestTheGridIsBoundedAndACappedReadNeverRetires(t *testing.T) {
	f := newFake()
	f.hold("INBOX",
		msg("a", "one", "2026-01-05T09:00:00Z"),
		msg("b", "two", "2026-01-05T10:00:00Z"),
		msg("c", "three", "2026-01-05T11:00:00Z"))
	clock := at("2026-01-06T12:00:00Z")
	p := stable(f, Options{MaxMessages: 2, Refresh: time.Minute, Now: func() time.Time { return clock }})
	ctx := context.Background()

	resp, err := p.List(ctx, &pluginv1.ListRequest{Context: mailbox.InboxContext})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Entries) != 2 {
		t.Fatalf("a grid bounded at 2 held %d", len(resp.Entries))
	}
	// The newest two are what a person is looking at.
	if resp.Entries[0].Key != "msg:b" || resp.Entries[1].Key != "msg:c" {
		t.Fatalf("entries = %+v", resp.Entries)
	}
	// A message that was on the grid and is no longer read still has its
	// tile: absence below a capped read's watermark is not evidence.
	clock = clock.Add(2 * time.Minute)
	if _, err := p.List(ctx, &pluginv1.ListRequest{Context: mailbox.StarredContext}); err != nil {
		t.Fatal(err)
	}
	got, _ := p.Probe(ctx, &pluginv1.ProbeRequest{Key: "msg:b"})
	if got.Presence != pluginv1.ProbeResponse_PRESENCE_PRESENT {
		t.Errorf("a message on the grid probed %v", got.Presence)
	}
}

// The cache is the plugin's memory of Gmail across a restart: the next
// process answers the same listing without calling Gmail at all.
func TestTheCacheSurvivesARestart(t *testing.T) {
	dir := t.TempDir()
	f := newFake()
	f.hold("INBOX", msg("a", "lunch", "2026-01-05T14:00:00Z"))
	f.labels["INBOX+UNREAD"] = []string{"a"}
	clock := at("2026-01-06T12:00:00Z")
	opts := Options{StateDir: dir, Refresh: time.Hour, Now: func() time.Time { return clock },
		Logf: func(string, ...any) {}}
	p := stable(f, opts)
	ctx := context.Background()
	listAll(t, p)
	if _, err := os.Stat(filepath.Join(dir, mailbox.CacheFile)); err != nil {
		t.Fatalf("no cache file: %v", err)
	}

	cold := newFake() // a source that answers nothing: only the cache can
	back := stable(cold, opts)
	resp, err := back.List(ctx, &pluginv1.ListRequest{Context: mailbox.InboxContext})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Entries) != 1 || resp.Entries[0].Key != "msg:a" {
		t.Fatalf("restored listing = %+v", resp.Entries)
	}
	if !strings.HasPrefix(resp.Entries[0].Label, mailbox.UnreadMark) {
		t.Errorf("the unread mark did not survive the restart: %q", resp.Entries[0].Label)
	}
	if got := cold.count("INBOX"); got != 0 {
		t.Errorf("a restart inside the refresh window walked %d times", got)
	}
	// Completeness rides the file too, so a restart can still say GONE.
	got, _ := back.Probe(ctx, &pluginv1.ProbeRequest{Key: "msg:z"})
	if got.Presence != pluginv1.ProbeResponse_PRESENCE_GONE {
		t.Errorf("a restored sweep probed %v", got.Presence)
	}
}

// The state directory is disposable: deleting it must cost a walk, never a
// start-up.
func TestAnUnreadableCacheStartsColdAndSaysSo(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, mailbox.CacheFile), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	var lines []string
	f := newFake()
	f.hold("INBOX", msg("a", "lunch", "2026-01-05T14:00:00Z"))
	p := stable(f, Options{StateDir: dir, Logf: func(format string, args ...any) {
		lines = append(lines, format)
	}})
	if len(lines) == 0 {
		t.Fatal("an unreadable cache was swallowed")
	}
	resp, err := p.List(context.Background(), &pluginv1.ListRequest{Context: mailbox.InboxContext})
	if err != nil || len(resp.Entries) != 1 {
		t.Fatalf("cold start did not recover: %v %+v", err, resp)
	}
}

// No credential is ever written to the state directory: it is disposable, and
// a deleted credential is not rewarmed by use.
func TestTheCacheHoldsNoCredential(t *testing.T) {
	dir := t.TempDir()
	f := newFake()
	f.hold("INBOX", msg("a", "lunch", "2026-01-05T14:00:00Z"))
	p := stable(f, Options{StateDir: dir})
	listAll(t, p)
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != mailbox.CacheFile {
		t.Fatalf("state dir = %v; the plugin writes one cache file and nothing else", entries)
	}
	raw, err := os.ReadFile(filepath.Join(dir, mailbox.CacheFile))
	if err != nil {
		t.Fatal(err)
	}
	for _, word := range []string{"token", "refresh_token", "client_secret", "access_token"} {
		if strings.Contains(string(raw), word) {
			t.Errorf("the cache file names %q", word)
		}
	}
}

func TestSearchReadsMemoryOnly(t *testing.T) {
	f := newFake()
	f.hold("STARRED", msg("b", "invoice", "2026-01-04T09:00:00Z"))
	p := stable(f, Options{})
	ctx := context.Background()
	listAll(t, p)
	before := f.count("STARRED")
	res, err := p.Search(ctx, &pluginv1.SearchRequest{Query: "invoice"})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Results) != 1 || res.Results[0].Entry.Key != "msg:b" {
		t.Fatalf("results = %+v", res.Results)
	}
	if got := res.Results[0].ContextPath; len(got) != 1 || got[0] != mailbox.StarredContext {
		t.Errorf("context path = %v", got)
	}
	if f.count("STARRED") != before {
		t.Error("search called Gmail")
	}
	empty, _ := p.Search(ctx, &pluginv1.SearchRequest{Query: "  "})
	if len(empty.Results) != 0 {
		t.Error("an empty query matched")
	}
}
