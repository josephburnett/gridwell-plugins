package plugin

import (
	"context"
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
)

// retrying is a Source whose failed page is retried in place. It wraps the
// source once, in New, so every walk — a read's, a descent's, the background
// refresher's — gets the same resilience, and Memory.Sync stays the walk and
// nothing else.
type retrying struct {
	src      todos.Source
	attempts int
	backoff  time.Duration
}

// Page tries src.Page until it answers, the attempts run out, the context is
// done, or the failure is a verdict rather than weather. A verdict — a token
// without the read_api scope, a malformed request — will not read differently
// the second time, and retrying it only delays the reason reaching the user.
func (r retrying) Page(ctx context.Context, state string, page int) ([]todos.Todo, bool, error) {
	wait := r.backoff
	for attempt := 1; ; attempt++ {
		out, more, err := r.src.Page(ctx, state, page)
		if err == nil {
			return out, more, nil
		}
		if attempt >= r.attempts || !transient(err) || ctx.Err() != nil {
			return nil, false, err
		}
		select {
		case <-ctx.Done():
			return nil, false, err
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
	case codes.PermissionDenied, codes.Unauthenticated, codes.InvalidArgument:
		return false
	}
	return true
}
