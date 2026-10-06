package plugin

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/proto"

	"github.com/josephburnett/gridwell-plugins/proc/procsource"
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

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) After(d time.Duration) <-chan time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	ch := make(chan time.Time, 1)
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

// watched is a plugin over a stub /proc whose clock the test moves and whose
// child reads it counts.
type watched struct {
	*Plugin
	clock *fakeClock
	root  string
	mu    sync.Mutex
	reads int
}

func newWatched(t *testing.T, ppids map[int64]int64) *watched {
	t.Helper()
	w := &watched{clock: &fakeClock{now: time.Date(2026, 10, 3, 9, 0, 0, 0, time.UTC)}, root: stubProc(t, ppids)}
	w.Plugin = newPlugin(w.root, 1, nil, w.clock)
	t.Cleanup(w.life.End)
	scan := w.scan
	w.scan = func(ctx context.Context, pid int64) ([]procsource.Stat, error) {
		w.mu.Lock()
		w.reads++
		w.mu.Unlock()
		return scan(ctx, pid)
	}
	return w
}

func (w *watched) read() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.reads
}

// spawn adds pid under ppid to the stub /proc.
func (w *watched) spawn(t *testing.T, pid, ppid int64) {
	t.Helper()
	if err := os.Mkdir(filepath.Join(w.root, strconv.FormatInt(pid, 10)), 0o755); err != nil {
		t.Fatal(err)
	}
	setState(t, w.root, pid, ppid, 'S')
}

// tick moves the clock one poll once the poll is waiting on it, and returns
// once that poll's read is done.
func (w *watched) tick(t *testing.T) {
	t.Helper()
	before := w.read()
	eventually(t, "the poll to wait on the clock", func() bool { return w.clock.pending() > 0 })
	w.clock.Advance(PollEvery)
	eventually(t, "the poll's read", func() bool { return w.read() > before })
}

type watchStream struct {
	grpc.ServerStream
	ctx     context.Context
	mu      sync.Mutex
	header  bool
	sent    []string
	entries []*pluginv1.EntryChanged
}

func (s *watchStream) Context() context.Context { return s.ctx }

func (s *watchStream) SendHeader(metadata.MD) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.header = true
	return nil
}

func (s *watchStream) Send(c *pluginv1.Change) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e := c.GetEntryChanged(); e != nil {
		s.entries = append(s.entries, e)
		return nil
	}
	s.sent = append(s.sent, c.GetContextChanged().GetContext())
	return nil
}

func (s *watchStream) told() []*pluginv1.EntryChanged {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]*pluginv1.EntryChanged(nil), s.entries...)
}

func (s *watchStream) got() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.sent...)
}

func (s *watchStream) hasHeader() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.header
}

// open holds a Watch on contexts until the test ends.
func open(t *testing.T, p *Plugin, contexts ...string) *watchStream {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	s := &watchStream{ctx: ctx}
	done := make(chan error, 1)
	go func() { done <- p.Watch(&pluginv1.WatchRequest{Contexts: contexts}, s) }()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	eventually(t, "the header", s.hasHeader)
	return s
}

func TestInfoDeclaresWatch(t *testing.T) {
	info, err := served(t, stubProc(t, map[int64]int64{1: 0}), 1).Info(context.Background(), &pluginv1.InfoRequest{})
	if err != nil || !info.GetWatch() {
		t.Errorf("Info → %v, %v; want watch declared", info, err)
	}
}

// The node counts the stream open at the header, so it is sent on accept,
// before any change.
func TestWatchSendsItsHeaderOnAccept(t *testing.T) {
	w := newWatched(t, map[int64]int64{1: 0})
	if s := open(t, w.Plugin, "1"); len(s.got()) != 0 {
		t.Errorf("a quiet table announced %v", s.got())
	}
}

// /proc cannot tell, so the plugin polls, and only while a stream shows a
// pid: with no stream the clock passes many polls and the table is never
// read; with one it is read every poll.
func TestNoStreamNoReads(t *testing.T) {
	w := newWatched(t, map[int64]int64{1: 0, 10: 1})
	for range 5 {
		w.clock.Advance(PollEvery)
	}
	if w.read() != 0 {
		t.Fatalf("the table was read %d times with nothing shown", w.read())
	}
	open(t, w.Plugin, "1")
	for range 3 {
		w.tick(t)
	}
}

// A poll announces a shown pid once however many children came and went
// since the last, and announces nothing for a pid nobody shows or for a
// change that is not in the listing (a state letter with no mark). A child
// that turns zombie changes its mark, which is.
func TestAPollAnnouncesOnlyAShownChildSetThatDiffers(t *testing.T) {
	w := newWatched(t, map[int64]int64{1: 0, 10: 1, 20: 10})
	s := open(t, w.Plugin, "1")
	eventually(t, "the first read", func() bool { return w.read() > 0 })

	w.spawn(t, 11, 1)
	w.spawn(t, 12, 1)
	w.spawn(t, 13, 1)
	if err := os.RemoveAll(filepath.Join(w.root, "13")); err != nil {
		t.Fatal(err)
	}
	w.tick(t)
	eventually(t, "the burst's change", func() bool { return len(s.got()) > 0 })

	w.spawn(t, 21, 10)
	setState(t, w.root, 10, 1, 'R')
	w.tick(t)
	w.tick(t)
	if got := s.got(); len(got) != 1 || got[0] != "1" {
		t.Fatalf("announced %v; want the burst once as [1] and nothing for the rest", got)
	}

	setState(t, w.root, 11, 1, 'Z')
	w.tick(t)
	eventually(t, "the zombie's change", func() bool { return len(s.got()) == 2 })
}

// @info's body changes in place behind its unchanged key, so a poll that
// reads it changed tells it as one EntryChanged: the entry as List answers
// it now, under a new stamp. A poll that reads it unchanged tells nothing,
// and no listing is announced for it.
func TestAPollTellsAChangedInfoBodyOnce(t *testing.T) {
	w := newWatched(t, map[int64]int64{1: 0, 10: 1})
	before := listedInfo(t, w.Plugin, "1")
	s := open(t, w.Plugin, "1")
	w.tick(t)
	status := "Name:\tp1\nState:\tR (running)\nPPid:\t0\nVmRSS:\t999 kB\n"
	if err := os.WriteFile(filepath.Join(w.root, "1", "status"), []byte(status), 0o644); err != nil {
		t.Fatal(err)
	}
	w.tick(t)
	eventually(t, "the entry change", func() bool { return len(s.told()) > 0 })
	w.tick(t)
	w.tick(t)
	told := s.told()
	if len(told) != 1 {
		t.Fatalf("told %d entry changes, want 1", len(told))
	}
	now := listedInfo(t, w.Plugin, "1")
	if told[0].GetContext() != "1" || !proto.Equal(told[0].GetEntry(), now) {
		t.Errorf("told %v, want context 1 and List's entry %v", told[0], now)
	}
	if now.GetContentStamp() == before.GetContentStamp() {
		t.Errorf("the stamp stayed %q across a changed body", now.GetContentStamp())
	}
	if got := s.got(); len(got) != 0 {
		t.Errorf("announced listings %v for a body change", got)
	}
}

// listedInfo is context's @info entry as List answers it.
func listedInfo(t *testing.T, p *Plugin, context string) *pluginv1.Entry {
	t.Helper()
	resp, err := p.List(t.Context(), &pluginv1.ListRequest{Context: context})
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range resp.GetEntries() {
		if e.GetKey() == infoKeyPrefix+context {
			return e
		}
	}
	t.Fatalf("no @info in %v", resp.GetEntries())
	return nil
}
