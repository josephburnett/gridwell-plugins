package plugin

import (
	"context"
	"errors"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/josephburnett/gridwell-plugins/gitlab/todos"
	pluginv1 "github.com/josephburnett/gridwell/api/gen/plugin/v1"
)

// flaky fails the first failures calls with err, then serves one todo.
type flaky struct {
	err      error
	failures int
	calls    int
}

func (f *flaky) Page(_ context.Context, _ string, _ int) ([]todos.Todo, bool, error) {
	f.calls++
	if f.calls <= f.failures {
		return nil, false, f.err
	}
	return []todos.Todo{mk(1, "2026-08-18T10:00:00Z", "pending")}, false, nil
}

// A page that fails on weather is retried in place: the walk sees one
// answer, not one failure, and a lid closing mid-walk costs a page.
func TestAPageRetriesInPlace(t *testing.T) {
	src := &flaky{err: status.Error(codes.Unavailable, "connection reset"), failures: 2}
	got, _, err := retrying{src: src, attempts: pageAttempts, backoff: time.Millisecond}.Page(context.Background(), todos.StatePending, 1)
	if err != nil || len(got) != 1 {
		t.Fatalf("page = (%v, %v), want the answer after the retries", got, err)
	}
	if src.calls != 3 {
		t.Errorf("tried %d times, want 3", src.calls)
	}
}

// The attempts are bounded: the walk fails with the real reason rather than
// hanging on a GitLab that is down.
func TestRetriesRunOutAndTheReasonSurvives(t *testing.T) {
	src := &flaky{err: status.Error(codes.Unavailable, "connection reset"), failures: 99}
	_, _, err := retrying{src: src, attempts: pageAttempts, backoff: time.Millisecond}.Page(context.Background(), todos.StatePending, 1)
	if status.Code(err) != codes.Unavailable {
		t.Fatalf("err = %v, want the source's Unavailable", err)
	}
	if src.calls != pageAttempts {
		t.Errorf("tried %d times, want %d", src.calls, pageAttempts)
	}
}

// A verdict is not weather: a token without the scope reads the same every
// time, and retrying it only delays the reason reaching the user.
func TestAVerdictIsNotRetried(t *testing.T) {
	for _, code := range []codes.Code{codes.PermissionDenied, codes.Unauthenticated, codes.InvalidArgument} {
		src := &flaky{err: status.Error(code, "no"), failures: 99}
		if _, _, err := (retrying{src: src, attempts: pageAttempts, backoff: time.Millisecond}).Page(context.Background(), todos.StatePending, 1); status.Code(err) != code {
			t.Fatalf("%v = %v", code, err)
		}
		if src.calls != 1 {
			t.Errorf("%v tried %d times, want 1", code, src.calls)
		}
	}
	// An error carrying no status is weather: nothing says it is a verdict.
	plain := &flaky{err: errors.New("boom"), failures: 1}
	if _, _, err := (retrying{src: plain, attempts: pageAttempts, backoff: time.Millisecond}).Page(context.Background(), todos.StatePending, 1); err != nil {
		t.Fatalf("plain error = %v", err)
	}
}

// A cancelled context stops the retries at once, with the source's error: a
// read the node gave up on must not keep paging GitLab.
func TestACancelledContextStopsRetrying(t *testing.T) {
	src := &flaky{err: status.Error(codes.Unavailable, "gone"), failures: 99}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	if _, _, err := (retrying{src: src, attempts: pageAttempts, backoff: time.Hour}).Page(ctx, todos.StatePending, 1); status.Code(err) != codes.Unavailable {
		t.Fatalf("err = %v", err)
	}
	if src.calls != 1 || time.Since(start) > time.Second {
		t.Errorf("tried %d times in %v; a cancelled read must stop at once", src.calls, time.Since(start))
	}
}

// New wraps its source once, so every walk — a read's, a descent's, the
// refresher's — is resilient without Memory.Sync knowing anything about
// retries.
func TestTheWalkSurvivesAFlakyPage(t *testing.T) {
	src := &flaky{err: status.Error(codes.Unavailable, "reset"), failures: 1}
	p := New(src, Options{})
	root, err := p.List(context.Background(), &pluginv1.ListRequest{Context: todos.RootContext})
	if err != nil {
		t.Fatalf("List = %v; one flaky page must not fail the walk", err)
	}
	if len(root.Entries) != 1 || src.calls != 3 {
		t.Errorf("root = %v after %d page calls (one retried pending page, then done)", root.Entries, src.calls)
	}
}
