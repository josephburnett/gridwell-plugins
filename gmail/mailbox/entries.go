package mailbox

import (
	pluginv1 "github.com/josephburnett/gridwell/api/gen/plugin/v1"
	"github.com/josephburnett/gridwell/api/rpc"
)

// CollectionEntries derives one collection's grid: every message it holds as
// a text tile that serves a page, hinted as a calendar — a row per day,
// newest at the top, the day's messages left to right in arrival order.
// views must be oldest first, which is what Memory.Collection answers.
//
// Kind is "text" for every entry and serves_page rides beside it. A page tile
// is not a url tile: a url entry owns an address of its own, and the node
// derives a page's address when the page is opened, so there is none to
// declare here.
func CollectionEntries(views []View) []*pluginv1.Entry {
	perDay := map[int64]int{}
	out := make([]*pluginv1.Entry, 0, len(views))
	for _, v := range views {
		day := Day(v.Date)
		index := perDay[day]
		perDay[day] = index + 1
		x, y := Cell(v.Date, index)
		out = append(out, &pluginv1.Entry{
			Key:   v.Key(),
			Kind:  rpc.KindText,
			Label: v.Label(),
			// The body ReadContent answers is markdown, and the page
			// ServeContent answers is the email itself. One presentation for
			// every entry, no per-message switch.
			TextPresentation: rpc.TextPresentationBoth,
			ServesPage:       true,
			StatusDetail:     v.StatusDetail(),
			PlacementHint:    &pluginv1.PlacementHint{X: x, Y: y, W: MessageTileW, H: 1},
		})
	}
	return out
}

// MenuEntries declares every collection this plugin serves, one (+) menu
// entry each. There is no privileged collection and no landing grid: a plugin
// is not a place, it contributes doorways, and each of these is one.
func MenuEntries() []*pluginv1.MenuEntry {
	out := make([]*pluginv1.MenuEntry, 0, len(Collections))
	for _, c := range Collections {
		out = append(out, &pluginv1.MenuEntry{
			Id:      c.Key,
			Label:   c.Label,
			Context: c.Key,
		})
	}
	return out
}
