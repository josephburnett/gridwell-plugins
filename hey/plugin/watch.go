package plugin

import (
	"context"
	"sync"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/josephburnett/gridwell-plugins/hey/mail"
	pluginv1 "github.com/josephburnett/gridwell/api/gen/plugin/v1"
)

// DefaultWatchBackoff is the first wait before a feed that ended is started
// again. It doubles on every failure in a row, to MaxWatchBackoff, and starts
// over once a feed reaches ready.
const DefaultWatchBackoff = time.Second

// MaxWatchBackoff caps the wait between feed restarts, so a CLI the user has
// just signed in to is live again within minutes, not never.
const MaxWatchBackoff = 5 * time.Minute

// DefaultRecoverAfter is how long a disconnected feed may reconnect on its
// own. The CLI reconnects itself; one that has not said ready by then is
// restarted.
const DefaultRecoverAfter = 2 * time.Minute

// SubscriberBuffer is how many changes one Watch subscriber may fall behind.
// Past it the feed does not wait: the subscriber's changes are dropped and it
// is told every collection changed, which is always true enough to re-list.
const SubscriberBuffer = 64

// watch keeps one `hey watch` running until ctx ends, restarting it with
// backoff when it stops. A feed the CLI refuses as usage (1 or 8: a CLI with
// no watch) is not restarted: the refresher's walks keep memory instead, and
// the log says so once. Any other verdict is also every read's answer
// (watchErr) until a feed reaches ready.
func (p *Plugin) watch(ctx context.Context) {
	backoff := p.watchBackoff
	for {
		live, err := p.watchOnce(ctx)
		p.setLive(false)
		if ctx.Err() != nil {
			return
		}
		switch status.Code(err) {
		case codes.InvalidArgument:
			p.logf("hey plugin: this CLI cannot watch (%v); collections are re-walked every %s instead", err, p.refresh)
			return
		case codes.Unavailable:
		default:
			p.mu.Lock()
			p.watchErr = err
			p.mu.Unlock()
		}
		if live {
			backoff = p.watchBackoff
		}
		p.logf("hey plugin: watch ended: %v; restarting in %s", err, backoff)
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(2*backoff, MaxWatchBackoff)
	}
}

// watchOnce runs the feed once and reports whether it ever reached ready. A
// disconnect that has not recovered within recoverAfter ends the run, so the
// loop above starts a fresh one.
func (p *Plugin) watchOnce(ctx context.Context) (live bool, err error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var stalled *time.Timer
	defer func() {
		if stalled != nil {
			stalled.Stop()
		}
	}()
	err = p.src.Watch(ctx, func(ev mail.Event, perr error) {
		if perr != nil {
			p.logf("hey plugin: watch: %v (line skipped)", perr)
			return
		}
		switch ev.Change {
		case mail.ChangeReady:
			live = true
			if stalled != nil {
				stalled.Stop()
				stalled = nil
			}
		case mail.ChangeDisconnected:
			if stalled == nil {
				stalled = time.AfterFunc(p.recoverAfter, func() {
					p.logf("hey plugin: watch disconnected for %s; restarting it", p.recoverAfter)
					cancel()
				})
			}
		}
		p.apply(ev)
	})
	return live, err
}

// apply is one line of the feed, landed. A thread line moves memory and, if
// the listing changed, tells every subscriber; resync and a delete memory
// cannot map re-read the box; ready re-reads every box, because the feed
// reports nothing from before it was live and the catch-up may have skipped.
// A box this plugin does not project is ignored.
func (p *Plugin) apply(ev mail.Event) {
	switch ev.Change {
	case mail.ChangeReady:
		p.mu.Lock()
		p.live = true
		p.liveGen++
		p.watchErr = nil
		p.mu.Unlock()
		for _, c := range mail.Collections {
			p.rewalk(c)
		}
	case mail.ChangeDisconnected:
		p.setLive(false)
	case mail.ChangeResync:
		if c, ok := mail.LookupBox(ev.Box); ok {
			p.rewalk(c)
		}
	case mail.ChangeAdded, mail.ChangeUpdated, mail.ChangeDeleted:
		c, ok := mail.LookupBox(ev.Box)
		if !ok {
			return
		}
		eff := p.mem.Apply(c.Key, ev)
		if eff.Changed {
			p.saveCache()
			p.changes.publish(c.Key)
		}
		if eff.Rewalk {
			p.rewalk(c)
		}
	}
}

func (p *Plugin) setLive(live bool) {
	p.mu.Lock()
	p.live = live
	p.mu.Unlock()
}

// Watch streams a ContextChanged for every collection whose listing changes
// from now on, by the feed or by a walk. It never sends EntryRemoved: this
// plugin's listings are not authoritative, and a thread leaving one box is
// usually in another, so absence is Probe's to settle, not the feed's.
func (p *Plugin) Watch(_ *pluginv1.WatchRequest, stream pluginv1.Plugin_WatchServer) error {
	s := p.changes.subscribe()
	defer p.changes.unsubscribe(s)
	// The node counts the stream open when its header arrives, and only then
	// clears a refusal or catches up after a drop (docs/plugin-authoring.md).
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
			for _, c := range mail.Collections {
				if err := send(c.Key); err != nil {
					return err
				}
			}
		}
	}
}

// fanout hands each change to every subscriber without ever waiting on one.
type fanout struct {
	mu   sync.Mutex
	subs map[*subscriber]struct{}
}

// subscriber is one Watch stream's queue. lost is raised when a change could
// not be queued.
type subscriber struct {
	ch   chan string
	lost chan struct{}
}

func (f *fanout) subscribe() *subscriber {
	s := &subscriber{ch: make(chan string, SubscriberBuffer), lost: make(chan struct{}, 1)}
	f.mu.Lock()
	f.subs[s] = struct{}{}
	f.mu.Unlock()
	return s
}

func (f *fanout) unsubscribe(s *subscriber) {
	f.mu.Lock()
	delete(f.subs, s)
	f.mu.Unlock()
}

func (f *fanout) publish(key string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for s := range f.subs {
		select {
		case s.ch <- key:
		default:
			select {
			case s.lost <- struct{}{}:
			default:
			}
		}
	}
}
