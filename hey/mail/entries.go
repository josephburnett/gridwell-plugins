package mail

import (
	"github.com/josephburnett/gridwell-plugins/memo/calendar"
	pluginv1 "github.com/josephburnett/gridwell/api/gen/plugin/v1"
	"github.com/josephburnett/gridwell/api/rpc"
)

// GridEntries derives everything's grid: every thread it holds as a
// url tile serving its own page, named by its Stamp, at calendar.Cell of its
// creation time, so a thread's hint is the same whatever else arrives. Seen
// is a box's fact, so these tiles carry no status.
//
// A thread is a page, so it is a url entry that serves one: the node derives
// the address at its /content/ API, so there is none to declare here, and a
// url entry offers no text body beside the page.
func GridEntries(threads []Thread) []*pluginv1.Entry {
	out := make([]*pluginv1.Entry, 0, len(threads))
	for i := range threads {
		t := &threads[i]
		x, y := calendar.Cell(t.CreatedAt, ThreadTileW)
		out = append(out, &pluginv1.Entry{
			Key:           t.Key(),
			Kind:          rpc.KindURL,
			Label:         t.Label(),
			ServesPage:    true,
			PlacementHint: &pluginv1.PlacementHint{X: x, Y: y, W: ThreadTileW, H: 1},
			ContentStamp:  t.Stamp(),
		})
	}
	return out
}

// BoxEntries derives one box's grid: GridEntries, each a link to the
// same thread in everything carrying its seen state in this box. The content
// facts stay, so a node that predates link_target still shows the box as
// pages of its own.
func BoxEntries(threads []Thread) []*pluginv1.Entry {
	out := GridEntries(threads)
	for i, e := range out {
		e.LinkTarget = &pluginv1.EntryRef{Grid: EverythingGrid, Key: e.Key}
		e.StatusDetail = threads[i].StatusDetail()
	}
	return out
}

// Grids declares every door grid, one (+) declared grid each,
// everything last. There is no privileged grid and no landing grid: a
// plugin is not a place, it contributes doors. A box that is not a door
// still lists, so a link into it keeps resolving.
func Declared() []*pluginv1.PluginGrid {
	out := make([]*pluginv1.PluginGrid, 0, len(Grids)+1)
	for _, c := range Grids {
		if !c.Door {
			continue
		}
		out = append(out, &pluginv1.PluginGrid{
			Id:    c.Key,
			Label: c.Label,
			Grid:  c.Key,
		})
	}
	return append(out, &pluginv1.PluginGrid{Id: EverythingGrid, Label: EverythingLabel, Grid: EverythingGrid})
}
