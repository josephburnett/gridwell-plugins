package mail

import (
	"testing"

	"github.com/josephburnett/gridwell/api/rpc"
)

func TestCollectionEntriesAreTextPagesNeverURLs(t *testing.T) {
	entries := CollectionEntries([]Thread{
		{TopicID: 1, Subject: "a", CreatedAt: at("2026-01-02T09:00:00Z")},
	})
	if len(entries) != 1 {
		t.Fatalf("got %d entries", len(entries))
	}
	e := entries[0]
	if e.Kind != rpc.KindText {
		t.Errorf("kind = %q, want %q", e.Kind, rpc.KindText)
	}
	if e.Kind == rpc.KindURL {
		t.Error("a page tile must never be a url entry")
	}
	if !e.ServesPage {
		t.Error("serves_page not declared")
	}
	if e.UrlString != "" {
		t.Errorf("url_string = %q; the node derives a page's address", e.UrlString)
	}
	if e.Key != "thread:1" {
		t.Errorf("key = %q", e.Key)
	}
	if e.TextPresentation != rpc.TextPresentationBoth {
		t.Errorf("text_presentation = %q", e.TextPresentation)
	}
}

// One row per day, the day's threads left to right in arrival order: a hint
// derived from the thread's own timestamp, so it is the same on every host.
func TestCollectionEntriesHintAsACalendar(t *testing.T) {
	entries := CollectionEntries([]Thread{
		{TopicID: 1, CreatedAt: at("2026-01-02T09:00:00Z")},
		{TopicID: 2, CreatedAt: at("2026-01-02T11:00:00Z")},
		{TopicID: 3, CreatedAt: at("2026-01-03T09:00:00Z")},
	})
	got := make([][2]int64, len(entries))
	for i, e := range entries {
		got[i] = [2]int64{e.PlacementHint.X, e.PlacementHint.Y}
	}
	want := [][2]int64{{0, -1}, {ThreadTileW, -1}, {0, -2}}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("entry %d hinted at %v, want %v", i, got[i], want[i])
		}
	}
}

// Every collection is a menu entry. There is no privileged one, because a
// plugin is not a place: it contributes doorways, and each collection is one.
func TestMenuEntriesDeclareEveryCollection(t *testing.T) {
	got := MenuEntries()
	if len(got) != len(Collections) {
		t.Fatalf("got %d menu entries, want one per collection (%d)", len(got), len(Collections))
	}
	for i, e := range got {
		if e.Context != Collections[i].Key || e.Id != Collections[i].Key || e.Label != Collections[i].Label {
			t.Errorf("entry %d = %+v, want collection %+v", i, e, Collections[i])
		}
	}
}
