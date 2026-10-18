package memo

import (
	"context"
	"sync"
	"time"
)

// Life is the plugin's lifetime: the context every detached walk and every
// piece of watched work runs under, so none outlives the plugin and none runs
// under context.Background (rule 13). A plugin process ends by being killed,
// so in production nothing calls End; a test calls it to stop what it started.
type Life struct {
	ctx    context.Context
	cancel context.CancelFunc
	mu     sync.Mutex
	ended  bool
	wg     sync.WaitGroup
}

// NewLife starts a lifetime.
func NewLife() *Life {
	ctx, cancel := context.WithCancel(context.Background())
	return &Life{ctx: ctx, cancel: cancel}
}

// Context ends when the lifetime does.
func (l *Life) Context() context.Context { return l.ctx }

// Go runs fn in its own goroutine under the lifetime's context. After End, fn
// still runs, with a context already done, so a caller waiting on its result
// is released rather than stranded.
func (l *Life) Go(fn func(ctx context.Context)) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.ended {
		go fn(l.ctx)
		return
	}
	l.wg.Add(1)
	go func() {
		defer l.wg.Done()
		fn(l.ctx)
	}()
}

// End cancels the lifetime and waits for everything Go started before it.
func (l *Life) End() {
	l.mu.Lock()
	l.ended = true
	l.mu.Unlock()
	l.cancel()
	l.wg.Wait()
}

// Clock is time as memo reads it; a test injects one it advances by hand.
type Clock interface {
	Now() time.Time
	After(d time.Duration) <-chan time.Time
}

// System is the wall clock.
var System Clock = systemClock{}

type systemClock struct{}

func (systemClock) Now() time.Time                         { return time.Now() }
func (systemClock) After(d time.Duration) <-chan time.Time { return time.After(d) }

// Poll calls fn every interval until ctx ends, the first time after one
// interval: the stream that started the work is followed by the node's own
// listing, which is the first refresh. It is the loop for a source that
// cannot tell, run as a Changes unit's Do so it polls only while watched.
func Poll(ctx context.Context, clock Clock, every time.Duration, fn func(ctx context.Context)) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-clock.After(every):
			if ctx.Err() != nil {
				return // both were ready: the end wins
			}
			fn(ctx)
		}
	}
}
