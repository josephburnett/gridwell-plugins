package plugin

import (
	"context"
	"errors"

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

// watch is the feed unit's Do: it keeps one `hey watch` running until ctx
// ends — the last Watch stream left memo's Linger ago — restarting it with
// backoff when it stops. A feed the CLI refuses as usage (1 or 8: a CLI with
// no watch) is not restarted: reads walk on the refresh window instead, and
// the log says so once. Any other verdict is every read's unreachable reason
// (watchErr) until a feed reaches ready.
func (p *Plugin) watch(ctx context.Context) {
	backoff := p.watchBackoff
	// failing is the log's one episode: the first end is said, every retry
	// until the feed is live again is not.
	failing := false
	for {
		live, err := p.watchOnce(ctx)
		p.setLive(false)
		if ctx.Err() != nil {
			return
		}
		if live && failing {
			failing = false
			p.logf("hey plugin: watch is live again")
		}
		switch status.Code(err) {
		case codes.InvalidArgument:
			p.logf("hey plugin: this CLI cannot watch (%v); a read re-walks a collection older than %s instead", err, p.refresh)
			return
		case codes.Unavailable:
		default:
			p.verdict(err)
		}
		if live {
			backoff = p.watchBackoff
		}
		if !failing && !errors.Is(err, context.Canceled) {
			failing = true
			p.logf("hey plugin: watch ended: %v; restarting in %s", err, backoff)
		}
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
	skipped := false
	err = p.src.Watch(ctx, func(ev mail.Event, perr error) {
		if perr != nil {
			if !skipped {
				skipped = true
				p.logf("hey plugin: watch: %v (line skipped; later ones are not logged)", perr)
			}
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
		p.mu.Unlock()
		p.verdict(nil)
		for _, c := range mail.Collections {
			p.flights.Rewalk(c.Key)
		}
	case mail.ChangeDisconnected:
		p.setLive(false)
	case mail.ChangeResync:
		if c, ok := mail.LookupBox(ev.Box); ok {
			p.ask(c.Key)
		}
	case mail.ChangeAdded, mail.ChangeUpdated, mail.ChangeDeleted:
		c, ok := mail.LookupBox(ev.Box)
		if !ok {
			return
		}
		eff := p.mem.Apply(c.Key, ev)
		if eff.Changed || eff.Everything {
			p.save()
		}
		p.publish(c.Key, eff)
		if eff.Rewalk {
			p.ask(c.Key)
		}
	}
}

// verdict records why the feed last ended, nil once one is live. Every read
// answers it as its unreachable reason, so its edges repaint every context.
func (p *Plugin) verdict(err error) {
	p.mu.Lock()
	edge := (p.watchErr == nil) != (err == nil)
	p.watchErr = err
	p.mu.Unlock()
	if edge {
		p.changes.Publish(contexts()...)
	}
}

func (p *Plugin) setLive(live bool) {
	p.mu.Lock()
	p.live = live
	p.mu.Unlock()
}

// Watch streams a ContextChanged for every collection whose listing changes
// from now on, by the feed or by a walk, everything included: a thread's
// record changing moves everything, which the node passes on to every box
// that links into it. Absence is Probe's to settle, not the feed's. The feed
// is account-wide, so every context in any
// scope needs the one feed unit, and a stream that names none watches every
// collection.
func (p *Plugin) Watch(req *pluginv1.WatchRequest, stream pluginv1.Plugin_WatchServer) error {
	return p.changes.Serve(req.GetContexts(), stream)
}
