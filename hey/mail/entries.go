package mail

import (
	pluginv1 "github.com/josephburnett/gridwell/api/gen/plugin/v1"
	"github.com/josephburnett/gridwell/api/rpc"
)

// CollectionEntries derives one collection's grid: every thread it holds as a
// url tile serving its own page, hinted as a calendar — a row per day, newest
// at the top, the day's threads left to right in arrival order. threads must
// be oldest first, which is what Memory.Collection answers.
//
// A thread is a page, so it is a url entry that serves one: the node derives
// the address at its /content/ door, so there is none to declare here, and a
// url entry offers no text body beside the page.
func CollectionEntries(threads []Thread) []*pluginv1.Entry {
	perDay := map[int64]int{}
	out := make([]*pluginv1.Entry, 0, len(threads))
	for i := range threads {
		t := &threads[i]
		day := Day(t.CreatedAt)
		index := perDay[day]
		perDay[day] = index + 1
		x, y := Cell(t.CreatedAt, index)
		out = append(out, &pluginv1.Entry{
			Key:           t.Key(),
			Kind:          rpc.KindURL,
			Label:         t.Label(),
			ServesPage:    true,
			StatusDetail:  t.StatusDetail(),
			PlacementHint: &pluginv1.PlacementHint{X: x, Y: y, W: ThreadTileW, H: 1},
		})
	}
	return out
}

// BoxEntries derives one box's grid: CollectionEntries, each a link to the
// same thread in everything. The content facts stay, so a node that predates
// link_target still shows the box as pages of its own.
func BoxEntries(threads []Thread) []*pluginv1.Entry {
	out := CollectionEntries(threads)
	for _, e := range out {
		e.LinkTarget = &pluginv1.EntryRef{Context: EverythingContext, Key: e.Key}
	}
	return out
}

// MenuEntries declares every collection this plugin serves, one (+) menu
// entry each, everything last. There is no privileged collection and no
// landing grid: a plugin is not a place, it contributes doorways, and each of
// these is one.
func MenuEntries() []*pluginv1.MenuEntry {
	out := make([]*pluginv1.MenuEntry, 0, len(Collections)+1)
	for _, c := range Collections {
		out = append(out, &pluginv1.MenuEntry{
			Id:      c.Key,
			Label:   c.Label,
			Context: c.Key,
		})
	}
	return append(out, &pluginv1.MenuEntry{Id: EverythingContext, Label: EverythingLabel, Context: EverythingContext})
}
