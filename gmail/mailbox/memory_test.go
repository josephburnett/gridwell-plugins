package mailbox

import (
	"os"
	"path/filepath"
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
	m := NewMemory()
	m.Absorb(InboxContext, []string{"b", "a"}, map[string]bool{"b": true},
		[]Message{msg("a", "one", "2026-01-03T09:00:00Z"), msg("b", "two", "2026-01-04T09:00:00Z")}, true)
	if got := ids(m.Collection(InboxContext)); !eq(got, []string{"a", "b"}) { // oldest first
		t.Fatalf("collection = %v", got)
	}
	m.Absorb(InboxContext, []string{"b"}, nil, nil, true)
	if got := ids(m.Collection(InboxContext)); !eq(got, []string{"b"}) {
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
	m := NewMemory()
	all := []Message{
		msg("old", "old", "2026-01-01T09:00:00Z"),
		msg("mid", "mid", "2026-01-03T09:00:00Z"),
		msg("new", "new", "2026-01-05T09:00:00Z"),
	}
	m.Absorb(InboxContext, []string{"new", "mid", "old"}, nil, all, true)

	// "new" is archived and "newer" arrives; the next read is capped two
	// messages down, at "mid". "new" was above the watermark and did not come
	// back, so it has left. "old" is below it, where the read said nothing, so
	// it stays.
	m.Absorb(InboxContext, []string{"newer", "mid"}, nil,
		[]Message{msg("newer", "newer", "2026-01-06T09:00:00Z")}, false)
	got := ids(m.Collection(InboxContext))
	if !eq(got, []string{"old", "mid", "newer"}) {
		t.Fatalf("collection = %v; want old (below the watermark), mid and newer", got)
	}
	// A capped read that HAS a watermark is still a usable membership, so it
	// counts towards the sweep that lets Probe ever answer GONE.
	m.Absorb(StarredContext, []string{"newer"}, nil, nil, false)
	if !m.Swept() {
		t.Error("a capped read with a watermark did not count as a usable membership")
	}
}

// A capped read with nothing datable in it is no evidence at all: it must
// merge rather than retire, and it must not claim the membership is usable.
func TestACappedReadWithNoDatesIsNoEvidence(t *testing.T) {
	m := NewMemory()
	m.Absorb(InboxContext, []string{"a"}, nil, []Message{msg("a", "one", "2026-01-03T09:00:00Z")}, true)
	// "z" has no record, so the read has no watermark to stand on.
	m.Absorb(StarredContext, []string{"z"}, nil, nil, false)
	if m.Swept() {
		t.Error("a walk that proved nothing declared the collection usable")
	}
	m.Absorb(StarredContext, []string{"a"}, nil, nil, true)
	if !m.Swept() {
		t.Error("every collection read and still not swept")
	}
}

// Read/unread and starred have one owner. A message in both grids reads the
// same in each, whichever walk ran last.
func TestStateComesFromTheMemoryNotTheRecord(t *testing.T) {
	m := NewMemory()
	rec := []Message{msg("a", "one", "2026-01-03T09:00:00Z")}
	m.Absorb(InboxContext, []string{"a"}, map[string]bool{"a": true}, rec, true)
	m.Absorb(StarredContext, []string{"a"}, map[string]bool{"a": true}, nil, true)

	inbox, starred := m.Collection(InboxContext), m.Collection(StarredContext)
	if len(inbox) != 1 || len(starred) != 1 {
		t.Fatalf("collections = %v %v", ids(inbox), ids(starred))
	}
	if inbox[0].Label() != starred[0].Label() {
		t.Errorf("one message read two ways: %q vs %q", inbox[0].Label(), starred[0].Label())
	}
	if !inbox[0].Unread || !inbox[0].Starred {
		t.Errorf("state = %+v", inbox[0])
	}
	// Read at Gmail: the next walk of either collection clears the mark.
	m.Absorb(InboxContext, []string{"a"}, nil, nil, true)
	if v, _ := m.View("a"); v.Unread {
		t.Error("a message read at Gmail kept its unread mark")
	}
	// Unstarred: it leaves the starred membership, and the mark with it.
	m.Absorb(StarredContext, nil, nil, nil, true)
	if v, _ := m.View("a"); v.Starred {
		t.Error("an unstarred message kept its star")
	}
}

// Only what the memory has a record for becomes a tile. A member whose
// metadata read failed keeps its place in the membership — so nothing calls
// it gone — and gets its tile on the next walk.
func TestAMemberWithNoRecordIsNotATileAndIsNotGone(t *testing.T) {
	m := NewMemory()
	m.Absorb(InboxContext, []string{"a", "b"}, nil, []Message{msg("a", "one", "2026-01-03T09:00:00Z")}, true)
	m.Absorb(StarredContext, nil, nil, nil, true)
	if got := ids(m.Collection(InboxContext)); !eq(got, []string{"a"}) {
		t.Fatalf("collection = %v", got)
	}
	if !m.Member("b") {
		t.Error("a member with no record read as gone")
	}
	if got := m.Missing([]string{"a", "b", "c"}); !eq(got, []string{"b", "c"}) {
		t.Errorf("missing = %v", got)
	}
}

// The cache is the plugin's memory of Gmail across a restart, and it must
// carry every fact the memory owns: the records, the unread set, each
// membership and its usability.
func TestSnapshotRoundTripsEveryFact(t *testing.T) {
	m := NewMemory()
	m.Absorb(InboxContext, []string{"b", "a"}, map[string]bool{"b": true},
		[]Message{msg("a", "one", "2026-01-03T09:00:00Z"), msg("b", "two", "2026-01-04T09:00:00Z")}, true)
	m.Absorb(StarredContext, []string{"b"}, map[string]bool{"b": true}, nil, true)

	path := filepath.Join(t.TempDir(), CacheFile)
	if err := SaveCache(path, m.Snapshot()); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("cache mode = %v", info.Mode().Perm())
	}
	snap, err := LoadCache(path)
	if err != nil {
		t.Fatal(err)
	}
	back := NewMemory()
	back.Restore(snap)
	if got := ids(back.Collection(InboxContext)); !eq(got, []string{"a", "b"}) {
		t.Fatalf("restored inbox = %v", got)
	}
	v, ok := back.View("b")
	if !ok || !v.Unread || !v.Starred || v.Subject != "two" {
		t.Fatalf("restored view = %+v %v", v, ok)
	}
	if !back.Swept() {
		t.Error("a restored sweep did not count")
	}
	// Two snapshots of the same memory are byte-identical, so a save that
	// changes nothing does not churn the file.
	a, b := m.Snapshot(), m.Snapshot()
	if len(a.Messages) != len(b.Messages) || a.Messages[0].ID != b.Messages[0].ID {
		t.Error("the snapshot order is not stable")
	}
}

// A cache from a future format is refused rather than misread: a refused file
// costs one walk, a misread one costs the truth.
func TestACacheOfTheWrongVersionIsRefused(t *testing.T) {
	path := filepath.Join(t.TempDir(), CacheFile)
	if err := os.WriteFile(path, []byte(`{"version":99}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadCache(path); err == nil {
		t.Fatal("a cache from another format was accepted")
	}
	if _, err := LoadCache(filepath.Join(t.TempDir(), "absent")); !os.IsNotExist(err) {
		t.Fatalf("a missing cache = %v, want a not-exist error the caller can read as a first boot", err)
	}
}
