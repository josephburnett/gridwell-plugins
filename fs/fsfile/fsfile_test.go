package fsfile

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	gridwellv1 "github.com/josephburnett/gridwell/api/gen/gridwell/v1"
	"github.com/josephburnett/gridwell/api/rpc"
)

// A text file past the cap shows its first renderableBodyCap bytes, in its
// declared presentation: a long log reads as its beginning, not as a summary
// of itself.
func TestBodyPastTheCapIsTheFilesBeginning(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"big.log", "big.md"} {
		content := bytes.Repeat([]byte("0123456789abcdef"), renderableBodyCap/16+1)
		if err := os.WriteFile(filepath.Join(dir, name), content, 0o644); err != nil {
			t.Fatal(err)
		}
		data, _ := Body(dir, name)
		if !bytes.Equal(data, content[:renderableBodyCap]) {
			t.Errorf("Body(%s) = %d bytes starting %q, want its first %d bytes", name, len(data), data[:min(len(data), 16)], renderableBodyCap)
		}
	}
}

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
	if err := ServeFile(&missing, dir, dir, "gone.html", ""); err != nil || len(missing) != 1 || missing[0].Status != 404 {
		t.Errorf("absent file = %v, %v; want one 404 page", missing, err)
	}

	var locked chunks
	err := ServeFile(&locked, dir, dir, "locked.html", "")
	if status.Code(err) != codes.PermissionDenied || len(locked) != 0 {
		t.Errorf("unreadable file = %v, %v; want PermissionDenied and no page", locked, err)
	}
}

// Two saves of a picture within one second are two pictures: the stamp is the
// mtime to the nanosecond, so the second save's face is asked for.
func TestTwoSavesInOneSecondAreTwoStamps(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "pic.png")
	if err := os.WriteFile(path, []byte("\x89PNG\r\n\x1a\nA"), 0o644); err != nil {
		t.Fatal(err)
	}
	second := time.Unix(1_700_000_000, 0)
	stamps := map[int64]bool{}
	for _, at := range []time.Time{second.Add(100 * time.Millisecond), second.Add(600 * time.Millisecond)} {
		if err := os.Chtimes(path, at, at); err != nil {
			t.Fatal(err)
		}
		stamps[PreviewStamp(dir, "pic.png")] = true
	}
	if len(stamps) != 2 {
		t.Fatalf("two saves within one second stamped %v; want two stamps", stamps)
	}
}
