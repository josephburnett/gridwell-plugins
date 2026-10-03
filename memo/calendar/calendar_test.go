package calendar

import (
	"testing"
	"time"
	_ "time/tzdata"
)

func TestEpochIsPinned(t *testing.T) {
	// Moving Epoch moves every untouched calendar tile on every node.
	if want := time.Date(2026, time.August, 24, 0, 0, 0, 0, time.UTC); !Epoch.Equal(want) {
		t.Fatalf("Epoch = %v, want %v", Epoch, want)
	}
}

func TestCellIn(t *testing.T) {
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	utc := time.UTC
	at := func(y int, m time.Month, d, h, min int, loc *time.Location) time.Time {
		return time.Date(y, m, d, h, min, 0, 0, loc)
	}
	for _, c := range []struct {
		name string
		t    time.Time
		w    int64
		loc  *time.Location
		x, y int64
	}{
		{"epoch midnight", at(2026, 8, 24, 0, 0, utc), 1, utc, 0, 0},
		{"same hour same day", at(2026, 8, 24, 0, 59, utc), 1, utc, 0, 0},
		{"next hour next row", at(2026, 8, 24, 1, 0, utc), 1, utc, 0, 1},
		{"last hour", at(2026, 8, 24, 23, 59, utc), 1, utc, 0, 23},
		{"next day next column", at(2026, 8, 25, 0, 0, utc), 1, utc, 1, 0},
		{"day before is left", at(2026, 8, 23, 12, 0, utc), 1, utc, -1, 12},
		{"w=2 spaces columns", at(2026, 8, 26, 5, 0, utc), 2, utc, 4, 5},
		{"w=0 is one cell", at(2026, 8, 26, 5, 0, utc), 0, utc, 2, 5},
		{"new year", at(2026, 1, 1, 9, 0, utc), 2, utc, -2 * 235, 9},
		{"local day and hour, not UTC", at(2026, 8, 25, 2, 0, utc), 1, ny, 0, 22},
		// 2026-03-08: New York skips 02:00-03:00.
		{"before spring forward", at(2026, 3, 8, 1, 30, ny), 1, ny, -169, 1},
		{"after spring forward", at(2026, 3, 8, 3, 30, ny), 1, ny, -169, 3},
		{"day after spring forward", at(2026, 3, 9, 0, 0, ny), 1, ny, -168, 0},
		// 2026-11-01: New York repeats 01:00-02:00; both share a cell.
		{"fall back first 01:30", time.Date(2026, 11, 1, 5, 30, 0, 0, utc), 1, ny, 69, 1},
		{"fall back second 01:30", time.Date(2026, 11, 1, 6, 30, 0, 0, utc), 1, ny, 69, 1},
		{"day after fall back", at(2026, 11, 2, 0, 0, ny), 1, ny, 70, 0},
	} {
		x, y := CellIn(c.t, c.w, c.loc)
		if x != c.x || y != c.y {
			t.Errorf("%s: CellIn(%v, %d) = (%d, %d), want (%d, %d)", c.name, c.t, c.w, x, y, c.x, c.y)
		}
	}
}

func TestCellIsLocal(t *testing.T) {
	now := time.Date(2026, 10, 3, 15, 4, 0, 0, time.UTC)
	x, y := Cell(now, 2)
	wx, wy := CellIn(now, 2, time.Local)
	if x != wx || y != wy {
		t.Fatalf("Cell = (%d, %d), CellIn(Local) = (%d, %d)", x, y, wx, wy)
	}
}

func TestWeekCell(t *testing.T) {
	for _, c := range []struct {
		monday time.Time
		x, y   int64
	}{
		{time.Date(2026, 8, 3, 0, 0, 0, 0, time.UTC), 0, 0},
		{time.Date(2026, 8, 24, 0, 0, 0, 0, time.UTC), 3, 0},
		{time.Date(2026, 8, 31, 0, 0, 0, 0, time.UTC), 4, 0},
		{time.Date(2026, 9, 7, 0, 0, 0, 0, time.UTC), 0, -1},
		{time.Date(2026, 7, 27, 0, 0, 0, 0, time.UTC), 3, 1},
		{time.Date(2027, 1, 4, 0, 0, 0, 0, time.UTC), 0, -5},
	} {
		x, y := WeekCell(c.monday)
		if x != c.x || y != c.y {
			t.Errorf("WeekCell(%v) = (%d, %d), want (%d, %d)", c.monday.Format("2006-01-02"), x, y, c.x, c.y)
		}
	}
}
