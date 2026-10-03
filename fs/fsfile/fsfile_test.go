package fsfile

import (
	"os"
	"path/filepath"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	gridwellv1 "github.com/josephburnett/gridwell/api/gen/gridwell/v1"
	"github.com/josephburnett/gridwell/api/rpc"
)

// Every name declares a presentation; a name with no family of its own
// renders, which is what its metadata body needs.
func TestTextPresentationDeclaresEveryName(t *testing.T) {
	for name, want := range map[string]string{
		"notes.md":  rpc.TextPresentationBoth,
		"plan.org":  rpc.TextPresentationBoth,
		"app.log":   rpc.TextPresentationPlain,
		"Makefile":  rpc.TextPresentationPlain,
		"blob.bin":  rpc.TextPresentationBoth,
		"untitled":  rpc.TextPresentationBoth,
		"photo.raw": rpc.TextPresentationBoth,
	} {
		if got := TextPresentation(name); got != want {
			t.Errorf("TextPresentation(%q) = %q, want %q", name, got, want)
		}
	}
}

// A body is the media type its name's declaration names, under the cap and
// past it: plain is text/plain, both is text/markdown.
func TestBodyAgreesWithItsDeclaration(t *testing.T) {
	dir := t.TempDir()
	sizes := map[string]int64{
		"small.log": 10, "big.log": renderableBodyCap + 1,
		"small.md": 10, "big.md": renderableBodyCap + 1,
		"blob.bin": 10,
	}
	for name, size := range sizes {
		if err := os.WriteFile(filepath.Join(dir, name), nil, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Truncate(filepath.Join(dir, name), size); err != nil {
			t.Fatal(err)
		}
	}
	want := map[string]string{rpc.TextPresentationPlain: "text/plain", rpc.TextPresentationBoth: "text/markdown"}
	for name := range sizes {
		data, mediaType := Body(dir, name)
		if data == nil {
			t.Errorf("Body(%q) = nil", name)
		}
		if w := want[TextPresentation(name)]; mediaType != w {
			t.Errorf("Body(%q) is %q under declaration %q; want %q", name, mediaType, TextPresentation(name), w)
		}
	}
}

type chunks []*gridwellv1.ServeContentChunk

func (c *chunks) Send(ch *gridwellv1.ServeContentChunk) error {
	*c = append(*c, ch)
	return nil
}

// Absence is a 404 page; a file the plugin may not read is an error that says
// so, never a page claiming it is absent.
func TestServeFileTellsUnreadableFromAbsent(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads every file")
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "locked.html"), []byte("x"), 0o000); err != nil {
		t.Fatal(err)
	}

	var missing chunks
	if err := ServeFile(&missing, dir, "gone.html", ""); err != nil || len(missing) != 1 || missing[0].Status != 404 {
		t.Errorf("absent file = %v, %v; want one 404 page", missing, err)
	}

	var locked chunks
	err := ServeFile(&locked, dir, "locked.html", "")
	if status.Code(err) != codes.PermissionDenied || len(locked) != 0 {
		t.Errorf("unreadable file = %v, %v; want PermissionDenied and no page", locked, err)
	}
}
