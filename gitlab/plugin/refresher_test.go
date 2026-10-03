package plugin

import (
	"context"
	"reflect"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/josephburnett/gridwell-plugins/gitlab/todos"
	pluginv1 "github.com/josephburnett/gridwell/api/gen/plugin/v1"
)

// ledger is a oneShot that writes down every page asked of it.
type ledger struct {
	oneShot
	mu    sync.Mutex
	asked []string
}

func (l *ledger) Page(ctx context.Context, state string, page int) (todos.Reply, error) {
	l.mu.Lock()
	l.asked = append(l.asked, state+"/"+strconv.Itoa(page))
	l.mu.Unlock()
	return l.oneShot.Page(ctx, state, page)
}

// take answers the pages asked since the last take.
func (l *ledger) take() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := l.asked
	l.asked = nil
	return out
}

// ticking builds a plugin over a GitLab holding todos 1 and 2, walked once at
// the plugin's clock, and a watcher on it that has heard that walk.
func ticking(t *testing.T) (*Plugin, *ledger, *time.Time, *watchStream) {
	t.Helper()
	src := &ledger{oneShot: oneShot{pending: []todos.Todo{mk(2, "2026-08-25T10:00:00Z", "pending"), mk(1, "2026-08-18T10:00:00Z", "pending")}}}
	clock := at("2026-08-25T12:00:00Z")
	p := New(src, Options{Now: func() time.Time { return clock }})
	w := watching(t, p, nil)
	p.tick(context.Background())
	landed(t, p)
	changed(t, w, 3)
	if got := src.take(); !reflect.DeepEqual(got, []string{"pending/1", "done/1"}) {
		t.Fatalf("the cold tick asked %v, want a full walk", got)
	}
	return p, src, &clock, w
}

// Inside the full-refresh window a tick asks GitLab for the newest pending
// page and nothing else, and a page showing nothing new changes nothing. No
// test counted the requests one refresh made, so every refresh walked every
// page, done list included.
func TestATickAsksOnePageWhenNothingChanged(t *testing.T) {
	p, src, clock, w := ticking(t)
	*clock = clock.Add(DefaultRefresh)
	p.tick(context.Background())
	landed(t, p)
	if got := src.take(); !reflect.DeepEqual(got, []string{"pending/1"}) {
		t.Errorf("a quiet tick asked %v, want only the newest pending page", got)
	}
	if got := changed(t, w, 0); got != nil {
		t.Errorf("a quiet tick announced %v", got)
	}
}

// A todo newer than every known one is absorbed and announced from that one
// page, with no walk, and without stretching the full walk's window.
func TestATickAbsorbsNewTodosWithoutAWalk(t *testing.T) {
	p, src, clock, w := ticking(t)
	src.pending = append([]todos.Todo{mk(3, "2026-09-01T10:00:00Z", "pending")}, src.pending...)
	*clock = clock.Add(DefaultRefresh)
	p.tick(context.Background())
	landed(t, p)
	if got := src.take(); !reflect.DeepEqual(got, []string{"pending/1"}) {
		t.Errorf("a tick with news asked %v, want only the newest pending page", got)
	}
	if got := changed(t, w, 2); !reflect.DeepEqual(got, []string{"todos", "week:2026-08-31"}) {
		t.Errorf("the new todo announced %v", got)
	}
	wk, err := p.List(context.Background(), &pluginv1.ListRequest{Context: "week:2026-08-31"})
	if err != nil || len(wk.Entries) != 1 || wk.Entries[0].Key != "todo:3" {
		t.Fatalf("the new todo's week = (%v, %v)", wk.GetEntries(), err)
	}
	p.mu.Lock()
	walked := p.syncedAt[todos.RootContext]
	p.mu.Unlock()
	if !walked.Equal(at("2026-08-25T12:00:00Z")) {
		t.Errorf("a glance moved the walk stamp to %v", walked)
	}
}

