package plugin

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pluginv1 "github.com/josephburnett/gridwell/api/gen/plugin/v1"
)

// symlinkTree is a root beside a directory outside it:
//
//	root/real.md, root/dir/inner.txt, root/page.html
//	root/file -> real.md        root/dirlink -> dir      root/cycle -> .
//	root/broken -> missing.md   root/out -> ../outside/secret.txt
//	root/outdir -> ../outside   root/dir/leak.css -> ../../outside/secret.txt
func symlinkTree(t *testing.T) (root, outside string) {
	t.Helper()
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	root, outside = filepath.Join(base, "root"), filepath.Join(base, "outside")
	mkdirs(t, base, "root/dir", "outside")
	write(t, filepath.Join(root, "real.md"), "# real")
	write(t, filepath.Join(root, "dir", "inner.txt"), "inner")
	write(t, filepath.Join(root, "page.html"), "<p>page</p>")
	write(t, filepath.Join(outside, "secret.txt"), "secret")
	for name, dest := range map[string]string{
		"file": "real.md", "dirlink": "dir", "cycle": ".", "broken": "missing.md",
		"out": "../outside/secret.txt", "outdir": "../outside", "dir/leak.css": "../../outside/secret.txt",
	} {
		if err := os.Symlink(dest, filepath.Join(root, filepath.FromSlash(name))); err != nil {
			t.Skip("symlinks unsupported")
		}
	}
	return root, outside
}

func listed(t *testing.T, p *Plugin, ctxKey string) (*pluginv1.ListResponse, map[string]*pluginv1.Entry) {
	t.Helper()
	resp, err := p.List(context.Background(), &pluginv1.ListRequest{Context: ctxKey})
	if err != nil {
		t.Fatalf("List(%q): %v", ctxKey, err)
	}
	byKey := map[string]*pluginv1.Entry{}
	for _, e := range resp.Entries {
		byKey[e.Key] = e
	}
	return resp, byKey
}

// A symlink is a link to the thing it lands on, so every real file and
// directory has one key: a linked file is an entry whose link_target is the
// file's own key, and a linked directory a well onto the directory's own
// context, so a link back up the tree is one grid, not an endless descent.
// A link out of the root, or to nothing, targets a key this plugin never
// lists, which the node reads as dead.
func TestSymlinksListAsLinksToTheOneKey(t *testing.T) {
	root, _ := symlinkTree(t)
	p := New(root, nil)
	_, top := listed(t, p, ".")

	ref := func(key string) *pluginv1.EntryRef {
		if e := top[key]; e != nil {
			return e.LinkTarget
		}
		return nil
	}
	for key, want := range map[string]*pluginv1.EntryRef{
		"file":   {Context: ".", Key: "real.md"},
		"broken": {Context: ".", Key: "missing.md"},
		"out":    {Context: "../outside", Key: "../outside/secret.txt"},
		"outdir": {Context: "..", Key: "../outside"},
	} {
		got := ref(key)
		if got.GetContext() != want.Context || got.GetKey() != want.Key {
			t.Errorf("%s links to %v, want %v", key, got, want)
		}
		if top[key].GetKind() == "well" {
			t.Errorf("%s is a well; a link that is not a reachable directory is a link entry", key)
		}
	}
	if top["file"].GetKind() != "text" || top["file"].GetLabel() != "file" {
		t.Errorf("file = %v, want a text link labelled with its own name", top["file"])
	}
	for key, child := range map[string]string{"dirlink": "dir", "cycle": "."} {
		if e := top[key]; e.GetKind() != "well" || e.GetChildContext() != child || e.GetLinkTarget() != nil {
			t.Errorf("%s = %v, want a well onto %q", key, e, child)
		}
	}
	if e := top["real.md"]; e.GetLinkTarget() != nil {
		t.Errorf("real.md = %v, want the file itself", e)
	}

	// The targets the dead links name are never listed and probe gone.
	for _, ctxKey := range []string{"../outside", "..", "dirlink", "cycle", "file"} {
		resp, byKey := listed(t, p, ctxKey)
		if !resp.Authoritative || len(byKey) != 0 {
			t.Errorf("List(%q) = %v, want an authoritative empty listing", ctxKey, resp)
		}
	}
	for _, k := range [][2]string{{".", "missing.md"}, {"../outside", "../outside/secret.txt"}, {"dirlink", "dirlink/inner.txt"}} {
		pr, err := p.Probe(context.Background(), &pluginv1.ProbeRequest{Context: k[0], Key: k[1]})
		if err != nil || pr.Presence != pluginv1.ProbeResponse_PRESENCE_GONE {
			t.Errorf("Probe(%s) = %v, %v; want GONE", k[1], pr, err)
		}
	}
}

