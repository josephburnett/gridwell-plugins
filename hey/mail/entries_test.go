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

// The plugin's own (+) row already opens the root context; declaring a menu
// entry for it too would offer the same grid twice.
func TestMenuEntriesSkipTheRootContext(t *testing.T) {
	got := MenuEntries(ImboxContext)
	if len(got) != 2 {
		t.Fatalf("got %d menu entries, want 2", len(got))
	}
	for _, e := range got {
		if e.Context == ImboxContext {
			t.Errorf("the root context %q rides the menu too", e.Context)
		}
		if e.Context == "" || e.Id == "" || e.Label == "" {
			t.Errorf("incomplete menu entry %+v", e)
		}
	}
	if len(MenuEntries("")) != 3 {
		t.Error("a rootless plugin should offer all three collections")
	}
}