// A todo gone from pending is judged done by the full walk alone, when its
// window comes round: one page cannot show absence.
func TestOnlyTheFullWalkJudgesAbsence(t *testing.T) {
	p, src, clock, w := ticking(t)
	src.pending = src.pending[:1] // todo 1 is done at GitLab
	*clock = clock.Add(DefaultRefresh)
	p.tick(context.Background())
	landed(t, p)
	if got := weekState(t, p, "week:2026-08-17", "todo:1"); got != todos.StatePending {
		t.Errorf("a glance judged todo 1 %q", got)
	}
	src.take()
	*clock = at("2026-08-25T12:00:00Z").Add(DefaultFullRefresh)
	p.tick(context.Background())
	landed(t, p)
	if got := src.take(); !reflect.DeepEqual(got, []string{"pending/1", "done/1"}) {
		t.Errorf("the tick past the window asked %v, want a full walk", got)
	}
	if got := weekState(t, p, "week:2026-08-17", "todo:1"); got != todos.StateDone {
		t.Errorf("after the full walk todo 1 is %q", got)
	}
	if got := changed(t, w, 2); !reflect.DeepEqual(got, []string{"todos", "week:2026-08-17"}) {
		t.Errorf("the full walk announced %v", got)
	}
}

// The full walk keeps its own cadence under the ticks: across an hour of
// ticks it runs once per window, and every other tick glances.
func TestTheFullWalkRunsOnItsOwnCadence(t *testing.T) {
	p, src, clock, _ := ticking(t)
	start := *clock
	walks, glances := 0, 0
	for *clock = start.Add(DefaultRefresh); clock.Before(start.Add(time.Hour)); *clock = clock.Add(DefaultRefresh) {
		p.tick(context.Background())
		landed(t, p)
		switch got := src.take(); {
		case reflect.DeepEqual(got, []string{"pending/1"}):
			glances++
		case reflect.DeepEqual(got, []string{"pending/1", "done/1"}):
			walks++
		default:
			t.Fatalf("a tick asked %v", got)
		}
	}
	if want := int(time.Hour/DefaultFullRefresh) - 1; walks != want {
		t.Errorf("an hour of ticks walked %d times, want %d", walks, want)
	}
	if glances == 0 {
		t.Error("no tick glanced")
	}
}

// hanging is a oneShot that, once hang is set, answers nothing until the
// caller gives up, and says so on hung, then on gaveUp.
type hanging struct {
	oneShot
	hang   atomic.Bool
	hung   chan struct{}
	gaveUp chan struct{}
}

func (h *hanging) Page(ctx context.Context, state string, page int) (todos.Reply, error) {
	if h.hang.Load() {
		h.hung <- struct{}{}
		<-ctx.Done()
		defer func() { h.gaveUp <- struct{}{} }()
		return todos.Reply{}, ctx.Err()
	}
	return h.oneShot.Page(ctx, state, page)
}

// A glance cut short because the last stream ended is the refresher
// stopping, not GitLab failing: the next read answers memory with no error.
func TestAGlanceCutShortIsNotAFailure(t *testing.T) {
	src := &hanging{oneShot: oneShot{pending: []todos.Todo{mk(1, "2026-08-18T10:00:00Z", "pending")}},
		hung: make(chan struct{}, 1), gaveUp: make(chan struct{}, 1)}
	clock := at("2026-08-25T12:00:00Z")
	p := New(src, Options{Refresh: MinRefresherInterval, Now: func() time.Time { return clock }})
	t.Cleanup(p.Close)
	if _, err := p.List(context.Background(), &pluginv1.ListRequest{Context: todos.RootContext}); err != nil {
		t.Fatal(err)
	}
	landed(t, p)
	src.hang.Store(true)
	w := watching(t, p, nil)
	select {
	case <-src.hung:
	case <-time.After(5 * time.Second):
		t.Fatal("the refresher never glanced")
	}
	w.cancel()
	<-src.gaveUp
	time.Sleep(50 * time.Millisecond) // the glance takes its verdict
	// Past the window a warm read answers the last failure without waiting
	// on the walk it starts: there must be none.
	src.hang.Store(false)
	clock = clock.Add(DefaultFullRefresh)
	if _, err := p.List(context.Background(), &pluginv1.ListRequest{Context: todos.RootContext}); err != nil {
		t.Fatalf("the read after the stream ended answered %v", err)
	}
}
