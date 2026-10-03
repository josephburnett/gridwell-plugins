package plugin

import (
	"bytes"
	"context"
	"fmt"
	"log"
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

// refusing is a oneShot that refuses every page while refuse is set.
type refusing struct {
	oneShot
	refuse atomic.Bool
}

func (r *refusing) Page(ctx context.Context, state string, page int) (todos.Reply, error) {
	if r.refuse.Load() {
		return todos.Reply{}, status.Error(codes.PermissionDenied, "401 Unauthorized")
	}
	return r.oneShot.Page(ctx, state, page)
}

// lines collects what the plugin logs.
type lines struct {
	mu  sync.Mutex
	got []string
}

func (l *lines) logf(format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.got = append(l.got, fmt.Sprintf(format, args...))
}

func (l *lines) take() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := l.got
	l.got = nil
	return out
}

// GitLab failing is one episode: logged once when it starts, whether a
// glance or a walk meets it, and again only after a read has landed. A walk
// that works logs nothing, and nothing goes past the plugin's one logger.
func TestGitLabFailingLogsOncePerEpisode(t *testing.T) {
	var std bytes.Buffer
	was := log.Writer()
	log.SetOutput(&std)
	t.Cleanup(func() { log.SetOutput(was) })

	src := &refusing{oneShot: oneShot{pending: []todos.Todo{mk(1, "2026-08-18T10:00:00Z", "pending")}}}
	clock := at("2026-08-25T12:00:00Z")
	var l lines
	p := New(src, Options{Now: func() time.Time { return clock }, Logf: l.logf})
	tick := func() {
		t.Helper()
		clock = clock.Add(DefaultRefresh)
		p.tick(context.Background())
		landed(t, p)
	}
	if _, err := p.List(context.Background(), &pluginv1.ListRequest{Context: todos.RootContext}); err != nil {
		t.Fatal(err)
	}
	landed(t, p)
	tick()
	if got := l.take(); len(got) != 0 {
		t.Errorf("a walk and a glance that worked logged %q", got)
	}

	src.refuse.Store(true)
	tick()
	tick()
	clock = clock.Add(DefaultFullRefresh) // the next tick walks, and fails too
	tick()
	if got := l.take(); len(got) != 1 || !strings.Contains(got[0], "401") {
		t.Errorf("three failures in one episode logged %q, want one line", got)
	}

	src.refuse.Store(false)
	tick()
	if got := l.take(); len(got) != 0 {
		t.Errorf("GitLab answering again logged %q", got)
	}
	src.refuse.Store(true)
	tick()
	if got := l.take(); len(got) != 1 {
		t.Errorf("a second episode logged %q, want one line", got)
	}
	if std.Len() != 0 {
		t.Errorf("the plugin logged past its logger: %q", std.String())
	}
}
