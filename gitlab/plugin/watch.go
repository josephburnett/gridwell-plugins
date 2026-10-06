package plugin

import (
	pluginv1 "github.com/josephburnett/gridwell/api/gen/plugin/v1"
)

// Watch streams a ContextChanged for every listing a change to memory moved:
// a walk landing, or failing having absorbed pages, a glance, and a
// mark-done. A todo that leaves pending is a ContextChanged for its week: it
// stays listed, done-marked (see the package comment).
func (p *Plugin) Watch(req *pluginv1.WatchRequest, stream pluginv1.Plugin_WatchServer) error {
	return p.changes.Serve(req.GetContexts(), stream)
}

// announce hands every stream the listings memory's latest changes moved,
// and reports whether there were any. Everything that changes memory calls it
// once its change has landed.
func (p *Plugin) announce() bool {
	cs := p.mem.TakeChanges().Contexts()
	p.changes.Publish(cs...)
	return len(cs) > 0
}
