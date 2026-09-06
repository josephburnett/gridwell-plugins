package mail

import (
	"path/filepath"
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
	m := NewMemory()
	m.Absorb(ImboxContext, []Thread{thread(1, "a", "2026-01-02"), thread(2, "b", "2026-01-03")}, true)
	if got, want := keys(m.Collection(ImboxContext)), []int64{1, 2}; !reflect.DeepEqual(got, want) {
		t.Fatalf("imbox = %v, want %v", got, want)
	}
	// Thread 1 archived at HEY: a whole walk no longer sees it, so it leaves
	// the grid. The record survives, so its tile can still read as the email.
	m.Absorb(ImboxContext, []Thread{thread(2, "b", "2026-01-03")}, true)
	if got, want := keys(m.Collection(ImboxContext)), []int64{2}; !reflect.DeepEqual(got, want) {
		t.Fatalf("after archive imbox = %v, want %v", got, want)
	}
	if _, ok := m.Get(1); !ok {
		t.Error("the archived thread's record was forgotten")
	}
	if m.Member(1) {
		t.Error("the archived thread is still a member")
	}
}

// A partial read proves nothing about what it did not reach: merging is the
// only safe fold, or a capped listing would empty the user's grid.
func TestPartialWalkOnlyAdds(t *testing.T) {
	m := NewMemory()
	m.Absorb(ImboxContext, []Thread{thread(1, "a", "2026-01-02"), thread(2, "b", "2026-01-03")}, true)
	m.Absorb(ImboxContext, []Thread{thread(3, "c", "2026-01-04")}, false)
	if got, want := keys(m.Collection(ImboxContext)), []int64{1, 2, 3}; !reflect.DeepEqual(got, want) {
		t.Fatalf("imbox = %v, want %v", got, want)
	}
}

// Nothing is ever GONE until every collection has been read to its end: a
// thread missing from a half-swept memory may simply be in the half not read.
func TestSweptNeedsEveryCollection(t *testing.T) {
	m := NewMemory()
	if m.Swept() {
		t.Fatal("a cold memory reports itself swept")
	}
	m.Absorb(ImboxContext, nil, true)
	m.Absorb(ReplyLaterContext, nil, true)
	if m.Swept() {
		t.Fatal("two of three collections reported a full sweep")
	}
	m.Absorb(SetAsideContext, nil, true)
	if !m.Swept() {
		t.Fatal("three complete walks did not make a sweep")
	}
	// A partial walk of one collection does not un-sweep the memory: the box
	// HAS been read to its end once, and that is what the flag records.
	m.Absorb(SetAsideContext, nil, false)
	if !m.Swept() {
		t.Fatal("a later partial walk forgot the completed one")
	}
}

// A thread that moves between collections keeps its key and its record, and
// belongs to exactly the collection that last listed it.
func TestAThreadMovesBetweenCollections(t *testing.T) {
	m := NewMemory()
	m.Absorb(ImboxContext, []Thread{thread(1, "a", "2026-01-02")}, true)
	m.Absorb(ReplyLaterContext, []Thread{thread(1, "a", "2026-01-02")}, true)
	m.Absorb(ImboxContext, nil, true)
	if got := m.Collection(ImboxContext); len(got) != 0 {
		t.Fatalf("imbox still holds %v", keys(got))
	}
	if got, want := keys(m.Collection(ReplyLaterContext)), []int64{1}; !reflect.DeepEqual(got, want) {
		t.Fatalf("reply later = %v, want %v", got, want)
	}
	th, ok := m.Get(1)
	if !ok || th.Collection != ReplyLaterContext {
		t.Fatalf("thread 1 records collection %q", th.Collection)
	}
	if !m.Member(1) {
		t.Error("a thread in reply later is not a member")
	}
}

func TestCollectionIsOldestFirstAndTotal(t *testing.T) {
	m := NewMemory()
	// Same instant, listed newest first, as HEY answers.
	a := Thread{TopicID: 9, CreatedAt: at("2026-01-05T09:00:00Z")}
	b := Thread{TopicID: 4, CreatedAt: at("2026-01-05T09:00:00Z")}
	c := Thread{TopicID: 2, CreatedAt: at("2026-01-01T09:00:00Z")}
	m.Absorb(ImboxContext, []Thread{a, b, c}, true)
	if got, want := keys(m.Collection(ImboxContext)), []int64{2, 4, 9}; !reflect.DeepEqual(got, want) {
		t.Fatalf("order = %v, want %v", got, want)
	}
}

func TestCacheRoundTrips(t *testing.T) {
	m := NewMemory()
	m.Absorb(ImboxContext, []Thread{thread(1, "a", "2026-01-02")}, true)
	m.Absorb(ReplyLaterContext, []Thread{thread(2, "b", "2026-01-03")}, false)

	path := filepath.Join(t.TempDir(), "sub", CacheFile)
	if err := SaveCache(path, m.Snapshot()); err != nil {
		t.Fatalf("SaveCache: %v", err)
	}
	snap, err := LoadCache(path)
	if err != nil {
		t.Fatalf("LoadCache: %v", err)
	}
	back := NewMemory()
	back.Restore(snap)

	if got, want := keys(back.Collection(ImboxContext)), []int64{1}; !reflect.DeepEqual(got, want) {
		t.Errorf("imbox = %v, want %v", got, want)
	}
	if got, want := keys(back.Collection(ReplyLaterContext)), []int64{2}; !reflect.DeepEqual(got, want) {
		t.Errorf("reply later = %v, want %v", got, want)
	}
	// Completeness rides the file: a restart must not be able to say GONE on
	// the strength of a walk that never finished.
	if back.Swept() {
		t.Error("a restored half sweep reports itself swept")
	}
	back.Absorb(ReplyLaterContext, nil, true)
	back.Absorb(SetAsideContext, nil, true)
	if !back.Swept() {
		t.Error("the restored imbox forgot its completed walk")
	}
}

func TestLoadCacheRefusesAnotherVersion(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, CacheFile)
	if err := SaveCache(path, Snapshot{Version: snapshotVersion + 1}); err != nil {
		t.Fatalf("SaveCache: %v", err)
	}
	if _, err := LoadCache(path); err == nil {
		t.Fatal("LoadCache accepted a foreign version")
	}
}
