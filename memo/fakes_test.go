package memo

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc/metadata"

	pluginv1 "github.com/josephburnett/gridwell/api/gen/plugin/v1"
)

// fakeClock moves only when Advance says so.
type fakeClock struct {
	mu      sync.Mutex
	now     time.Time
	waiters []waiter
}

type waiter struct {
	at time.Time
	ch chan time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{now: time.Date(2026, 10, 2, 9, 0, 0, 0, time.UTC)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) After(d time.Duration) <-chan time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	ch := make(chan time.Time, 1)
	if d <= 0 {
		ch <- c.now
		return ch
	}
	c.waiters = append(c.waiters, waiter{at: c.now.Add(d), ch: ch})
	return ch
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
	kept := c.waiters[:0]
	for _, w := range c.waiters {
		if w.at.After(c.now) {
			kept = append(kept, w)
		} else {
			w.ch <- c.now
		}
	}
	c.waiters = kept
}

func (c *fakeClock) pending() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.waiters)
}

// awaitWaiters returns once n goroutines are blocked on the clock.
func (c *fakeClock) awaitWaiters(t *testing.T, n int) {
	t.Helper()
	eventually(t, fmt.Sprintf("%d clock waiters", n), func() bool { return c.pending() >= n })
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

// fakeStream records what a Watch sends. While gate is non-nil every Send
// waits for a token on it, so a test can hold a stream behind.
type fakeStream struct {
	ctx    context.Context
	cancel context.CancelFunc
	gate   chan struct{}
	// entered is signalled as each Send begins, before it waits on gate.
	entered chan struct{}

	mu     sync.Mutex
	header bool
	sent   []string
	// beforeHeader is how many changes were sent before the header: always 0.
	beforeHeader int
}

func newFakeStream() *fakeStream {
	ctx, cancel := context.WithCancel(context.Background())
	return &fakeStream{ctx: ctx, cancel: cancel, entered: make(chan struct{}, 1024)}
}

// held is a stream whose every Send waits for the test.
func held() *fakeStream {
	s := newFakeStream()
	s.gate = make(chan struct{})
	return s
}

func (s *fakeStream) Context() context.Context { return s.ctx }

func (s *fakeStream) SendHeader(metadata.MD) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.header = true
	return nil
}

func (s *fakeStream) Send(c *pluginv1.Change) error {
	s.entered <- struct{}{}
	if s.gate != nil {
		select {
		case <-s.gate:
		case <-s.ctx.Done():
			return s.ctx.Err()
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.header {
		s.beforeHeader++
	}
	s.sent = append(s.sent, said(c))
	return nil
}

// said is one change as a test reads it: a context's key, or an entry's
// context, key and label.
func said(c *pluginv1.Change) string {
	if e := c.GetEntryChanged(); e != nil {
		return "entry " + e.GetContext() + "/" + e.GetEntry().GetKey() + "@" + e.GetEntry().GetLabel()
	}
	return c.GetContextChanged().GetContext()
}

func (s *fakeStream) hasHeader() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.header
}

func (s *fakeStream) got() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.sent...)
}

// logs collects log lines.
type logs struct {
	mu    sync.Mutex
	lines []string
}

func (l *logs) logf(format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, fmt.Sprintf(format, args...))
}

func (l *logs) count() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.lines)
}
