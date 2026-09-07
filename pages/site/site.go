// Package site is the pure core of the pages plugin: the sample site, and
// every byte the plugin serves for it.
//
// Nothing here reads a file, a clock, a network or an environment. A page is
// generated from the data below by one html/template, so a key answers the
// same bytes on every machine forever. That is what lets a test assert the
// bytes, and what makes the plugin a demonstration rather than a viewer: the
// HTML does not exist anywhere until it is asked for.
package site

import (
	"bytes"
	"fmt"
	"html/template"
	"image"
	"image/color"
	"image/draw"
	"image/jpeg"
)

// RootContext is the plugin's one collection — the one context this site has.
// Keys are flat because the site is flat; a plugin with a tree of contexts
// gives its keys a path shape, as fs does.
const RootContext = "."

// PreviewStamp is the generation number of every page face. The site is
// fixed, so there is exactly one generation: the client's thumbnail cache
// keys on this and never has to fetch a face twice.
const PreviewStamp = 1

// Doc is one thing in the site: a tile the plugin lists, and everything it can
// answer about that tile. It is the one owner of a doc's facts — the label,
// the markdown body, the page and its assets all read from here, so adding a
// doc is one entry in docs and nothing else.
type Doc struct {
	// Key is stable forever. It is what the node mints an id against, so
	// changing one orphans every stored reference to the tile.
	Key   string
	Label string
	// Note is the doc's markdown body: what ReadContent answers, and what the
	// tile reads as a document. Every doc has one, page or not.
	Note string
	// Page is the doc's web half, or nil for a doc that is only text. A
	// non-nil Page is what makes the entry declare serves_page.
	Page *Page
	// Col and Row seed the tile's first placement. A hint is a suggestion for
	// a tile the node has not placed yet; the user's arrangement wins from
	// then on.
	Col, Row int64
}

// Page is a doc's web half: the HTML ServeContent answers at subpath "", and
// the page-relative resources that HTML names.
type Page struct {
	Title string
	// Accent is the page's one colour, and the reason it is here rather than
	// written into the HTML: the page's CSS and the tile's preview face are
	// two renderings of the same fact.
	Accent color.RGBA
	// Body is the authored fragment that goes inside <body>, below the
	// heading. It is template.HTML because it is written here, not taken from
	// input; anything derived from data goes through a template that escapes
	// it, as renderIndex does.
	Body template.HTML
	// Stylesheet, when set, is a page-relative subpath linked from <head>. It
	// proves the door's subpath half: the browser resolves it against
	// .../<tile-id>/ and asks the plugin for it by name.
	Stylesheet string
	// Assets are the page-relative resources by subpath. A subpath the map
	// does not hold is a 404, decided here rather than at the door.
	Assets map[string]Asset
}

// Asset is one page-relative resource: bytes and the type they are served as.
type Asset struct {
	MediaType string
	Data      []byte
}

// docs is the site. Order is the listing order, and it is a slice rather than
// a map so that order is a fact rather than a coincidence.
var docs = []*Doc{
	{
		Key:   "about",
		Label: "about",
		Col:   0, Row: 0,
		Note: aboutNote,
	},
	{
		Key:   "hello",
		Label: "hello",
		Col:   2, Row: 0,
		Note: "The smallest thing a plugin can put on the web: one page, no " +
			"resources, generated when it is asked for.",
		Page: &Page{
			Title:  "Hello, world",
			Accent: color.RGBA{R: 0x2f, G: 0x6f, B: 0x4f, A: 0xff},
			Body: `<p>These bytes did not exist a moment ago. The pages plugin
generated them to answer this request, and it will generate the same bytes
again for the same key.</p>
<p>There is nothing else here: no stylesheet, no image, no script. This page
is the whole answer.</p>`,
		},
	},
	{
		Key:   "styled",
		Label: "styled",
		Col:   4, Row: 0,
		Note: "A page with resources of its own: a stylesheet and an image, " +
			"each fetched back through the door by its subpath.",
		Page: &Page{
			Title:      "A page with resources",
			Accent:     styledAccent,
			Stylesheet: "style.css",
			Body: `<img class="mark" src="mark.svg" alt="" width="96" height="96">
<p>The stylesheet and the image above are page-relative: the browser asked for
<code>style.css</code> and <code>mark.svg</code> against this page's own
address, and the node turned each into a ServeContent call with that subpath.
The plugin generated both.</p>
<p class="card">This box is styled by the stylesheet, so if you can see its
border the subpath fetch worked.</p>`,
			Assets: map[string]Asset{
				"style.css": {MediaType: "text/css; charset=utf-8", Data: []byte(styledCSS)},
				"mark.svg":  {MediaType: "image/svg+xml", Data: markSVG(styledAccent)},
			},
		},
	},
	{
		Key:   "report",
		Label: "report",
		Col:   6, Row: 0,
		Note: "A page built from data rather than written out: the table is " +
			"this plugin's own listing, rendered at request time.",
		Page: &Page{
			Title:  "What this plugin serves",
			Accent: color.RGBA{R: 0x8f, G: 0x52, B: 0x2f, A: 0xff},
			// Body is filled by init: it is generated from docs, which does
			// not exist yet here.
		},
	},
}

