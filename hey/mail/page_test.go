package mail

import (
	"encoding/json"
	"html"
	"regexp"
	"strings"
	"testing"
)

// Every fixture here is synthetic markup in the shape `hey thread read --html`
// answers; none of it is anyone's mail.

// figure is a HEY figure block: its whole payload is a JSON attribute.
func figure(t *testing.T, payload map[string]any) string {
	t.Helper()
	b, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	return `<figure data-trix-attachment="` + html.EscapeString(string(b)) + `"></figure>`
}

func shadow(inner string) string {
	return "<shadow-content><template>" + inner + "</template></shadow-content>"
}

func doc(articles ...string) string {
	return "<!doctype html>\n<html lang=\"en\"><head><meta charset=\"utf-8\"><title>Picnic plans</title></head><body>" +
		strings.Join(articles, "") + "</body></html>"
}

func article(id, at, body string) string {
	return `<article id="entry-` + id + `" data-entry-id="` + id + `" data-created-at="` + at + `" data-body-state="hydrated">` + body + `</article>`
}

func page(t *testing.T, d string) string {
	t.Helper()
	return string(Page([]byte(d), 42))
}

func TestPageInjectsItsOwnStylesheetAndViewport(t *testing.T) {
	got := page(t, doc(article("1", "2026-01-05T14:03", "<header>From: Wren — 2026-01-05T14:03</header><div>bring plums</div>")))
	if n := strings.Count(got, "<style>"); n != 1 {
		t.Errorf("want one <style>, got %d:\n%s", n, got)
	}
	if !strings.Contains(got, `<meta name="viewport" content="width=device-width, initial-scale=1"/>`) {
		t.Errorf("no viewport meta:\n%s", got)
	}
	if !strings.Contains(got, "prefers-color-scheme: dark") {
		t.Error("stylesheet has no dark scheme")
	}
	for _, kept := range []string{`<article id="entry-1" data-entry-id="1"`, "bring plums", "<title>Picnic plans</title>"} {
		if !strings.Contains(got, kept) {
			t.Errorf("page lost %q:\n%s", kept, got)
		}
	}
}

// The page names the thread and carries the one way back to HEY's own app,
// opened as a new window so Gridwell opens it below and the page stays put.
func TestPageHeadsTheThreadWithItsTitleAndAnOpenInHEYLink(t *testing.T) {
	got := page(t, doc(article("1", "2026-01-05T14:03", "<div>x</div>")))
	if !strings.Contains(got, "<h1>Picnic plans</h1>") {
		t.Errorf("no title heading:\n%s", got)
	}
	if !strings.Contains(got, `<a href="https://app.hey.com/topics/42" target="_blank">Open in HEY</a>`) {
		t.Errorf("no target=_blank Open in HEY link:\n%s", got)
	}
}

func TestAppURLIsTheTopicsAddress(t *testing.T) {
	if got := AppURL(103); got != "https://app.hey.com/topics/103" {
		t.Errorf("AppURL = %q", got)
	}
}

// HEY's own header line stays; an entry without one gets one from its
// created-at, so every entry in the thread is introduced, in HEY's order.
func TestEveryEntryHasAHeaderLineOldestFirst(t *testing.T) {
	got := page(t, doc(
		article("1", "2026-01-05T14:03", "<header>From: Wren — 2026-01-05T14:03</header><div>first</div>"),
		article("2", "2026-01-06T09:30:00Z", "<div>second</div>"),
	))
	i := strings.Index(got, "From: Wren — 2026-01-05T14:03")
	j := strings.Index(got, "<header>2026-01-06 09:30 UTC</header>")
	if i < 0 || j < 0 || i > j {
		t.Errorf("headers missing or out of order (%d, %d):\n%s", i, j, got)
	}
	if strings.Index(got, "first") > strings.Index(got, "second") {
		t.Error("entries reordered")
	}
}

