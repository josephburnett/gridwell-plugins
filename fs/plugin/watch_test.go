package plugin

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/fsnotify/fsnotify"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	pluginv1 "github.com/josephburnett/gridwell/api/gen/plugin/v1"
)

// patience bounds every wait on the OS, which delivers in milliseconds.
const patience = 10 * time.Second

type watchStream struct {
	grpc.ServerStream
	ctx     context.Context
	header  chan struct{}
	changes chan string
}

func (s *watchStream) Context() context.Context { return s.ctx }

func (s *watchStream) SendHeader(metadata.MD) error {
	close(s.header)
	return nil
}

func (s *watchStream) Send(c *pluginv1.Change) error {
	s.changes <- c.GetContextChanged().GetContext()
	return nil
}

// opened is one Watch stream the node holds open.
type opened struct {
	*watchStream
	done  chan error
	stop  context.CancelFunc
	ended sync.Once
}

func openWatch(t *testing.T, p *Plugin, contexts ...string) *opened {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	s := &watchStream{ctx: ctx, header: make(chan struct{}), changes: make(chan string, 100)}
	o := &opened{watchStream: s, done: make(chan error, 1), stop: cancel}
	go func() { o.done <- p.Watch(&pluginv1.WatchRequest{Contexts: contexts}, s) }()
	t.Cleanup(func() { o.close(t) })
	select {
	case <-s.header:
	case err := <-o.done:
		t.Fatalf("Watch(%v) ended before its header: %v", contexts, err)
	case <-time.After(patience):
		t.Fatalf("Watch(%v) sent no header", contexts)
	}
	return o
}

func (o *opened) close(t *testing.T) {
	t.Helper()
	o.ended.Do(func() {
		o.stop()
		select {
		case <-o.done:
		case <-time.After(patience):
			t.Error("Watch did not end with its context")
		}
	})
}

// until collects announcements until want arrives, which proves every
// earlier OS event on the instance was delivered and its window closed.
func (o *opened) until(t *testing.T, want string) []string {
	t.Helper()
	var got []string
	deadline := time.After(patience)
	for {
		select {
		case key := <-o.changes:
			if key == want {
				return got
			}
			got = append(got, key)
		case <-deadline:
			t.Fatalf("no ContextChanged{%q}; got %v", want, got)
		}
	}
}

// all waits until every key in want has been announced.
func (o *opened) all(t *testing.T, want ...string) {
	t.Helper()
	missing := map[string]bool{}
	for _, k := range want {
		missing[k] = true
	}
	deadline := time.After(patience)
	for len(missing) > 0 {
		select {
		case key := <-o.changes:
			delete(missing, key)
		case <-deadline:
			t.Fatalf("never announced %v", missing)
		}
	}
}

