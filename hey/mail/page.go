package mail

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

// AppURL is the thread's address in HEY's own web app.
func AppURL(topicID int64) string {
	return "https://app.hey.com/topics/" + strconv.FormatInt(topicID, 10)
}

// Page is the document ServeContent answers. `hey thread read --html` is
// HEY's message markup without HEY's stylesheet or scripts, and its figure
// blocks keep their content in a JSON attribute behind a shadow root that
// script would attach, so served as is it reads as plain text with holes.
// Page gives it a stylesheet of its own, a heading that links the thread in
// HEY, a header line on every entry, and every figure as visible HTML. HEY's
// articles and their order are kept. A document that does not parse is
// served as it came.
func Page(doc []byte, topicID int64) []byte {
	root, err := html.Parse(bytes.NewReader(doc))
	if err != nil {
		return doc
	}
	for _, f := range all(root, func(n *html.Node) bool { return n.DataAtom == atom.Figure && hasAttr(n, "data-trix-attachment") }) {
		unfigure(f)
	}
	for _, n := range all(root, func(n *html.Node) bool { return n.DataAtom == atom.Template || n.Data == "shadow-content" }) {
		unwrap(n)
	}
	for _, n := range all(root, func(n *html.Node) bool { return n.Data == "action-text-attachment" }) {
		attachment(n)
	}
	for _, a := range all(root, func(n *html.Node) bool { return n.DataAtom == atom.Article }) {
		entryHeader(a)
	}
	head, body := find(root, atom.Head), find(root, atom.Body)
	if head == nil || body == nil {
		return doc
	}
	head.AppendChild(elem(atom.Meta, "name", "viewport", "content", "width=device-width, initial-scale=1"))
	style := elem(atom.Style)
	style.AppendChild(&html.Node{Type: html.TextNode, Data: stylesheet})
	head.AppendChild(style)
	body.InsertBefore(threadHeader(textOf(find(head, atom.Title)), topicID), body.FirstChild)

	var out bytes.Buffer
	if err := html.Render(&out, root); err != nil {
		return doc
	}
	return out.Bytes()
}

// trix is a figure's data-trix-attachment payload: either HTML content (a
// quote, or a whole HTML message) or one file HEY holds.
type trix struct {
	ContentType string `json:"contentType"`
	Content     string `json:"content"`
	Filename    string `json:"filename"`
	Filesize    int64  `json:"filesize"`
	URL         string `json:"url"`
}

func unfigure(f *html.Node) {
	var t trix
	if json.Unmarshal([]byte(attr(f, "data-trix-attachment")), &t) != nil {
		unwrap(f)
		return
	}
	switch {
	case t.Content != "":
		nodes, err := html.ParseFragment(strings.NewReader(t.Content), &html.Node{Type: html.ElementNode, Data: "div", DataAtom: atom.Div})
		if err != nil {
			unwrap(f)
			return
		}
		var box *html.Node
		switch {
		case t.ContentType == "text/html" && quoted(f, nodes):
			box = elem(atom.Details, "class", "hey-quote")
			summary := elem(atom.Summary)
			summary.AppendChild(text("Quoted text"))
			box.AppendChild(summary)
		case t.ContentType == "text/html":
			box = elem(atom.Div, "class", "hey-html")
		default:
			box = elem(atom.Div)
		}
		for _, n := range nodes {
			box.AppendChild(n)
		}
		f.Parent.InsertBefore(box, f)
		f.Parent.RemoveChild(f)
		if box.DataAtom == atom.Div && box.Attr == nil {
			unwrap(box)
		}
	case t.Filename != "" || t.URL != "":
		list := attachmentList(f.Parent)
		f.Parent.RemoveChild(f)
		list.AppendChild(fileItem(t))
	default:
		unwrap(f)
	}
}

// quoted says a figure is quoted text: it sits in a sender's quote container,
// or its content opens with a blockquote. A figure already inside a details
// is collapsible as it stands.
func quoted(f *html.Node, content []*html.Node) bool {
	for p := f.Parent; p != nil; p = p.Parent {
		if p.DataAtom == atom.Details {
			return false
		}
		if p.DataAtom == atom.Blockquote || strings.Contains(attr(p, "class"), "quote") {
			return true
		}
	}
	for _, n := range content {
		for n != nil && (n.DataAtom == atom.Template || n.Data == "shadow-content") {
			n = firstElement(n.FirstChild)
		}
		if n != nil && n.Type == html.ElementNode {
			return n.DataAtom == atom.Blockquote
		}
	}
	return false
}

