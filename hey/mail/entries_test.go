package mail

import (
	"testing"

	"github.com/josephburnett/gridwell-plugins/memo/calendar"
	pluginv1 "github.com/josephburnett/gridwell/api/gen/plugin/v1"
	"github.com/josephburnett/gridwell/api/rpc"
)

// A thread is a page, and a page is a url entry the plugin serves from: the
// address is the node's to derive, and no text body rides beside it.
func TestCollectionEntriesAreURLsThatServeTheirPage(t *testing.T) {
	entries := CollectionEntries([]Thread{
		{TopicID: 1, Subject: "a", CreatedAt: at("2026-01-02T09:00:00Z")},
	})
	if len(entries) != 1 {
		t.Fatalf("got %d entries", len(entries))
	}
	e := entries[0]
	if e.Kind != rpc.KindURL {
		t.Errorf("kind = %q, want %q", e.Kind, rpc.KindURL)
	}
	if !e.ServesPage {
		t.Error("serves_page not declared")
	}
	if e.UrlString != "" {
		t.Errorf("url_string = %q; the node derives a served page's address", e.UrlString)
	}
	if e.Key != "thread:1" {
		t.Errorf("key = %q", e.Key)
	}
	if e.TextPresentation != "" {
		t.Errorf("text_presentation = %q; a url entry has no text body", e.TextPresentation)
	}
}

// A thread's hint is calendar.Cell of its own creation time and nothing
// else: a thread arriving earlier the same day leaves every other hint where
// it was, in a box and in everything alike.
func TestEntriesHintIsPureInTheCreationTime(t *testing.T) {
	later := Thread{TopicID: 2, CreatedAt: at("2026-09-02T11:00:00Z")}
	earlier := Thread{TopicID: 1, CreatedAt: at("2026-09-02T09:00:00Z")}
	wx, wy := calendar.Cell(later.CreatedAt, ThreadTileW)
	for name, derive := range map[string]func([]Thread) []*pluginv1.Entry{
		"everything": CollectionEntries,
		"box":        BoxEntries,
	} {
		alone := derive([]Thread{later})
		both := derive([]Thread{earlier, later})
		for _, h := range []*pluginv1.PlacementHint{alone[0].PlacementHint, both[1].PlacementHint} {
			if h.X != wx || h.Y != wy || h.W != ThreadTileW || h.H != 1 {
				t.Errorf("%s: hint = %+v, want (%d, %d) %dx1", name, h, wx, wy, ThreadTileW)
			}
		}
	}
}

// Seen is a box's fact: a box tile carries the unseen mark only while it is
// unseen there, and everything, which is no box, carries none.
func TestStatusIsTheUnseenMarkInABoxOnly(t *testing.T) {
	threads := []Thread{
		{TopicID: 1, Subject: "a", CreatedAt: at("2026-09-02T09:00:00Z")},
		{TopicID: 2, Subject: "b", CreatedAt: at("2026-09-02T10:00:00Z"), Seen: true},
	}
	box := BoxEntries(threads)
	if box[0].StatusDetail != UnseenMark {
		t.Errorf("unseen box tile status = %q, want %q", box[0].StatusDetail, UnseenMark)
	}
	if box[1].StatusDetail != "" {
		t.Errorf("seen box tile status = %q, want none", box[1].StatusDetail)
	}
	for _, e := range CollectionEntries(threads) {
		if e.StatusDetail != "" {
			t.Errorf("everything tile %s status = %q, want none", e.Key, e.StatusDetail)
		}
	}
}

// The (+) menu offers imbox, reply later, set aside and everything; the
// other boxes are walked into everything but are not doorways.
func TestMenuEntriesDeclareTheDoorways(t *testing.T) {
	got := MenuEntries()
	want := []Collection{
		{Key: ImboxContext, Label: "imbox"},
		{Key: ReplyLaterContext, Label: "reply later"},
		{Key: SetAsideContext, Label: "set aside"},
		{Key: EverythingContext, Label: EverythingLabel},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d menu entries, want the four doorways (%d)", len(got), len(want))
	}
	for i, e := range got {
		if e.Context != want[i].Key || e.Id != want[i].Key || e.Label != want[i].Label {
			t.Errorf("entry %d = %+v, want collection %+v", i, e, want[i])
		}
	}
}
