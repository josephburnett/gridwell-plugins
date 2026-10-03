package mail

import (
	"reflect"
	"testing"

	"github.com/josephburnett/gridwell/api/rpc"
)

// Everything is every thread some box holds, each once, oldest first: the
// one home a thread's tile has, whichever box it is in.
func TestEverythingIsTheUnionOfEveryBox(t *testing.T) {
	m := NewMemory()
	m.Absorb(ImboxContext, []Thread{thread(1, "a", "2026-01-02"), thread(2, "b", "2026-01-04")}, true)
	m.Absorb(ReplyLaterContext, []Thread{thread(2, "b", "2026-01-04"), thread(3, "c", "2026-01-03")}, true)
	m.Absorb(FeedContext, []Thread{thread(4, "d", "2026-01-01")}, true)
	if got, want := keys(m.Everything()), []int64{4, 1, 3, 2}; !reflect.DeepEqual(got, want) {
		t.Fatalf("everything = %v, want %v", got, want)
	}
	// A thread that leaves every box leaves everything; its record stays.
	m.Absorb(FeedContext, nil, true)
	if got, want := keys(m.Everything()), []int64{1, 3, 2}; !reflect.DeepEqual(got, want) {
		t.Fatalf("everything after the feed emptied = %v, want %v", got, want)
	}
}

// A box lists links to everything: the thread's own key and face, its
// content facts kept for a node that predates links, and its target the same
// thread in everything.
func TestBoxEntriesLinkToEverything(t *testing.T) {
	th := thread(7, "lunch", "2026-01-02")
	got := BoxEntries([]Thread{th})
	want := CollectionEntries([]Thread{th})
	if len(got) != 1 || len(want) != 1 {
		t.Fatalf("entries = %+v", got)
	}
	e := got[0]
	if lt := e.GetLinkTarget(); lt == nil || lt.Context != EverythingContext || lt.Key != th.Key() {
		t.Fatalf("link_target = %+v, want %s/%s", e.GetLinkTarget(), EverythingContext, th.Key())
	}
	if e.Key != want[0].Key || e.Label != want[0].Label || e.Kind != rpc.KindURL || !e.ServesPage ||
		e.PlacementHint.X != want[0].PlacementHint.X || e.PlacementHint.Y != want[0].PlacementHint.Y {
		t.Fatalf("box entry = %+v, want the thread's entry %+v with a link_target", e, want[0])
	}
	if want[0].GetLinkTarget() != nil {
		t.Fatal("everything's own entry is a link")
	}
}

// A box says a thread has left it only when it knows: a whole walk that did
// not list it, or the feed's delete of the posting that held it. A capped
// walk and an empty memory cannot say.
func TestInBoxSaysGoneOnlyWhenTheBoxKnows(t *testing.T) {
	m := NewMemory()
	if got := m.InBox(ImboxContext, 1); got != Unknown {
		t.Fatalf("cold = %v", got)
	}
	m.Absorb(ImboxContext, []Thread{thread(1, "a", "2026-01-02"), thread(2, "b", "2026-01-03")}, false)
	if got := m.InBox(ImboxContext, 1); got != Present {
		t.Fatalf("listed = %v", got)
	}
	if got := m.InBox(ImboxContext, 9); got != Unknown {
		t.Fatalf("absent from a capped walk = %v", got)
	}
	m.Apply(ImboxContext, deleted("imbox", 10))
	if got := m.InBox(ImboxContext, 1); got != Gone {
		t.Fatalf("after the feed deleted its posting = %v", got)
	}
	m.Apply(ImboxContext, added("imbox", thread(1, "a", "2026-01-02")))
	if got := m.InBox(ImboxContext, 1); got != Present {
		t.Fatalf("added back = %v", got)
	}
	m.Absorb(ReplyLaterContext, nil, true)
	if got := m.InBox(ReplyLaterContext, 1); got != Gone {
		t.Fatalf("absent from a whole walk = %v", got)
	}
}

// Every change says whether everything's listing moved too: a thread's own
// record changing, or the union gaining or losing it. A thread leaving one
// box for another moves the boxes, not everything.
func TestAChangeSaysWhetherEverythingMoved(t *testing.T) {
	m := NewMemory()
	if eff := m.Absorb(ImboxContext, []Thread{thread(1, "a", "2026-01-02")}, true); !eff.Changed || !eff.Everything {
		t.Fatalf("first walk = %+v", eff)
	}
	m.Absorb(ReplyLaterContext, nil, true)
	seen := thread(1, "a", "2026-01-02")
	seen.Seen = true
	if eff := m.Apply(ImboxContext, Event{Change: ChangeUpdated, Box: "imbox", PostingID: 10, Thread: seen}); !eff.Everything {
		t.Fatalf("seen = %+v, want everything moved", eff)
	}
	if eff := m.Apply(ReplyLaterContext, added("laterbox", seen)); !eff.Changed || eff.Everything {
		t.Fatalf("added to a second box = %+v, want only the box moved", eff)
	}
	if eff := m.Apply(ImboxContext, deleted("imbox", 10)); !eff.Changed || eff.Everything {
		t.Fatalf("left the first box = %+v, want only the box moved", eff)
	}
	if eff := m.Apply(ReplyLaterContext, deleted("laterbox", 10)); !eff.Changed || !eff.Everything {
		t.Fatalf("left its last box = %+v, want everything moved", eff)
	}
}
