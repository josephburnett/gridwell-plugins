package mailbox

import (
	"strings"
	"testing"
	"time"

	"github.com/josephburnett/gridwell-plugins/memo/calendar"
)

func at(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return t
}

func msg(id, subject, date string) Message {
	return Message{
		ID: id, ThreadID: "t" + id, Subject: subject, Snippet: "about " + subject,
		FromName: "Alice", FromEmail: "alice@example.com", Date: at(date),
	}
}

// A key names the same email forever. Both halves are pinned: a key the
// plugin mints must parse, and a key it never minted must not.
func TestKeysRoundTripAndRejectWhatIsNotOne(t *testing.T) {
	m := msg("18c2a1b3f4d5e6f7", "lunch", "2026-01-05T14:00:00Z")
	if m.Key() != "msg:18c2a1b3f4d5e6f7" {
		t.Fatalf("key = %q", m.Key())
	}
	id, ok := ParseKey(m.Key())
	if !ok || id != m.ID {
		t.Fatalf("ParseKey = %q %v", id, ok)
	}
	for _, bad := range []string{"", "msg:", "label:INBOX", "thread:1", "msg:a/b", "msg:a.b", "msg:../x"} {
		if _, ok := ParseKey(bad); ok {
			t.Errorf("ParseKey(%q) accepted", bad)
		}
	}
}

// The two collections are the projection, and the inbox leads because it is
// also the root context.
func TestCollectionsAreInboxThenStarred(t *testing.T) {
	if len(Collections) != 2 || Collections[0].Key != InboxContext || Collections[1].Key != StarredContext {
		t.Fatalf("collections = %+v", Collections)
	}
	c, ok := LookupCollection(StarredContext)
	if !ok || len(c.LabelIDs) != 1 || c.LabelIDs[0] != "STARRED" {
		t.Fatalf("starred = %+v %v", c, ok)
	}
	if _, ok := LookupCollection("label:SPAM"); ok {
		t.Error("an undeclared label resolved to a collection")
	}
}

// A tile's name is its subject whatever its state, and its state is one
// emoji in status_detail only when there is something to notice: unread, or
// starred where the grid does not already say so. A read message says
// nothing.
func TestTheLabelIsTheSubjectAndStateIsQuiet(t *testing.T) {
	m := msg("a1", "lunch", "2026-01-05T14:00:00Z")
	cases := []struct {
		v                   View
		all, inbox, starred string
	}{
		{View{Message: m}, "", "", ""},
		{View{Message: m, Unread: true}, UnreadMark, UnreadMark, UnreadMark},
		{View{Message: m, Starred: true}, StarMark, StarMark, ""},
		{View{Message: m, Unread: true, Starred: true}, UnreadMark, UnreadMark, UnreadMark},
	}
	for _, c := range cases {
		entries := map[string]*Entry{
			AllMailContext: CollectionEntries([]View{c.v})[0],
			InboxContext:   LabelEntries(InboxContext, []View{c.v})[0],
			StarredContext: LabelEntries(StarredContext, []View{c.v})[0],
		}
		for ctx, want := range map[string]string{AllMailContext: c.all, InboxContext: c.inbox, StarredContext: c.starred} {
			e := entries[ctx]
			if e.Label != "lunch" {
				t.Errorf("%+v in %s: label = %q, want the subject", c.v, ctx, e.Label)
			}
			if e.StatusDetail != want {
				t.Errorf("%+v in %s: status = %q, want %q", c.v, ctx, e.StatusDetail, want)
			}
		}
	}
}

// A subject Gmail named nothing still gets a banner: a blank tile cannot be
// read, and the snippet is the next best headline.
func TestTitleFallsBackToTheSnippetThenSaysItIsBlank(t *testing.T) {
	if got := (&Message{Subject: " ", Snippet: "first line\nsecond"}).Title(); got != "first line" {
		t.Errorf("title = %q", got)
	}
	if got := (&Message{}).Title(); got != NoSubject {
		t.Errorf("title = %q", got)
	}
}

// A From header no parser will take must not lose the sender: the whole
// header becomes the address, so the tile still says where the mail came from.
func TestParseFromKeepsWhatItCannotParse(t *testing.T) {
	cases := []struct {
		in, name, addr string
	}{
		{`Alice <alice@example.com>`, "Alice", "alice@example.com"},
		{`alice@example.com`, "", "alice@example.com"},
		{`"Bob, of Sales" <bob@example.com>`, "Bob, of Sales", "bob@example.com"},
		{`Not An Address At All`, "", "Not An Address At All"},
		{"  ", "", ""},
	}
	for _, c := range cases {
		name, addr := ParseFrom(c.in)
		if name != c.name || addr != c.addr {
			t.Errorf("ParseFrom(%q) = %q/%q, want %q/%q", c.in, name, addr, c.name, c.addr)
		}
	}
}

