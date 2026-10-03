package mail

import (
	"html"
	"strings"
)

// SnippetRunes bounds the preview a search result carries.
const SnippetRunes = 240

// Snippet is HEY's preview on one line, bounded.
func (t *Thread) Snippet() string {
	s := strings.Join(strings.Fields(t.Summary), " ")
	if r := []rune(s); len(r) > SnippetRunes {
		return string(r[:SnippetRunes]) + "…"
	}
	return s
}

// NoticeHTML is the document for a thread HEY served no body for. It says
// which thread and why rather than showing an empty frame; Page heads it
// with its title like any thread.
func NoticeHTML(title, detail string) []byte {
	return []byte("<!doctype html>\n<html lang=\"en\">\n<head><meta charset=\"utf-8\">\n<title>" +
		html.EscapeString(title) + "</title></head>\n<body>\n<p>" + html.EscapeString(detail) + "</p>\n</body>\n</html>\n")
}
