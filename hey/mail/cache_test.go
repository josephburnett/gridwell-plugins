package mail

import (
	"encoding/json"
	"reflect"
	"testing"
)

func thread(id int64, subject, day string) Thread {
	return Thread{TopicID: id, PostingID: id * 10, Subject: subject, CreatedAt: at(day + "T09:00:00Z")}
}

func keys(ts []Thread) []int64 {
	out := make([]int64, len(ts))
	for i, t := range ts {
		out[i] = t.TopicID
	}
	return out
}

func TestWholeWalkReplacesMembership(t *testing.T) {
	m := NewCache()
	m.Absorb(ImboxGrid, []Thread{thread(1, "a", "2026-01-02"), thread(2, "b", "2026-01-03")}, true)
	if got, want := keys(m.Grid(ImboxGrid)), []int64{1, 2}; !reflect.DeepEqual(got, want) {
		t.Fatalf("imbox = %v, want %v", got, want)
	}
	// Thread 1 archived at HEY: a whole walk no longer sees it, so it leaves
	// the grid. The record survives, so its tile can still read as the email.
	m.Absorb(ImboxGrid, []Thread{thread(2, "b", "2026-01-03")}, true)
	if got, want := keys(m.Grid(ImboxGrid)), []int64{2}; !reflect.DeepEqual(got, want) {
		t.Fatalf("after archive imbox = %v, want %v", got, want)
	}
	if _, ok := m.Get(1); !ok {
		t.Error("the archived thread's record was forgotten")
	}
	if m.Member(1) {
		t.Error("the archived thread is still a member")
	}
}

// A thread whose stamp moved, a reply landed on it, is the effect's to name,
// by the feed or by a walk; a line or a walk that moves only its seen state,
// or a stamp that was never known, names nothing.
func TestARepliedThreadIsMovedInPlace(t *testing.T) {
	m := NewCache()
	one := thread(1, "a", "2026-01-02")
	m.Absorb(ImboxGrid, []Thread{one}, true)
	one.ActiveAt = at("2026-01-02T10:00:00Z")
	if eff := m.Apply(ImboxGrid, Event{Change: ChangeUpdated, Thread: one}); len(eff.Moved) != 0 {
		t.Errorf("a stamp first known moved %v", eff.Moved)
	}
	one.Seen = true
	if eff := m.Apply(ImboxGrid, Event{Change: ChangeUpdated, Thread: one}); len(eff.Moved) != 0 || !eff.Changed {
		t.Errorf("a seen line: %+v, want the listing changed and nothing moved", eff)
	}
	one.ActiveAt = at("2026-01-03T10:00:00Z")
	if eff := m.Apply(ImboxGrid, Event{Change: ChangeUpdated, Thread: one}); !reflect.DeepEqual(eff.Moved, []int64{1}) {
		t.Errorf("a reply moved %v, want [1]", eff.Moved)
	}
	one.ActiveAt = at("2026-01-04T10:00:00Z")
	if eff := m.Absorb(ImboxGrid, []Thread{one}, true); !reflect.DeepEqual(eff.Moved, []int64{1}) {
		t.Errorf("a walk that read a reply moved %v, want [1]", eff.Moved)
	}
	if eff := m.Absorb(ImboxGrid, []Thread{one}, true); len(eff.Moved) != 0 {
		t.Errorf("an unchanged walk moved %v", eff.Moved)
	}
}

// A partial read proves nothing about what it did not reach: merging is the
// only safe fold, or a capped listing would empty the user's grid.
func TestPartialWalkOnlyAdds(t *testing.T) {
	m := NewCache()
	m.Absorb(ImboxGrid, []Thread{thread(1, "a", "2026-01-02"), thread(2, "b", "2026-01-03")}, true)
	m.Absorb(ImboxGrid, []Thread{thread(3, "c", "2026-01-04")}, false)
	if got, want := keys(m.Grid(ImboxGrid)), []int64{1, 2, 3}; !reflect.DeepEqual(got, want) {
		t.Fatalf("imbox = %v, want %v", got, want)
	}
}

// Nothing is ever GONE until every grid has been read to its end: a
// thread missing from a half-swept cache may simply be in the half not read.
func TestSweptNeedsEveryGrid(t *testing.T) {
	m := NewCache()
	if m.Swept() {
		t.Fatal("a cold cache reports itself swept")
	}
	for _, c := range Grids[:len(Grids)-1] {
		m.Absorb(c.Key, nil, true)
	}
	if m.Swept() {
		t.Fatal("all but one grid reported a full sweep")
	}
	m.Absorb(Grids[len(Grids)-1].Key, nil, true)
	if !m.Swept() {
		t.Fatal("every grid walked whole did not make a sweep")
	}
	// A partial walk of one grid does not un-sweep the cache: the box
	// HAS been read to its end once, and that is what the flag records.
	m.Absorb(SetAsideGrid, nil, false)
	if !m.Swept() {
		t.Fatal("a later partial walk forgot the completed one")
	}
}

// A thread that moves between grids keeps its key and its record, and
// belongs to exactly the grid that last listed it.
func TestAThreadMovesBetweenGrids(t *testing.T) {
	m := NewCache()
	m.Absorb(ImboxGrid, []Thread{thread(1, "a", "2026-01-02")}, true)
	m.Absorb(ReplyLaterGrid, []Thread{thread(1, "a", "2026-01-02")}, true)
	m.Absorb(ImboxGrid, nil, true)
	if got := m.Grid(ImboxGrid); len(got) != 0 {
		t.Fatalf("imbox still holds %v", keys(got))
	}
	if got, want := keys(m.Grid(ReplyLaterGrid)), []int64{1}; !reflect.DeepEqual(got, want) {
		t.Fatalf("reply later = %v, want %v", got, want)
	}
	th, ok := m.Get(1)
	if !ok || th.Grid != ReplyLaterGrid {
		t.Fatalf("thread 1 records grid %q", th.Grid)
	}
	if !m.Member(1) {
		t.Error("a thread in reply later is not a member")
	}
}

