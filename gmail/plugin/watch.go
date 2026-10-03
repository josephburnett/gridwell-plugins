package plugin

import (
	"sync"

	"github.com/josephburnett/gridwell-plugins/gmail/mailbox"
	pluginv1 "github.com/josephburnett/gridwell/api/gen/plugin/v1"
)

// watchers fans out which collections a landed refresh changed. Each
// subscriber holds at most one pending mark per collection and a one-slot
// wake, so a publish never blocks and a slow subscriber costs a mark per
// collection, never a refresh.
type watchers struct {
	mu   sync.Mutex
	subs map[*watcher]struct{}
}

type watcher struct {
	mu      sync.Mutex
	pending map[string]bool
	wake    chan struct{}
}

func (w *watchers) subscribe() (*watcher, func()) {
	s := &watcher{pending: map[string]bool{}, wake: make(chan struct{}, 1)}
	w.mu.Lock()
	if w.subs == nil {
		w.subs = map[*watcher]struct{}{}
	}
	w.subs[s] = struct{}{}
	w.mu.Unlock()
	return s, func() {
		w.mu.Lock()
		delete(w.subs, s)
		w.mu.Unlock()
	}
}

func (w *watchers) publish(keys []string) {
	if len(keys) == 0 {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	for s := range w.subs {
		s.mu.Lock()
		for _, k := range keys {
			s.pending[k] = true
		}
		s.mu.Unlock()
		select {
		case s.wake <- struct{}{}:
		default:
		}
	}
}

// take empties the subscriber's marks, in the order the contexts are
// declared.
func (s *watcher) take() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for _, key := range mailbox.Contexts() {
		if s.pending[key] {
			out = append(out, key)
		}
	}
	clear(s.pending)
	return out
}

// views is what one context answers now.
func (p *Plugin) views(key string) []mailbox.View {
	if key == mailbox.AllMailContext {
		return p.mem.AllMail()
	}
	return p.mem.Collection(key)
}

// faces is what every context answers now.
func (p *Plugin) faces() map[string][]mailbox.View {
	out := map[string][]mailbox.View{}
	for _, key := range mailbox.Contexts() {
		out[key] = p.views(key)
	}
	return out
}

// changedSince names the contexts whose answer is no longer before's.
func (p *Plugin) changedSince(before map[string][]mailbox.View) []string {
	var out []string
	for _, key := range mailbox.Contexts() {
		if !mailbox.SameViews(before[key], p.views(key)) {
			out = append(out, key)
		}
	}
	return out
}

// Watch streams ContextChanged for each collection a landed refresh
// changed, until the node hangs up. It never sends EntryRemoved: a listing
// here is not authoritative, and whether a message has left is Probe's to
// say, so "the inbox changed, list it again" is the whole announcement.
// Marks that arrive while a send is slow coalesce, one per collection.
func (p *Plugin) Watch(_ *pluginv1.WatchRequest, stream pluginv1.Plugin_WatchServer) error {
	s, cancel := p.watchers.subscribe()
	defer cancel()
	// The node counts the stream open when its header arrives, and only then
	// clears a refusal or catches up after a drop (docs/plugin-authoring.md).
	if err := stream.SendHeader(nil); err != nil {
		return err
	}
	for {
		select {
		case <-stream.Context().Done():
			return nil
		case <-s.wake:
		}
		for _, key := range s.take() {
			change := &pluginv1.Change{Payload: &pluginv1.Change_ContextChanged{
				ContextChanged: &pluginv1.ContextChanged{Context: key},
			}}
			if err := stream.Send(change); err != nil {
				return err
			}
		}
	}
}