func firstElement(n *html.Node) *html.Node {
	for ; n != nil; n = n.NextSibling {
		if n.Type == html.ElementNode {
			return n
		}
	}
	return nil
}

// attachmentList is the labelled list at the end of the figure's entry, made
// on first use; outside any entry it ends the body.
func attachmentList(from *html.Node) *html.Node {
	owner := from
	for ; owner.Parent != nil && owner.DataAtom != atom.Article && owner.DataAtom != atom.Body; owner = owner.Parent {
	}
	if last := owner.LastChild; last != nil && attr(last, "class") == "hey-attachments" {
		return last.LastChild
	}
	section := elem(atom.Section, "class", "hey-attachments", "aria-label", "Attachments")
	h := elem(atom.H2)
	h.AppendChild(text("Attachments"))
	section.AppendChild(h)
	ul := elem(atom.Ul)
	section.AppendChild(ul)
	owner.AppendChild(section)
	return ul
}

func fileItem(t trix) *html.Node {
	li := elem(atom.Li)
	name := t.Filename
	if name == "" {
		name = t.ContentType
	}
	li.AppendChild(text(name + " "))
	meta := []string{kindLabel(t.ContentType)}
	if t.Filesize > 0 {
		meta = append(meta, size(t.Filesize))
	}
	image := strings.HasPrefix(t.ContentType, "image")
	if image && !absolute(t.URL) {
		meta = append(meta, "not shown: HEY serves it only to its own app")
	}
	span := elem(atom.Span, "class", "hey-meta")
	span.AppendChild(text(strings.Join(meta, " · ")))
	li.AppendChild(span)
	if image && absolute(t.URL) {
		li.AppendChild(elem(atom.Img, "src", t.URL, "alt", name))
	}
	return li
}

func kindLabel(contentType string) string {
	switch {
	case contentType == "application/pdf":
		return "PDF"
	case contentType == "text/calendar":
		return "calendar invite"
	case strings.HasPrefix(contentType, "image"):
		return "image"
	case contentType == "":
		return "file"
	}
	return contentType
}

func size(n int64) string {
	switch {
	case n < 1024:
		return fmt.Sprintf("%d B", n)
	case n < 1024*1024:
		return fmt.Sprintf("%.0f KB", float64(n)/1024)
	}
	return fmt.Sprintf("%.1f MB", float64(n)/(1024*1024))
}

// attachment turns HEY's <action-text-attachment>, which only HEY's script
// draws, into what it stands for: an image, a note that the image cannot load
// here, or the file's name.
func attachment(n *html.Node) {
	ct, url := attr(n, "content-type"), attr(n, "url")
	var repl *html.Node
	switch {
	case strings.HasPrefix(ct, "image") && absolute(url):
		repl = elem(atom.Img, "src", url, "alt", attr(n, "caption"))
	case strings.HasPrefix(ct, "image"):
		repl = elem(atom.Span, "class", "hey-missing")
		repl.AppendChild(text("Image not shown: its address resolves only inside HEY."))
	case attr(n, "filename") != "":
		repl = elem(atom.Span, "class", "hey-meta")
		repl.AppendChild(text("Attachment: " + attr(n, "filename")))
	default:
		unwrap(n)
		return
	}
	n.Parent.InsertBefore(repl, n)
	n.Parent.RemoveChild(n)
}

// entryHeader leaves HEY's own header line, and gives an entry without one a
// line from its created-at.
func entryHeader(a *html.Node) {
	for c := a.FirstChild; c != nil; c = c.NextSibling {
		if c.DataAtom == atom.Header {
			return
		}
	}
	at := attr(a, "data-created-at")
	if at == "" {
		return
	}
	h := elem(atom.Header)
	h.AppendChild(text(when(at)))
	a.InsertBefore(h, a.FirstChild)
}

func when(at string) string {
	if t, err := time.Parse(time.RFC3339, at); err == nil {
		return t.UTC().Format("2006-01-02 15:04 UTC")
	}
	for _, layout := range []string{"2006-01-02T15:04:05", "2006-01-02T15:04"} {
		if t, err := time.Parse(layout, at); err == nil {
			return t.Format("2006-01-02 15:04")
		}
	}
	return at
}

func threadHeader(title string, topicID int64) *html.Node {
	h := elem(atom.Header, "class", "hey-thread")
	if title != "" {
		h1 := elem(atom.H1)
		h1.AppendChild(text(title))
		h.AppendChild(h1)
	}
	a := elem(atom.A, "href", AppURL(topicID), "target", "_blank")
	a.AppendChild(text("Open in HEY"))
	h.AppendChild(a)
	return h
}

