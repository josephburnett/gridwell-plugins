package plugin

import (
	"github.com/josephburnett/gridwell-plugins/gmail/mailbox"
	pluginv1 "github.com/josephburnett/gridwell/api/gen/plugin/v1"
)

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

// Watch streams ContextChanged for each context a refresh changed, until
// the node hangs up, and polls Gmail's history while the stream's scope
// holds a context this plugin lists (memo.Changes, work, poll). Whether a
// message has left is the listing's and Probe's to say, so "the inbox
// changed, list it again" is the whole announcement.
func (p *Plugin) Watch(req *pluginv1.WatchRequest, stream pluginv1.Plugin_WatchServer) error {
	return p.changes.Serve(req.GetContexts(), stream)
}
