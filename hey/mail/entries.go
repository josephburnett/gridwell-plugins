package mail

import (
	pluginv1 "github.com/josephburnett/gridwell/api/gen/plugin/v1"
	"github.com/josephburnett/gridwell/api/rpc"
)

// CollectionEntries derives one collection's grid: every thread it holds as a
// text tile that serves a page, hinted as a calendar — a row per day, newest
// at the top, the day's threads left to right in arrival order. threads must
// be oldest first, which is what Memory.Collection answers.
//
// Kind is "text" for every entry and serves_page rides beside it. A page tile
// is not a url tile: a url entry owns an address of its own, and the node
// derives a page's address when the page is opened, so there is none to
// declare here.
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
			Key:   t.Key(),
			Kind:  rpc.KindText,
			Label: t.Label(),
			// The body ReadContent answers is markdown, and the page
			// ServeContent answers is the email itself. One presentation for
			// every entry, no per-thread switch.
			TextPresentation: rpc.TextPresentationBoth,
			ServesPage:       true,
			StatusDetail:     t.StatusDetail(),
			PlacementHint:    &pluginv1.PlacementHint{X: x, Y: y, W: ThreadTileW, H: 1},
		})
	}
	return out
}

// MenuEntries is the (+) menu addition for every collection that is not the
// plugin's root context. A root grid is already the plugin's own row on that
// menu, so declaring an entry for it too would offer the same grid twice.
func MenuEntries(rootContext string) []*pluginv1.MenuEntry {
	out := make([]*pluginv1.MenuEntry, 0, len(Collections))
	for _, c := range Collections {
		if c.Key == rootContext {
			continue
		}
		out = append(out, &pluginv1.MenuEntry{
			Id:      c.Key,
			Label:   c.Label,
			Context: c.Key,
		})
	}
	return out
}
