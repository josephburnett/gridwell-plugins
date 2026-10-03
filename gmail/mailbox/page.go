package mailbox

import (
	"html"
	"strings"
)

// SnippetRunes bounds a search hit's preview.
const SnippetRunes = 240

// Preview is Gmail's snippet on one line, bounded.
func (m *Message) Preview() string {
	s := strings.Join(strings.Fields(m.Snippet), " ")
	if r := []rune(s); len(r) > SnippetRunes {
		return string(r[:SnippetRunes]) + "…"
	}
	return s
}

// NoticeHTML is the page for a message with no body to show. It is a whole
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
