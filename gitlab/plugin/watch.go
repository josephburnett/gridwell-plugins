package plugin

import (
	"sync"

	"github.com/josephburnett/gridwell-plugins/gitlab/todos"
	pluginv1 "github.com/josephburnett/gridwell/api/gen/plugin/v1"
)

// watchBuffer bounds one subscriber's backlog: distinct contexts announced and
// not yet sent. A subscriber that falls this far behind has its backlog
// collapsed to the root alone, so a slow stream costs memory it can name and
// never blocks a walk. The weeks it lost repaint on the node's next read of
// them, as they did before there was a Watch.
const watchBuffer = 64

// watchers is the fan-out of memory changes to every Watch stream.
type watchers struct {
	mu   sync.Mutex
	subs map[*watcher]struct{}
}

// watcher is one Watch stream's backlog: a set of contexts in announcement
// order, deduplicated, since a context changed twice needs one repaint.
type watcher struct {
	wake    chan struct{}
	mu      sync.Mutex
	pending []string
	queued  map[string]bool
}

func (w *watchers) subscribe() *watcher {
	s := &watcher{wake: make(chan struct{}, 1), queued: map[string]bool{}}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.subs == nil {
		w.subs = map[*watcher]struct{}{}
	}
	w.subs[s] = struct{}{}
	return s
}

func (w *watchers) unsubscribe(s *watcher) {
	w.mu.Lock()
	defer w.mu.Unlock()
	delete(w.subs, s)
}

// publish queues contexts on every subscriber without waiting on any.
func (w *watchers) publish(contexts []string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	for s := range w.subs {
		s.add(contexts)
	}
}

func (s *watcher) add(contexts []string) {
	s.mu.Lock()
	for _, c := range contexts {
		if s.queued[c] {
			continue
		}
		if len(s.pending) >= watchBuffer {
			// The collapse keeps the context being announced: what arrives
			// after the overflow is exactly what a repaint of the root alone
			// would miss.
			s.pending, s.queued = []string{todos.RootContext}, map[string]bool{todos.RootContext: true}
			if s.queued[c] {
				continue
			}
		}
		s.pending = append(s.pending, c)
		s.queued[c] = true
	}
	s.mu.Unlock()
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// next hands out one context, so the backlog is the stream's only buffer and
// watchBuffer bounds what a stalled subscriber can ever be owed: the one in
// flight plus the backlog. A context re-announced while in flight queues
// again, since the change landed after its send.
func (s *watcher) next() (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.pending) == 0 {
		return "", false
	}
	c := s.pending[0]
	s.pending = s.pending[1:]
	delete(s.queued, c)
	return c, true
}

// Watch streams a ContextChanged for every listing a change to memory moved:
// a walk landing, or failing having absorbed pages, a glance, and a
// mark-done. A todo
// that leaves pending is a ContextChanged for its week, never an
// EntryRemoved: it stays listed, done-marked (see the package comment), and an
// EntryRemoved would have the node drop a tile the next listing returns.
func (p *Plugin) Watch(_ *pluginv1.WatchRequest, stream pluginv1.Plugin_WatchServer) error {
	s := p.watch.subscribe()
	defer p.watch.unsubscribe(s)
	for {
		select {
		case <-stream.Context().Done():
			return stream.Context().Err()
		case <-s.wake:
		}
		for c, ok := s.next(); ok; c, ok = s.next() {
			change := &pluginv1.Change{Payload: &pluginv1.Change_ContextChanged{ContextChanged: &pluginv1.ContextChanged{Context: c}}}
			if err := stream.Send(change); err != nil {
				return err
			}
		}
	}
}

// announce hands every watcher the listings memory's latest changes moved,
// and reports whether there were any. Everything that changes memory calls it
// once its change has landed.
func (p *Plugin) announce() bool {
	cs := p.mem.TakeChanges().Contexts()
	if len(cs) > 0 {
		p.watch.publish(cs)
	}
	return len(cs) > 0
}
