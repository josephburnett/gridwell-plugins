package plugin

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/josephburnett/gridwell-plugins/hey/heycli"
	"github.com/josephburnett/gridwell-plugins/hey/mail"
	pluginv1 "github.com/josephburnett/gridwell/api/gen/plugin/v1"
)

// The fixture's lines, by index: the contract heycli pins, landed here.
const (
	lineReady = iota
	lineAdded
	lineUpdated
	lineFeedbox
	lineDeleted
	lineResync
	lineDisconnected
	lineReadyAgain
)

func fixture(t *testing.T) []mail.Event {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "heycli", "testdata", "watch.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	var out []mail.Event
	for _, l := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		ev, err := heycli.ParseWatchLine([]byte(l))
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, ev)
	}
	return out
}

// watcher is one Watch subscriber: every context it is told changed lands on
// got. A non-nil block holds each Send until closed.
type watcher struct {
	pluginv1.Plugin_WatchServer
	ctx   context.Context
	got   chan string
	block chan struct{}
}

func (w *watcher) Send(c *pluginv1.Change) error {
	if w.block != nil {
		<-w.block
	}
	w.got <- c.GetContextChanged().GetContext()
	return nil
}
func (w *watcher) Context() context.Context { return w.ctx }

func subscribers(p *Plugin) int {
	p.changes.mu.Lock()
	defer p.changes.mu.Unlock()
	return len(p.changes.subs)
}

