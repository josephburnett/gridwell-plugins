package plugin

import (
	"github.com/josephburnett/gridwell-plugins/gitlab/todos"
	pluginv1 "github.com/josephburnett/gridwell/api/gen/plugin/v1"
)

// Watch streams a ContextChanged for every listing a change to memory moved,
// and an EntryChanged for every todo whose body did: a walk landing, or
// failing having absorbed pages, a glance, and a mark-done. A todo that
// leaves pending is a ContextChanged for its week, where it stays listed
// done-marked (see the package comment), and its EntryChanged, since its
// body says done.
func (p *Plugin) Watch(req *pluginv1.WatchRequest, stream pluginv1.Plugin_WatchServer) error {
	return p.changes.Serve(req.GetContexts(), stream)
}

// announce hands every stream what memory's latest changes moved, and
// reports whether there was anything. Everything that changes memory calls
// it once its change has landed.
func (p *Plugin) announce() bool {
	c := p.mem.TakeChanges()
	cs := c.Contexts()
	p.changes.Publish(cs...)
	for i := range c.Todos {
		week := todos.WeekStart(c.Todos[i].CreatedAt)
		for _, e := range todos.WeekEntries(week, c.Todos[i:i+1]) {
			p.changes.PublishEntry(todos.WeekKey(week), e)
		}
	}
	return len(cs) > 0 || len(c.Todos) > 0
}
