package plugin

import (
	"context"
	"sync"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/josephburnett/gridwell-plugins/gitlab/todos"
)

// pageAttempts and pageBackoff bound how hard one page tries before the walk
// fails. A walk is dozens of pages; a lid closing, a reset connection or a
// rate limit hits one of them, and losing the whole walk to a moment's
// weather costs every page again. The backoff doubles, so three attempts
// spend well under a second of waiting of their own.
//
// The two bounds compose and neither duplicates the other:
// gitlabapi.DefaultTimeout bounds one REQUEST, so a stall always ends in an
// error instead of hanging the walk, and this bounds how many times a page
// that ended in an error is asked for again.
const (
	pageAttempts = 3
	pageBackoff  = 250 * time.Millisecond
	// retryLogCap is how many retry lines one outage logs before it falls
	// silent until a page answers.
	retryLogCap = 5
)

// retrying is a Source whose failed page is retried in place. It wraps the
// source once, in New, so every walk — a read's, a descent's, the background
// refresher's — gets the same resilience, and Cache.Sync stays the walk and
// nothing else.
type retrying struct {
	log      *retryLog
	src      todos.Source
	attempts int
	backoff  time.Duration
}

// retryLog is the retries' share of the plugin's log: a line per retry, with
// the error's class and never its body, up to retryLogCap per episode; any
// page that answers ends the episode.
type retryLog struct {
	logf   func(format string, args ...any)
	mu     sync.Mutex
	logged int
}

func newRetryLog(logf func(format string, args ...any)) *retryLog {
	return &retryLog{logf: logf}
}

func (l *retryLog) retry(state string, page int, err error, attempt, of int, wait time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	switch {
	case l.logged < retryLogCap:
		l.logf("gitlab plugin: page %d of %s failed (%s); retry %d of %d in %s", page, state, status.Code(err), attempt, of, wait)
	case l.logged == retryLogCap:
		l.logf("gitlab plugin: further retries are not logged until a page answers")
	default:
		return
	}
	l.logged++
}

func (l *retryLog) answered() {
	l.mu.Lock()
	l.logged = 0
	l.mu.Unlock()
}

// Page tries src.Page until it answers, the attempts run out, the grid is
// done, or the failure is a verdict rather than weather. A verdict — a token
// without the read_api scope, a malformed request — will not read differently
// the second time, and retrying it only delays the reason reaching the user.
func (r retrying) Page(ctx context.Context, state string, page int) (todos.Reply, error) {
	wait := r.backoff
	for attempt := 1; ; attempt++ {
		out, err := r.src.Page(ctx, state, page)
		if err == nil {
			r.log.answered()
			return out, nil
		}
		if attempt >= r.attempts || !transient(err) || ctx.Err() != nil {
			return todos.Reply{}, err
		}
		r.log.retry(state, page, err, attempt, r.attempts-1, wait)
		select {
		case <-ctx.Done():
			return todos.Reply{}, err
		case <-time.After(wait):
		}
		wait *= 2
	}
}

// transient reports whether an error is worth another try. Only the codes
// that are verdicts about the configuration are not: everything else,
// including an error carrying no status at all, is treated as weather.
func transient(err error) bool {
	switch status.Code(err) {
	case codes.PermissionDenied, codes.Unauthenticated, codes.InvalidArgument, codes.FailedPrecondition:
		return false
	}
	return true
}
