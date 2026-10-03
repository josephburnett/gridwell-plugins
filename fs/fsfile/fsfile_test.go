package fsfile

import (
	"os"
	"path/filepath"
	"testing"

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