// A message's hint is the shared calendar's cell for its date, a function of
// the message alone: a message that arrives late, earlier in the same day,
// moves no other message's hint.
func TestHintsAreTheCalendarCellOfTheDate(t *testing.T) {
	one := []View{
		{Message: msg("b", "two", "2026-01-03T10:00:00Z")},
		{Message: msg("c", "three", "2026-01-04T10:00:00Z")},
	}
	more := append([]View{{Message: msg("a", "one", "2026-01-03T09:00:00Z")}}, one...)
	before := map[string]*Entry{}
	for _, e := range CollectionEntries(one) {
		before[e.Key] = e
	}
	for _, e := range CollectionEntries(more) {
		m, _ := ParseKey(e.Key)
		var date time.Time
		for _, v := range more {
			if v.ID == m {
				date = v.Date
			}
		}
		x, y := calendar.Cell(date, MessageTileW)
		if h := e.PlacementHint; h.X != x || h.Y != y || h.W != MessageTileW || h.H != 1 {
			t.Errorf("%s hint = %+v, want the calendar's (%d,%d)", e.Key, h, x, y)
		}
		if b, ok := before[e.Key]; ok && (b.PlacementHint.X != e.PlacementHint.X || b.PlacementHint.Y != e.PlacementHint.Y) {
			t.Errorf("%s moved from %+v to %+v when an earlier message arrived", e.Key, b.PlacementHint, e.PlacementHint)
		}
	}
}

// The card is markdown ABOUT the email and carries no link: the email is what
// the tile opens into, and a link would open a second, weaker copy of it.
func TestMarkdownIsTheCard(t *testing.T) {
	v := View{Message: msg("a1", "lunch", "2026-01-05T14:03:00Z"), Unread: true, Starred: true}
	got := string(Markdown(v))
	for _, want := range []string{"# " + UnreadMark + StarMark + " lunch", "from Alice <alice@example.com>", "2026-01-05 14:03 UTC", "> about lunch"} {
		if !strings.Contains(got, want) {
			t.Errorf("card missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "](") {
		t.Errorf("the card carries a link:\n%s", got)
	}
}

// A plain-text mail is escaped into its page, not injected: escaping is not
// sanitizing (the node sandboxes), it is what stops "<b>" typed in a plain
// mail from being read as markup it never was.
func TestWrapPlainEscapes(t *testing.T) {
	got := string(WrapPlain("hi", []byte("a <b> & \"c\"\nnext line")))
	if !strings.HasPrefix(got, "<!doctype html>") {
		t.Fatalf("not a document: %q", got)
	}
	if strings.Contains(got, "<b>") || !strings.Contains(got, "&lt;b&gt;") {
		t.Errorf("plain text was not escaped:\n%s", got)
	}
	if !strings.Contains(got, "next line") {
		t.Errorf("the body was lost:\n%s", got)
	}
	// The title is escaped too: a subject is the sender's bytes.
	if strings.Contains(string(WrapPlain(`<script>`, nil)), "<script>") {
		t.Error("the subject reached the document as markup")
	}
}

// Every entry is a url tile that serves a page, and every context is a menu
// entry.
func TestEntriesAndMenu(t *testing.T) {
	views := []View{
		{Message: msg("a", "one", "2026-01-03T09:00:00Z")},
		{Message: msg("b", "two", "2026-01-03T10:00:00Z"), Unread: true},
		{Message: msg("c", "three", "2026-01-04T10:00:00Z")},
	}
	es := CollectionEntries(views)
	if len(es) != 3 {
		t.Fatalf("entries = %d", len(es))
	}
	for _, e := range es {
		if e.Kind != "url" || !e.ServesPage || e.UrlString != "" || e.TextPresentation != "" {
			t.Fatalf("entry = %+v", e)
		}
	}

	menu := MenuEntries()
	if len(menu) != len(Collections)+1 || menu[0].Context != InboxContext ||
		menu[1].Context != StarredContext || menu[1].Label != "starred" || menu[2].Context != AllMailContext {
		t.Fatalf("menu = %+v, want one entry per label, then all mail", menu)
	}
}
