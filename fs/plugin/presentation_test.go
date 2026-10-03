package plugin

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/josephburnett/gridwell/api/rpc"

	pluginv1 "github.com/josephburnett/gridwell/api/gen/plugin/v1"
)

// Every text entry a listing answers declares plain or both, and no entry
// carries a status_detail: a file has no state worth a word.
func TestEveryTextEntryDeclaresAPresentation(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"notes.md", "plan.org", "app.log", "main.go", "Makefile", "blob.bin", "untitled", "photo.png", "page.html"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(filepath.Join(root, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	resp, err := New(root, nil).List(context.Background(), &pluginv1.ListRequest{Context: "."})
	if err != nil {
		t.Fatal(err)
	}
	texts := 0
	for _, e := range resp.Entries {
		if e.StatusDetail != "" {
			t.Errorf("%s carries status_detail %q", e.Key, e.StatusDetail)
		}
		if e.Kind != rpc.KindText {
			continue
		}
		texts++
		if e.TextPresentation != rpc.TextPresentationPlain && e.TextPresentation != rpc.TextPresentationBoth {
			t.Errorf("%s declares text_presentation %q; want plain or both", e.Key, e.TextPresentation)
		}
	}
	if texts != 7 {
		t.Fatalf("%d text entries, want 7", texts)
	}
}
