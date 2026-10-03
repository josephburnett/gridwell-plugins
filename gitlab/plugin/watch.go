package plugin

import (
	"context"
	"sync"

	"github.com/josephburnett/gridwell-plugins/gitlab/todos"
	pluginv1 "github.com/josephburnett/gridwell/api/gen/plugin/v1"
)

// watchBuffer bounds one subscriber's backlog: distinct contexts announced and
// not yet sent. A subscriber that falls this far behind has its backlog
// collapsed to its whole scope, so a slow stream costs memory it can name,
// never blocks a walk, and loses no change to a context it shows.
const watchBuffer = 64

// watchers is the fan-out of memory changes to every Watch stream, and the
// owner of whether any is open: the node holds one only while a client shows
// one of these grids, so work runs from the first stream's open until the
// last one's end, and never else.
type watchers struct {
	work func(context.Context)

	mu   sync.Mutex
	subs map[*watcher]struct{}
	idle context.CancelFunc // ends work; nil while no stream is open
}

// watcher is one Watch stream's backlog: a set of contexts in announcement
// order, deduplicated, since a context changed twice needs one repaint.
type watcher struct {
	wake chan struct{}
	// scope is what the stream announces when it loses track: the contexts
	// the node named, or the root when it named none.
	scope   []string
	mu      sync.Mutex
	pending []string
	queued  map[string]bool
}

// subscribe opens a stream over scope, starting work under life if it is the
// first.
func (w *watchers) subscribe(life context.Context, scope []string) *watcher {
	if len(scope) == 0 {
		scope = []string{todos.RootContext}
	}
	s := &watcher{wake: make(chan struct{}, 1), scope: scope, queued: map[string]bool{}}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.subs == nil {
		w.subs = map[*watcher]struct{}{}
	}
	w.subs[s] = struct{}{}
	if w.idle == nil && w.work != nil {
		var ctx context.Context
		ctx, w.idle = context.WithCancel(life)
		go w.work(ctx)
	}
	return s
}

// unsubscribe ends a stream, and work with the last one.
func (w *watchers) unsubscribe(s *watcher) {
	w.mu.Lock()
	defer w.mu.Unlock()
	delete(w.subs, s)
	if len(w.subs) == 0 && w.idle != nil {
		w.idle()
		w.idle = nil
	}
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
			// The collapse keeps the context being announced: it may be one
			// the scope does not name, such as a week first shown since.
			s.pending, s.queued = nil, map[string]bool{}
			for _, k := range s.scope {
				if !s.queued[k] {
					s.pending = append(s.pending, k)
					s.queued[k] = true
				}
			}
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
// mark-done. The to-do list is one account-wide feed, so every change is
// announced whatever the scope; the scope is what an overflow re-announces. A
// todo that leaves pending is a ContextChanged for its week, never an
// EntryRemoved: it stays listed, done-marked (see the package comment), and an
// EntryRemoved would have the node drop a tile the next listing returns.
func (p *Plugin) Watch(req *pluginv1.WatchRequest, stream pluginv1.Plugin_WatchServer) error {
	s := p.watch.subscribe(p.life, req.GetContexts())
	defer p.watch.unsubscribe(s)
	// The node counts the stream open when its header arrives, and only then
	// clears a refusal or catches up after a drop (docs/plugin-authoring.md).
	if err := stream.SendHeader(nil); err != nil {
		return err
	}
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
