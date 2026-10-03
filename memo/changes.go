package memo

import (
	"context"
	"slices"
	"sync"
	"time"

	"google.golang.org/grpc/metadata"

	pluginv1 "github.com/josephburnett/gridwell/api/gen/plugin/v1"
)

// DefaultBuffer is how many distinct contexts one stream may be owed before
// it is told its whole scope instead.
const DefaultBuffer = 64

// DefaultLinger is how long work no stream needs keeps running. The node
// changes a stream's scope by ending it and opening another, and a feed or a
// poll stopped and restarted across that gap would cost the source a start.
const DefaultLinger = 10 * time.Second

// Stream is the half of a Watch stream Changes uses;
// pluginv1.Plugin_WatchServer is one.
type Stream interface {
	Context() context.Context
	SendHeader(metadata.MD) error
	Send(*pluginv1.Change) error
}

// ChangeOptions configures Changes. All are optional.
type ChangeOptions struct {
	// Buffer is DefaultBuffer when zero.
	Buffer int
	// Unscoped is the scope taken for a stream that names none (a node from
	// before scopes): what the plugin judges cheap to watch.
	Unscoped []string
	// Work maps a context in scope to the units of background work it needs:
	// an account-wide feed maps every context to one unit, a poll of a root
	// maps the root and everything under it to the root. Nil maps a context
	// to itself.
	Work func(context string) []string
	// Do runs one unit while some stream needs it, until ctx ends: when the
	// last stream needing it has been gone Linger, or the plugin's life ends.
	// Nil runs nothing; Watching still answers.
	Do func(ctx context.Context, unit string)
	// Linger is DefaultLinger when zero; negative stops work at once.
	Linger time.Duration
	Clock  Clock
}

// Changes is the plugin's Watch: it fans each change out to every stream
// without waiting on any, and it is the one place background work starts,
// so a walk, a glance or a feed runs only while a stream has its context in
// scope (rule 8).
type Changes struct {
	life *Life
	o    ChangeOptions

	mu    sync.Mutex
	subs  map[*sub]struct{}
	units map[string]*unit
}

type unit struct {
	streams int
	gen     int // bumped on every release, so a stale linger stops nothing
	cancel  context.CancelFunc
}

// sub is one stream's queue: distinct contexts in announcement order, since a
// context changed twice needs one repaint.
type sub struct {
	scope []string
	units []string
	wake  chan struct{}

	mu      sync.Mutex
	pending []string
	queued  map[string]bool
}

// NewChanges builds the fan-out for one plugin, its work run under life.
func NewChanges(life *Life, o ChangeOptions) *Changes {
	if o.Buffer <= 0 {
		o.Buffer = DefaultBuffer
	}
	if o.Linger == 0 {
		o.Linger = DefaultLinger
	}
	if o.Work == nil {
		o.Work = func(c string) []string { return []string{c} }
	}
	if o.Clock == nil {
		o.Clock = System
	}
	return &Changes{life: life, o: o, subs: map[*sub]struct{}{}, units: map[string]*unit{}}
}

// Serve is a plugin's Watch method: it takes the stream's scope, starts the
// work that scope needs, sends the header (the node counts the stream open
// only then), and sends a ContextChanged per queued context until the node
// hangs up. Every change goes to every stream whatever its scope, because a
// change to one context can be what repaints another that links to it; the
// scope decides what work runs and what an overflow announces.
func (c *Changes) Serve(contexts []string, stream Stream) error {
	scope := contexts
	if len(scope) == 0 {
		scope = c.o.Unscoped
	}
	s := &sub{scope: slices.Clone(scope), wake: make(chan struct{}, 1), queued: map[string]bool{}}
	c.attach(s)
	defer c.detach(s)
	if err := stream.SendHeader(nil); err != nil {
		return err
	}
	for {
		select {
		case <-stream.Context().Done():
			return nil
		case <-s.wake:
		}
		for key, ok := s.next(); ok; key, ok = s.next() {
			if err := stream.Send(&pluginv1.Change{Payload: &pluginv1.Change_ContextChanged{
				ContextChanged: &pluginv1.ContextChanged{Context: key},
			}}); err != nil {
				return err
			}
		}
	}
}

// Publish tells every stream these contexts' listings changed.
func (c *Changes) Publish(contexts ...string) {
	if len(contexts) == 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for s := range c.subs {
		s.add(contexts, c.o.Buffer)
	}
}

// Watching reports whether unit's work is running: some stream needs it, or
// the last one left less than Linger ago.
func (c *Changes) Watching(unit string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	_, ok := c.units[unit]
	return ok
}

func (c *Changes) attach(s *sub) {
	seen := map[string]bool{}
	for _, ctx := range s.scope {
		for _, u := range c.o.Work(ctx) {
			if !seen[u] {
				seen[u] = true
				s.units = append(s.units, u)
			}
		}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.subs[s] = struct{}{}
	for _, name := range s.units {
		u, ok := c.units[name]
		if !ok {
			u = c.startLocked(name)
		}
		u.streams++
	}
}

func (c *Changes) startLocked(name string) *unit {
	ctx, cancel := context.WithCancel(c.life.Context())
	u := &unit{cancel: cancel}
	c.units[name] = u
	if c.o.Do != nil {
		c.life.Go(func(context.Context) { c.o.Do(ctx, name) })
	}
	return u
}

func (c *Changes) detach(s *sub) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.subs, s)
	for _, name := range s.units {
		u := c.units[name]
		u.streams--
		if u.streams > 0 {
			continue
		}
		u.gen++
		if c.o.Linger < 0 {
			c.stopLocked(name, u)
			continue
		}
		gen := u.gen
		c.life.Go(func(life context.Context) {
			select {
			case <-life.Done():
			case <-c.o.Clock.After(c.o.Linger):
				c.mu.Lock()
				if c.units[name] == u && u.streams == 0 && u.gen == gen {
					c.stopLocked(name, u)
				}
				c.mu.Unlock()
			}
		})
	}
}

func (c *Changes) stopLocked(name string, u *unit) {
	u.cancel()
	delete(c.units, name)
}

// add queues contexts. A stream owed more than buffer distinct contexts has
// lost track of which: it is owed its whole scope, and the context being
// announced, which a repaint of the scope alone might miss.
func (s *sub) add(contexts []string, buffer int) {
	s.mu.Lock()
	for _, ctx := range contexts {
		if s.queued[ctx] {
			continue
		}
		if len(s.pending) >= buffer {
			s.pending = slices.Clone(s.scope)
			clear(s.queued)
			for _, k := range s.scope {
				s.queued[k] = true
			}
			if s.queued[ctx] {
				continue
			}
		}
		s.pending = append(s.pending, ctx)
		s.queued[ctx] = true
	}
	s.mu.Unlock()
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// next hands out one context, so the queue is the stream's only buffer. A
// context announced again while in flight queues again: it changed after its
// send.
func (s *sub) next() (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.pending) == 0 {
		return "", false
	}
	ctx := s.pending[0]
	s.pending = s.pending[1:]
	delete(s.queued, ctx)
	return ctx, true
}
