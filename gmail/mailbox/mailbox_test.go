package mailbox

import (
	"strings"
	"testing"
	"time"
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

// The tile's banner reads the state marks first, so unread and starred read
// from a zoomed-out grid where the text does not — and the SAME message reads
// the same in both grids, because both derive it from the same view.
func TestLabelLeadsWithTheStateMarks(t *testing.T) {
	m := msg("a1", "lunch", "2026-01-05T14:00:00Z")
	cases := []struct {
		v    View
		want string
	}{
		{View{Message: m}, "Alice: lunch"},
		{View{Message: m, Unread: true}, UnreadMark + " Alice: lunch"},
		{View{Message: m, Starred: true}, StarMark + " Alice: lunch"},
		{View{Message: m, Unread: true, Starred: true}, UnreadMark + StarMark + " Alice: lunch"},
	}
	for _, c := range cases {
		if got := c.v.Label(); got != c.want {
			t.Errorf("label = %q, want %q", got, c.want)
		}
	}
	if got := (View{Message: m, Unread: true}).StatusDetail(); got != "unread" {
		t.Errorf("status = %q", got)
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
	// No display name: the address is who it is from.
	if got := (&Message{FromEmail: "bot@example.com"}).From(); got != "bot@example.com" {
		t.Errorf("from = %q", got)
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

// The calendar hint is anchored to a fixed epoch in UTC, so a message lands
// on the same cell on every host and after every restart.
func TestPlacementIsACalendarInUTC(t *testing.T) {
	if got := Day(HintEpoch); got != 0 {
		t.Errorf("epoch day = %d", got)
	}
	// Late on the 5th in a zone eight hours west is still the 6th in UTC, and
	// the hint must not shift with the host.
	east := time.FixedZone("east", 8*3600)
	if Day(at("2026-01-06T01:00:00Z")) != Day(at("2026-01-06T01:00:00Z").In(east)) {
		t.Error("the hint moved with the zone")
	}
	x, y := Cell(at("2026-01-03T00:00:00Z"), 2)
	if x != 2*MessageTileW || y != -2 {
		t.Errorf("cell = %d,%d", x, y)
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

// Every entry is a text tile that serves a page, hinted as a calendar. The
// root context gets no menu row of its own: the plugin's own (+) row already
// opens it.
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
		if e.Kind != "text" || !e.ServesPage || e.TextPresentation != "both" {
			t.Fatalf("entry = %+v", e)
		}
	}
	// One row per day, in arrival order across the row.
	if es[0].PlacementHint.X != 0 || es[1].PlacementHint.X != MessageTileW {
		t.Errorf("same-day hints = %+v %+v", es[0].PlacementHint, es[1].PlacementHint)
	}
	if es[2].PlacementHint.X != 0 || es[2].PlacementHint.Y != es[0].PlacementHint.Y-1 {
		t.Errorf("next-day hint = %+v", es[2].PlacementHint)
	}

	menu := MenuEntries(InboxContext)
	if len(menu) != 1 || menu[0].Context != StarredContext || menu[0].Label != "starred" {
		t.Fatalf("menu = %+v", menu)
	}
}
