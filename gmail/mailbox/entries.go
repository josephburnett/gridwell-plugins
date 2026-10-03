package mailbox

import (
	"github.com/josephburnett/gridwell-plugins/memo/calendar"
	pluginv1 "github.com/josephburnett/gridwell/api/gen/plugin/v1"
	"github.com/josephburnett/gridwell/api/rpc"
)

// Entry is one listed tile.
type Entry = pluginv1.Entry

// CollectionEntries derives all mail's grid: every message as a url tile
// serving its own page, named by its subject and hinted at the shared
// calendar's cell for its date (calendar.Cell), which reads nothing but the
// message.
//
// A message is a page, so it is a url entry that serves one: the node derives
// the address at its /content/ door, so there is none to declare here, and a
// url entry offers no text body beside the page.
func CollectionEntries(views []View) []*Entry {
	return entries(AllMailContext, views)
}

// LabelEntries derives one label's grid: its messages as in all mail, each a
// link to the same message there. The content facts stay, so a node that
// predates link_target still shows the label as pages of its own.
func LabelEntries(label string, views []View) []*Entry {
	out := entries(label, views)
	for _, e := range out {
		e.LinkTarget = &pluginv1.EntryRef{Context: AllMailContext, Key: e.Key}
	}
	return out
}

func entries(context string, views []View) []*Entry {
	out := make([]*Entry, 0, len(views))
	for _, v := range views {
		x, y := calendar.Cell(v.Date, MessageTileW)
		out = append(out, &pluginv1.Entry{
			Key:           v.Key(),
			Kind:          rpc.KindURL,
			Label:         v.Title(),
			ServesPage:    true,
			StatusDetail:  v.StatusDetail(context),
			PlacementHint: &pluginv1.PlacementHint{X: x, Y: y, W: MessageTileW, H: 1},
		})
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
