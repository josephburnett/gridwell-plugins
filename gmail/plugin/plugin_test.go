package plugin

import (
	"context"
	"errors"
	"fmt"
	"google.golang.org/grpc/metadata"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
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
	// idErr fails Headers for one message.
	idErr map[string]error
	// failLabel fails the listing of that one label intersection.
	failLabel string
	// profileErr fails HistoryID only.
	profileErr error
	calls      map[string]int
	block      chan struct{} // when non-nil, Label waits on it
	// history is Gmail's change log: off, every catch-up is told its id
	// expired, so every refresh is a full walk. On, hid is the current id
	// and log every change recorded after floor, which is the oldest id
	// History still accepts.
	history bool
	hid     uint64
	floor   uint64
	log     []change
	// ctxs is the context each kind of call ("label", "headers", "html",
	// "profile") was last made under.
	ctxs map[string]context.Context
}

func (f *fakeGmail) saw(kind string, ctx context.Context) {
	f.ctxs[kind] = ctx
}

// ctxOf is the context the last call of kind was made under.
func (f *fakeGmail) ctxOf(kind string) context.Context {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.ctxs[kind]
}

// waitCtx waits for a call of kind and answers its context.
func (f *fakeGmail) waitCtx(t *testing.T, kind string) context.Context {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(time.Millisecond) {
		if ctx := f.ctxOf(kind); ctx != nil {
			return ctx
		}
	}
	t.Fatalf("no %s call", kind)
	return nil
}

// change is one entry in the fake's history.
type change struct {
	hid     uint64
	id      string
	deleted bool
}