// aboutNote is the doc tile that explains the plugin, kept out of the table
// above only for its length.
const aboutNote = `# pages

A plugin that serves web pages it makes up.

Every tile in this grid is a key this plugin answers for. Three of them carry
a page: descending into one asks the plugin for HTML through the node's
content door, and what comes back is the page you see. Nothing is read from
disk — the bytes are generated to answer the request.

## The door

The node serves a page at ` + "`/content/<token>/<tile-id>/`" + ` and turns
anything after that trailing slash into the subpath of a ServeContent call.
So a page can name its own resources with ordinary relative URLs and the
plugin decides, by subpath, what they are. The **styled** page does exactly
that with a stylesheet and an image.

## What a page cannot do

A page never learns its own address and cannot link to another tile: ids and
layout are node facts, and the address is derived when the page is opened.
The page runs sandboxed, with no cookie and no reach into Gridwell. A plugin
that wants two pages connected puts both behind one key, as subpaths.
`

// styledCSS is the styled page's stylesheet, served at the subpath
// "style.css".
const styledCSS = `.mark { display: block; margin: 0 auto 1.5rem; }
.card {
  border: 2px solid var(--accent);
  border-radius: 8px;
  padding: 0.75rem 1rem;
}
`

// styledAccent is the styled page's colour, named because three things read
// it: the page's CSS, the tile's preview face, and the image below.
var styledAccent = color.RGBA{R: 0x35, G: 0x52, B: 0x8f, A: 0xff}

// markSVG is the styled page's image, served at the subpath "mark.svg". It is
// generated from the accent like the page is generated from the site: an
// image a plugin makes up is as ordinary as a document it makes up.
func markSVG(accent color.RGBA) []byte {
	return fmt.Appendf(nil, `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 96 96" width="96" height="96" role="img">
  <rect width="96" height="96" rx="12" fill="%s"/>
  <rect x="16" y="16" width="28" height="28" rx="4" fill="#ffffff" opacity="0.9"/>
  <rect x="52" y="16" width="28" height="28" rx="4" fill="#ffffff" opacity="0.55"/>
  <rect x="16" y="52" width="28" height="28" rx="4" fill="#ffffff" opacity="0.55"/>
  <rect x="52" y="52" width="28" height="28" rx="4" fill="#ffffff" opacity="0.9"/>
</svg>
`, hexColor(accent))
}

// byKey indexes docs. docs stays the one owner: this is derived from it.
var byKey = map[string]*Doc{}

func init() {
	for _, d := range docs {
		byKey[d.Key] = d
	}
	// The report page is generated from the site itself, so its body is
	// filled here rather than written into the table: a page is a function of
	// data, and this one's data is the list of docs.
	byKey["report"].Page.Body = renderIndex(docs)
}

// Docs returns the site's docs in listing order. The slice is the plugin's,
// not the caller's to reorder, so callers only read it.
func Docs() []*Doc { return docs }

// Lookup returns the doc for a key, or nil. A nil answer is "this plugin has
// no such key", which every verb turns into its own polite refusal rather
// than an error.
func Lookup(key string) *Doc { return byKey[key] }

// shell is the one HTML document template. Every page goes through it, so the
// doctype, the charset, the viewport and the base styling are written once
// and no page can be half a document.
var shell = template.Must(template.New("page").Parse(`<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>{{.Title}}</title>
<style>
:root { color-scheme: light dark; --accent: {{.Accent}}; }
body {
  font: 16px/1.6 system-ui, -apple-system, "Segoe UI", sans-serif;
  margin: 0 auto;
  max-width: 38rem;
  padding: 2.5rem 1.25rem;
}
h1 { color: var(--accent); font-size: 1.6rem; margin: 0 0 1rem; }
code { font-family: ui-monospace, monospace; font-size: 0.9em; }
footer { margin-top: 2.5rem; font-size: 0.8rem; opacity: 0.6; }
table { border-collapse: collapse; width: 100%; }
th, td { border-bottom: 1px solid var(--accent); padding: 0.4rem 0.5rem; text-align: left; }
</style>
{{with .Stylesheet}}<link rel="stylesheet" href="{{.}}">
{{end}}</head>
<body>
<h1>{{.Title}}</h1>
{{.Body}}
<footer>Generated by the pages plugin for the key <code>{{.Key}}</code>.</footer>
</body>
</html>
`))

