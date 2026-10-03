package plugin

import (
	"context"
	"errors"
	"google.golang.org/grpc/metadata"
	"reflect"
	"slices"
	"sort"
	"sync/atomic"
	"testing"
	"time"

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
		p.watch.mu.Lock()
		n := len(p.watch.subs)
		p.watch.mu.Unlock()
		if n > 0 {
			return w
		}
		if time.Now().After(deadline) {
			t.Fatal("Watch never subscribed")
		}
		time.Sleep(time.Millisecond)
	}
}

// unwatched waits until p has let go of every Watch stream.
func unwatched(t *testing.T, p *Plugin) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		p.watch.mu.Lock()
		n := len(p.watch.subs)
		p.watch.mu.Unlock()
		if n == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("Watch never let go of its stream")
		}
		time.Sleep(time.Millisecond)
	}
}

// changed collects the contexts announced until want of them arrived, or
// until a quiet spell when want is zero, sorted.
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
			got = append(got, c.GetContextChanged().GetContext())
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
	// Todo 1 is done at GitLab: its week and the root's counts move.
	src.pending = src.pending[1:]
	walk()
	if got := changed(t, w, 2); !reflect.DeepEqual(got, []string{"todos", "week:2026-08-17"}) {
		t.Errorf("a derived done announced %v", got)
	}
	// The trash gesture announces its flip without waiting for a walk.
	if _, err := p.Delete(context.Background(), &pluginv1.DeleteRequest{Key: "todo:2"}); err != nil {
		t.Fatal(err)
	}
	if got := changed(t, w, 2); !reflect.DeepEqual(got, []string{"todos", "week:2026-08-24"}) {
		t.Errorf("mark-done announced %v", got)
	}
}

// stall overflows a stream that stops reading: each of more walks than the
// backlog holds brings a todo in a week of its own, and none may wait on the
// stream. It answers what the stream hears once it reads again, and the
// week the last walk brought.
func stall(t *testing.T, scope ...string) (heard []string, last string) {
	t.Helper()
	src := &oneShot{}
	clock := at("2026-08-25T12:00:00Z")
	p := New(src, Options{Now: func() time.Time { return clock }})
	block := make(chan struct{})
	w := watchingScope(t, p, block, scope)
	monday := at("2026-08-24T10:00:00Z")
	for i := 0; i < watchBuffer+5; i++ {
		src.pending = append(src.pending, mk(int64(i+1), monday.AddDate(0, 0, -7*i).Format(time.RFC3339), "pending"))
		clock = clock.Add(DefaultFullRefresh + time.Second)
		walked := make(chan struct{})
		go func() {
			_, _ = p.List(context.Background(), &pluginv1.ListRequest{Context: todos.RootContext})
			landed(t, p)
			close(walked)
		}()
		select {
		case <-walked:
		case <-time.After(5 * time.Second):
			t.Fatalf("walk %d waited on a watcher that is not reading", i)
		}
	}
	close(block)
	return changed(t, w, 0), todos.WeekKey(todos.WeekStart(monday.AddDate(0, 0, -7*(watchBuffer+4))))
}

// A subscriber that stops reading never holds up a walk, and when it reads
// again it is owed at most the one in flight plus a full backlog. With no
// scope its overflowed backlog has become the root, and it still hears the
// last walk's week.
func TestASlowWatcherNeverBlocksAWalk(t *testing.T) {
	got, last := stall(t)
	if len(got) > watchBuffer+1 || !slices.Contains(got, todos.RootContext) || !slices.Contains(got, last) {
		t.Errorf("after the stall the watcher heard %d contexts %v", len(got), got)
	}
}

// An overflow announces the stream's whole scope: a week shown and announced
// before the backlog overflowed is told again, not lost with the backlog.
func TestAnOverflowAnnouncesTheScope(t *testing.T) {
	first := todos.WeekKey(todos.WeekStart(at("2026-08-24T10:00:00Z")))
	got, last := stall(t, todos.RootContext, first)
	for _, want := range []string{todos.RootContext, first, last} {
		if !slices.Contains(got, want) {
			t.Errorf("after the overflow the watcher heard %v, missing %s", got, want)
		}
	}
	if len(got) > watchBuffer+1 {
		t.Errorf("after the overflow the watcher heard %d contexts", len(got))
	}
}
