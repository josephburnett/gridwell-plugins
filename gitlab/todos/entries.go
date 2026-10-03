package todos

import (
	"time"

	"github.com/josephburnett/gridwell-plugins/memo/calendar"
	pluginv1 "github.com/josephburnett/gridwell/api/gen/plugin/v1"
)

// RootEntries derives the collection grid: one well per week, newest first,
// hinted by calendar.WeekCell and named by its Monday.
func RootEntries(weeks []WeekSummary) []*pluginv1.Entry {
	out := make([]*pluginv1.Entry, 0, len(weeks))
	for _, w := range weeks {
		key := WeekKey(w.Start)
		x, y := calendar.WeekCell(w.Start)
		out = append(out, &pluginv1.Entry{
			Key:           key,
			Kind:          "well",
			Label:         WeekLabel(w.Start),
			ChildContext:  key,
			PlacementHint: &pluginv1.PlacementHint{X: x, Y: y, W: 1, H: 1},
		})
	}
	return out
}

// TodoTileW is a todo tile's hinted width: two cells, so the label reads;
// calendar.Cell spaces the day columns to match.
const TodoTileW = 2

// WeekEntries derives one week's grid: every todo created that week as a
// markdown text tile, which is its face and its rendered document, hinted by
// calendar.Cell from its creation time alone. The hint seeds first placement
// only; the user's arrangement wins from then on.
func WeekEntries(start time.Time, todos []Todo) []*pluginv1.Entry {
	out := make([]*pluginv1.Entry, 0, len(todos))
	for i := range todos {
		t := &todos[i]
		if !WeekStart(t.CreatedAt).Equal(start) {
			continue
		}
		x, y := calendar.Cell(t.CreatedAt, TodoTileW)
		out = append(out, &pluginv1.Entry{
			Key:              t.Key(),
			Kind:             "text",
			TextPresentation: TextPresentation,
			Label:            t.Label(),
			StatusDetail:     t.StatusDetail(),
			PlacementHint:    &pluginv1.PlacementHint{X: x, Y: y, W: TodoTileW, H: 1},
		})
	}
	return out
}