// shellData is what shell renders. Accent is template.CSS because it is built
// by hexColor from three bytes and cannot carry anything else; everything
// derived from data is escaped normally.
type shellData struct {
	Key        string
	Title      string
	Accent     template.CSS
	Stylesheet string
	Body       template.HTML
}

// HTML returns a doc's page, or nil if the doc has no page. Rendering cannot
// fail here — the template is fixed and the data is a struct of strings — so
// an execution error is a programming error, and it is returned as a page
// saying so rather than as a silent empty body.
func (d *Doc) HTML() []byte {
	if d.Page == nil {
		return nil
	}
	var buf bytes.Buffer
	err := shell.Execute(&buf, shellData{
		Key:        d.Key,
		Title:      d.Page.Title,
		Accent:     template.CSS(hexColor(d.Page.Accent)),
		Stylesheet: d.Page.Stylesheet,
		Body:       d.Page.Body,
	})
	if err != nil {
		return []byte("<!doctype html><title>pages</title><p>pages plugin: " +
			template.HTMLEscapeString(err.Error()) + "</p>")
	}
	return buf.Bytes()
}

// indexTemplate renders the report page's table from the site's docs. The
// values go through the template's escaping, which is the difference between
// data and the authored fragments above.
var indexTemplate = template.Must(template.New("index").Parse(
	`<p>Every key this plugin answers for, and what it answers with:</p>
<table>
<tr><th>key</th><th>page</th><th>resources</th></tr>
{{range .}}<tr><td><code>{{.Key}}</code></td><td>{{if .Page}}yes{{else}}no{{end}}</td><td>{{if .Page}}{{len .Page.Assets}}{{else}}0{{end}}</td></tr>
{{end}}</table>
<p>The table is rendered from the same list the grid is listed from, so it
cannot drift from what the plugin serves.</p>`))

// renderIndex is init's helper; a failure is impossible for a fixed template
// over a fixed slice, and would show as an empty table rather than a crash at
// import time.
func renderIndex(all []*Doc) template.HTML {
	var buf bytes.Buffer
	if err := indexTemplate.Execute(&buf, all); err != nil {
		return template.HTML("<p>pages plugin: " + template.HTMLEscapeString(err.Error()) + "</p>")
	}
	return template.HTML(buf.String())
}

// hexColor renders a colour as CSS hex. It is the one place the accent
// crosses from a Go value into a document.
func hexColor(c color.RGBA) string {
	return fmt.Sprintf("#%02x%02x%02x", c.R, c.G, c.B)
}

const (
	// previewW and previewH are the face's pixels: a small card, since the
	// page itself is one descent away.
	previewW = 256
	previewH = 160
	// bandH is the height of the darker strip along the top, which is all the
	// structure a face needs to read as a page rather than a colour swatch.
	bandH = 40
)

// PreviewJPEG returns a doc's face: a flat card in the page's accent with a
// darker band, or nil for a doc with no page. Nil is "no thumbnail", never an
// error — the tile falls back to its label.
func (d *Doc) PreviewJPEG() []byte {
	if d.Page == nil {
		return nil
	}
	img := image.NewRGBA(image.Rect(0, 0, previewW, previewH))
	draw.Draw(img, img.Bounds(), &image.Uniform{d.Page.Accent}, image.Point{}, draw.Src)
	band := image.Rect(0, 0, previewW, bandH)
	draw.Draw(img, band, &image.Uniform{shade(d.Page.Accent, 0.6)}, image.Point{}, draw.Src)
	var out bytes.Buffer
	if err := jpeg.Encode(&out, img, &jpeg.Options{Quality: 80}); err != nil {
		return nil
	}
	return out.Bytes()
}

// shade scales a colour toward black by f.
func shade(c color.RGBA, f float64) color.RGBA {
	return color.RGBA{
		R: uint8(float64(c.R) * f),
		G: uint8(float64(c.G) * f),
		B: uint8(float64(c.B) * f),
		A: c.A,
	}
}
