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
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/fsnotify/fsnotify"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/josephburnett/gridwell-plugins/fs/fssource"
	pluginv1 "github.com/josephburnett/gridwell/api/gen/plugin/v1"
)

// DebounceWindow is how long a directory's changes gather before they are
// told, once each: an editor's save or a build writes many files within
// milliseconds, and every announcement costs the node a listing.
const DebounceWindow = 200 * time.Millisecond

// Watch tells a shown directory's changes: a ContextChanged when a name in it
// may have come, gone or moved, and an EntryChanged, the entry as List would
// answer it now, for each file in it written or replaced in place, whose
// bytes no listing carries. Empty contexts is a node from before scopes, and
// no directory is cheap to watch, so it watches none. A context not in the
// tree, such as a dead link's target, never changes and is not watched.
func (p *Plugin) Watch(req *pluginv1.WatchRequest, stream pluginv1.Plugin_WatchServer) error {
	scope := map[string]string{}
	for _, key := range req.GetContexts() {
		dir, err := p.dir(key)
		if errors.Is(err, errOutside) {
			continue
		}
		if err != nil {
			return status.Errorf(codes.Unavailable, "fs plugin: watch %s: %v", key, pathErr(err))
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
	for {
		select {
		case <-stream.Context().Done():
			return nil
		case <-s.ready:
			if err := p.tell(stream, s); err != nil {
				return err
			}
		case err := <-s.failed:
			return err
		}
	}
}

// tell sends what s is owed, every listing before any entry. A file that is
// gone or a directory by now has no entry to send; its listing says so.
func (p *Plugin) tell(stream pluginv1.Plugin_WatchServer, s *watchSub) error {
	contexts, files := p.watch.take(s)
	for _, key := range contexts {
		if err := stream.Send(&pluginv1.Change{Payload: &pluginv1.Change_ContextChanged{
			ContextChanged: &pluginv1.ContextChanged{Context: key},
		}}); err != nil {
			return err
		}
	}
	if len(files) == 0 {
		return nil
	}
	root, err := p.realRoot()
	if err != nil {
		return status.Errorf(codes.Unavailable, "fs plugin: watch: %v", pathErr(err))
	}
	for _, f := range files {
		e, err := fssource.Stat(f.path)
		if err != nil || e.Kind == fssource.KindDir {
			continue
		}
		if err := stream.Send(&pluginv1.Change{Payload: &pluginv1.Change_EntryChanged{
			EntryChanged: &pluginv1.EntryChanged{Context: f.context, Entry: entryOf(f.context, filepath.Dir(f.path), root, e)},
		}}); err != nil {
			return err
		}
	}
	return nil
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
	pending map[string]*window
	// limitLogged and errLogged hold each condition to one log line per
	// episode: the limit's ends at a subscribe the OS accepts, an OS error's
	// at the next event delivered.
	limitLogged bool
	errLogged   bool
}

// window is one directory's changes while its debounce is open: whether its
// listing may have moved, and the files whose entries were touched.
type window struct {
	listing bool
	files   map[string]bool
}

// watchSub is one Watch stream: its scope (absolute directory to context
// key) and what it is owed, under the watcher's mu. Owing is a set, so a
// stream that falls behind owes each thing once and loses none; all is owed
// after an OS overflow, when which things changed is lost.
type watchSub struct {
	scope    map[string]string
	contexts map[string]bool
	files    map[string]string // path to its directory's context key
	all      bool
	ready    chan struct{}
	failed   chan error
}

// owedFile is a file whose entry a stream is owed.
type owedFile struct {
	context, path string
}

// take empties what s is owed, sorted, for its stream to send. After an
// overflow that is every directory of the scope and every file in them.
func (w *watcher) take(s *watchSub) (contexts []string, files []owedFile) {
	w.mu.Lock()
	all := s.all
	for k := range s.contexts {
		contexts = append(contexts, k)
	}
	for path, k := range s.files {
		files = append(files, owedFile{context: k, path: path})
	}
	s.all, s.contexts, s.files = false, map[string]bool{}, map[string]string{}
	w.mu.Unlock()
	if all {
		contexts, files = nil, nil
		for dir, k := range s.scope {
			contexts = append(contexts, k)
			entries, _ := fssource.Read(dir)
			for _, e := range entries {
				files = append(files, owedFile{context: k, path: e.AbsPath})
			}
		}
	}
	slices.Sort(contexts)
	slices.SortFunc(files, func(a, b owedFile) int { return strings.Compare(a.path, b.path) })
	return contexts, files
}

func newWatcher() *watcher {
	return &watcher{
		window:  DebounceWindow,
		add:     (*fsnotify.Watcher).Add,
		watched: map[string]bool{},
		subs:    map[*watchSub]struct{}{},
		pending: map[string]*window{},
	}
}

// subscribe watches scope for a new stream. A scope the OS refuses to watch
// whole is the stream's error, and leaves no watch of its behind.
func (w *watcher) subscribe(scope map[string]string) (*watchSub, error) {
	s := &watchSub{
		scope:    scope,
		contexts: map[string]bool{},
		files:    map[string]string{},
		ready:    make(chan struct{}, 1),
		failed:   make(chan error, 1),
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

// event maps one OS event to what it touches. A name that came, went or
// moved, or whose attributes changed (an image's mtime is its picture's
// stamp), touches the listing of the directory holding it; bytes written, or
// a file created over its name as an editor's save does, touch that file's
// entry. A shown directory that vanished or appeared touches its own listing,
// and a vanished one drops its watch.
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
		w.touch(path, true, "")
	case !w.watched[path] && ev.Has(fsnotify.Create) && w.wanted(path):
		if err := w.watchDir(path); err != nil {
			w.fail(path, err)
		}
		w.touch(path, true, "")
	}
	parent := filepath.Dir(path)
	if !w.watched[parent] {
		return
	}
	file := ""
	if ev.Has(fsnotify.Write | fsnotify.Create) {
		file = path
	}
	w.touch(parent, ev.Has(fsnotify.Create|fsnotify.Remove|fsnotify.Rename|fsnotify.Chmod), file)
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
		s.all = true
		raise(s.ready)
	}
}

// touch adds to dir's debounce window, opening one if none is open; its
// close owes what it gathered, once, to every stream showing dir. Callers
// hold mu.
func (w *watcher) touch(dir string, listing bool, file string) {
	win := w.pending[dir]
	if win == nil {
		win = &window{files: map[string]bool{}}
		w.pending[dir] = win
		time.AfterFunc(w.window, func() { w.owe(dir) })
	}
	win.listing = win.listing || listing
	if file != "" {
		win.files[file] = true
	}
}

// owe closes dir's window.
func (w *watcher) owe(dir string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	win := w.pending[dir]
	delete(w.pending, dir)
	for s := range w.subs {
		key, ok := s.scope[dir]
		if !ok {
			continue
		}
		if win.listing {
			s.contexts[key] = true
		}
		for f := range win.files {
			s.files[f] = key
		}
		raise(s.ready)
	}
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
