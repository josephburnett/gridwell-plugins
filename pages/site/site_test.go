package site

import (
	"bytes"
	"image/jpeg"
	"regexp"
	"strings"
	"testing"
)

// The site's one promise: the same key answers the same bytes. Everything a
// test can assert about generated content rests on this, and so does the
// node's content cache, which keys on (tile, subpath) and never revalidates.
func TestAPageIsTheSameBytesEveryTime(t *testing.T) {
	for _, d := range Docs() {
		if d.Page == nil {
			continue
		}
		first, second := d.HTML(), d.HTML()
		if !bytes.Equal(first, second) {
			t.Errorf("%s: two renders differ; a page must be a pure function of its key", d.Key)
		}
		for sub, a := range d.Page.Assets {
			if !bytes.Equal(a.Data, d.Page.Assets[sub].Data) {
				t.Errorf("%s/%s: two reads differ", d.Key, sub)
			}
		}
	}
}

// Every page is a whole document, because the node hands the bytes straight to
// a browser: there is no wrapper, no template around it at the door, and a
// fragment would render as one.
func TestEveryPageIsAWholeDocument(t *testing.T) {
	for _, d := range Docs() {
		if d.Page == nil {
			continue
		}
		html := string(d.HTML())
		for _, want := range []string{"<!doctype html>", "<title>" + d.Page.Title + "</title>", "</html>"} {
			if !strings.Contains(html, want) {
				t.Errorf("%s page is missing %q", d.Key, want)
			}
		}
		if !strings.Contains(html, "<code>"+d.Key+"</code>") {
			t.Errorf("%s page does not name the key it was generated for", d.Key)
		}
	}
}

// A doc with no page renders no HTML: nil is the answer, so ServeContent can
// tell "not a page" from "an empty page".
func TestADocWithNoPageRendersNothing(t *testing.T) {
	if got := Lookup("about").HTML(); got != nil {
		t.Errorf("about.HTML() = %q, want nil", got)
	}
	if got := Lookup("about").PreviewJPEG(); got != nil {
		t.Errorf("about.PreviewJPEG() = %d bytes, want nil", len(got))
	}
}

// relRef finds the relative URLs a page names.
var relRef = regexp.MustCompile(`(?:src|href)="([^"]+)"`)

// A page may only name resources the plugin actually serves. This is the
// class the styled page exists to demonstrate and the one a page author gets
// wrong: the browser resolves a relative URL against the tile's address and
// asks the plugin for that subpath, so a name with no asset behind it is a
// broken image the plugin answers 404 for.
func TestEveryRelativeURLIsAnAssetThePageServes(t *testing.T) {
	for _, d := range Docs() {
		if d.Page == nil {
			continue
		}
		for _, m := range relRef.FindAllStringSubmatch(string(d.HTML()), -1) {
			ref := m[1]
			if strings.Contains(ref, "://") || strings.HasPrefix(ref, "#") ||
				strings.HasPrefix(ref, "data:") || strings.HasPrefix(ref, "/") {
				continue
			}
			if _, ok := d.Page.Assets[ref]; !ok {
				t.Errorf("%s page names %q, which it serves no asset for", d.Key, ref)
			}
		}
	}
	// And the demonstration is real: the styled page names two.
	if got := len(Lookup("styled").Page.Assets); got != 2 {
		t.Errorf("styled has %d assets, want the stylesheet and the image", got)
	}
}

// The accent has one owner. It reached the CSS, the image and the face from
// the same Go value, so a page cannot drift from the tile that represents it.
func TestTheAccentHasOneOwner(t *testing.T) {
	d := Lookup("styled")
	hex := hexColor(d.Page.Accent)
	if !strings.Contains(string(d.HTML()), "--accent: "+hex) {
		t.Errorf("styled page CSS does not carry %s", hex)
	}
	if !bytes.Contains(d.Page.Assets["mark.svg"].Data, []byte(hex)) {
		t.Errorf("mark.svg does not carry %s", hex)
	}
}

// The face is a decodable JPEG of the declared size, in the page's accent.
// The tile shows it without asking what it is, so an undecodable answer is a
// blank tile with nothing said about it.
func TestTheFaceIsAJPEGInTheAccent(t *testing.T) {
	d := Lookup("hello")
	img, err := jpeg.Decode(bytes.NewReader(d.PreviewJPEG()))
	if err != nil {
		t.Fatalf("preview does not decode: %v", err)
	}
	if b := img.Bounds(); b.Dx() != previewW || b.Dy() != previewH {
		t.Errorf("preview is %v, want %dx%d", b, previewW, previewH)
	}
	// Below the band, the face is the accent. JPEG is lossy, so this is a
	// nearness check, not an equality one.
	r, g, b, _ := img.At(previewW/2, previewH-8).RGBA()
	want := d.Page.Accent
	near := func(got uint32, want uint8) bool {
		d := int(got>>8) - int(want)
		return d > -12 && d < 12
	}
	if !near(r, want.R) || !near(g, want.G) || !near(b, want.B) {
		t.Errorf("face pixel = %d,%d,%d, want near %v", r>>8, g>>8, b>>8, want)
	}
}

// Every doc has a markdown body, page or not: a page tile is still a tile, and
// what it reads as a document is this.
func TestEveryDocHasANote(t *testing.T) {
	for _, d := range Docs() {
		if strings.TrimSpace(d.Note) == "" {
			t.Errorf("%s has no note", d.Key)
		}
	}
}

// Keys are unique and the listing order is the slice's, which is what makes
// the grid's first placement reproducible.
func TestKeysAreUniqueAndIndexed(t *testing.T) {
	seen := map[string]bool{}
	for _, d := range Docs() {
		if seen[d.Key] {
			t.Errorf("duplicate key %q", d.Key)
		}
		seen[d.Key] = true
		if Lookup(d.Key) != d {
			t.Errorf("Lookup(%q) did not return the listed doc", d.Key)
		}
	}
	if Lookup("no-such-key") != nil {
		t.Error("Lookup of an unknown key must answer nil")
	}
}

// The report page is generated from the site, not written out beside it: add
// a doc and the table gains a row without anyone editing the page.
func TestTheReportIsGeneratedFromTheSite(t *testing.T) {
	html := string(Lookup("report").HTML())
	for _, d := range Docs() {
		if !strings.Contains(html, "<code>"+d.Key+"</code>") {
			t.Errorf("the report does not list %q", d.Key)
		}
	}
}
