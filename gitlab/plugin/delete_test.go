package plugin

import (
	"context"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/josephburnett/gridwell-plugins/gitlab/todos"
	pluginv1 "github.com/josephburnett/gridwell/api/gen/plugin/v1"
)

// fakeMarker records mark-done calls, fails on demand, and mirrors GitLab:
// an accepted write moves the todo out of the source's pending list, the way
// the real API does, so a later walk agrees with the flip.
type fakeMarker struct {
	src   *oneShot
	calls []int64
	err   error
}

func (m *fakeMarker) MarkDone(_ context.Context, id int64) error {
	if m.err != nil {
		return m.err
	}
	m.calls = append(m.calls, id)
	kept := m.src.pending[:0]
	for _, t := range m.src.pending {
		if t.ID == id {
			t.State = todos.StateDone
			m.src.done = append(m.src.done, t)
			continue
		}
		kept = append(kept, t)
	}
	m.src.pending = kept
	return nil
}

// weekState reads one todo's state out of the week listing, "" when unlisted.
func weekState(t *testing.T, p *Plugin, week string, key string) string {
	t.Helper()
	resp, err := p.List(context.Background(), &pluginv1.ListRequest{Context: week})
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range resp.Entries {
		if e.Key == key {
			return e.StatusDetail
		}
	}
	return ""
}

// The trash gesture marks the todo done: GitLab accepts the write, the
// listing flips, the counts move, and the flip survives a restart through the
// cache file — before any new walk confirms it.
func TestDeleteMarksTheTodoDone(t *testing.T) {
	dir := t.TempDir()
	src := &oneShot{pending: []todos.Todo{mk(1, "2026-08-18T10:00:00Z", "pending")}}
	m := &fakeMarker{src: src}
	p := New(src, Options{StateDir: dir, Marker: m})
	ctx := context.Background()

	root, err := p.List(ctx, &pluginv1.ListRequest{Context: todos.RootContext})
	if err != nil {
		t.Fatal(err)
	}
	if root.SourceLabel != "gitlab todos · 1 open · 0 done" {
		t.Fatalf("before: %q", root.SourceLabel)
	}
	week := todos.WeekKey(todos.WeekStart(at("2026-08-18T10:00:00Z")))
	key := "todo:1"

	if _, err := p.Delete(ctx, &pluginv1.DeleteRequest{Key: key}); err != nil {
		t.Fatal(err)
	}
	if len(m.calls) != 1 || m.calls[0] != 1 {
		t.Fatalf("marker calls = %v, want the one todo", m.calls)
	}
	if got := weekState(t, p, week, key); got != todos.StateDone {
		t.Fatalf("after delete the listing says %q, want done", got)
	}
	root, err = p.List(ctx, &pluginv1.ListRequest{Context: todos.RootContext})
	if err != nil {
		t.Fatal(err)
	}
	if root.SourceLabel != "gitlab todos · 0 open · 1 done" {
		t.Fatalf("after: %q", root.SourceLabel)
	}

	// Idempotent: the second gesture succeeds without a second write.
	if _, err := p.Delete(ctx, &pluginv1.DeleteRequest{Key: key}); err != nil || len(m.calls) != 1 {
		t.Fatalf("second delete: err=%v calls=%v", err, m.calls)
	}

	// The flip survives a restart from the cache file alone: the reborn
	// plugin's source answers nothing, and the walk stamp is fresh, so what
	// it lists is what the file remembered.
	reborn := New(&oneShot{}, Options{StateDir: dir, Marker: m})
	if got := weekState(t, reborn, week, key); got != todos.StateDone {
		t.Fatalf("after restart the listing says %q, want done", got)
	}
}

// A refused write changes nothing anywhere: the error surfaces, the listing
// keeps saying pending, and no restart resurrects a flip that never landed.
func TestDeleteFailureLeavesTheMemoryAlone(t *testing.T) {
	dir := t.TempDir()
	src := &oneShot{pending: []todos.Todo{mk(1, "2026-08-18T10:00:00Z", "pending")}}
	m := &fakeMarker{src: src, err: status.Error(codes.PermissionDenied, "gitlab: 403 (marking done needs the token's api scope)")}
	p := New(src, Options{StateDir: dir, Marker: m})
	ctx := context.Background()
	if _, err := p.List(ctx, &pluginv1.ListRequest{Context: todos.RootContext}); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Delete(ctx, &pluginv1.DeleteRequest{Key: "todo:1"}); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("delete = %v, want the write's own verdict", err)
	}
	week := todos.WeekKey(todos.WeekStart(at("2026-08-18T10:00:00Z")))
	if got := weekState(t, p, week, "todo:1"); got != todos.StatePending {
		t.Fatalf("after a refused write the listing says %q, want pending", got)
	}
}

// The gesture's refusals: a week is not one todo, an unknown key is nothing,
// an unwalked memory cannot judge, and a read-only plugin says so.
func TestDeleteRefusals(t *testing.T) {
	src := &oneShot{pending: []todos.Todo{mk(1, "2026-08-18T10:00:00Z", "pending")}}
	m := &fakeMarker{src: src}
	p := New(src, Options{Marker: m})
	ctx := context.Background()
	if _, err := p.List(ctx, &pluginv1.ListRequest{Context: todos.RootContext}); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		key  string
		want codes.Code
	}{
		{"week:2026-08-17", codes.FailedPrecondition},
		{"junk", codes.InvalidArgument},
		{"todo:999", codes.NotFound},
	} {
		if _, err := p.Delete(ctx, &pluginv1.DeleteRequest{Key: tc.key}); status.Code(err) != tc.want {
			t.Errorf("delete %q = %v, want %v", tc.key, err, tc.want)
		}
	}
	if len(m.calls) != 0 {
		t.Fatalf("a refusal must not write: calls = %v", m.calls)
	}

	unwalked := New(&oneShot{}, Options{Marker: m, Refresh: time.Hour, Now: func() time.Time { return at("2026-08-18T10:00:00Z") }})
	unwalked.syncedAt[todos.RootContext] = at("2026-08-18T10:00:00Z") // fresh, so Delete never waits on a walk
	if _, err := unwalked.Delete(ctx, &pluginv1.DeleteRequest{Key: "todo:999"}); status.Code(err) != codes.Unavailable {
		t.Errorf("unknown todo before the first walk = %v, want Unavailable", err)
	}

	readonly := New(src, Options{})
	if _, err := readonly.Delete(ctx, &pluginv1.DeleteRequest{Key: "todo:1"}); status.Code(err) != codes.Unimplemented {
		t.Errorf("no marker = %v, want Unimplemented", err)
	}
}
