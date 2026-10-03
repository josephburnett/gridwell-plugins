package mailbox

import (
	pluginv1 "github.com/josephburnett/gridwell/api/gen/plugin/v1"
	"github.com/josephburnett/gridwell/api/rpc"
)

// CollectionEntries derives all mail's grid: every message as a url tile
// serving its own page, hinted as a calendar — a row per day,
// newest at the top, the day's messages left to right in arrival order.
// views must be oldest first, which is what Memory.Collection answers.
//
// A message is a page, so it is a url entry that serves one: the node derives
// the address at its /content/ door, so there is none to declare here, and a
// url entry offers no text body beside the page.
func CollectionEntries(views []View) []*pluginv1.Entry {
	perDay := map[int64]int{}
	out := make([]*pluginv1.Entry, 0, len(views))
	for _, v := range views {
		day := Day(v.Date)
		index := perDay[day]
		perDay[day] = index + 1
		x, y := Cell(v.Date, index)
		out = append(out, &pluginv1.Entry{
			Key:           v.Key(),
			Kind:          rpc.KindURL,
			Label:         v.Label(),
			ServesPage:    true,
			StatusDetail:  v.StatusDetail(),
			PlacementHint: &pluginv1.PlacementHint{X: x, Y: y, W: MessageTileW, H: 1},
		})
	}
	return out
}

// LabelEntries derives one label's grid: CollectionEntries, each a link to
// the same message in all mail. The content facts stay, so a node that
// predates link_target still shows the label as pages of its own.
func LabelEntries(views []View) []*pluginv1.Entry {
	out := CollectionEntries(views)
	for _, e := range out {
		e.LinkTarget = &pluginv1.EntryRef{Context: AllMailContext, Key: e.Key}
	}
	return out
}

// MenuEntries declares every context, one (+) menu entry each, all mail
// last. There is no privileged collection and no landing grid: a plugin is
// not a place, it contributes doorways, and each of these is one.
func MenuEntries() []*pluginv1.MenuEntry {
	out := make([]*pluginv1.MenuEntry, 0, len(Collections)+1)
	for _, c := range Collections {
		out = append(out, &pluginv1.MenuEntry{
			Id:      c.Key,
			Label:   c.Label,
			Context: c.Key,
		})
	}
	return append(out, &pluginv1.MenuEntry{Id: AllMailContext, Label: AllMailLabel, Context: AllMailContext})
}
