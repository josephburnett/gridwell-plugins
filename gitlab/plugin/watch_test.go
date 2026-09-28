package plugin

import (
	"context"
	"reflect"
	"slices"
	"sort"
	"testing"
	"time"

	"github.com/josephburnett/gridwell-plugins/gitlab/todos"
	pluginv1 "github.com/josephburnett/gridwell/api/gen/plugin/v1"
)

// watchStream is a Watch stream the test reads: every sent Change lands on
// sent, and Send parks on block when it is non-nil — a slow node.
type watchStream struct {
	pluginv1.Plugin_WatchServer
	ctx   context.Context
	sent  chan *pluginv1.Change
	block chan struct{}
}

func (w *watchStream) Context() context.Context { return w.ctx }
func (w *watchStream) Send(c *pluginv1.Change) error {
	if w.block != nil {
		<-w.block
	}
	w.sent <- c
	return nil
}

// watching subscribes a Watch stream to p and returns it once subscribed.
func watching(t *testing.T, p *Plugin, block chan struct{}) *watchStream {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	w := &watchStream{ctx: ctx, sent: make(chan *pluginv1.Change, 1024), block: block}
	done := make(chan error, 1)
	go func() { done <- p.Watch(&pluginv1.WatchRequest{}, w) }()
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

// A subscriber that stops reading never holds up a walk, and when it reads
// again its overflowed backlog has become the root.
func TestASlowWatcherNeverBlocksAWalk(t *testing.T) {
	src := &oneShot{}
	clock := at("2026-08-25T12:00:00Z")
	p := New(src, Options{Now: func() time.Time { return clock }})
	block := make(chan struct{})
	w := watching(t, p, block)
	// Each walk brings a todo in a week of its own: more distinct weeks than
	// the backlog holds.
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
	// Every walk announced a week of its own and the root: 70 distinct
	// contexts. The stalled watcher is owed at most the one in flight plus a
	// full backlog, and still hears the root and the last walk's week.
	got := changed(t, w, 0)
	last := todos.WeekKey(todos.WeekStart(monday.AddDate(0, 0, -7*(watchBuffer+4))))
	if len(got) > watchBuffer+1 || !slices.Contains(got, todos.RootContext) || !slices.Contains(got, last) {
		t.Errorf("after the stall the watcher heard %d contexts %v", len(got), got)
	}
}
