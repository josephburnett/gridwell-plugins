package plugin

import (
	"context"
	"errors"
	"google.golang.org/grpc/metadata"
	"reflect"
	"sort"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/josephburnett/gridwell-plugins/gitlab/todos"
	pluginv1 "github.com/josephburnett/gridwell/api/gen/plugin/v1"
)

// watchStream is a Watch stream the test reads: every sent Change lands on
// sent, and Send parks on block when it is non-nil — a slow node.
type watchStream struct {
	pluginv1.Plugin_WatchServer
	ctx    context.Context
	cancel context.CancelFunc // the node ending the stream
	sent   chan *pluginv1.Change
	block  chan struct{}
	header atomic.Bool
}

func (w *watchStream) Context() context.Context     { return w.ctx }
func (w *watchStream) SendHeader(metadata.MD) error { w.header.Store(true); return nil }
func (w *watchStream) Send(c *pluginv1.Change) error {
	if !w.header.Load() {
		return errors.New("a Change before the header")
	}
	if w.block != nil {
		<-w.block
	}
	w.sent <- c
	return nil
}

// The node counts a Watch stream open at its header, so a plugin that only
// speaks at its first change leaves a refusal standing and a dropped stream
// uncaught-up until something happens to change.
func TestWatchSendsItsHeaderOnAccept(t *testing.T) {
	p := New(&oneShot{}, Options{})
	w := watching(t, p, nil)
	deadline := time.Now().Add(5 * time.Second)
	for !w.header.Load() {
		if time.Now().After(deadline) {
			t.Fatal("Watch accepted the stream and sent no header")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// The node opens Watch only on the declaration, so a Watch left undeclared
// leaves a todo finished at GitLab open on screen until the user reads again.
func TestInfoDeclaresWatch(t *testing.T) {
	info, err := New(&oneShot{}, Options{}).Info(context.Background(), &pluginv1.InfoRequest{})
	if err != nil || !info.GetWatch() {
		t.Fatalf("Info = (%v, %v), want watch declared", info, err)
	}
}

// watching subscribes a Watch stream to p with no scope and returns it once
// subscribed.
func watching(t *testing.T, p *Plugin, block chan struct{}) *watchStream {
	t.Helper()
	return watchingScope(t, p, block, nil)
}

// watchingScope is watching with the scope the node names.
func watchingScope(t *testing.T, p *Plugin, block chan struct{}, scope []string) *watchStream {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	w := &watchStream{ctx: ctx, cancel: cancel, sent: make(chan *pluginv1.Change, 1024), block: block}
	done := make(chan error, 1)
	go func() { done <- p.Watch(&pluginv1.WatchRequest{Contexts: scope}, w) }()
	t.Cleanup(func() {
		cancel()
		if block != nil {
			select {
			case <-block:
			default:
				close(block)
			}
		}
		<-done
	})
	deadline := time.Now().Add(5 * time.Second)
	for {
		if p.changes.Watching(refreshUnit) {
			return w
		}
		if time.Now().After(deadline) {
			t.Fatal("Watch never subscribed")
		}
		time.Sleep(time.Millisecond)
	}
}

// unwatched waits until p's refresher has stopped: every stream has ended
// and its linger with it.
func unwatched(t *testing.T, p *Plugin) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if !p.changes.Watching(refreshUnit) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("Watch never let go of its stream")
		}
		time.Sleep(time.Millisecond)
	}
}

// changed collects the changes sent until want of them arrived, or until a
// quiet spell when want is zero, sorted: a context by its key, an entry as
// "entry <context>/<key>".
func changed(t *testing.T, w *watchStream, want int) []string {
	t.Helper()
	var got []string
	quiet := 50 * time.Millisecond
	if want > 0 {
		quiet = 5 * time.Second
	}
	for want == 0 || len(got) < want {
		select {
		case c := <-w.sent:
			got = append(got, said(c))
		case <-time.After(quiet):
			if want > 0 {
				t.Fatalf("announced %v, want %d contexts", got, want)
			}
			sort.Strings(got)
			return got
		}
	}
	// Nothing past what was expected.
	select {
	case c := <-w.sent:
		t.Fatalf("announced %v and then %v", got, c)
	case <-time.After(20 * time.Millisecond):
	}
	sort.Strings(got)
	return got
}

func said(c *pluginv1.Change) string {
	if e := c.GetEntryChanged(); e != nil {
		return "entry " + e.GetContext() + "/" + e.GetEntry().GetKey()
	}
	return c.GetContextChanged().GetContext()
}