func mkdirs(t *testing.T, root string, dirs ...string) {
	t.Helper()
	for _, d := range dirs {
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
}

func write(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func (p *Plugin) watchedDirs() map[string]bool {
	p.watch.mu.Lock()
	defer p.watch.mu.Unlock()
	out := map[string]bool{}
	for d := range p.watch.watched {
		root, _ := p.realRoot()
		rel, _ := filepath.Rel(root, d)
		out[rel] = true
	}
	return out
}

// sentinel is an in-scope directory whose announcement, after a gesture,
// shows nothing else was said about it.
const sentinel = "s"

func poke(t *testing.T, root string) {
	t.Helper()
	write(t, filepath.Join(root, sentinel, fmt.Sprint(time.Now().UnixNano())), "")
}

func TestInfoDeclaresWatch(t *testing.T) {
	info, err := New(t.TempDir(), nil).Info(context.Background(), &pluginv1.InfoRequest{})
	if err != nil || !info.Watch {
		t.Fatalf("Info = %v, %v; want watch declared", info, err)
	}
}

// An editor's save or a build is many OS events in one directory; the
// stream says so once. A directory outside the scope says nothing.
func TestWatchAnnouncesABurstOnceForItsDirectory(t *testing.T) {
	root := t.TempDir()
	mkdirs(t, root, "a", "b", sentinel)
	p := New(root, nil)
	o := openWatch(t, p, "a", sentinel)

	a := filepath.Join(root, "a")
	write(t, filepath.Join(a, "x"), "one")
	write(t, filepath.Join(a, "x"), "two")
	write(t, filepath.Join(a, ".x.swp"), "")
	if err := os.Rename(filepath.Join(a, ".x.swp"), filepath.Join(a, "y")); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(a, "x")); err != nil {
		t.Fatal(err)
	}
	if got := o.until(t, "a"); len(got) != 0 {
		t.Fatalf("before a: %v", got)
	}
	write(t, filepath.Join(root, "b", "z"), "")
	poke(t, root)
	if got := o.until(t, sentinel); len(got) != 0 {
		t.Fatalf("one burst in a and a write in b said %v beyond the one ContextChanged{a}", got)
	}
}

// A directory leaving the scope loses its OS watch at once, and the last
// stream ending leaves no watch at all.
func TestWatchFollowsTheScope(t *testing.T) {
	root := t.TempDir()
	mkdirs(t, root, "a", "b")
	p := New(root, nil)
	first := openWatch(t, p, "a")
	if got := p.watchedDirs(); !got["a"] || len(got) != 1 {
		t.Fatalf("scope [a] watches %v", got)
	}
	first.close(t)
	if got := p.watchedDirs(); len(got) != 0 {
		t.Fatalf("no stream, still watching %v", got)
	}
	if p.watch.fsw != nil {
		t.Fatal("no stream, the OS instance is still open")
	}

	second := openWatch(t, p, "b")
	if got := p.watchedDirs(); !got["b"] || len(got) != 1 {
		t.Fatalf("scope [b] watches %v", got)
	}
	write(t, filepath.Join(root, "a", "x"), "")
	write(t, filepath.Join(root, "b", "x"), "")
	if got := second.until(t, "b"); len(got) != 0 {
		t.Fatalf("scope [b] also said %v", got)
	}
}

// A node from before scopes asks with none; no directory is cheap enough to
// watch unasked.
func TestWatchWithNoScopeWatchesNothing(t *testing.T) {
	root := t.TempDir()
	p := New(root, nil)
	openWatch(t, p)
	if got := p.watchedDirs(); len(got) != 0 || p.watch.fsw != nil {
		t.Fatalf("empty scope watches %v", got)
	}
}

// A shown directory that stops existing changes its parent's listing and
// its own, and its watch is gone.
func TestWatchAnnouncesAVanishedDirectory(t *testing.T) {
	for name, vanish := range map[string]func(dir string) error{
		"removed": os.RemoveAll,
		"renamed": func(dir string) error { return os.Rename(dir, dir+".moved") },
	} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			mkdirs(t, root, "a", sentinel)
			write(t, filepath.Join(root, "a", "f"), "")
			p := New(root, nil)
			o := openWatch(t, p, ".", "a", sentinel)
			if err := vanish(filepath.Join(root, "a")); err != nil {
				t.Fatal(err)
			}
			o.all(t, ".", "a")
			if got := p.watchedDirs(); got["a"] {
				t.Fatalf("a is gone and still watched: %v", got)
			}
		})
	}
}

// Two streams at once each hear their own scope.
func TestWatchFansOutToEveryStream(t *testing.T) {
	root := t.TempDir()
	mkdirs(t, root, "a", "b")
	p := New(root, nil)
	one := openWatch(t, p, "a")
	two := openWatch(t, p, "a", "b")
	write(t, filepath.Join(root, "b", "x"), "")
	write(t, filepath.Join(root, "a", "x"), "")
	if got := one.until(t, "a"); len(got) != 0 {
		t.Fatalf("scope [a] said %v", got)
	}
	two.until(t, "a")
	two.close(t)
	if got := p.watchedDirs(); !got["a"] || got["b"] {
		t.Fatalf("after [a b] ended, watching %v; want [a]", got)
	}
}

// Running out of OS watches is the stream's error, so the node turns live
// updates off while listings still answer; no half-scope stays watched.
func TestWatchLimitIsTheStreamError(t *testing.T) {
	root := t.TempDir()
	mkdirs(t, root, "a", "b")
	p := New(root, nil)
	p.watch.add = func(fsw *fsnotify.Watcher, dir string) error {
		if filepath.Base(dir) == "b" {
			return fmt.Errorf("inotify_add_watch: %w", syscall.ENOSPC)
		}
		return fsw.Add(dir)
	}

	for range 2 {
		s := &watchStream{ctx: context.Background(), header: make(chan struct{}), changes: make(chan string, 1)}
		err := p.Watch(&pluginv1.WatchRequest{Contexts: []string{"a", "b"}}, s)
		if status.Code(err) != codes.ResourceExhausted {
			t.Fatalf("Watch = %v; want ResourceExhausted", err)
		}
		select {
		case <-s.header:
			t.Fatal("a refused scope sent its header")
		default:
		}
		if got := p.watchedDirs(); len(got) != 0 {
			t.Fatalf("a refused scope left %v watched", got)
		}
	}
}

