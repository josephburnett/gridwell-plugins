package plugin

import (
	"context"
	"slices"
	"strconv"
	"time"

	"github.com/josephburnett/gridwell-plugins/memo"
	pluginv1 "github.com/josephburnett/gridwell/api/gen/plugin/v1"
)

// PollEvery is how often a shown pid's children are read. /proc cannot tell:
// inotify sees nothing there and the netlink process connector needs
// CAP_NET_ADMIN, so the plugin polls, and only while a stream shows the pid.
const PollEvery = 2 * time.Second

// Watch announces a shown pid when its listing changes; see memo.Changes.
func (p *Plugin) Watch(req *pluginv1.WatchRequest, stream pluginv1.Plugin_WatchServer) error {
	return p.changes.Serve(req.GetContexts(), stream)
}

// follow is one shown context's work. It reads the context's child set at
// once and every PollEvery after, and announces the context when a read
// differs from the last one that succeeded: a read that fails is not a
// change.
func (p *Plugin) follow(ctx context.Context, key string) {
	pid, err := contextPID(key)
	if err != nil {
		return
	}
	last, known := p.childSet(ctx, pid)
	memo.Poll(ctx, p.clock, PollEvery, func(ctx context.Context) {
		now, ok := p.childSet(ctx, pid)
		if !ok {
			return
		}
		if known && !slices.Equal(now, last) {
			p.changes.Publish(key)
		}
		last, known = now, true
	})
}

// childSet is the part of pid's listing that can change between reads: each
// child's key and status mark. @info's body is not in it; its tile is the
// same whatever the process's memory or state.
func (p *Plugin) childSet(ctx context.Context, pid int64) ([]string, bool) {
	kids, err := p.scan(ctx, pid)
	if err != nil {
		return nil, false
	}
	set := make([]string, len(kids))
	for i, k := range kids {
		set[i] = strconv.FormatInt(k.PID, 10) + stateMark(k.State)
	}
	return set, true
}
