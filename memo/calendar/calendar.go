// Package calendar is the one placement rule for things with a creation
// time: a column per day since one shared Epoch, newest to the right, and a
// row per hour of the day it was created, in the host's local time.
package calendar

import "time"

// Epoch is the shared calendar's day zero and month zero. It is a fixed date,
// so a hint is the same on every restart and two plugins agree on where a day
// lands. It is gitlab's original anchor, so its week calendar keeps its rows.
var Epoch = time.Date(2026, time.August, 24, 0, 0, 0, 0, time.UTC)

// Cell is the hint for a thing created at created, w cells wide: x is its
// local day since Epoch times w, so w-wide tiles of neighbouring days never
// overlap, and y is its local hour, 0 to 23. Two things from the same hour
// share a cell and the node stacks them.
func Cell(created time.Time, w int64) (x, y int64) {
	return CellIn(created, w, time.Local)
}

// CellIn is Cell in loc. Local time because a calendar means the user's day;
// a host's zone moves only tiles the user has not touched, since a minted
// tile keeps its place.
func CellIn(created time.Time, w int64, loc *time.Location) (x, y int64) {
	t := created.In(loc)
	return Day(t) * max(w, 1), int64(t.Hour())
}

// Day is the number of calendar days from Epoch to t's date in t's own zone.
// It counts dates, not elapsed hours, so a DST day is still one day.
func Day(t time.Time) int64 {
	d := time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
	return int64(d.Sub(Epoch).Hours()) / 24
}

// WeekCell is the hint for the week starting on the Monday start, a date in
// UTC: one row per month, newest at the top with Epoch's month at y=0, and the
// month's weeks left to right by the Monday's place in the month, x 0 to 4.
func WeekCell(start time.Time) (x, y int64) {
	u := start.UTC()
	months := (u.Year()-Epoch.Year())*12 + int(u.Month()-Epoch.Month())
	return int64((u.Day() - 1) / 7), -int64(months)
}
