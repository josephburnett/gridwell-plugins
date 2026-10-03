package plugin

import (
	"context"
	"errors"
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

	"github.com/josephburnett/gridwell-plugins/hey/heycli"
	"github.com/josephburnett/gridwell-plugins/hey/mail"
	"github.com/josephburnett/gridwell-plugins/memo"
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
	ctx    context.Context
	got    chan string
	block  chan struct{}
	header atomic.Bool
}

func (w *watcher) SendHeader(metadata.MD) error { w.header.Store(true); return nil }
func (w *watcher) Send(c *pluginv1.Change) error {
	if !w.header.Load() {
		return errors.New("a Change before the header")
	}
	if w.block != nil {
		<-w.block
	}
	w.got <- c.GetContextChanged().GetContext()
	return nil
}

// The node counts a Watch stream open at its header, so a plugin that only
// speaks at its first change leaves a refusal standing and a dropped stream
// uncaught-up until something happens to change.
func TestWatchSendsItsHeaderOnAccept(t *testing.T) {
	f := newFake()
	f.feed = newFeed()
	p := stable(t, f, Options{Refresh: time.Hour})
	w := watchFrom(t, p, nil)
	deadline := time.Now().Add(5 * time.Second)
	for !w.header.Load() {
		if time.Now().After(deadline) {
			t.Fatal("Watch accepted the stream and sent no header")
		}
		time.Sleep(5 * time.Millisecond)
	}
}
func (w *watcher) Context() context.Context { return w.ctx }

// watchFrom opens a Watch stream that lives until the test ends, and waits
// until it is listening.
func watchFrom(t *testing.T, p *Plugin, block chan struct{}) *watcher {
	t.Helper()
	return watchUntil(t, t.Context(), p, block)
}

// watchUntil opens a Watch stream that lives until ctx ends.
func watchUntil(t *testing.T, ctx context.Context, p *Plugin, block chan struct{}) *watcher {
	t.Helper()
	w := &watcher{ctx: ctx, got: make(chan string, 1024), block: block}
	go func() { _ = p.Watch(&pluginv1.WatchRequest{}, w) }()
	eventually(t, "the watcher is listening", w.header.Load)
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

func isLive(p *Plugin) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.live
}