// A quoted figure — inside a quote container, or a blockquote at its root —
// is collapsed behind a disclosure that needs no script.
func TestQuotedFigureBecomesAClosedDetails(t *testing.T) {
	fig := func(inner string) string {
		return figure(t, map[string]any{"contentType": "text/html", "data": "{}", "content": shadow(inner)})
	}
	for name, c := range map[string]struct{ body, shown string }{
		"in a quote container": {
			`<div class="gmail_quote gmail_quote_container"><div class="gmail_attr">On Monday, Wren wrote:</div>` + fig(`<div>earlier words about kites</div>`) + `</div>`,
			`<div>earlier words about kites</div>`,
		},
		"blockquote root": {
			`<div>` + fig(`<blockquote class="gmail_quote"><div>earlier words about kites</div></blockquote>`) + `</div>`,
			`<blockquote class="gmail_quote"><div>earlier words about kites</div></blockquote>`,
		},
	} {
		got := page(t, doc(article("1", "2026-01-05T14:03", c.body)))
		if !strings.Contains(got, `<details class="hey-quote"><summary>Quoted text</summary>`+c.shown+`</details>`) {
			t.Errorf("%s: quote not collapsed:\n%s", name, got)
		}
		if strings.Contains(got, "<figure data-trix") || strings.Contains(got, "<template") || strings.Contains(got, "shadow-content") {
			t.Errorf("%s: figure wrapper survived:\n%s", name, got)
		}
		if strings.Contains(got, "<details open") {
			t.Errorf("%s: quote is open by default", name)
		}
	}
}

// An HTML body that is not a quote — a newsletter's whole message — is the
// email: it is shown, in a light panel of its own, because its inline colors
// assume one.
func TestHTMLBodyFigureIsShownInPlace(t *testing.T) {
	body := figure(t, map[string]any{
		"contentType": "text/html", "data": "{}",
		"content": shadow(`<div class="__body-layout"><table><tbody><tr><td>gazette of the week</td></tr></tbody></table></div>`),
	})
	got := page(t, doc(article("1", "2026-01-05T14:03", `<div>`+body+`</div>`)))
	if !strings.Contains(got, `<div class="hey-html"><div class="__body-layout"><table><tbody><tr><td>gazette of the week</td></tr></tbody></table></div></div>`) {
		t.Errorf("body not unwrapped in place:\n%s", got)
	}
	if strings.Contains(got, "<details") {
		t.Errorf("a body is not a quote:\n%s", got)
	}
}

// Inside an HTML figure, an image attachment with an absolute address is an
// image; one whose address cannot resolve here says so.
func TestImagesInsideHTMLFigures(t *testing.T) {
	body := figure(t, map[string]any{
		"contentType": "text/html", "data": "{}",
		"content": shadow(`<div><action-text-attachment content-type="image" url="https://img.example/kite.png" caption="A red kite" previewable="true"></action-text-attachment>` +
			`<action-text-attachment content-type="image" url="cid:part1" previewable="true"></action-text-attachment></div>`),
	})
	got := page(t, doc(article("1", "2026-01-05T14:03", body)))
	if !strings.Contains(got, `<img src="https://img.example/kite.png" alt="A red kite"/>`) {
		t.Errorf("absolute image not shown:\n%s", got)
	}
	if !strings.Contains(got, `<span class="hey-missing">Image not shown: its address resolves only inside HEY.</span>`) {
		t.Errorf("unresolvable image not captioned:\n%s", got)
	}
	if strings.Contains(got, "action-text-attachment") {
		t.Errorf("attachment element survived:\n%s", got)
	}
}

// File figures — a PDF, an invite, an image HEY keeps behind its own app —
// leave the body and become one labelled list at the end of their entry.
func TestFileFiguresBecomeALabelledList(t *testing.T) {
	pdf := figure(t, map[string]any{"contentType": "application/pdf", "data": "{}", "filename": "route-map.pdf", "filesize": 20480, "sgid": "s", "url": "/attachments/1"})
	ics := figure(t, map[string]any{"contentType": "text/calendar", "data": "{}", "filename": "picnic.ics", "filesize": 900, "sgid": "s", "url": "/attachments/2"})
	png := figure(t, map[string]any{"contentType": "image/png", "data": "{}", "filename": "meadow.png", "filesize": 3000000, "previewable": true, "sgid": "s", "url": "/attachments/3"})
	got := page(t, doc(article("1", "2026-01-05T14:03", `<div>see attached`+pdf+`</div>`+ics+png)))
	re := regexp.MustCompile(`<section class="hey-attachments" aria-label="Attachments"><h2>Attachments</h2><ul>` +
		`<li>route-map\.pdf <span class="hey-meta">PDF · 20 KB</span></li>` +
		`<li>picnic\.ics <span class="hey-meta">calendar invite · 900 B</span></li>` +
		`<li>meadow\.png <span class="hey-meta">image · 2.9 MB · not shown: HEY serves it only to its own app</span></li>` +
		`</ul></section></article>`)
	if !re.MatchString(got) {
		t.Errorf("attachments not listed at the entry's end:\n%s", got)
	}
	if strings.Contains(got, "<figure") {
		t.Errorf("figure survived:\n%s", got)
	}
}

