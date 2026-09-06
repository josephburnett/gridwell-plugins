package mailbox

import (
	"fmt"
	"html"
	"strings"
)

// A message has two bodies, and they are different things. ReadContent
// answers markdown — the tile's face on its grid, and the read-only document
// a text tile shows without a live surface. ServeContent answers the email
// itself, as the HTML the sender wrote, which is what descending into the
// tile opens. The markdown is a card ABOUT the email; the page is the email.

// SnippetRunes bounds the preview on the face.
const SnippetRunes = 240

// Preview is Gmail's snippet on one line, bounded.
func (m *Message) Preview() string {
	s := strings.Join(strings.Fields(m.Snippet), " ")
	if r := []rune(s); len(r) > SnippetRunes {
		return string(r[:SnippetRunes]) + "…"
	}
	return s
}

// Sender names who the message is from: the display name, with the address
// when it adds anything.
func (m *Message) Sender() string {
	name := strings.TrimSpace(m.FromName)
	addr := strings.TrimSpace(m.FromEmail)
	switch {
	case name != "" && addr != "" && !strings.EqualFold(name, addr):
		return name + " <" + addr + ">"
	case name != "":
		return name
	}
	return addr
}

// Markdown renders the message as its tile content: the subject as the
// heading, who it is from and when, its state, and the snippet. It carries no
// link: the email is what the tile opens into, and a markdown link would open
// a second, weaker copy of it.
func Markdown(v View) []byte {
	var b strings.Builder
	head := v.Title()
	if mark := marks(v); mark != "" {
		head = mark + " " + head
	}
	fmt.Fprintf(&b, "# %s\n\n", head)
	line := ""
	if s := v.Sender(); s != "" {
		line = "from " + s
	}
	if !v.Date.IsZero() {
		if line != "" {
			line += " · "
		}
		line += v.Date.UTC().Format("2006-01-02 15:04 UTC")
	}
	if line != "" {
		fmt.Fprintf(&b, "%s\n\n", line)
	}
	if s := v.Preview(); s != "" {
		fmt.Fprintf(&b, "> %s\n", s)
	}
	return []byte(b.String())
}

func marks(v View) string {
	var b strings.Builder
	if v.Unread {
		b.WriteString(UnreadMark)
	}
	if v.Starred {
		b.WriteString(StarMark)
	}
	return b.String()
}

// GoneMarkdown is the content for a key the memory does not hold: the node
// remembers the tile until a walk says the message has left every label, and
// until then this process may simply not have seen it.
func GoneMarkdown(key string) []byte {
	return []byte("_This message (`" + key + "`) is not in the inbox or starred, or has not been seen since the plugin started._\n")
}

// NoticeHTML is the page for a message whose HTML Gmail would not answer —
// one the plugin has never seen, or one with no body at all. It is a whole
// document because that is what the content door serves, and it says which
// message and why rather than showing an empty frame.
func NoticeHTML(title, detail string) []byte {
	return []byte("<!doctype html>\n<html lang=\"en\">\n<head><meta charset=\"utf-8\">\n<title>" +
		html.EscapeString(title) + "</title></head>\n<body>\n<h1>" +
		html.EscapeString(title) + "</h1>\n<p>" + html.EscapeString(detail) + "</p>\n</body>\n</html>\n")
}

// WrapPlain is the page for a message that came with no HTML part: the plain
// text, escaped, in a document that keeps its line breaks. Escaping is not
// sanitizing — the node sandboxes what it serves — it is what stops a plain
// text mail that happens to contain "<b>" from being read as markup it never
// was.
func WrapPlain(title string, text []byte) []byte {
	return []byte("<!doctype html>\n<html lang=\"en\">\n<head><meta charset=\"utf-8\">\n<title>" +
		html.EscapeString(title) + "</title></head>\n<body>\n" +
		"<pre style=\"white-space:pre-wrap;word-wrap:break-word;font:inherit\">" +
		html.EscapeString(string(text)) + "</pre>\n</body>\n</html>\n")
}