// idle waits until no walk is in flight or owed.
func idle(t *testing.T, p *Plugin) {
	t.Helper()
	eventually(t, "the walks land", func() bool {
		for _, c := range mail.Collections {
			if p.flights.Busy(c.Key) {
				return false
			}
		}
		return true
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
	p := stable(t, f, o)
	w := watchFrom(t, p, nil)
	<-f.feed.starts
	evs := fixture(t)
	send(t, f.feed, evs[lineReady])
	expect(t, w, mail.ImboxContext)
	expect(t, w, mail.EverythingContext)
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

// Every thread line moves memory and tells the watcher which collections
// changed, with no walk: a thread arrives, is read, and leaves, each line
// moving its box and everything, and a line about another box moves that box.
func TestTheFeedMovesTheListingAndTellsTheWatcher(t *testing.T) {
	p, f, w, evs := live(t, Options{})

	send(t, f.feed, evs[lineAdded])
	expect(t, w, mail.ImboxContext)
	expect(t, w, mail.EverythingContext)
	if got := listed(t, p, mail.ImboxContext); !slices.Contains(got, "thread:103") {
		t.Fatalf("imbox = %v, want the added thread", got)
	}

	send(t, f.feed, evs[lineUpdated])
	expect(t, w, mail.ImboxContext)
	expect(t, w, mail.EverythingContext)
	if got, _ := p.mem.Get(103); !got.Seen {
		t.Error("the update did not land")
	}

	send(t, f.feed, evs[lineFeedbox])
	expect(t, w, mail.FeedContext)
	expect(t, w, mail.EverythingContext)
	send(t, f.feed, evs[lineDeleted])
	expect(t, w, mail.ImboxContext)
	expect(t, w, mail.EverythingContext)
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
	expect(t, w, mail.EverythingContext)
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
	expect(t, w, mail.EverythingContext)
	idle(t, p)
	for _, box := range []string{"imbox", "laterbox", "asidebox", "feedbox", "trailbox", "bubblebox"} {
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
	p := stable(t, f, Options{Refresh: time.Hour, WatchBackoff: base, Logf: logf})
	watchFrom(t, p, nil)

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
	p := stable(t, f, Options{Refresh: time.Hour, WatchBackoff: time.Millisecond, Logf: func(format string, args ...any) {
		mu.Lock()
		defer mu.Unlock()
		logs = append(logs, format)
	}})
	watchFrom(t, p, nil)
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
	p := stable(t, f, Options{Refresh: time.Hour, WatchBackoff: time.Millisecond, RecoverAfter: 20 * time.Millisecond})
	watchFrom(t, p, nil)
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
	p := stable(t, f, Options{Refresh: time.Hour, RecoverAfter: 50 * time.Millisecond})
	watchFrom(t, p, nil)
	<-f.feed.starts
	send(t, f.feed, mail.Event{Change: mail.ChangeDisconnected})
	send(t, f.feed, mail.Event{Change: mail.ChangeReady})
	select {
	case <-f.feed.stops:
		t.Fatal("a feed that reconnected was ended")
	case <-time.After(200 * time.Millisecond):
	}
}

// Nothing is done for nobody (rule 8): with no Watch stream open, no feed
// runs and no clock walks a box, however much time passes. The first stream
// starts the feed; the last one leaving stops it, after the linger.
func TestTheFeedRunsOnlyWhileWatched(t *testing.T) {
	f := newFake()
	f.feed = newFeed()
	p := stable(t, f, Options{Refresh: time.Millisecond, Linger: -1})
	time.Sleep(50 * time.Millisecond)
	if n := f.count("watch") + f.count("imbox"); n != 0 {
		t.Fatalf("with no stream open HEY was asked %d times", n)
	}

	ctx, cancel := context.WithCancel(t.Context())
	watchUntil(t, ctx, p, nil)
	<-f.feed.starts
	cancel()
	select {
	case <-f.feed.stops:
	case <-time.After(10 * time.Second):
		t.Fatal("the last stream left and the feed ran on")
	}
	select {
	case <-f.feed.starts:
		t.Fatal("a feed was started with no stream open")
	case <-time.After(100 * time.Millisecond):
	}
	if n := f.count("imbox"); n != 0 {
		t.Errorf("a feed that never said ready cost %d walks", n)
	}
}

// The plugin's end ends the feed.
func TestTheEndOfLifeStopsTheFeed(t *testing.T) {
	f := newFake()
	f.feed = newFeed()
	life := memo.NewLife()
	p := stable(t, f, Options{Life: life, WatchBackoff: time.Millisecond})
	watchFrom(t, p, nil)
	<-f.feed.starts
	life.End()
	select {
	case <-f.feed.stops:
	default:
		t.Fatal("End returned with the feed still running")
	}
}

// A watcher that stops reading never holds the feed or another watcher up,
// and a burst it missed is owed once per context: one repaint each, not one
// per change.
func TestASlowWatcherNeverBlocksTheFeed(t *testing.T) {
	p, f, fast, _ := live(t, Options{})
	block := make(chan struct{})
	slow := watchFrom(t, p, block)

	for i := range 3 * memo.DefaultBuffer {
		id := int64(1000 + i)
		send(t, f.feed, mail.Event{Change: mail.ChangeAdded, Box: "imbox", PostingID: id,
			Thread: th(id, "news", "2026-01-06T09:00:00Z")})
		expect(t, fast, mail.ImboxContext)
		expect(t, fast, mail.EverythingContext)
	}
	close(block)
	got := quiet(slow)
	n := map[string]int{}
	for _, k := range got {
		n[k]++
	}
	// The first change was in flight when the watcher stalled; the rest
	// collapse behind it.
	if n[mail.ImboxContext] < 1 || n[mail.ImboxContext] > 2 || n[mail.EverythingContext] < 1 || n[mail.EverythingContext] > 2 || len(n) != 2 {
		t.Fatalf("a slow watcher was owed %v, want the imbox and everything once each", got)
	}
}

// While the feed is live, memory is current: no read and no clock walks a
// box. Once it disconnects, the refresh window rules again.
func TestALiveFeedKeepsReadsFromWalking(t *testing.T) {
	var mu sync.Mutex
	clock := at("2026-01-06T12:00:00Z")
	now := func() time.Time { mu.Lock(); defer mu.Unlock(); return clock }
	p, f, _, evs := live(t, Options{Refresh: time.Minute, Now: now})
	mu.Lock()
	clock = clock.Add(time.Hour)
	mu.Unlock()
	for _, c := range mail.Collections {
		listed(t, p, c.Key)
	}
	idle(t, p)
	if n := f.count("imbox"); n != 1 {
		t.Fatalf("a read under a live feed walked (%d walks)", n)
	}

	send(t, f.feed, evs[lineDisconnected])
	eventually(t, "the feed is down", func() bool { return !isLive(p) })
	listed(t, p, mail.ImboxContext)
	idle(t, p)
	if n := f.count("imbox"); n != 2 {
		t.Fatalf("a stale read with the feed down walked %d times, want 2", n)
	}
}

// A catch-up walk that failed leaves its box not current, though the feed is
// live: the next read walks it.
func TestAFailedCatchUpIsWalkedByTheNextRead(t *testing.T) {
	p, f, _, evs := live(t, Options{})
	f.mu.Lock()
	f.err = status.Error(codes.Unavailable, "network")
	f.mu.Unlock()
	send(t, f.feed, evs[lineReadyAgain])
	eventually(t, "the catch-up runs", func() bool { return f.count("imbox") == 2 })
	idle(t, p)
	f.mu.Lock()
	f.err = nil
	f.mu.Unlock()
	_, _ = p.List(context.Background(), &pluginv1.ListRequest{Context: mail.ImboxContext})
	idle(t, p)
	if n := f.count("imbox"); n != 3 {
		t.Fatalf("imbox walked %d times, want the first, the failed catch-up and the read's", n)
	}
	if _, err := p.List(context.Background(), &pluginv1.ListRequest{Context: mail.ImboxContext}); err != nil {
		t.Fatalf("a landed walk left the failure standing: %v", err)
	}
}

// A feed that ends on a verdict — not signed in — is every read's answer, not
// only the log's, until a feed reaches ready again. The memory it leaves is
// still there to answer from; the error says it is no longer current.
func TestAFeedVerdictSurfacesOnTheNextRead(t *testing.T) {
	var mu sync.Mutex
	var logs []string
	p, f, _, evs := live(t, Options{WatchBackoff: 20 * time.Millisecond, Logf: func(format string, args ...any) {
		mu.Lock()
		defer mu.Unlock()
		logs = append(logs, format)
	}})
	f.feed.exit <- status.Error(codes.PermissionDenied, "hey plugin: watch: Not logged in (Run: hey auth login)")
	eventually(t, "the verdict reaches a read", func() bool {
		_, err := p.List(context.Background(), &pluginv1.ListRequest{Context: mail.ImboxContext})
		return status.Code(err) == codes.PermissionDenied && strings.Contains(err.Error(), "Not logged in")
	})
	mu.Lock()
	if !slices.ContainsFunc(logs, func(l string) bool { return strings.Contains(l, "watch ended") }) {
		t.Errorf("the verdict was not logged: %v", logs)
	}
	mu.Unlock()

	<-f.feed.starts
	send(t, f.feed, evs[lineReady])
	eventually(t, "the feed is live", func() bool { return isLive(p) })
	idle(t, p)
	if _, err := p.List(context.Background(), &pluginv1.ListRequest{Context: mail.ImboxContext}); err != nil {
		t.Fatalf("a feed that is live again left the verdict standing: %v", err)
	}
}

// A feed that keeps ending is one episode in the log: the first end is said,
// the retries are not, and the feed reaching ready again is said once.
func TestAFeedThatKeepsEndingLogsOnce(t *testing.T) {
	var mu sync.Mutex
	var logs []string
	logf := func(format string, args ...any) {
		mu.Lock()
		defer mu.Unlock()
		logs = append(logs, format)
	}
	f := newFake()
	f.feed = newFeed()
	p := stable(t, f, Options{Refresh: time.Hour, WatchBackoff: 10 * time.Millisecond, Logf: logf})
	watchFrom(t, p, nil)
	count := func(sub string) int {
		mu.Lock()
		defer mu.Unlock()
		n := 0
		for _, l := range logs {
			if strings.Contains(l, sub) {
				n++
			}
		}
		return n
	}
	<-f.feed.starts
	for i := 0; i < 3; i++ {
		f.feed.exit <- status.Error(codes.Unavailable, "network")
		<-f.feed.starts
	}
	if n := count("watch ended"); n != 1 {
		t.Fatalf("three ends logged %d times, want once", n)
	}
	send(t, f.feed, mail.Event{Change: mail.ChangeReady})
	eventually(t, "the feed is live", func() bool { return isLive(p) })
	f.feed.exit <- status.Error(codes.Unavailable, "network")
	<-f.feed.starts
	if n := count("live again"); n != 1 {
		t.Fatalf("the recovery was logged %d times, want once", n)
	}
	if n := count("watch ended"); n != 2 {
		t.Fatalf("a new episode after a live feed logged %d ends in total, want 2", n)
	}
}
