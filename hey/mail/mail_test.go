package mail

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

func TestKeyRoundTrips(t *testing.T) {
	th := Thread{TopicID: 12345}
	if got := th.Key(); got != "thread:12345" {
		t.Fatalf("Key() = %q", got)
	}
	id, ok := ParseKey("thread:12345")
	if !ok || id != 12345 {
		t.Fatalf("ParseKey = %d, %v", id, ok)
	}
	for _, bad := range []string{"12345", "thread:", "thread:0", "thread:-1", "thread:abc", "box:imbox"} {
		if _, ok := ParseKey(bad); ok {
			t.Errorf("ParseKey(%q) accepted", bad)
		}
	}
}

func TestTitleFallsBackToPreviewThenNotice(t *testing.T) {
	cases := []struct {
		name string
		in   Thread
		want string
	}{
		{"subject", Thread{Subject: "Lunch plans", Summary: "are you free"}, "Lunch plans"},
		{"blank subject", Thread{Subject: "   ", Summary: "are you free\nfriday?"}, "are you free"},
		{"nothing", Thread{}, NoSubject},
	}
	for _, c := range cases {
		if got := c.in.Title(); got != c.want {
			t.Errorf("%s: Title() = %q, want %q", c.name, got, c.want)
		}
	}
}

func TestLabelMarksUnseenAndNamesTheSender(t *testing.T) {
	unread := Thread{Subject: "Lunch plans", FromName: "Alice", Seen: false}
	if got, want := unread.Label(), UnseenMark+" Alice: Lunch plans"; got != want {
		t.Errorf("Label() = %q, want %q", got, want)
	}
	read := Thread{Subject: "Lunch plans", FromName: "Alice", Seen: true}
	if got, want := read.Label(), "Alice: Lunch plans"; got != want {
		t.Errorf("Label() = %q, want %q", got, want)
	}
	// No name: the address is who it is from.
	anon := Thread{Subject: "Receipt", FromEmail: "billing@example.com", Seen: true}
	if got, want := anon.Label(), "billing@example.com: Receipt"; got != want {
		t.Errorf("Label() = %q, want %q", got, want)
	}
}

// The hint must not shift with the host's zone: a thread that landed just
// before midnight UTC belongs to the UTC day, whatever TZ the node runs in.
func TestDayIsUTCAndAnchoredAtTheEpoch(t *testing.T) {
	if got := Day(HintEpoch); got != 0 {
		t.Errorf("Day(epoch) = %d, want 0", got)
	}
	if got := Day(at("2026-01-03T23:59:00Z")); got != 2 {
		t.Errorf("Day = %d, want 2", got)
	}
	// Same instant, expressed nine hours east: still the 3rd in UTC.
	east := time.FixedZone("east", 9*3600)
	if got := Day(at("2026-01-03T23:59:00Z").In(east)); got != 2 {
		t.Errorf("Day in +09:00 = %d, want 2", got)
	}
	if got := Day(at("2025-12-31T00:00:00Z")); got != -1 {
		t.Errorf("Day before epoch = %d, want -1", got)
	}
}

func TestCellPutsNewerDaysHigher(t *testing.T) {
	_, older := Cell(at("2026-01-01T00:00:00Z"), 0)
	_, newer := Cell(at("2026-01-05T00:00:00Z"), 0)
	if !(newer < older) {
		t.Fatalf("newer y=%d is not above older y=%d", newer, older)
	}
	x0, _ := Cell(at("2026-01-05T00:00:00Z"), 0)
	x1, _ := Cell(at("2026-01-05T00:00:00Z"), 1)
	if x1-x0 != ThreadTileW {
		t.Fatalf("second tile of the day at x=%d, first at x=%d", x1, x0)
	}
}

func TestCollectionsAreTheThreeStacks(t *testing.T) {
	if len(Collections) != 3 {
		t.Fatalf("got %d collections", len(Collections))
	}
	boxes := map[string]bool{}
	for _, c := range Collections {
		if _, ok := LookupCollection(c.Key); !ok {
			t.Errorf("LookupCollection(%q) missed", c.Key)
		}
		boxes[c.Box] = true
	}
	for _, want := range []string{"imbox", "laterbox", "asidebox"} {
		if !boxes[want] {
			t.Errorf("no collection reads box %q", want)
		}
	}
	if _, ok := LookupCollection("box:trailbox"); ok {
		t.Error("LookupCollection accepted a box this plugin does not project")
	}
}

func TestMarkdownIsACardAboutTheEmail(t *testing.T) {
	th := Thread{
		TopicID: 7, Collection: ReplyLaterContext, Subject: "Lunch plans",
		Summary: "Are you  free\nfriday?", FromName: "Alice", FromEmail: "alice@example.com",
		CreatedAt: at("2026-01-05T14:03:00Z"),
	}
	got := string(Markdown(&th))
	for _, want := range []string{
		"# " + UnseenMark + " Lunch plans",
		"from Alice <alice@example.com>",
		"2026-01-05 14:03 UTC",
		"reply later",
		"> Are you free friday?",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("markdown missing %q:\n%s", want, got)
		}
	}
}

func TestNoticeHTMLEscapes(t *testing.T) {
	got := string(NoticeHTML("a <script>", "b & c"))
	if strings.Contains(got, "<script>") {
		t.Errorf("notice did not escape its title:\n%s", got)
	}
	if !strings.Contains(got, "b &amp; c") {
		t.Errorf("notice did not escape its detail:\n%s", got)
	}
}