func absolute(url string) bool {
	return strings.HasPrefix(url, "https://") || strings.HasPrefix(url, "http://")
}

func all(root *html.Node, match func(*html.Node) bool) []*html.Node {
	var out []*html.Node
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode && match(n) {
			out = append(out, n)
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(root)
	return out
}

func find(root *html.Node, a atom.Atom) *html.Node {
	if m := all(root, func(n *html.Node) bool { return n.DataAtom == a }); len(m) > 0 {
		return m[0]
	}
	return nil
}

func unwrap(n *html.Node) {
	for c := n.FirstChild; c != nil; {
		next := c.NextSibling
		n.RemoveChild(c)
		n.Parent.InsertBefore(c, n)
		c = next
	}
	n.Parent.RemoveChild(n)
}

func elem(a atom.Atom, kv ...string) *html.Node {
	n := &html.Node{Type: html.ElementNode, Data: a.String(), DataAtom: a}
	for i := 0; i+1 < len(kv); i += 2 {
		n.Attr = append(n.Attr, html.Attribute{Key: kv[i], Val: kv[i+1]})
	}
	return n
}

func text(s string) *html.Node { return &html.Node{Type: html.TextNode, Data: s} }

func attr(n *html.Node, key string) string {
	for _, a := range n.Attr {
		if a.Key == key {
			return a.Val
		}
	}
	return ""
}

func hasAttr(n *html.Node, key string) bool {
	for _, a := range n.Attr {
		if a.Key == key {
			return true
		}
	}
	return false
}

func textOf(n *html.Node) string {
	if n == nil {
		return ""
	}
	var b strings.Builder
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		if c.Type == html.TextNode {
			b.WriteString(c.Data)
		}
	}
	return strings.TrimSpace(b.String())
}

// stylesheet loads nothing: every rule is here, so the page renders the same
// under any content policy. An HTML message (.hey-html) keeps a light panel
// in the dark scheme, because its inline colors assume a white page.
const stylesheet = `
:root { color-scheme: light dark; --fg: #1d1d1f; --bg: #fdfdfb; --muted: #6b6b70; --rule: #dcdcd6; --link: #0b62c4; --quote: #f2f1ec; }
@media (prefers-color-scheme: dark) {
  :root { --fg: #e6e6e3; --bg: #1b1b1d; --muted: #a0a0a6; --rule: #3a3a3e; --link: #7ab4ff; --quote: #252528; }
}
html { background: var(--bg); color: var(--fg); }
body { margin: 0 auto; padding: 1.5rem 1rem 3rem; max-width: 42rem; font: 17px/1.6 -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, "Helvetica Neue", Arial, sans-serif; overflow-wrap: anywhere; }
a { color: var(--link); }
header.hey-thread { display: flex; flex-wrap: wrap; align-items: baseline; justify-content: space-between; gap: .5rem 1rem; margin-bottom: 1.5rem; }
header.hey-thread h1 { margin: 0; font-size: 1.5rem; line-height: 1.3; }
header.hey-thread a { font-size: .9rem; white-space: nowrap; }
article { padding: 1.25rem 0; border-top: 1px solid var(--rule); }
article > header { margin-bottom: .75rem; color: var(--muted); font-size: .9rem; }
p { margin: 0 0 1em; }
img { max-width: 100%; height: auto; }
blockquote { margin: 1em 0; padding: .25em 1em; border-left: 3px solid var(--rule); color: var(--muted); }
details.hey-quote { margin: 1em 0; padding: .5em .75em; border-radius: 6px; background: var(--quote); }
details.hey-quote > summary { cursor: pointer; color: var(--muted); font-size: .9rem; }
details.hey-quote[open] > summary { margin-bottom: .5em; }
pre, code { font: .9em/1.5 ui-monospace, SFMono-Regular, Menlo, Consolas, monospace; }
pre { padding: .75em; overflow-x: auto; background: var(--quote); border-radius: 6px; white-space: pre-wrap; }
table { border-collapse: collapse; max-width: 100%; }
td, th { vertical-align: top; }
.hey-html { margin: 1em 0; padding: 1em; border-radius: 6px; background: #fff; color: #1d1d1f; color-scheme: light; overflow-x: auto; }
.hey-html a { color: #0b62c4; }
.hey-missing, .hey-meta { color: var(--muted); font-size: .85rem; font-style: italic; }
section.hey-attachments h2 { margin: 1em 0 .25em; font-size: .9rem; color: var(--muted); text-transform: uppercase; letter-spacing: .05em; }
section.hey-attachments ul { margin: 0; padding-left: 1.25em; }
section.hey-attachments img { display: block; margin: .5em 0; }
`
