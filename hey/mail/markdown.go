package mail

import (
	"fmt"
	"html"
	"strings"
)

// A thread has two bodies, and they are different things. ReadContent answers
// markdown — the tile's face on its grid, and the read-only document a text
// tile shows without a live surface. ServeContent answers the email itself,
// as HEY's own HTML, which is what descending into the tile opens. The
// markdown is a card ABOUT the email; the page is the email.

// SnippetRunes bounds the preview on the face.
const SnippetRunes = 240

// Snippet is HEY's preview on one line, bounded.
func (t *Thread) Snippet() string {
	s := strings.Join(strings.Fields(t.Summary), " ")
	if r := []rune(s); len(r) > SnippetRunes {
		return string(r[:SnippetRunes]) + "…"
	}
	return s
}

// Sender names who the thread is from: the display name, with the address
// when it adds anything.
func (t *Thread) Sender() string {
	name := strings.TrimSpace(t.FromName)
	addr := strings.TrimSpace(t.FromEmail)
	switch {
	case name != "" && addr != "" && !strings.EqualFold(name, addr):
		return name + " <" + addr + ">"
	case name != "":
		return name
	}
	return addr
}

// Markdown renders the thread as its tile content: the subject as the
// heading, who it is from and when, the collection it sits in, and the
// preview. It carries no link: the email is what the tile opens into, and a
// markdown link would open a second, weaker copy of it.
func Markdown(t *Thread) []byte {
	var b strings.Builder
	head := t.Title()
	if !t.Seen {
		head = UnseenMark + " " + head
	}
	fmt.Fprintf(&b, "# %s\n\n", head)
	line := ""
	if s := t.Sender(); s != "" {
		line = "from " + s
	}
	if !t.CreatedAt.IsZero() {
		if line != "" {
			line += " · "
		}
		line += t.CreatedAt.UTC().Format("2006-01-02 15:04 UTC")
	}
	if c, ok := LookupCollection(t.Collection); ok {
		if line != "" {
			line += " · "
		}
		line += c.Label
	}
	if line != "" {
		fmt.Fprintf(&b, "%s\n\n", line)
	}
	if s := t.Snippet(); s != "" {
		fmt.Fprintf(&b, "> %s\n", s)
	}
	return []byte(b.String())
}

// GoneMarkdown is the content for a key the memory does not hold: the node
// remembers the tile until a sweep says the thread has left every collection,
// and until then this process may simply not have seen it.
func GoneMarkdown(key string) []byte {
	return []byte("_This thread (`" + key + "`) is not in the imbox, reply later or set aside, or has not been seen since the plugin started._\n")
}

// NoticeHTML is the page for a thread whose HTML the CLI would not answer —
// one the plugin has never seen, or one HEY served no body for. It is a whole
// document because that is what the content door serves, and it says which
// thread and why rather than showing an empty frame.
func NoticeHTML(title, detail string) []byte {
	return []byte("<!doctype html>\n<html lang=\"en\">\n<head><meta charset=\"utf-8\">\n<title>" +
		html.EscapeString(title) + "</title></head>\n<body>\n<h1>" +
		html.EscapeString(title) + "</h1>\n<p>" + html.EscapeString(detail) + "</p>\n</body>\n</html>\n")
}
