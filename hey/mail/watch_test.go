package mail

import (
	"reflect"
	"testing"
)

func added(box string, t Thread) Event {
	return Event{Change: ChangeAdded, Box: box, PostingID: t.PostingID, Thread: t}
}

func deleted(box string, posting int64) Event {
	return Event{Change: ChangeDeleted, Box: box, PostingID: posting}
}

func TestLookupBoxIsTheSelectorTable(t *testing.T) {
	for _, c := range Collections {
		got, ok := LookupBox(c.Box)
		if !ok || got.Key != c.Key {
			t.Errorf("LookupBox(%q) = %+v, %v", c.Box, got, ok)
		}
	}
	if _, ok := LookupBox("feedbox"); ok {
		t.Error("a box this plugin does not project resolved")
	}
}

// Every line moves memory the way a walk would have: a posting added lists
// its thread, an update rewrites it, a delete — which names only the posting
// — takes it out, and each says whether the listing changed.
func TestApplyMovesTheListingOnePostingAtATime(t *testing.T) {
	m := NewMemory()
	m.Absorb(ImboxContext, []Thread{thread(1, "a", "2026-01-02")}, true)

	eff := m.Apply(ImboxContext, added("imbox", thread(2, "b", "2026-01-03")))
	if !eff.Changed || eff.Rewalk {
		t.Fatalf("added = %+v", eff)
	}
	if got, want := keys(m.Collection(ImboxContext)), []int64{1, 2}; !reflect.DeepEqual(got, want) {
		t.Fatalf("imbox = %v, want %v", got, want)
	}

	seen := thread(2, "b", "2026-01-03")
	seen.Seen = true
	if eff := m.Apply(ImboxContext, Event{Change: ChangeUpdated, Box: "imbox", PostingID: 20, Thread: seen}); !eff.Changed {
		t.Fatal("an update the listing shows reported no change")
	}
	if got, _ := m.Get(2); !got.Seen {
		t.Error("the update did not land")
	}
	if eff := m.Apply(ImboxContext, Event{Change: ChangeUpdated, Box: "imbox", PostingID: 20, Thread: seen}); eff.Changed {
		t.Error("a repeated update reported a change")
	}

	if eff := m.Apply(ImboxContext, deleted("imbox", 10)); !eff.Changed || eff.Rewalk {
		t.Fatalf("deleted = %+v", eff)
	}
	if got, want := keys(m.Collection(ImboxContext)), []int64{2}; !reflect.DeepEqual(got, want) {
		t.Fatalf("after delete imbox = %v, want %v", got, want)
	}
	if _, ok := m.Get(1); !ok {
		t.Error("a deleted posting cost the thread's record")
	}
}

// A posting the memory never mapped is either one no listing shows (a
// bundle) or a member a restored snapshot brought back without its posting.
// The first changes nothing; the second only a read of the box can settle.
func TestAnUnmappedDeleteRewalksOnlyWhenItCouldBeAMember(t *testing.T) {
	m := NewMemory()
	m.Absorb(ImboxContext, []Thread{thread(1, "a", "2026-01-02")}, true)
	if eff := m.Apply(ImboxContext, deleted("imbox", 999)); eff.Changed || eff.Rewalk {
		t.Fatalf("a bundle's delete over a mapped listing = %+v", eff)
	}

	restored := NewMemory()
	restored.Restore(m.Snapshot())
	if eff := restored.Apply(ImboxContext, deleted("imbox", 10)); !eff.Rewalk {
		t.Fatalf("a delete a restored memory cannot map = %+v", eff)
	}
	restored.Absorb(ImboxContext, []Thread{thread(1, "a", "2026-01-02")}, true)
	if eff := restored.Apply(ImboxContext, deleted("imbox", 10)); !eff.Changed || eff.Rewalk {
		t.Fatalf("after a walk the delete maps: %+v", eff)
	}
}

// Before any walk has listed a collection there is no listing for an event to
// join: a lone thread would read as the whole box.
func TestAnEventBeforeTheFirstWalkListsNothing(t *testing.T) {
	m := NewMemory()
	eff := m.Apply(ImboxContext, added("imbox", thread(1, "a", "2026-01-02")))
	if eff.Changed || m.Shows(ImboxContext) {
		t.Fatalf("an event made a listing: %+v, shows=%v", eff, m.Shows(ImboxContext))
	}
	if _, ok := m.Get(1); !ok {
		t.Error("the event's thread was not remembered")
	}
}

// A walk's answer was read before the events that arrived while it ran, so
// those events stand over it: a thread added and a thread deleted during the
// read are where the feed put them, not where the read saw them.
func TestEventsDuringAWalkStandOverItsAnswer(t *testing.T) {
	m := NewMemory()
	m.Absorb(ImboxContext, []Thread{thread(1, "a", "2026-01-02"), thread(2, "b", "2026-01-03")}, true)

	m.BeginWalk(ImboxContext)
	m.Apply(ImboxContext, added("imbox", thread(3, "c", "2026-01-04")))
	m.Apply(ImboxContext, deleted("imbox", 10))
	// The read started before both: it still holds 1 and lacks 3.
	changed := m.Absorb(ImboxContext, []Thread{thread(1, "a", "2026-01-02"), thread(2, "b", "2026-01-03")}, true)
	if got, want := keys(m.Collection(ImboxContext)), []int64{2, 3}; !reflect.DeepEqual(got, want) {
		t.Fatalf("imbox = %v, want %v", got, want)
	}
	if changed {
		t.Error("the walk reported a change the events had already made")
	}

	// A closed walk journals nothing further.
	m.Apply(ImboxContext, deleted("imbox", 20))
	m.Absorb(ImboxContext, []Thread{thread(2, "b", "2026-01-03"), thread(3, "c", "2026-01-04")}, true)
	if got, want := keys(m.Collection(ImboxContext)), []int64{2, 3}; !reflect.DeepEqual(got, want) {
		t.Fatalf("a closed walk replayed: imbox = %v, want %v", got, want)
	}

	// A walk that failed drops its journal.
	m.BeginWalk(ImboxContext)
	m.Apply(ImboxContext, deleted("imbox", 20))
	m.EndWalk(ImboxContext)
	m.Absorb(ImboxContext, []Thread{thread(2, "b", "2026-01-03")}, true)
	if got, want := keys(m.Collection(ImboxContext)), []int64{2}; !reflect.DeepEqual(got, want) {
		t.Fatalf("a failed walk's journal replayed: imbox = %v, want %v", got, want)
	}
}

func TestAbsorbReportsWhetherTheListingChanged(t *testing.T) {
	m := NewMemory()
	if !m.Absorb(ImboxContext, []Thread{thread(1, "a", "2026-01-02")}, true) {
		t.Fatal("a first listing reported no change")
	}
	if m.Absorb(ImboxContext, []Thread{thread(1, "a", "2026-01-02")}, true) {
		t.Fatal("the same listing reported a change")
	}
	if !m.Absorb(ImboxContext, nil, true) {
		t.Fatal("an emptied box reported no change")
	}
	if !m.Shows(ImboxContext) {
		t.Error("a walked empty box is not a listing to answer with")
	}
}