// watchFrom subscribes a watcher and waits until it is listening.
func watchFrom(t *testing.T, p *Plugin, block chan struct{}) *watcher {
	t.Helper()
	w := &watcher{ctx: t.Context(), got: make(chan string, 1024), block: block}
	n := subscribers(p)
	go func() { _ = p.Watch(&pluginv1.WatchRequest{}, w) }()
	eventually(t, "the watcher subscribes", func() bool { return subscribers(p) > n })
	return w
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out: %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

// idle waits until no walk is in flight or owed.
func idle(t *testing.T, p *Plugin) {
	t.Helper()
	eventually(t, "the walks land", func() bool {
		p.mu.Lock()
		defer p.mu.Unlock()
		return len(p.flights) == 0 && len(p.again) == 0
	})
}

func expect(t *testing.T, w *watcher, key string) {
	t.Helper()
	select {
	case got := <-w.got:
		if got != key {
			t.Fatalf("change = %q, want %q", got, key)
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("no change for %q", key)
	}
}

// quiet drains what arrives in a short window: nothing else is owed.
func quiet(w *watcher) []string {
	var got []string
	for {
		select {
		case k := <-w.got:
			got = append(got, k)
		case <-time.After(100 * time.Millisecond):
			return got
		}
	}
}

func (f *fakeHEY) setBox(box string, ts ...mail.Thread) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.boxes[box] = ts
}

func send(t *testing.T, fd *feed, ev mail.Event) {
	t.Helper()
	select {
	case fd.lines <- ev:
	case <-time.After(10 * time.Second):
		t.Fatalf("the feed would not take %s: it is blocked", ev.Change)
	}
}

// live starts a plugin over the fake with the feed scripted, and lands the
// first ready: every box walked once, the Imbox holding one thread.
func live(t *testing.T, o Options) (*Plugin, *fakeHEY, *watcher, []mail.Event) {
	t.Helper()
	f := newFake()
	f.feed = newFeed()
	f.boxes["imbox"] = []mail.Thread{th(1, "lunch", "2026-01-05T14:00:00Z")}
	if o.Refresh == 0 {
		o.Refresh = time.Hour
	}
	p := stable(f, o)
	w := watchFrom(t, p, nil)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go p.Run(ctx)
	<-f.feed.starts
	evs := fixture(t)
	send(t, f.feed, evs[lineReady])
	expect(t, w, mail.ImboxContext)
	eventually(t, "every box is walked", func() bool {
		return f.count("imbox") == 1 && f.count("laterbox") == 1 && f.count("asidebox") == 1
	})
	idle(t, p)
	if got := quiet(w); len(got) != 0 {
		t.Fatalf("empty boxes walked empty reported %v", got)
	}
	return p, f, w, evs
}

func listed(t *testing.T, p *Plugin, ctx string) []string {
	t.Helper()
	resp, err := p.List(context.Background(), &pluginv1.ListRequest{Context: ctx})
	if err != nil {
		t.Fatal(err)
	}
	var keys []string
	for _, e := range resp.Entries {
		keys = append(keys, e.Key)
	}
	return keys
}

// Every thread line moves memory and tells the watcher which collection
// changed, with no walk: a thread arrives, is read, and leaves, and a line
// about a box this plugin does not project changes nothing.
func TestTheFeedMovesTheListingAndTellsTheWatcher(t *testing.T) {
	p, f, w, evs := live(t, Options{})

	send(t, f.feed, evs[lineAdded])
	expect(t, w, mail.ImboxContext)
	if got := listed(t, p, mail.ImboxContext); !slices.Contains(got, "thread:103") {
		t.Fatalf("imbox = %v, want the added thread", got)
	}

	send(t, f.feed, evs[lineUpdated])
	expect(t, w, mail.ImboxContext)
	if got, _ := p.mem.Get(103); !got.Seen {
		t.Error("the update did not land")
	}

	send(t, f.feed, evs[lineFeedbox])
	send(t, f.feed, evs[lineDeleted])
	expect(t, w, mail.ImboxContext)
	if got := listed(t, p, mail.ImboxContext); slices.Contains(got, "thread:103") {
		t.Fatalf("imbox = %v, the deleted posting's thread stayed", got)
	}
	if n := f.count("imbox"); n != 1 {
		t.Errorf("the feed cost %d walks of the Imbox, want only the first", n)
	}
	if got := quiet(w); len(got) != 0 {
		t.Errorf("owed nothing, got %v", got)
	}
}

// resync says the feed skipped: the box is read again and the watcher told.
func TestResyncRereadsTheBox(t *testing.T) {
	p, f, w, evs := live(t, Options{})
	f.setBox("laterbox", th(2, "invoice", "2026-01-04T09:00:00Z"))
	send(t, f.feed, evs[lineResync])
	expect(t, w, mail.ReplyLaterContext)
	idle(t, p)
	if n := f.count("laterbox"); n != 2 {
		t.Errorf("laterbox walked %d times, want 2", n)
	}
}

// After a reconnect the feed says nothing about the gap, so ready re-reads
// every box, and only a box that differs is reported.
func TestAReconnectCatchesUpEveryBox(t *testing.T) {
	p, f, w, evs := live(t, Options{})
	f.setBox("asidebox", th(3, "recipe", "2026-01-03T09:00:00Z"))
	send(t, f.feed, evs[lineDisconnected])
	send(t, f.feed, evs[lineReadyAgain])
	expect(t, w, mail.SetAsideContext)
	idle(t, p)
	for _, box := range []string{"imbox", "laterbox", "asidebox"} {
		if n := f.count(box); n != 2 {
			t.Errorf("%s walked %d times, want 2", box, n)
		}
	}
	if got := quiet(w); len(got) != 0 {
		t.Errorf("boxes that did not differ were reported: %v", got)
	}
}

// A feed that ends is started again, waiting longer each time it fails in a
// row and starting over once one reaches ready.
func TestAFeedThatEndsRestartsWithBackoff(t *testing.T) {
	const base = 100 * time.Millisecond
	var mu sync.Mutex
	var logs []string
	logf := func(format string, args ...any) {
		mu.Lock()
		defer mu.Unlock()
		logs = append(logs, format)
	}
	f := newFake()
	f.feed = newFeed()
	p := stable(f, Options{Refresh: time.Hour, WatchBackoff: base, Logf: logf})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go p.Run(ctx)

	<-f.feed.starts
	gap := func() time.Duration {
		t.Helper()
		f.feed.exit <- status.Error(codes.Unavailable, "network")
		start := time.Now()
		select {
		case <-f.feed.starts:
		case <-time.After(10 * time.Second):
			t.Fatal("the feed was not restarted")
		}
		return time.Since(start)
	}
	if g := gap(); g < base {
		t.Errorf("first restart after %s, want at least %s", g, base)
	}
	if g := gap(); g < 2*base {
		t.Errorf("second restart after %s, want at least %s", g, 2*base)
	}
	send(t, f.feed, mail.Event{Change: mail.ChangeReady})
	if g := gap(); g >= 3*base {
		t.Errorf("a feed that reached ready restarted after %s; the backoff did not start over", g)
	}
	mu.Lock()
	defer mu.Unlock()
	if !slices.ContainsFunc(logs, func(l string) bool { return strings.Contains(l, "watch ended") }) {
		t.Errorf("an ended feed was not logged: %v", logs)
	}
}

// A CLI that refuses `watch` as usage has no feed to restart; the refresher
// keeps memory instead, and the log says so.
func TestACLIThatCannotWatchIsNotRestarted(t *testing.T) {
	var mu sync.Mutex
	var logs []string
	f := newFake()
	f.feed = newFeed()
	p := stable(f, Options{Refresh: time.Hour, WatchBackoff: time.Millisecond, Logf: func(format string, args ...any) {
		mu.Lock()
		defer mu.Unlock()
		logs = append(logs, format)
	}})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go p.Run(ctx)
	<-f.feed.starts
	f.feed.exit <- status.Error(codes.InvalidArgument, "unknown command")
	select {
	case <-f.feed.starts:
		t.Fatal("a feed the CLI refused as usage was restarted")
	case <-time.After(100 * time.Millisecond):
	}
	mu.Lock()
	defer mu.Unlock()
	if !slices.ContainsFunc(logs, func(l string) bool { return strings.Contains(l, "cannot watch") }) {
		t.Errorf("the refusal was swallowed: %v", logs)
	}
}

// disconnected is health, and the CLI reconnects on its own; one that has not
// said ready within RecoverAfter is ended and started again.
func TestADisconnectThatDoesNotRecoverRestarts(t *testing.T) {
	f := newFake()
	f.feed = newFeed()
	p := stable(f, Options{Refresh: time.Hour, WatchBackoff: time.Millisecond, RecoverAfter: 20 * time.Millisecond})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go p.Run(ctx)
	<-f.feed.starts
	send(t, f.feed, mail.Event{Change: mail.ChangeDisconnected})
	select {
	case <-f.feed.stops:
	case <-time.After(10 * time.Second):
		t.Fatal("a feed stuck disconnected was never ended")
	}
	select {
	case <-f.feed.starts:
	case <-time.After(10 * time.Second):
		t.Fatal("a feed stuck disconnected was never restarted")
	}
}

// A disconnect that recovers is not restarted.
func TestADisconnectThatRecoversKeepsTheFeed(t *testing.T) {
	f := newFake()
	f.feed = newFeed()
	p := stable(f, Options{Refresh: time.Hour, RecoverAfter: 50 * time.Millisecond})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go p.Run(ctx)
	<-f.feed.starts
	send(t, f.feed, mail.Event{Change: mail.ChangeDisconnected})
	send(t, f.feed, mail.Event{Change: mail.ChangeReady})
	select {
	case <-f.feed.stops:
		t.Fatal("a feed that reconnected was ended")
	case <-time.After(200 * time.Millisecond):
	}
}

// Shutdown ends the feed and starts no other.
func TestShutdownStopsTheFeed(t *testing.T) {
	f := newFake()
	f.feed = newFeed()
	p := stable(f, Options{Refresh: time.Hour, WatchBackoff: time.Millisecond})
	ctx, cancel := context.WithCancel(context.Background())
	go p.Run(ctx)
	<-f.feed.starts
	cancel()
	select {
	case <-f.feed.stops:
	case <-time.After(10 * time.Second):
		t.Fatal("shutdown did not end the feed")
	}
	select {
	case <-f.feed.starts:
		t.Fatal("a feed was started after shutdown")
	case <-time.After(100 * time.Millisecond):
	}
}

// A watcher that stops reading never holds the feed or another watcher up.
// Its queue overflows, its changes are dropped, and once it reads again it is
// told every collection changed, which re-lists all it missed.
func TestASlowWatcherNeverBlocksTheFeed(t *testing.T) {
	p, f, fast, _ := live(t, Options{})
	block := make(chan struct{})
	slow := watchFrom(t, p, block)

	n := SubscriberBuffer + 10
	for i := range n {
		id := int64(1000 + i)
		send(t, f.feed, mail.Event{Change: mail.ChangeAdded, Box: "imbox", PostingID: id,
			Thread: th(id, "news", "2026-01-06T09:00:00Z")})
		expect(t, fast, mail.ImboxContext)
	}
	close(block)
	got := map[string]bool{}
	eventually(t, "the slow watcher is told every collection", func() bool {
		select {
		case k := <-slow.got:
			got[k] = true
		default:
		}
		return got[mail.ImboxContext] && got[mail.ReplyLaterContext] && got[mail.SetAsideContext]
	})
}