// An absolute image file figure is shown inline in the list.
func TestAbsoluteImageFileFigureIsShown(t *testing.T) {
	png := figure(t, map[string]any{"contentType": "image/png", "filename": "dune.png", "url": "https://img.example/dune.png"})
	got := page(t, doc(article("1", "2026-01-05T14:03", png)))
	if !strings.Contains(got, `<li>dune.png <span class="hey-meta">image</span><img src="https://img.example/dune.png" alt="dune.png"/></li>`) {
		t.Errorf("absolute image file not shown:\n%s", got)
	}
}

// A figure kind this page does not know is unwrapped to its content, never
// hidden.
func TestUnknownFigureIsUnwrapped(t *testing.T) {
	odd := figure(t, map[string]any{"contentType": "application/x-novel", "content": shadow(`<p>a lantern</p>`)})
	plain := `<figure class="x"><p>a plain figure</p></figure>`
	broken := `<figure data-trix-attachment="{not json"><p>a broken figure</p></figure>`
	got := page(t, doc(article("1", "2026-01-05T14:03", odd+plain+broken)))
	for _, want := range []string{"<p>a lantern</p>", "<p>a plain figure</p>", "<p>a broken figure</p>"} {
		if !strings.Contains(got, want) {
			t.Errorf("lost %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "<template") {
		t.Errorf("a template survived, and a template renders nothing:\n%s", got)
	}
}

// A declarative shadow root outside any figure is unwrapped too: the page has
// no stylesheet of HEY's for the shadow to scope.
func TestLiteralShadowTemplatesAreUnwrapped(t *testing.T) {
	got := page(t, doc(article("1", "2026-01-05T14:03", `<div><template shadowrootmode="open"><p>tucked away</p></template></div>`)))
	if !strings.Contains(got, "<div><p>tucked away</p></div>") {
		t.Errorf("shadow template not unwrapped:\n%s", got)
	}
}

// A document with no figures and no templates keeps its body as it was.
func TestPlainDocumentPassesThrough(t *testing.T) {
	body := `<header>From: Wren — 2026-01-05T14:03</header><div dir="auto">see you <b>at noon</b><br/></div><details class="hey-quote"><summary>…</summary><div>old</div></details>`
	got := page(t, doc(article("1", "2026-01-05T14:03", body)))
	if !strings.Contains(got, body) {
		t.Errorf("body changed:\n%s", got)
	}
}

// The page introduces no address but the thread's own in HEY: the stylesheet
// loads nothing, so it renders under any policy the door sets.
func TestPageIntroducesNoExternalURL(t *testing.T) {
	in := doc(article("1", "2026-01-05T14:03", `<header>From: Wren — 2026-01-05T14:03</header><a href="https://kites.example/">kites</a>`+
		figure(t, map[string]any{"contentType": "text/html", "content": shadow(`<img src="https://img.example/a.png">`)})))
	got := page(t, in)
	urls := regexp.MustCompile(`(?i)(https?:)?//[^\s"'<>)&\\]+|url\(`)
	have := map[string]bool{}
	for _, u := range urls.FindAllString(in, -1) {
		have[u] = true
	}
	for _, u := range urls.FindAllString(got, -1) {
		if !have[u] && u != AppURL(42) {
			t.Errorf("page introduced %q", u)
		}
	}
	if strings.Contains(got, "@import") {
		t.Error("stylesheet imports")
	}
}

// The notice for an empty answer is served through the same page, so it reads
// like the rest.
func TestNoticeGetsTheSameStylesheet(t *testing.T) {
	got := string(Page(NoticeHTML("Picnic plans", "HEY served no body for this thread."), 42))
	if strings.Count(got, "<style>") != 1 || strings.Count(got, "<h1>") != 1 || !strings.Contains(got, "HEY served no body") {
		t.Errorf("notice page:\n%s", got)
	}
}
