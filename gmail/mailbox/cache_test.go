package mailbox

import (
	"encoding/json"
	"testing"
)

func ids(vs []View) []string {
	out := make([]string, len(vs))
	for i, v := range vs {
		out[i] = v.ID
	}
	return out
}

func eq(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// A whole read is the label's live membership: a message archived at Gmail
// leaves the grid, and the record of it stays so a tile the node still holds
// still reads as the email it was.
func TestAWholeReadReplacesTheMembership(t *testing.T) {
	m := NewCache()
	m.Absorb(InboxGrid, []string{"b", "a"}, map[string]bool{"b": true},
		[]Message{msg("a", "one", "2026-01-03T09:00:00Z"), msg("b", "two", "2026-01-04T09:00:00Z")}, true)
	if got := ids(m.Grid(InboxGrid)); !eq(got, []string{"a", "b"}) { // oldest first
		t.Fatalf("grid = %v", got)
	}
	m.Absorb(InboxGrid, []string{"b"}, nil, nil, true)
	if got := ids(m.Grid(InboxGrid)); !eq(got, []string{"b"}) {
		t.Fatalf("after an archive = %v", got)
	}
	if _, ok := m.View("a"); !ok {
		t.Error("the record of an archived message was dropped")
	}
	if m.Member("a") {
		t.Error("an archived message is still a member")
	}
}

// The watermark is what makes a capped read usable: Gmail lists newest first,
// so a read that stopped short still says exactly which of the messages it
// reached the label holds, and nothing about the older ones. Without it a
// capped read either loses every old tile or never lets one go.
func TestACappedReadIsAuthoritativeAboveItsWatermark(t *testing.T) {
	m := NewCache()
	all := []Message{
		msg("old", "old", "2026-01-01T09:00:00Z"),
		msg("mid", "mid", "2026-01-03T09:00:00Z"),
		msg("new", "new", "2026-01-05T09:00:00Z"),
	}
	m.Absorb(InboxGrid, []string{"new", "mid", "old"}, nil, all, true)

	// "new" is archived and "newer" arrives; the next read is capped two
	// messages down, at "mid". "new" was above the watermark and did not come
	// back, so it has left. "old" is below it, where the read said nothing, so
	// it stays.
	m.Absorb(InboxGrid, []string{"newer", "mid"}, nil,
		[]Message{msg("newer", "newer", "2026-01-06T09:00:00Z")}, false)
	got := ids(m.Grid(InboxGrid))
	if !eq(got, []string{"old", "mid", "newer"}) {
		t.Fatalf("grid = %v; want old (below the watermark), mid and newer", got)
	}
	// A capped read that HAS a watermark is still a usable membership, so it
	// counts towards the sweep that lets Probe ever answer GONE.
	m.Absorb(StarredGrid, []string{"newer"}, nil, nil, false)
	if !m.Swept() {
		t.Error("a capped read with a watermark did not count as a usable membership")
	}
}

// A capped read with nothing datable in it is no evidence at all: it must
// merge rather than retire, and it must not claim the membership is usable.
func TestACappedReadWithNoDatesIsNoEvidence(t *testing.T) {
	m := NewCache()
	m.Absorb(InboxGrid, []string{"a"}, nil, []Message{msg("a", "one", "2026-01-03T09:00:00Z")}, true)
	// "z" has no record, so the read has no watermark to stand on.
	m.Absorb(StarredGrid, []string{"z"}, nil, nil, false)
	if m.Swept() {
		t.Error("a walk that proved nothing declared the grid usable")
	}
	m.Absorb(StarredGrid, []string{"a"}, nil, nil, true)
	if !m.Swept() {
		t.Error("every grid read and still not swept")
	}
}

// Read/unread and starred have one owner. A message in both grids reads the
// same in each, whichever walk ran last.
func TestStateComesFromTheCacheNotTheRecord(t *testing.T) {
	m := NewCache()
	rec := []Message{msg("a", "one", "2026-01-03T09:00:00Z")}
	m.Absorb(InboxGrid, []string{"a"}, map[string]bool{"a": true}, rec, true)
	m.Absorb(StarredGrid, []string{"a"}, map[string]bool{"a": true}, nil, true)

	inbox, starred := m.Grid(InboxGrid), m.Grid(StarredGrid)
	if len(inbox) != 1 || len(starred) != 1 {
		t.Fatalf("grids = %v %v", ids(inbox), ids(starred))
	}
	if inbox[0] != starred[0] {
		t.Errorf("one message read two ways: %+v vs %+v", inbox[0], starred[0])
	}
	if !inbox[0].Unread || !inbox[0].Starred {
		t.Errorf("state = %+v", inbox[0])
	}
	// Read at Gmail: the next walk of either grid clears the mark.
	m.Absorb(InboxGrid, []string{"a"}, nil, nil, true)
	if v, _ := m.View("a"); v.Unread {
		t.Error("a message read at Gmail kept its unread mark")
	}
	// Unstarred: it leaves the starred membership, and the mark with it.
	m.Absorb(StarredGrid, nil, nil, nil, true)
	if v, _ := m.View("a"); v.Starred {
		t.Error("an unstarred message kept its star")
	}
}

// Only what the cache has a record for becomes a tile. A member whose
// metadata read failed keeps its place in the membership — so nothing calls
// it gone — and gets its tile on the next walk.
func TestAMemberWithNoRecordIsNotATileAndIsNotGone(t *testing.T) {
	m := NewCache()
	m.Absorb(InboxGrid, []string{"a", "b"}, nil, []Message{msg("a", "one", "2026-01-03T09:00:00Z")}, true)
	m.Absorb(StarredGrid, nil, nil, nil, true)
	if got := ids(m.Grid(InboxGrid)); !eq(got, []string{"a"}) {
		t.Fatalf("grid = %v", got)
	}
	if !m.Member("b") {
		t.Error("a member with no record read as gone")
	}
	if got := m.Missing([]string{"a", "b", "c"}); !eq(got, []string{"b", "c"}) {
		t.Errorf("missing = %v", got)
	}
}

// The snapshot is the plugin's cache of Gmail across a restart, as the cache
// file's JSON, and it must carry every fact the cache owns: the records, the
// unread set, each membership, its usability and its wholeness.
func TestSnapshotRoundTripsEveryFact(t *testing.T) {
	m := NewCache()
	m.Absorb(InboxGrid, []string{"b", "a"}, map[string]bool{"b": true},
		[]Message{msg("a", "one", "2026-01-03T09:00:00Z"), msg("b", "two", "2026-01-04T09:00:00Z")}, true)
	m.Absorb(StarredGrid, []string{"b"}, map[string]bool{"b": true}, nil, true)
	m.SetHistoryID(4242)

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
	if got := ids(back.Grid(InboxGrid)); !eq(got, []string{"a", "b"}) {
		t.Fatalf("restored inbox = %v", got)
	}
	v, ok := back.View("b")
	if !ok || !v.Unread || !v.Starred || v.Subject != "two" {
		t.Fatalf("restored view = %+v %v", v, ok)
	}
	if !back.Swept() {
		t.Error("a restored sweep did not count")
	}
	if !back.Definitive(InboxGrid) {
		t.Error("a restored whole read did not count")
	}
	if got := back.HistoryID(); got != 4242 {
		t.Errorf("restored history id = %d; a restart would re-walk every grid", got)
	}
	// Two snapshots of the same cache are byte-identical, so a save that
	// changes nothing does not churn the file.
	a, b := m.Snapshot(), m.Snapshot()
	if len(a.Messages) != len(b.Messages) || a.Messages[0].ID != b.Messages[0].ID {
		t.Error("the snapshot order is not stable")
	}
}

// A catch-up places each message by the labels it carries now, whatever it
// was before, and a deletion leaves every grid. Applying it twice is
// applying it once.
func TestApplyPlacesByCurrentLabels(t *testing.T) {
	m := NewCache()
	m.Absorb(InboxGrid, []string{"b", "a"}, map[string]bool{"a": true},
		[]Message{msg("a", "one", "2026-01-03T09:00:00Z"), msg("b", "two", "2026-01-04T09:00:00Z")}, true)
	m.Absorb(StarredGrid, nil, nil, nil, true)

	delta := []Labelled{
		{Message: msg("a", "one", "2026-01-03T09:00:00Z"), Labels: []string{"STARRED"}},         // archived, starred, read
		{Message: msg("c", "new", "2026-01-05T09:00:00Z"), Labels: []string{"INBOX", "UNREAD"}}, // arrived
	}
	for range 2 {
		m.Apply(delta, []string{"b"})
		if got := ids(m.Grid(InboxGrid)); !eq(got, []string{"c"}) {
			t.Fatalf("inbox = %v", got)
		}
		if got := ids(m.Grid(StarredGrid)); !eq(got, []string{"a"}) {
			t.Fatalf("starred = %v", got)
		}
		if a, _ := m.View("a"); a.Unread || !a.Starred {
			t.Errorf("a = %+v", a)
		}
		if c, _ := m.View("c"); !c.Unread {
			t.Errorf("c = %+v", c)
		}
		if m.Member("b") {
			t.Error("a deleted message is still a member")
		}
	}
}