type pageStream struct {
	grpc.ServerStream
	chunks []*pluginv1.ServeContentChunk
}

func (s *pageStream) Send(c *pluginv1.ServeContentChunk) error {
	s.chunks = append(s.chunks, c)
	return nil
}

// Confinement sees through links: a name under the root that links out of it
// serves nothing, at every content verb, with the reason; a page's resource
// that links out of its directory is a 404 page; a link inside the root
// serves what it lands on.
func TestALinkOutOfTheRootServesNothing(t *testing.T) {
	root, _ := symlinkTree(t)
	p := New(root, nil)

	for _, key := range []string{"out", "../outside/secret.txt", "dirlink/inner.txt"} {
		s := &pageStream{}
		err := p.ServeContent(&pluginv1.ServeContentRequest{Key: key}, s)
		if status.Code(err) != codes.FailedPrecondition || len(s.chunks) != 0 {
			t.Errorf("ServeContent(%s) = %v, %d chunks; want FailedPrecondition and nothing served", key, err, len(s.chunks))
		}
		c := &contentStream{}
		if err := p.ReadContent(&pluginv1.ReadContentRequest{Key: key}, c); status.Code(err) != codes.FailedPrecondition || len(c.chunks) != 0 {
			t.Errorf("ReadContent(%s) = %v, %v; want FailedPrecondition and no body", key, err, c.chunks)
		}
		if _, err := p.GetPreview(context.Background(), &pluginv1.GetPreviewRequest{Key: key}); status.Code(err) != codes.FailedPrecondition {
			t.Errorf("GetPreview(%s) = %v, want FailedPrecondition", key, err)
		}
	}

	s := &pageStream{}
	if err := p.ServeContent(&pluginv1.ServeContentRequest{Key: "dir/inner.txt", Subpath: "leak.css"}, s); err != nil ||
		len(s.chunks) != 1 || s.chunks[0].Status != 404 {
		t.Errorf("a resource linking out of the page's directory = %v, %v; want one 404 page", s.chunks, err)
	}

	c := &contentStream{}
	if err := p.ReadContent(&pluginv1.ReadContentRequest{Key: "file"}, c); err != nil || len(c.chunks) != 1 || string(c.chunks[0].Data) != "# real" {
		t.Errorf("ReadContent(file) = %v, %v; want the file it lands on", c.chunks, err)
	}
}

// A root that is itself a symlink is served through: confinement resolves
// the root as it resolves every name under it.
func TestARootBehindASymlinkServes(t *testing.T) {
	root, _ := symlinkTree(t)
	alias := filepath.Join(filepath.Dir(root), "alias")
	if err := os.Symlink(root, alias); err != nil {
		t.Skip("symlinks unsupported")
	}
	p := New(alias, nil)
	_, top := listed(t, p, ".")
	if ref := top["file"].GetLinkTarget(); ref.GetKey() != "real.md" {
		t.Errorf("file links to %v through a linked root, want real.md", ref)
	}
	s := &pageStream{}
	if err := p.ServeContent(&pluginv1.ServeContentRequest{Key: "page.html"}, s); err != nil || len(s.chunks) == 0 || s.chunks[0].Status != 200 {
		t.Errorf("ServeContent(page.html) through a linked root = %v, %v; want the page", s.chunks, err)
	}
}

// Delete acts on the link, never on what it points at.
func TestDeleteTrashesTheLinkItself(t *testing.T) {
	root, _ := symlinkTree(t)
	h := &recordingHost{}
	if _, err := New(root, h).Delete(context.Background(), &pluginv1.DeleteRequest{Key: "out"}); err != nil {
		t.Fatal(err)
	}
	if len(h.trashed) != 1 || h.trashed[0] != filepath.Join(root, "out") {
		t.Errorf("trashed %v, want the link %s", h.trashed, filepath.Join(root, "out"))
	}
}

// The node watches the contexts a shown grid links into; a dead link's
// target, or a link's own path, is no directory of the tree and is not
// watched, and asking is no error.
func TestWatchSkipsContextsNotInTheTree(t *testing.T) {
	root, _ := symlinkTree(t)
	p := New(root, nil)
	openWatch(t, p, ".", "..", "../outside", "dirlink", "cycle")
	if got := p.watchedDirs(); !got["."] || len(got) != 1 {
		t.Fatalf("watches %v, want only the root", got)
	}
}
