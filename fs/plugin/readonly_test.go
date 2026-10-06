package plugin

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/josephburnett/gridwell-plugins/fs/fsfile"
	pluginv1 "github.com/josephburnett/gridwell/api/gen/plugin/v1"
)

// A text entry whose body a write would refuse says so on the listing, with
// the reason, and one whose body a write takes says nothing: the declaration
// and the refusal are one rule, so the client never offers an edit that
// cannot land.
func TestATextEntryAWriteWouldRefuseIsReadOnly(t *testing.T) {
	if runtime.GOOS == "windows" || os.Getuid() == 0 {
		t.Skip("permissions do not bind here")
	}
	root := t.TempDir()
	write := func(name string, data []byte, perm os.FileMode) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(root, name), data, perm); err != nil {
			t.Fatal(err)
		}
	}
	write("notes.md", []byte("# mine\n"), 0o644)
	write("blob.bin", []byte{0, 1, 2}, 0o644)
	write("big.log", bytes.Repeat([]byte("x"), fsfile.MaxWrite+1), 0o644)
	write("ro.md", []byte("# locked\n"), 0o444)
	if err := os.Mkdir(filepath.Join(root, "sealed"), 0o755); err != nil {
		t.Fatal(err)
	}
	write("sealed/f.md", []byte("# f\n"), 0o644)
	if err := os.Chmod(filepath.Join(root, "sealed"), 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(filepath.Join(root, "sealed"), 0o755) })

	p := New(root, nil)
	entries := map[string]*pluginv1.Entry{}
	for _, ctx := range []string{".", "sealed"} {
		resp, err := p.List(context.Background(), &pluginv1.ListRequest{Context: ctx})
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range resp.Entries {
			entries[e.Key] = e
		}
	}
	for key, want := range map[string]string{
		"notes.md":    "",
		"blob.bin":    "summary",
		"big.log":     "larger",
		"ro.md":       "permission denied",
		"sealed/f.md": "directory",
	} {
		e := entries[key]
		if e == nil {
			t.Fatalf("%s not listed", key)
		}
		if (e.ReadOnly != "") != (want != "") || !strings.Contains(e.ReadOnly, want) {
			t.Errorf("%s: read_only %q, want one saying %q", key, e.ReadOnly, want)
		}
		_, err := writeKey(p, key, e.ContentStamp, []byte("typed"))
		if refused := err != nil; refused != (e.ReadOnly != "") {
			t.Errorf("%s: declared read_only %q but the write answered %v", key, e.ReadOnly, err)
		}
	}
}
