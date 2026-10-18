package plugin

import (
	"github.com/josephburnett/gridwell-plugins/gmail/mailbox"
	pluginv1 "github.com/josephburnett/gridwell/api/gen/plugin/v1"
)

// views is what one grid answers now.
func (p *Plugin) views(key string) []mailbox.View {
	if key == mailbox.AllMailGrid {
		return p.mem.AllMail()
	}
	return p.mem.Grid(key)
}

// listings is what every grid answers now.
func (p *Plugin) listings() map[string][]mailbox.View {
	out := map[string][]mailbox.View{}
	for _, key := range mailbox.GridKeys() {
		out[key] = p.views(key)
	}
	return out
}

// changedSince names the grids whose answer is no longer before's.
func (p *Plugin) changedSince(before map[string][]mailbox.View) []string {
	var out []string
	for _, key := range mailbox.GridKeys() {
		if !mailbox.SameViews(before[key], p.views(key)) {
			out = append(out, key)
		}
	}
	return out
}

// Watch streams GridChanged for each grid a refresh changed, until
// the node hangs up, and polls Gmail's history while the stream's scope
// holds a grid this plugin lists (memo.Changes, work, poll). Whether a
// message has left is the listing's and Probe's to say, so "the inbox
// changed, list it again" is the whole announcement. It sends no
// EntryChanged: Gmail never changes a message's content once it has it, so
// no page changes in place.
func (p *Plugin) Watch(req *pluginv1.WatchRequest, stream pluginv1.Plugin_WatchServer) error {
	return p.changes.Serve(req.GetGrids(), stream)
}
