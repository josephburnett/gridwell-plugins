package plugin

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	pluginv1 "github.com/josephburnett/gridwell/api/gen/plugin/v1"
)

// A text entry names its bytes by one stamp in its listing and in its read,
// so a body read under it is known current; a change to the file moves it,
// and an entry with no body to edit, a page, names none.
func TestATextEntryAndItsReadNameTheBytesByOneStamp(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "notes.md")
	if err := os.WriteFile(path, []byte("# one\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "page.html"), []byte("<p>hi</p>"), 0o644); err != nil {
		t.Fatal(err)
	}
	p := New(root, nil)
	listed := func() map[string]string {
		resp, err := p.List(context.Background(), &pluginv1.ListRequest{Context: "."})
		if err != nil {
			t.Fatal(err)
		}
		out := map[string]string{}
		for _, e := range resp.Entries {
			out[e.Key] = e.ContentStamp
		}
		return out
	}
	read := func() string {
		s := &contentStream{}
		if err := p.ReadContent(&pluginv1.ReadContentRequest{Key: "notes.md"}, s); err != nil {
			t.Fatal(err)
		}
		return s.chunks[0].ContentStamp
	}

	first := listed()
	if first["notes.md"] == "" || first["notes.md"] != read() {
		t.Fatalf("listing stamp %q, read stamp %q; want one non-empty stamp", first["notes.md"], read())
	}
	if first["page.html"] != "" {
		t.Errorf("a page entry names stamp %q; it has no body to edit", first["page.html"])
	}
	later := time.Now().Add(time.Second)
	if err := os.WriteFile(path, []byte("# two\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, later, later); err != nil {
		t.Fatal(err)
	}
	if now := listed()["notes.md"]; now == first["notes.md"] || now != read() {
		t.Errorf("after a write: listing %q (was %q), read %q; want a new stamp both agree on", now, first["notes.md"], read())
	}
}