func newFake() *fakeGmail {
	return &fakeGmail{labels: map[string][]string{}, whole: map[string]bool{},
		recs: map[string]mailbox.Message{}, html: map[string]string{}, calls: map[string]int{}, hid: 100,
		ctxs: map[string]context.Context{}}
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

func (f *fakeGmail) Label(ctx context.Context, labelIDs []string, limit int) ([]string, bool, error) {
	f.mu.Lock()
	f.saw("label", ctx)
	block := f.block
	f.mu.Unlock()
	if block != nil {
		<-block
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	key := strings.Join(labelIDs, "+")
	f.calls[key]++
	if f.err != nil {
		return nil, false, f.err
	}
	if f.failLabel != "" && key == f.failLabel {
		return nil, false, status.Errorf(codes.Unavailable, "fake: %s is down", key)
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

func (f *fakeGmail) Headers(ctx context.Context, id string) (mailbox.Message, []string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.saw("headers", ctx)
	f.calls["headers"]++
	if f.err != nil {
		return mailbox.Message{}, nil, f.err
	}
	if f.headerErr != nil {
		return mailbox.Message{}, nil, f.headerErr
	}
	if err := f.idErr[id]; err != nil {
		return mailbox.Message{}, nil, err
	}
	m, ok := f.recs[id]
	if !ok {
		return mailbox.Message{}, nil, status.Errorf(codes.NotFound, "no message %s", id)
	}
	var labels []string
	for key, ids := range f.labels {
		if !slices.Contains(ids, id) {
			continue
		}
		if strings.HasSuffix(key, "+"+mailbox.UnreadLabel) {
			if !slices.Contains(labels, mailbox.UnreadLabel) {
				labels = append(labels, mailbox.UnreadLabel)
			}
		} else {
			labels = append(labels, key)
		}
	}
	return m, labels, nil
}

func (f *fakeGmail) HistoryID(ctx context.Context) (uint64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.saw("profile", ctx)
	f.calls["profile"]++
	if f.err != nil {
		return 0, f.err
	}
	if f.profileErr != nil {
		return 0, f.profileErr
	}
	return f.hid, nil
}

func (f *fakeGmail) History(_ context.Context, since uint64, _ []string) (mailbox.Delta, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls["history"]++
	if f.err != nil {
		return mailbox.Delta{}, f.err
	}
	if !f.history || since < f.floor {
		return mailbox.Delta{}, fmt.Errorf("fake: history since %d: %w", since, mailbox.ErrHistoryExpired)
	}
	d := mailbox.Delta{HistoryID: f.hid}
	for _, c := range f.log {
		if c.hid <= since {
			continue
		}
		if c.deleted {
			d.Deleted = append(d.Deleted, c.id)
		} else {
			d.Touched = append(d.Touched, c.id)
		}
	}
	return d, nil
}

// changed records that these messages' labels changed at Gmail, after the
// test has changed them.
func (f *fakeGmail) changed(ids ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, id := range ids {
		f.hid++
		f.log = append(f.log, change{hid: f.hid, id: id})
	}
}

// deleted removes a message from Gmail and records it.
func (f *fakeGmail) deleted(id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for key, ids := range f.labels {
		f.labels[key] = slices.DeleteFunc(slices.Clone(ids), func(x string) bool { return x == id })
	}
	delete(f.recs, id)
	f.hid++
	f.log = append(f.log, change{hid: f.hid, id: id, deleted: true})
}

func (f *fakeGmail) HTML(ctx context.Context, id string) ([]byte, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.saw("html", ctx)
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

// server collects a ServeContent stream made under ctx, Background if nil.
type server struct {
	pluginv1.Plugin_ServeContentServer
	ctx    context.Context
	chunks []*pluginv1.ServeContentChunk
}

func (s *server) Send(c *pluginv1.ServeContentChunk) error {
	s.chunks = append(s.chunks, c)
	return nil
}

func (s *server) Context() context.Context {
	if s.ctx == nil {
		return context.Background()
	}
	return s.ctx
}

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
	landed(t, p)
}

// Every context is a (+) menu entry, and none is a root: root_context is
// retired.
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
	if len(info.MenuEntries) != 3 || info.MenuEntries[0].Context != mailbox.InboxContext ||
		info.MenuEntries[1].Context != mailbox.StarredContext || info.MenuEntries[2].Context != mailbox.AllMailContext {
		t.Fatalf("menu entries = %+v, want one per context", info.MenuEntries)
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
	landed(t, p)
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
	_, _ = p.List(ctx, &pluginv1.ListRequest{Context: mailbox.InboxContext})
	landed(t, p)
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
	if resp.Entries[0].StatusDetail != mailbox.UnreadMark {
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
	_, _ = p.List(ctx, &pluginv1.ListRequest{Context: mailbox.InboxContext})
	landed(t, p)
	resp, err = p.List(ctx, &pluginv1.ListRequest{Context: mailbox.InboxContext})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Entries[0].StatusDetail != "" {
		t.Errorf("a message read at Gmail kept its mark: %q", resp.Entries[0].StatusDetail)
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
	if inbox.Entries[0].StatusDetail != mailbox.StarMark {
		t.Errorf("a starred message lost its star in the inbox: %q", inbox.Entries[0].StatusDetail)
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

// A message is a url tile that serves a page, and the page is the email. It
// has no text body: nothing on the node reads one for a url entry, so the
// plugin serves none (ReadContent is the embedded Unimplemented).
func TestTheEmailIsThePageAndThereIsNoCard(t *testing.T) {
	f := newFake()
	f.hold("INBOX", msg("a", "lunch", "2026-01-05T14:00:00Z"))
	f.html["a"] = "<div>Are you free friday?</div>"
	p := stable(f, Options{})
	ctx := context.Background()
	if _, err := p.List(ctx, &pluginv1.ListRequest{Context: mailbox.InboxContext}); err != nil {
		t.Fatal(err)
	}

	if err := p.ReadContent(&pluginv1.ReadContentRequest{Key: "msg:a"}, &reader{}); status.Code(err) != codes.Unimplemented {
		t.Errorf("ReadContent = %v, want no card at all", err)
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
	f.failLabel = "STARRED" // the inbox walks, the starred mail does not
	clock := at("2026-01-06T12:00:00Z")
	p := stable(f, Options{Refresh: time.Minute, Now: func() time.Time { return clock }})
	ctx := context.Background()

	// Nothing walked yet: cannot say.
	got, _ := p.Probe(ctx, &pluginv1.ProbeRequest{Key: "msg:z"})
	if got.Presence != pluginv1.ProbeResponse_PRESENCE_UNSPECIFIED {
		t.Fatalf("cold probe = %v", got.Presence)
	}
	// The refresh walks the inbox, then fails on the starred mail.
	_, _ = p.List(ctx, &pluginv1.ListRequest{Context: mailbox.InboxContext})
	landed(t, p)
	got, _ = p.Probe(ctx, &pluginv1.ProbeRequest{Key: "msg:a"})
	if got.Presence != pluginv1.ProbeResponse_PRESENCE_PRESENT {
		t.Fatalf("a listed message probed %v", got.Presence)
	}
	got, _ = p.Probe(ctx, &pluginv1.ProbeRequest{Key: "msg:z"})
	if got.Presence != pluginv1.ProbeResponse_PRESENCE_UNSPECIFIED {
		t.Fatalf("half-swept probe = %v", got.Presence)
	}
	f.mu.Lock()
	f.failLabel = ""
	f.mu.Unlock()
	clock = clock.Add(2 * time.Minute)
	_, _ = p.List(ctx, &pluginv1.ListRequest{Context: mailbox.InboxContext}) // answers the last failure, and refreshes
	landed(t, p)
	listAll(t, p)
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

// One message Gmail will not answer for costs its tile, never the walk, on
// the first walk or any later one, when it is the only message left to read:
// a refresh that failed forever on one email would mark the whole account
// unreachable.
func TestAMessageGmailWillNotReadNeverFailsAWalk(t *testing.T) {
	for name, err := range map[string]error{
		"not found": status.Error(codes.NotFound, "no message b"),
		"malformed": status.Error(codes.Internal, "message b is malformed"),
	} {
		t.Run(name, func(t *testing.T) {
			f := newFake()
			f.hold("INBOX", msg("a", "one", "2026-01-05T09:00:00Z"), msg("b", "two", "2026-01-05T10:00:00Z"))
			f.idErr = map[string]error{"b": err}
			clock := at("2026-01-06T12:00:00Z")
			p := stable(f, Options{Refresh: time.Minute, Now: func() time.Time { return clock }})
			listAll(t, p)
			refreshed(t, p, &clock, 2*time.Minute) // b is all this walk has to read
			resp, err := p.List(context.Background(), &pluginv1.ListRequest{Context: mailbox.InboxContext})
			if err != nil || resp.Unreachable != "" {
				t.Fatalf("the walk after one unreadable message = %q, %v", resp.GetUnreachable(), err)
			}
			if got := entryKeys(resp.Entries); got != "msg:a" {
				t.Errorf("inbox = %s", got)
			}
		})
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
	if _, err := os.Stat(filepath.Join(dir, cacheFile)); err != nil {
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
	if resp.Entries[0].StatusDetail != mailbox.UnreadMark {
		t.Errorf("the unread mark did not survive the restart: %q", resp.Entries[0].StatusDetail)
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
	if err := os.WriteFile(filepath.Join(dir, cacheFile), []byte("{not json"), 0o600); err != nil {
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
	if len(entries) != 1 || entries[0].Name() != cacheFile {
		t.Fatalf("state dir = %v; the plugin writes one cache file and nothing else", entries)
	}
	raw, err := os.ReadFile(filepath.Join(dir, cacheFile))
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
	if got := res.Results[0].ContextPath; len(got) != 1 || got[0] != mailbox.AllMailContext {
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

// landed waits out the refresh in flight, its cache write included: a warm
// read answers before the refresh it started has landed.
func landed(t *testing.T, p *Plugin) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); p.flights.Busy(account) || p.landing.Load() > 0; time.Sleep(time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("a walk never landed")
		}
	}
}

// A read over a memory that has an answer gives it at once, however slow the
// walk behind it: past the refresh window every read would otherwise pay the
// first-answer bound for an answer memory already had. The walk still runs
// and lands. A walk that failed — here a token revoked after Info passed —
// costs no read its answer: the next warm read answers memory, every entry,
// and says why in unreachable, until a walk lands again.
func TestAWarmReadAnswersMemoryAndSaysWhyTheWalkFailed(t *testing.T) {
	f := newFake()
	f.hold("INBOX", msg("a", "lunch", "2026-01-05T14:00:00Z"))
	f.hold("STARRED", msg("a", "lunch", "2026-01-05T14:00:00Z"))
	clock := at("2026-01-06T12:00:00Z")
	p := stable(f, Options{Refresh: time.Minute, FirstAnswer: time.Hour, Now: func() time.Time { return clock }})
	ctx := context.Background()
	if _, err := p.List(ctx, &pluginv1.ListRequest{Context: mailbox.InboxContext}); err != nil {
		t.Fatal(err)
	}

	f.mu.Lock()
	f.block = make(chan struct{})
	f.mu.Unlock()
	clock = clock.Add(2 * time.Minute)
	answered := make(chan *pluginv1.ListResponse, 1)
	go func() {
		resp, err := p.List(ctx, &pluginv1.ListRequest{Context: mailbox.InboxContext})
		if err != nil {
			t.Error(err)
		}
		answered <- resp
	}()
	select {
	case resp := <-answered:
		if len(resp.GetEntries()) != 1 {
			t.Errorf("a warm read answered %d entries", len(resp.GetEntries()))
		}
	case <-time.After(time.Second):
		close(f.block)
		t.Fatal("a warm read waited on a blocked walk")
	}

	f.mu.Lock()
	f.err = status.Error(codes.PermissionDenied, "the stored token was refused")
	f.mu.Unlock()
	close(f.block)
	landed(t, p)
	for _, c := range mailbox.Contexts() {
		resp, err := p.List(ctx, &pluginv1.ListRequest{Context: c})
		if err != nil {
			t.Fatalf("%s after a failed walk = %v, want memory's answer", c, err)
		}
		if len(resp.Entries) != 1 || resp.Unreachable != "the stored token was refused" || resp.Authoritative {
			t.Errorf("%s after a failed walk = %d entries, unreachable %q, authoritative %v; want memory, the reason, no authority",
				c, len(resp.Entries), resp.Unreachable, resp.Authoritative)
		}
	}

	f.mu.Lock()
	f.err = nil
	f.mu.Unlock()
	clock = clock.Add(2 * time.Minute)
	_, _ = p.List(ctx, &pluginv1.ListRequest{Context: mailbox.InboxContext})
	landed(t, p)
	if resp, err := p.List(ctx, &pluginv1.ListRequest{Context: mailbox.InboxContext}); err != nil || resp.Unreachable != "" {
		t.Errorf("after a walk landed = unreachable %q, %v; want live again", resp.GetUnreachable(), err)
	}
}

// warmHistory is a plugin over a fake that keeps history, after its first
// full walk, with a clock the test moves.
func warmHistory(t *testing.T, f *fakeGmail, o Options) (*Plugin, *time.Time) {
	t.Helper()
	f.history = true
	clock := at("2026-01-06T12:00:00Z")
	o.Refresh = time.Minute
	o.Now = func() time.Time { return clock }
	p := stable(f, o)
	listAll(t, p)
	return p, &clock
}

// refreshed moves the clock past the refresh window and lets one refresh
// land.
func refreshed(t *testing.T, p *Plugin, clock *time.Time, by time.Duration) {
	t.Helper()
	*clock = clock.Add(by)
	_, _ = p.List(context.Background(), &pluginv1.ListRequest{Context: mailbox.InboxContext})
	landed(t, p)
}

func keys(t *testing.T, p *Plugin, ctxKey string) string {
	t.Helper()
	resp, err := p.List(context.Background(), &pluginv1.ListRequest{Context: ctxKey})
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range resp.Entries {
		out = append(out, e.Key)
	}
	return strings.Join(out, ",")
}

// Past the window, a refresh over a quiet mailbox is one history request and
// nothing else: no listing, no metadata, no change.
func TestAQuietRefreshIsOneHistoryRequest(t *testing.T) {
	f := newFake()
	f.hold("INBOX", msg("a", "one", "2026-01-05T09:00:00Z"))
	p, clock := warmHistory(t, f, Options{})
	before := keys(t, p, mailbox.InboxContext)
	inbox, headers := f.count("INBOX"), f.count("headers")

	refreshed(t, p, clock, 2*time.Minute)
	if got := f.count("history"); got != 1 {
		t.Errorf("history read %d times, want 1", got)
	}
	if f.count("INBOX") != inbox || f.count("headers") != headers || f.count("profile") != 1 {
		t.Errorf("a quiet refresh listed or read: calls = %v", f.calls)
	}
	if after := keys(t, p, mailbox.InboxContext); after != before {
		t.Errorf("inbox %s became %s", before, after)
	}
}

// A catch-up changes exactly what history names: an arrival joins the inbox,
// an archived message leaves it, a starred one joins the starred grid, a
// deleted one leaves every grid. Each touched message costs one metadata
// read, and nothing is listed.
func TestACatchUpAppliesExactlyWhatHistoryNames(t *testing.T) {
	f := newFake()
	a, b, d := msg("a", "one", "2026-01-05T09:00:00Z"), msg("b", "two", "2026-01-05T10:00:00Z"), msg("d", "four", "2026-01-04T10:00:00Z")
	untouched := msg("u", "five", "2026-01-05T08:00:00Z")
	f.hold("INBOX", untouched, a, b)
	f.hold("STARRED", d)
	p, clock := warmHistory(t, f, Options{})
	inbox, headers := f.count("INBOX"), f.count("headers")

	c := msg("c", "three", "2026-01-05T11:00:00Z")
	f.hold("INBOX", untouched, a, c) // c arrives, b is archived
	f.hold("STARRED", d, a)          // a is starred
	f.changed("c", "b", "a")
	f.deleted("d")
	refreshed(t, p, clock, 2*time.Minute)

	if got := keys(t, p, mailbox.InboxContext); got != "msg:u,msg:a,msg:c" {
		t.Errorf("inbox = %s", got)
	}
	if got := keys(t, p, mailbox.StarredContext); got != "msg:a" {
		t.Errorf("starred = %s", got)
	}
	if got := f.count("headers") - headers; got != 3 {
		t.Errorf("%d metadata reads, want 3 (a, b, c)", got)
	}
	if f.count("INBOX") != inbox {
		t.Error("a catch-up listed a label")
	}
	resp, _ := p.List(context.Background(), &pluginv1.ListRequest{Context: mailbox.InboxContext})
	for _, e := range resp.Entries {
		if e.Key == "msg:a" && e.StatusDetail != mailbox.StarMark {
			t.Errorf("a starred message has no star in the inbox: %q", e.StatusDetail)
		}
	}
}

// Gmail forgets old history. An id it refuses costs one full walk, which
// mints a fresh id, and the refresh after that is a catch-up again.
func TestAnExpiredHistoryIDWalksOnceThenCatchesUp(t *testing.T) {
	f := newFake()
	f.hold("INBOX", msg("a", "one", "2026-01-05T09:00:00Z"))
	p, clock := warmHistory(t, f, Options{})
	f.changed("a")
	f.mu.Lock()
	f.floor = f.hid // the id memory holds is now too old
	f.mu.Unlock()

	refreshed(t, p, clock, 2*time.Minute)
	if got := f.count("INBOX"); got != 2 {
		t.Fatalf("an expired id walked the inbox %d times in all, want 2", got)
	}
	if got := p.mem.HistoryID(); got != f.hid {
		t.Fatalf("history id after the walk = %d, want Gmail's %d", got, f.hid)
	}
	refreshed(t, p, clock, 2*time.Minute)
	if got := f.count("INBOX"); got != 2 {
		t.Errorf("the refresh after the walk listed again (%d)", got)
	}
	if got := f.count("history"); got != 2 {
		t.Errorf("history read %d times, want 2", got)
	}
}

// The history id rides the cache: a restart past the refresh window catches
// up from it instead of walking every collection.
func TestTheHistoryIDSurvivesARestart(t *testing.T) {
	dir := t.TempDir()
	f := newFake()
	f.hold("INBOX", msg("a", "one", "2026-01-05T09:00:00Z"))
	_, clock := warmHistory(t, f, Options{StateDir: dir})
	f.hold("INBOX", msg("a", "one", "2026-01-05T09:00:00Z"), msg("b", "two", "2026-01-05T10:00:00Z"))
	f.changed("b")
	inbox := f.count("INBOX")

	later := clock.Add(2 * time.Minute)
	back := stable(f, Options{StateDir: dir, Refresh: time.Minute, Now: func() time.Time { return later }})
	_, _ = back.List(context.Background(), &pluginv1.ListRequest{Context: mailbox.InboxContext})
	landed(t, back)
	if got := keys(t, back, mailbox.InboxContext); got != "msg:a,msg:b" {
		t.Errorf("inbox after restart = %s", got)
	}
	if f.count("INBOX") != inbox {
		t.Error("a restart walked instead of catching up")
	}
}

// History is the whole truth only for what a catch-up can see, so once
// SweepEvery has passed a refresh walks every collection again.
func TestTheConsistencyPassWalksAgain(t *testing.T) {
	f := newFake()
	f.hold("INBOX", msg("a", "one", "2026-01-05T09:00:00Z"))
	p, clock := warmHistory(t, f, Options{})
	refreshed(t, p, clock, 2*time.Minute)
	if got := f.count("INBOX"); got != 1 {
		t.Fatalf("a refresh inside SweepEvery walked (%d)", got)
	}
	refreshed(t, p, clock, SweepEvery)
	if got := f.count("INBOX"); got != 2 {
		t.Errorf("a refresh past SweepEvery walked %d times in all, want 2", got)
	}
}

// watchStream is the node's end of Watch: every change it is sent, until
// the test hangs up. A non-nil gate holds every Send until it closes.
type watchStream struct {
	pluginv1.Plugin_WatchServer
	ctx    context.Context
	sent   chan string
	gate   chan struct{}
	header atomic.Bool
}

func (w *watchStream) Context() context.Context     { return w.ctx }
func (w *watchStream) SendHeader(metadata.MD) error { w.header.Store(true); return nil }
func (w *watchStream) Send(c *pluginv1.Change) error {
	if !w.header.Load() {
		return errors.New("a Change before the header")
	}
	if w.gate != nil {
		<-w.gate
	}
	w.sent <- c.GetContextChanged().GetContext()
	return nil
}

// The node counts a Watch stream open at its header, so a plugin that only
// speaks at its first change leaves a refusal standing and a dropped stream
// uncaught-up until something happens to change.
func TestWatchSendsItsHeaderOnAccept(t *testing.T) {
	f := newFake()
	p, _ := warmHistory(t, f, Options{})
	w := watch(t, p, nil)
	deadline := time.Now().Add(5 * time.Second)
	for !w.header.Load() {
		if time.Now().After(deadline) {
			t.Fatal("Watch accepted the stream and sent no header")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// watch opens Watch and waits until it is subscribed.
func watch(t *testing.T, p *Plugin, gate chan struct{}) *watchStream {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	w := &watchStream{ctx: ctx, sent: make(chan string, 16), gate: gate}
	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := p.Watch(&pluginv1.WatchRequest{}, w); err != nil {
			t.Error(err)
		}
	}()
	t.Cleanup(func() {
		cancel()
		if gate != nil {
			close(gate)
		}
		<-done
	})
	for deadline := time.Now().Add(5 * time.Second); !w.header.Load(); time.Sleep(time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("Watch never subscribed")
		}
	}
	return w
}

// announced collects what the stream is sent within a short settle.
func announced(w *watchStream) []string {
	var out []string
	for {
		select {
		case k := <-w.sent:
			out = append(out, k)
		case <-time.After(100 * time.Millisecond):
			return out
		}
	}
}

// A refresh announces exactly the collections whose answer it changed, and a
// quiet one announces nothing.
func TestWatchAnnouncesExactlyWhatARefreshChanged(t *testing.T) {
	f := newFake()
	f.hold("INBOX", msg("a", "one", "2026-01-05T09:00:00Z"))
	f.hold("STARRED", msg("d", "four", "2026-01-04T10:00:00Z"))
	p, clock := warmHistory(t, f, Options{})
	info, _ := p.Info(context.Background(), &pluginv1.InfoRequest{})
	if !info.Watch {
		t.Error("Info does not declare watch")
	}
	w := watch(t, p, nil)

	refreshed(t, p, clock, 2*time.Minute)
	if got := announced(w); len(got) != 0 {
		t.Errorf("a quiet refresh announced %v", got)
	}

	f.deleted("d") // starred only
	refreshed(t, p, clock, 2*time.Minute)
	if got := announced(w); !slices.Equal(got, []string{mailbox.StarredContext, mailbox.AllMailContext}) {
		t.Errorf("a starred deletion announced %v", got)
	}

	f.hold("STARRED", msg("a", "one", "2026-01-05T09:00:00Z")) // a gains a star: every face changes
	f.changed("a")
	refreshed(t, p, clock, 2*time.Minute)
	if got := announced(w); !slices.Equal(got, []string{mailbox.InboxContext, mailbox.StarredContext, mailbox.AllMailContext}) {
		t.Errorf("starring an inbox message announced %v", got)
	}
}

// A subscriber that never reads costs no refresh anything: refreshes land.
func TestASlowWatcherNeverBlocksARefresh(t *testing.T) {
	f := newFake()
	f.hold("INBOX", msg("a", "one", "2026-01-05T09:00:00Z"))
	p, clock := warmHistory(t, f, Options{})
	watch(t, p, make(chan struct{})) // its first Send blocks until cleanup

	for i := range 5 {
		id := fmt.Sprintf("n%d", i)
		f.mu.Lock()
		f.labels["INBOX"] = append([]string{id}, f.labels["INBOX"]...)
		f.recs[id] = msg(id, "new", "2026-01-05T10:00:00Z")
		f.mu.Unlock()
		f.changed(id)
		refreshed(t, p, clock, 2*time.Minute) // fails the test if a refresh never lands
	}
}

// The history id is how memory keeps up cheaply, never what it shows: a
// Gmail that will not give one still gets its grids walked, the failure is
// said, and the next refresh walks again rather than catching up from
// nothing.
func TestAMissingHistoryIDCostsAWalkNotTheGrid(t *testing.T) {
	f := newFake()
	f.history = true
	f.profileErr = status.Error(codes.NotFound, "no profile")
	f.hold("INBOX", msg("a", "one", "2026-01-05T09:00:00Z"))
	var mu sync.Mutex
	var lines []string
	clock := at("2026-01-06T12:00:00Z")
	p := stable(f, Options{Refresh: time.Minute, Now: func() time.Time { return clock },
		Logf: func(format string, args ...any) {
			mu.Lock()
			lines = append(lines, fmt.Sprintf(format, args...))
			mu.Unlock()
		}})
	listAll(t, p)
	if got := keys(t, p, mailbox.InboxContext); got != "msg:a" {
		t.Fatalf("inbox = %q", got)
	}
	mu.Lock()
	said := strings.Contains(strings.Join(lines, "\n"), "no profile")
	mu.Unlock()
	if !said {
		t.Error("a failed history id read was swallowed")
	}
	refreshed(t, p, &clock, 2*time.Minute)
	if f.count("history") != 0 || f.count("INBOX") != 2 {
		t.Errorf("with no id the next refresh did not walk: calls = %v", f.calls)
	}
}