func TestGridIsOldestFirstAndTotal(t *testing.T) {
	m := NewCache()
	// Same instant, listed newest first, as HEY answers.
	a := Thread{TopicID: 9, CreatedAt: at("2026-01-05T09:00:00Z")}
	b := Thread{TopicID: 4, CreatedAt: at("2026-01-05T09:00:00Z")}
	c := Thread{TopicID: 2, CreatedAt: at("2026-01-01T09:00:00Z")}
	m.Absorb(ImboxGrid, []Thread{a, b, c}, true)
	if got, want := keys(m.Grid(ImboxGrid)), []int64{2, 4, 9}; !reflect.DeepEqual(got, want) {
		t.Fatalf("order = %v, want %v", got, want)
	}
}

// A thread that leaves every box is a stray until it is asked about or
// listed again, across a restart too; one still in another box is none.
func TestAThreadInNoBoxIsAStray(t *testing.T) {
	m := NewCache()
	m.Absorb(ImboxGrid, []Thread{thread(1, "a", "2026-01-02"), thread(2, "b", "2026-01-03")}, true)
	m.Absorb(ReplyLaterGrid, []Thread{thread(2, "b", "2026-01-03")}, true)
	m.Absorb(ImboxGrid, nil, true)
	back := NewCache()
	back.Restore(m.Snapshot())
	if got := back.TakeStrays(10); !reflect.DeepEqual(got, []int64{1}) {
		t.Fatalf("strays = %v, want the thread in no box", got)
	}
	if got := back.TakeStrays(10); len(got) != 0 {
		t.Fatalf("a stray handed out twice: %v", got)
	}
	m.Absorb(ImboxGrid, []Thread{thread(1, "a", "2026-01-02")}, true)
	if got := m.TakeStrays(10); len(got) != 0 {
		t.Fatalf("a thread listed again is still a stray: %v", got)
	}
	m.Forget(1)
	if _, ok := m.Get(1); !ok {
		t.Error("a thread a box holds was forgotten")
	}
}

// Strays are asked about in turn: one handed back waits behind every other,
// and the queue keeps its order across a restart.
func TestStraysAreAskedAboutInTurn(t *testing.T) {
	m := NewCache()
	m.Absorb(ImboxGrid, []Thread{thread(3, "c", "2026-01-01"), thread(1, "a", "2026-01-02"), thread(2, "b", "2026-01-03")}, true)
	m.Absorb(ImboxGrid, nil, true)
	var asked []int64
	for range 4 {
		got := m.TakeStrays(2)
		asked = append(asked, got...)
		for _, id := range got {
			m.Stray(id)
		}
		back := NewCache()
		back.Restore(m.Snapshot())
		m = back
	}
	if want := []int64{3, 1, 2, 3, 1, 2, 3, 1}; !reflect.DeepEqual(asked[:len(want)], want) {
		t.Errorf("asked %v, want each stray in turn %v", asked, want)
	}
}

// A cache file that kept a thread in no box off the strays (an archived one
// asked about once) queues it again on restore, behind the strays it names.
func TestRestoreQueuesARecordInNoBox(t *testing.T) {
	m := NewCache()
	m.Absorb(ImboxGrid, []Thread{thread(1, "a", "2026-01-02"), thread(2, "b", "2026-01-03")}, true)
	m.Absorb(ImboxGrid, nil, true)
	snap := m.Snapshot()
	snap.Strays = []int64{2}
	back := NewCache()
	back.Restore(snap)
	if got := back.TakeStrays(10); !reflect.DeepEqual(got, []int64{2, 1}) {
		t.Errorf("strays = %v, want the named one then the unnamed one", got)
	}
}

func TestSnapshotRoundTrips(t *testing.T) {
	m := NewCache()
	m.Absorb(ImboxGrid, []Thread{thread(1, "a", "2026-01-02")}, true)
	m.Absorb(ReplyLaterGrid, []Thread{thread(2, "b", "2026-01-03")}, false)

	raw, err := json.Marshal(m.Snapshot())
	if err != nil {
		t.Fatal(err)
	}
	var snap Snapshot
	if err := json.Unmarshal(raw, &snap); err != nil {
		t.Fatal(err)
	}
	back := NewCache()
	back.Restore(snap)

	if got, want := keys(back.Grid(ImboxGrid)), []int64{1}; !reflect.DeepEqual(got, want) {
		t.Errorf("imbox = %v, want %v", got, want)
	}
	if got, want := keys(back.Grid(ReplyLaterGrid)), []int64{2}; !reflect.DeepEqual(got, want) {
		t.Errorf("reply later = %v, want %v", got, want)
	}
	// Completeness rides the snapshot: a restart must not be able to say GONE
	// on the strength of a walk that never finished.
	if back.Swept() {
		t.Error("a restored half sweep reports itself swept")
	}
	for _, c := range Grids[1:] {
		back.Absorb(c.Key, nil, true)
	}
	if !back.Swept() {
		t.Error("the restored imbox forgot its completed walk")
	}
}
