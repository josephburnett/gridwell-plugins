package plugin

import (
	"context"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/josephburnett/gridwell-plugins/pages/site"
	pluginv1 "github.com/josephburnett/gridwell/api/gen/plugin/v1"
	"github.com/josephburnett/gridwell/api/rpc"
)

// reader collects a ReadContent stream.
type reader struct {
	pluginv1.Plugin_ReadContentServer
	chunks []*pluginv1.ContentChunk
}

func (r *reader) Send(c *pluginv1.ContentChunk) error { r.chunks = append(r.chunks, c); return nil }
func (r *reader) Context() context.Context            { return context.Background() }

// server collects a ServeContent stream.
type server struct {
	pluginv1.Plugin_ServeContentServer
	chunks []*pluginv1.ServeContentChunk
}

func (s *server) Send(c *pluginv1.ServeContentChunk) error {
	s.chunks = append(s.chunks, c)
	return nil
}
func (s *server) Context() context.Context { return context.Background() }

// serve runs one ServeContent and returns the first chunk, which is where the
// status and media type live.
func serve(t *testing.T, p *Plugin, key, subpath string) *pluginv1.ServeContentChunk {
	t.Helper()
	s := &server{}
	if err := p.ServeContent(&pluginv1.ServeContentRequest{Key: key, Subpath: subpath}, s); err != nil {
		t.Fatalf("ServeContent(%q, %q): %v", key, subpath, err)
	}
	if len(s.chunks) == 0 {
		t.Fatalf("ServeContent(%q, %q) sent nothing", key, subpath)
	}
	return s.chunks[0]
}

// A doc with a page is a URL row that serves it; a doc without one is a text
// row with its note. The address is still not the plugin's to hold: the node
// derives it at the /content/ door when the page is opened, which is what
// serves_page beside an empty url_string says.
func TestAPageDocIsAURLRowAndANoteATextRow(t *testing.T) {
	p := New()
	resp, err := p.List(context.Background(), &pluginv1.ListRequest{Context: site.RootContext})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Entries) != len(site.Docs()) || !resp.Authoritative {
		t.Fatalf("List = %d entries, authoritative %v", len(resp.Entries), resp.Authoritative)
	}
	pages := 0
	for i, e := range resp.Entries {
		d := site.Docs()[i]
		if e.Key != d.Key {
			t.Fatalf("entry %d is %q, want %q: the listing order is the site's", i, e.Key, d.Key)
		}
		if e.UrlString != "" {
			t.Errorf("%s declares a url %q; the node derives a served page's address", e.Key, e.UrlString)
		}
		if want := d.Page != nil; e.ServesPage != want {
			t.Errorf("%s serves_page = %v, want %v", e.Key, e.ServesPage, want)
		}
		if e.ServesPage {
			pages++
			if e.Kind != rpc.KindURL {
				t.Errorf("%s declares kind %q; a page is served from a url row", e.Key, e.Kind)
			}
			if e.TextPresentation != "" {
				t.Errorf("%s declares text presentation %q; a url row has no text body", e.Key, e.TextPresentation)
			}
			if e.PreviewStamp != site.PreviewStamp {
				t.Errorf("%s preview stamp = %d, want %d", e.Key, e.PreviewStamp, site.PreviewStamp)
			}
			continue
		}
		if e.Kind != rpc.KindText {
			t.Errorf("%s declares kind %q; a doc with no page is its note", e.Key, e.Kind)
		}
		if e.TextPresentation != rpc.TextPresentationBoth {
			t.Errorf("%s text presentation = %q; every note is markdown", e.Key, e.TextPresentation)
		}
		if e.PreviewStamp != 0 {
			t.Errorf("%s declares a preview stamp with no page", e.Key)
		}
	}
	if pages < 2 {
		t.Errorf("only %d page entries; the plugin exists to demonstrate pages", pages)
	}
}

// The site has one context. Any other answers an empty AUTHORITATIVE listing,
// which is the honest answer for a fixed site: there is nothing there, not
// "nothing seen this pass".
func TestAnyOtherContextIsEmptyAndAuthoritative(t *testing.T) {
	resp, err := New().List(context.Background(), &pluginv1.ListRequest{Context: "elsewhere"})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Entries) != 0 || !resp.Authoritative {
		t.Errorf("List(elsewhere) = %d entries, authoritative %v", len(resp.Entries), resp.Authoritative)
	}
}