// A shown directory created again at its path is watched again: its parent
// sees it appear.
func TestWatchRewatchesADirectoryCreatedAgain(t *testing.T) {
	root := t.TempDir()
	mkdirs(t, root, "a")
	p := New(root, nil)
	o := openWatch(t, p, ".", "a")
	if err := os.Remove(filepath.Join(root, "a")); err != nil {
		t.Fatal(err)
	}
	o.all(t, ".", "a")
	mkdirs(t, root, "a")
	o.all(t, ".", "a")
	if !p.watchedDirs()["a"] {
		t.Fatal("a is back and not watched")
	}
	write(t, filepath.Join(root, "a", "f"), "")
	o.until(t, "a")
}

// An OS overflow lost events, so every directory in the scope may have
// changed and each is announced.
func TestWatchOverflowAnnouncesTheWholeScope(t *testing.T) {
	root := t.TempDir()
	mkdirs(t, root, "a", "b")
	p := New(root, nil)
	o := openWatch(t, p, ".", "a", "b")
	p.watch.mu.Lock()
	fsw := p.watch.fsw
	p.watch.mu.Unlock()
	p.watch.eventError(fsw, fsnotify.ErrEventOverflow)
	o.all(t, ".", "a", "b")
}

func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var logged bytes.Buffer
	log.SetOutput(&logged)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	return &logged
}

// The watch limit logs when it starts, not on each refusal; a subscribe the
// OS accepts ends the episode, and the next refusal logs again.
func TestWatchLimitLogsOncePerEpisode(t *testing.T) {
	root := t.TempDir()
	mkdirs(t, root, "a", "b")
	p := New(root, nil)
	limited := true
	p.watch.add = func(fsw *fsnotify.Watcher, dir string) error {
		if limited && filepath.Base(dir) == "b" {
			return fmt.Errorf("inotify_add_watch: %w", syscall.ENOSPC)
		}
		return fsw.Add(dir)
	}
	logged := captureLog(t)
	refuse := func() {
		t.Helper()
		s := &watchStream{ctx: context.Background(), header: make(chan struct{}), changes: make(chan string, 1)}
		if err := p.Watch(&pluginv1.WatchRequest{Contexts: []string{"a", "b"}}, s); status.Code(err) != codes.ResourceExhausted {
			t.Fatalf("Watch = %v; want ResourceExhausted", err)
		}
	}
	count := func() int { return strings.Count(logged.String(), "refused another change watch") }

	refuse()
	refuse()
	if n := count(); n != 1 {
		t.Fatalf("one episode logged %d times:\n%s", n, logged)
	}
	limited = false
	openWatch(t, p, "a", "b").close(t)
	limited = true
	refuse()
	if n := count(); n != 2 {
		t.Fatalf("a second episode logged %d lines in all, want 2:\n%s", n, logged)
	}
}

// Any other OS notification error logs when it starts; an event delivered
// again ends the episode, and the next error logs again.
func TestWatchErrorLogsOncePerEpisode(t *testing.T) {
	root := t.TempDir()
	mkdirs(t, root, "a")
	p := New(root, nil)
	o := openWatch(t, p, "a")
	p.watch.mu.Lock()
	fsw := p.watch.fsw
	p.watch.mu.Unlock()
	logged := captureLog(t)
	broken := errors.New("read: input/output error")
	count := func() int { return strings.Count(logged.String(), broken.Error()) }

	p.watch.eventError(fsw, broken)
	p.watch.eventError(fsw, broken)
	if n := count(); n != 1 {
		t.Fatalf("one episode logged %d times:\n%s", n, logged)
	}
	write(t, filepath.Join(root, "a", "x"), "")
	o.until(t, "a")
	p.watch.eventError(fsw, broken)
	if n := count(); n != 2 {
		t.Fatalf("a second episode logged %d lines in all, want 2:\n%s", n, logged)
	}
}
