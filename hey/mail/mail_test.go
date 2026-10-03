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

// The label is the tile's stable name: seen or unseen, it reads the same,
// and the status carries the difference.
func TestLabelIsTheSenderAndSubjectWhateverTheSeenState(t *testing.T) {
	for _, seen := range []bool{false, true} {
		th := Thread{Subject: "Lunch plans", FromName: "Alice", Seen: seen}
		if got, want := th.Label(), "Alice: Lunch plans"; got != want {
			t.Errorf("seen=%v: Label() = %q, want %q", seen, got, want)
		}
	}
	// No name: the address is who it is from.
	anon := Thread{Subject: "Receipt", FromEmail: "billing@example.com"}
	if got, want := anon.Label(), "billing@example.com: Receipt"; got != want {
		t.Errorf("Label() = %q, want %q", got, want)
	}
}

func TestCollectionsAreTheSixBoxes(t *testing.T) {
	if len(Collections) != 6 {
		t.Fatalf("got %d collections", len(Collections))
	}
	boxes := map[string]bool{}
	for _, c := range Collections {
		if _, ok := LookupCollection(c.Key); !ok {
			t.Errorf("LookupCollection(%q) missed", c.Key)
		}
		boxes[c.Box] = true
	}
	for _, want := range []string{"imbox", "laterbox", "asidebox", "feedbox", "trailbox", "bubblebox"} {
		if !boxes[want] {
			t.Errorf("no collection reads box %q", want)
		}
	}
	if _, ok := LookupCollection("box:spambox"); ok {
		t.Error("LookupCollection accepted a box this plugin does not project")
	}
	if _, ok := LookupCollection(EverythingContext); ok {
		t.Error("everything is no box: nothing walks it")
	}
}

func TestSnippetIsOneBoundedLine(t *testing.T) {
	th := Thread{Summary: "Are you  free\nfriday?"}
	if got := th.Snippet(); got != "Are you free friday?" {
		t.Errorf("snippet = %q", got)
	}
	th.Summary = strings.Repeat("a", SnippetRunes+1)
	if got := []rune(th.Snippet()); len(got) != SnippetRunes+1 || got[SnippetRunes] != '…' {
		t.Errorf("a long summary was not bounded: %d runes", len(got))
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
