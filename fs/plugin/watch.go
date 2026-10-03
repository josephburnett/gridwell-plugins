package plugin

// Watch follows only the directories some client shows (WatchRequest.contexts)
// through the operating system's change notifications, so what it costs is
// what is on screen, never the size of the tree under the root.

import (
	"errors"
	iofs "io/fs"
	"log"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"syscall"
	"time"

	"github.com/fsnotify/fsnotify"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pluginv1 "github.com/josephburnett/gridwell/api/gen/plugin/v1"
)

// DebounceWindow is how long a directory's changes gather before one
// ContextChanged names it: an editor's save or a build writes many files
// within milliseconds, and every announcement costs each client showing the
// directory a listing.
const DebounceWindow = 200 * time.Millisecond

// SubscriberBuffer is how many announcements one Watch stream may fall behind.
// Past it the stream is told its whole scope changed, which is always true
// enough to list again.
const SubscriberBuffer = 64

// Watch announces a ContextChanged for each shown directory whose listing
// may have changed. It never sends EntryRemoved: the node treats that as
// ContextChanged for the context and retires the key by the listing's sweep,
// so naming the key would add nothing. Empty contexts is a node from before
// scopes, and no directory is cheap to watch, so it watches none.
func (p *Plugin) Watch(req *pluginv1.WatchRequest, stream pluginv1.Plugin_WatchServer) error {
	scope := map[string]string{}
	for _, key := range req.GetContexts() {
		dir, err := p.abs(key)
		if err != nil {
			return err
		}
		scope[dir] = key
	}
	s, err := p.watch.subscribe(scope)
	if err != nil {
		return err
	}
	defer p.watch.unsubscribe(s)
	// The watches exist before the header, so no change the node could miss
	// falls between the stream counting open and the OS watching.
	if err := stream.SendHeader(nil); err != nil {
		return err
	}
	send := func(key string) error {
		return stream.Send(&pluginv1.Change{Payload: &pluginv1.Change_ContextChanged{
			ContextChanged: &pluginv1.ContextChanged{Context: key},
		}})
	}
	for {
		select {
		case <-stream.Context().Done():
			return nil
		case key := <-s.ch:
			if err := send(key); err != nil {
				return err
			}
		case <-s.lost:
			for _, key := range s.keys() {
				if err := send(key); err != nil {
					return err
				}
			}
		case err := <-s.failed:
			return err
		}
	}
}

// watcher owns the one OS notification instance. It watches the union of its
// subscribers' scopes and exists only while that union is not empty.
type watcher struct {
	window time.Duration
	// add is the OS watch call; a test injects the limit the OS refuses with.
	add func(fsw *fsnotify.Watcher, dir string) error

	mu      sync.Mutex
	fsw     *fsnotify.Watcher
	watched map[string]bool
	subs    map[*watchSub]struct{}
	pending map[string]bool
	// limitLogged and errLogged hold each condition to one log line per
	// episode: the limit's ends at a subscribe the OS accepts, an OS error's
	// at the next event delivered.
	limitLogged bool
	errLogged   bool
}

// watchSub is one Watch stream: its scope (absolute directory to context
// key) and its queue.
type watchSub struct {
	scope  map[string]string
	ch     chan string
	lost   chan struct{}
	failed chan error
}