// Subpath "" is the page; a named subpath is one of its resources. Both are
// 200s with a real media type, because the door writes its headers from the
// first chunk and a plugin that left them empty would serve a typeless body
// under nosniff.
func TestServeContentServesThePageAndItsResources(t *testing.T) {
	p := New()

	root := serve(t, p, "styled", "")
	if root.Status != 200 || !strings.HasPrefix(root.MediaType, "text/html") {
		t.Errorf("root page = %d %q", root.Status, root.MediaType)
	}
	if !strings.HasPrefix(string(root.Data), "<!doctype html>") {
		t.Errorf("root page body starts %.30q", root.Data)
	}

	css := serve(t, p, "styled", "style.css")
	if css.Status != 200 || !strings.HasPrefix(css.MediaType, "text/css") {
		t.Errorf("style.css = %d %q", css.Status, css.MediaType)
	}
	svg := serve(t, p, "styled", "mark.svg")
	if svg.Status != 200 || svg.MediaType != "image/svg+xml" {
		t.Errorf("mark.svg = %d %q", svg.Status, svg.MediaType)
	}
	if !strings.HasPrefix(string(svg.Data), "<svg") {
		t.Errorf("mark.svg body starts %.20q", svg.Data)
	}
}

// Absence is a 404 PAGE, not an RPC error: the door maps an error to 404 too,
// but only after losing the chance to say anything, and a page whose image is
// missing must still render.
func TestAbsenceIsA404Page(t *testing.T) {
	p := New()
	for _, c := range []struct{ key, subpath string }{
		{"styled", "nope.css"},    // a resource the page does not name
		{"no-such-key", ""},       // a key the site does not hold
		{"about", ""},             // a doc with no page
		{"hello", "style.css"},    // another page's resource
		{"styled", "sub/dir.css"}, /* a subpath shape the page does not use */
	} {
		if got := serve(t, p, c.key, c.subpath); got.Status != 404 {
			t.Errorf("ServeContent(%q, %q) = %d, want 404", c.key, c.subpath, got.Status)
		}
	}
}

// ReadContent answers the markdown body for every doc, page or not. An
// unknown key is an empty chunk: Probe is where absence is decided.
func TestReadContentAnswersMarkdown(t *testing.T) {
	p := New()
	r := &reader{}
	if err := p.ReadContent(&pluginv1.ReadContentRequest{Key: "about"}, r); err != nil {
		t.Fatal(err)
	}
	if len(r.chunks) != 1 || r.chunks[0].MediaType != "text/markdown" ||
		!strings.Contains(string(r.chunks[0].Data), "# pages") {
		t.Errorf("about body = %v", r.chunks)
	}

	r = &reader{}
	if err := p.ReadContent(&pluginv1.ReadContentRequest{Key: "hello"}, r); err != nil {
		t.Fatal(err)
	}
	if len(r.chunks) != 1 || len(r.chunks[0].Data) == 0 {
		t.Errorf("a page tile still has a document body, got %v", r.chunks)
	}

	r = &reader{}
	if err := p.ReadContent(&pluginv1.ReadContentRequest{Key: "gone"}, r); err != nil {
		t.Fatal(err)
	}
	if len(r.chunks) != 1 || len(r.chunks[0].Data) != 0 {
		t.Errorf("unknown key = %v, want one empty chunk", r.chunks)
	}
}

// Probe is definitive both ways. The site is a fixed list, so a key it does
// not hold is GONE — and the node retires the id it minted only on that.
func TestProbeIsDefinitiveBothWays(t *testing.T) {
	p := New()
	ctx := context.Background()
	got, _ := p.Probe(ctx, &pluginv1.ProbeRequest{Key: "hello", Context: site.RootContext})
	if got.Presence != pluginv1.ProbeResponse_PRESENCE_PRESENT {
		t.Errorf("Probe(hello) = %v", got.Presence)
	}
	got, _ = p.Probe(ctx, &pluginv1.ProbeRequest{Key: "gone", Context: site.RootContext})
	if got.Presence != pluginv1.ProbeResponse_PRESENCE_GONE {
		t.Errorf("Probe(gone) = %v", got.Presence)
	}
}