// A walk that learns a todo's body changed behind its unchanged name tells
// it as one EntryChanged: the entry as its week's List answers it, under a
// new stamp, and no listing. A walk that learns nothing tells nothing.
func TestAWalkTellsAChangedBodyAsItsEntry(t *testing.T) {
	src := &oneShot{pending: []todos.Todo{mk(1, "2026-08-18T10:00:00Z", "pending")}}
	clock := at("2026-08-25T12:00:00Z")
	p := New(src, Options{Now: func() time.Time { return clock }})
	w := watching(t, p, nil)
	walk := func() []*pluginv1.Change {
		t.Helper()
		clock = clock.Add(DefaultFullRefresh + time.Second)
		if _, err := p.List(context.Background(), &pluginv1.ListRequest{Context: todos.RootContext}); err != nil {
			t.Fatal(err)
		}
		landed(t, p)
		var got []*pluginv1.Change
		for {
			select {
			case c := <-w.sent:
				got = append(got, c)
			case <-time.After(50 * time.Millisecond):
				return got
			}
		}
	}
	walk()
	before := listed(t, p, "week:2026-08-17", "todo:1")
	src.pending[0].Body = "a new note"
	got := walk()
	if len(got) != 1 || got[0].GetEntryChanged() == nil {
		t.Fatalf("a body change sent %v, want one EntryChanged", got)
	}
	now := listed(t, p, "week:2026-08-17", "todo:1")
	if e := got[0].GetEntryChanged(); e.GetContext() != "week:2026-08-17" || !proto.Equal(e.GetEntry(), now) {
		t.Errorf("told %v, want the week's listed entry %v", e, now)
	}
	if now.GetContentStamp() == before.GetContentStamp() {
		t.Errorf("the stamp stayed %q across a changed body", now.GetContentStamp())
	}
	if got := walk(); len(got) != 0 {
		t.Errorf("an unchanged walk sent %v", got)
	}
}

// listed is key's entry as context's List answers it.
func listed(t *testing.T, p *Plugin, context, key string) *pluginv1.Entry {
	t.Helper()
	resp, err := p.List(t.Context(), &pluginv1.ListRequest{Context: context})
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range resp.GetEntries() {
		if e.GetKey() == key {
			return e
		}
	}
	t.Fatalf("%s lists no %s", context, key)
	return nil
}

// A landed walk that changed memory tells every watcher exactly the listings
// it moved, and one that changed nothing tells nobody anything. Before Watch
// nothing outside the plugin observed a walk landing: the node repainted only
// when it next read.
func TestAWalkAnnouncesExactlyTheContextsItMoved(t *testing.T) {
	src := &oneShot{pending: []todos.Todo{mk(1, "2026-08-18T10:00:00Z", "pending"), mk(2, "2026-08-25T10:00:00Z", "pending")}}
	clock := at("2026-08-25T12:00:00Z")
	p := New(src, Options{Now: func() time.Time { return clock }, Marker: &fakeMarker{src: src}})
	w := watching(t, p, nil)
	walk := func() {
		t.Helper()
		clock = clock.Add(DefaultFullRefresh + time.Second)
		if _, err := p.List(context.Background(), &pluginv1.ListRequest{Context: todos.RootContext}); err != nil {
			t.Fatal(err)
		}
		landed(t, p)
	}

	walk()
	if got := changed(t, w, 3); !reflect.DeepEqual(got, []string{"todos", "week:2026-08-17", "week:2026-08-24"}) {
		t.Errorf("the first walk announced %v", got)
	}
	walk()
	if got := changed(t, w, 0); got != nil {
		t.Errorf("an unchanged walk announced %v", got)
	}
	// Todo 1 is done at GitLab: its week and the root's counts move, and its
	// body, which says done.
	src.pending = src.pending[1:]
	walk()
	if got := changed(t, w, 3); !reflect.DeepEqual(got, []string{"entry week:2026-08-17/todo:1", "todos", "week:2026-08-17"}) {
		t.Errorf("a derived done announced %v", got)
	}
	// The trash gesture announces its flip without waiting for a walk.
	if _, err := p.Delete(context.Background(), &pluginv1.DeleteRequest{Key: "todo:2"}); err != nil {
		t.Fatal(err)
	}
	if got := changed(t, w, 3); !reflect.DeepEqual(got, []string{"entry week:2026-08-24/todo:2", "todos", "week:2026-08-24"}) {
		t.Errorf("mark-done announced %v", got)
	}
}