func (s *watchSub) keys() []string {
	var out []string
	for _, k := range s.scope {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

func newWatcher() *watcher {
	return &watcher{
		window:  DebounceWindow,
		add:     (*fsnotify.Watcher).Add,
		watched: map[string]bool{},
		subs:    map[*watchSub]struct{}{},
		pending: map[string]bool{},
	}
}

// subscribe watches scope for a new stream. A scope the OS refuses to watch
// whole is the stream's error, and leaves no watch of its behind.
func (w *watcher) subscribe(scope map[string]string) (*watchSub, error) {
	s := &watchSub{
		scope:  scope,
		ch:     make(chan string, SubscriberBuffer),
		lost:   make(chan struct{}, 1),
		failed: make(chan error, 1),
	}
	w.mu.Lock()
	w.subs[s] = struct{}{}
	err := w.rescope()
	if err != nil {
		delete(w.subs, s)
		_ = w.rescope()
	}
	if err == nil {
		w.limitLogged = false
	}
	closing := w.detachIfIdle()
	w.mu.Unlock()
	closeWatcher(closing)
	if err != nil {
		return nil, err
	}
	return s, nil
}

func (w *watcher) unsubscribe(s *watchSub) {
	w.mu.Lock()
	delete(w.subs, s)
	_ = w.rescope()
	closing := w.detachIfIdle()
	w.mu.Unlock()
	closeWatcher(closing)
}

func (w *watcher) wanted(dir string) bool {
	for s := range w.subs {
		if _, ok := s.scope[dir]; ok {
			return true
		}
	}
	return false
}

// rescope makes the OS watches the union of the scopes by set difference: a
// directory leaving every scope loses its watch at once. Callers hold mu.
func (w *watcher) rescope() error {
	for dir := range w.watched {
		if !w.wanted(dir) {
			_ = w.fsw.Remove(dir)
			delete(w.watched, dir)
		}
	}
	for s := range w.subs {
		for dir := range s.scope {
			if err := w.watchDir(dir); err != nil {
				return err
			}
		}
	}
	return nil
}

// watchDir puts dir under an OS watch. A directory that is missing, not a
// directory, or unreadable is not watched and is no error here: its listing
// answers for it.
func (w *watcher) watchDir(dir string) error {
	if w.watched[dir] {
		return nil
	}
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		return nil
	}
	if w.fsw == nil {
		fsw, err := fsnotify.NewWatcher()
		if err != nil {
			return w.verdict(dir, err)
		}
		w.fsw = fsw
		go w.run(fsw)
	}
	if err := w.add(w.fsw, dir); err != nil {
		return w.verdict(dir, err)
	}
	w.watched[dir] = true
	return nil
}

// verdict is what an OS refusal means to the node. Running out of watches is
// ResourceExhausted: the node turns live updates off for the stream, and
// listings still answer.
func (w *watcher) verdict(dir string, err error) error {
	switch {
	case errors.Is(err, iofs.ErrNotExist), errors.Is(err, iofs.ErrPermission):
		return nil
	case errors.Is(err, syscall.ENOSPC), errors.Is(err, syscall.EMFILE), errors.Is(err, syscall.ENFILE):
		verdict := status.Errorf(codes.ResourceExhausted,
			"fs plugin: the OS refused another change watch at %s (%v): raise its watch limit (fs.inotify.max_user_watches on Linux, the open-file limit on macOS) or show fewer directories", dir, err)
		if !w.limitLogged {
			w.limitLogged = true
			log.Print(verdict)
		}
		return verdict
	default:
		return status.Errorf(codes.Internal, "fs plugin: watch %s: %v", dir, err)
	}
}

// detachIfIdle takes the OS instance away once nothing is watched, for the
// caller to close outside mu: closing waits for the reader, which may be
// waiting on mu.
func (w *watcher) detachIfIdle() *fsnotify.Watcher {
	if len(w.watched) > 0 || w.fsw == nil {
		return nil
	}
	fsw := w.fsw
	w.fsw = nil
	return fsw
}

func closeWatcher(fsw *fsnotify.Watcher) {
	if fsw != nil {
		_ = fsw.Close()
	}
}

func (w *watcher) run(fsw *fsnotify.Watcher) {
	for {
		select {
		case ev, ok := <-fsw.Events:
			if !ok {
				return
			}
			w.event(fsw, ev)
		case err, ok := <-fsw.Errors:
			if !ok {
				return
			}
			w.eventError(fsw, err)
		}
	}
}

// event maps one OS event to the directories whose listings it touches: the
// directory holding the path, and the path itself when it is a shown
// directory that vanished or appeared. A vanished directory drops its watch.
func (w *watcher) event(fsw *fsnotify.Watcher, ev fsnotify.Event) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.fsw != fsw {
		return
	}
	w.errLogged = false
	path := filepath.Clean(ev.Name)
	switch {
	case w.watched[path] && ev.Has(fsnotify.Remove|fsnotify.Rename):
		_ = fsw.Remove(path)
		delete(w.watched, path)
		w.touch(path)
	case !w.watched[path] && ev.Has(fsnotify.Create) && w.wanted(path):
		if err := w.watchDir(path); err != nil {
			w.fail(path, err)
		}
		w.touch(path)
	}
	if parent := filepath.Dir(path); w.watched[parent] {
		w.touch(parent)
	}
}

// eventError is an asynchronous OS verdict. An overflow lost events, so every
// shown directory may have changed.
func (w *watcher) eventError(fsw *fsnotify.Watcher, err error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.fsw != fsw {
		return
	}
	if !errors.Is(err, fsnotify.ErrEventOverflow) {
		if !w.errLogged {
			w.errLogged = true
			log.Printf("fs plugin: change notifications: %v", err)
		}
		return
	}
	for s := range w.subs {
		raise(s.lost)
	}
}

// touch opens dir's debounce window if none is open; its close announces dir
// once to every stream showing it.
func (w *watcher) touch(dir string) {
	if w.pending[dir] {
		return
	}
	w.pending[dir] = true
	time.AfterFunc(w.window, func() {
		w.mu.Lock()
		defer w.mu.Unlock()
		delete(w.pending, dir)
		for s := range w.subs {
			key, ok := s.scope[dir]
			if !ok {
				continue
			}
			select {
			case s.ch <- key:
			default:
				raise(s.lost)
			}
		}
	})
}

func (w *watcher) fail(dir string, err error) {
	for s := range w.subs {
		if _, ok := s.scope[dir]; ok {
			select {
			case s.failed <- err:
			default:
			}
		}
	}
}

func raise(ch chan struct{}) {
	select {
	case ch <- struct{}{}:
	default:
	}
}