// Probe answers for the context the request names (standard rule 6): a key
// the site holds is GONE from a context the site does not list it in, and an
// empty context asks after the plugin as a whole.
func TestProbeAnswersForTheContextAsked(t *testing.T) {
	p := New()
	for _, c := range []struct {
		context string
		want    pluginv1.ProbeResponse_Presence
	}{
		{site.RootContext, pluginv1.ProbeResponse_PRESENCE_PRESENT},
		{"elsewhere", pluginv1.ProbeResponse_PRESENCE_GONE},
		{"", pluginv1.ProbeResponse_PRESENCE_PRESENT},
	} {
		got, err := p.Probe(context.Background(), &pluginv1.ProbeRequest{Key: "hello", Context: c.context})
		if err != nil || got.Presence != c.want {
			t.Errorf("Probe(hello in %q) = %v, %v; want %v", c.context, got.GetPresence(), err, c.want)
		}
	}
}

// Info declares the one collection and, unlike fs and proc, NOT host content:
// these pages are the plugin's own, not a window onto state outside Gridwell.
func TestInfoDeclaresTheCollectionAndNotHostContent(t *testing.T) {
	impl, err := FromConfig(map[string]string{"uuid": "p1", "kind": Kind, "state_dir": "/tmp/x"})
	if err != nil {
		t.Fatal(err)
	}
	info, err := impl.(*Plugin).Info(context.Background(), &pluginv1.InfoRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if info.Kind != Kind || len(info.MenuEntries) != 1 || info.MenuEntries[0].Context != site.RootContext {
		t.Errorf("info = %v", info)
	}
	if info.HostContent {
		t.Error("host_content is true; the pages are the plugin's own content")
	}
	// Standard rule 1: the plugin implements no Watch, so it declares none.
	if info.Watch {
		t.Error("watch is declared; this plugin implements no Watch")
	}
	if err := impl.Watch(&pluginv1.WatchRequest{}, nil); status.Code(err) != codes.Unimplemented {
		t.Errorf("Watch = %v; a plugin that declares no watch must not implement one", err)
	}
}

// Every entry's label is its doc's and no entry carries state (standard rule
// 11): the site never changes, so there is nothing worth noticing. Every hint
// is the doc's own cell (rule 10), so it cannot move with the listing order.
func TestEntriesAreQuietAndHintedByTheirDoc(t *testing.T) {
	resp, err := New().List(context.Background(), &pluginv1.ListRequest{Context: site.RootContext})
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range resp.Entries {
		d := site.Lookup(e.Key)
		if d == nil {
			t.Fatalf("listed %q, which the site does not hold", e.Key)
		}
		if e.Label != d.Label {
			t.Errorf("%s label = %q, want the doc's %q", e.Key, e.Label, d.Label)
		}
		if e.StatusDetail != "" {
			t.Errorf("%s carries status_detail %q; the site has no state", e.Key, e.StatusDetail)
		}
		h := e.PlacementHint
		if h == nil || h.X != d.Col || h.Y != d.Row || h.W != 2 || h.H != 2 {
			t.Errorf("%s hint = %v, want the doc's cell (%d,%d) at 2x2", e.Key, h, d.Col, d.Row)
		}
	}
}

// Delete refuses with a reason rather than succeeding silently: a delete that
// left the tile in place would look like a delete that failed to stick.
func TestDeleteIsRefusedWithAReason(t *testing.T) {
	_, err := New().Delete(context.Background(), &pluginv1.DeleteRequest{Key: "hello"})
	if status.Code(err) != codes.Unimplemented || !strings.Contains(err.Error(), "fixed") {
		t.Errorf("Delete = %v, want Unimplemented with a reason", err)
	}
}

// GetPreview answers a page's face and nothing for a doc without one. nil is
// "no thumbnail", never an error: the tile falls back to its label.
func TestGetPreviewAnswersOnlyForPages(t *testing.T) {
	p := New()
	ctx := context.Background()
	got, err := p.GetPreview(ctx, &pluginv1.GetPreviewRequest{Key: "hello"})
	if err != nil || len(got.Jpeg) == 0 {
		t.Errorf("GetPreview(hello) = %d bytes, %v", len(got.GetJpeg()), err)
	}
	got, err = p.GetPreview(ctx, &pluginv1.GetPreviewRequest{Key: "about"})
	if err != nil || len(got.Jpeg) != 0 {
		t.Errorf("GetPreview(about) = %d bytes, %v; want none", len(got.GetJpeg()), err)
	}
	got, err = p.GetPreview(ctx, &pluginv1.GetPreviewRequest{Key: "gone"})
	if err != nil || len(got.Jpeg) != 0 {
		t.Errorf("GetPreview(gone) = %d bytes, %v; want none", len(got.GetJpeg()), err)
	}
}
