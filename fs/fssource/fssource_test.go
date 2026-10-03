package fssource

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestReadSortsAndClassifies(t *testing.T) {
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "alpha.md"), "hello")
	mustWrite(t, filepath.Join(root, "beta.bin"), "\x00\x01\x02\x03")
	if err := os.Mkdir(filepath.Join(root, "subdir"), 0o755); err != nil {
		t.Fatal(err)
	}

	got, err := Read(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("len(entries)=%d, want 3", len(got))
	}
	// Sorted alphabetically.
	wantNames := []string{"alpha.md", "beta.bin", "subdir"}
	for i, e := range got {
		if e.Name != wantNames[i] {
			t.Errorf("entries[%d].Name = %q, want %q", i, e.Name, wantNames[i])
		}
	}
	if got[0].Kind != KindFile || got[1].Kind != KindFile || got[2].Kind != KindDir {
		t.Errorf("kinds = %v/%v/%v, want file/file/dir",
			got[0].Kind, got[1].Kind, got[2].Kind)
	}
	if got[0].Size != int64(len("hello")) {
		t.Errorf("alpha.md size = %d, want 5", got[0].Size)
	}
	if got[2].AbsPath != filepath.Join(root, "subdir") {
		t.Errorf("subdir AbsPath = %q", got[2].AbsPath)
	}
}

// A symlink is listed as itself, never as what it points at: its Target is
// where it lands with every link followed, and its TargetKind what is there.
// A broken link has no kind, and its Target is where it points.
func TestReadListsSymlinksAsLinks(t *testing.T) {
	base := realTempDir(t)
	root := filepath.Join(base, "root")
	outside := filepath.Join(base, "outside")
	for _, d := range []string{root, outside, filepath.Join(root, "dir")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	mustWrite(t, filepath.Join(root, "real.md"), "abc")
	mustWrite(t, filepath.Join(outside, "secret.txt"), "s")
	links := map[string]string{
		"file":    "real.md",
		"dir":     "", // a real directory, not a link
		"dirlink": "dir",
		"cycle":   ".",
		"broken":  "missing.md",
		"out":     filepath.Join(outside, "secret.txt"),
		"chain":   "file",
	}
	for name, dest := range links {
		if dest == "" {
			continue
		}
		if err := os.Symlink(dest, filepath.Join(root, name)); err != nil {
			t.Skip("symlinks unsupported")
		}
	}
	got, err := Read(root)
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]Entry{}
	for _, e := range got {
		byName[e.Name] = e
	}
	want := map[string]Entry{
		"file":    {Kind: KindLink, Target: filepath.Join(root, "real.md"), TargetKind: KindFile},
		"dir":     {Kind: KindDir},
		"dirlink": {Kind: KindLink, Target: filepath.Join(root, "dir"), TargetKind: KindDir},
		"cycle":   {Kind: KindLink, Target: root, TargetKind: KindDir},
		"broken":  {Kind: KindLink, Target: filepath.Join(root, "missing.md")},
		"out":     {Kind: KindLink, Target: filepath.Join(outside, "secret.txt"), TargetKind: KindFile},
		"chain":   {Kind: KindLink, Target: filepath.Join(root, "real.md"), TargetKind: KindFile},
	}
	for name, w := range want {
		e := byName[name]
		if e.Kind != w.Kind || e.Target != w.Target || e.TargetKind != w.TargetKind {
			t.Errorf("%s = kind %q target %q (%q), want kind %q target %q (%q)",
				name, e.Kind, e.Target, e.TargetKind, w.Kind, w.Target, w.TargetKind)
		}
	}
}

// A link that loops, or that points at another link which never lands, names
// a path through itself: one that never resolves, so it is never mistaken for
// the link it passes through.
func TestReadNamesALoopThroughItself(t *testing.T) {
	root := realTempDir(t)
	for name, dest := range map[string]string{"a": "b", "b": "a", "self": "self", "dangle": "nowhere", "via": "dangle"} {
		if err := os.Symlink(dest, filepath.Join(root, name)); err != nil {
			t.Skip("symlinks unsupported")
		}
	}
	got, err := Read(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range got {
		if e.Kind != KindLink || e.TargetKind != "" {
			t.Errorf("%s = kind %q target kind %q, want a broken link", e.Name, e.Kind, e.TargetKind)
		}
		want := filepath.Join(root, e.Name, e.Name)
		if e.Name == "dangle" {
			want = filepath.Join(root, "nowhere")
		}
		if e.Target != want {
			t.Errorf("%s targets %q, want %q", e.Name, e.Target, want)
		}
	}
}

func TestReadNonexistentDirReturnsError(t *testing.T) {
	if _, err := Read("/this/path/should/not/exist/anywhere"); err == nil {
		t.Error("expected error")
	}
}

func TestMetadataMarkdownIsDeterministic(t *testing.T) {
	e := Entry{
		Name:    "foo.md",
		AbsPath: "/x/foo.md",
		Kind:    KindFile,
		Size:    42,
		ModTime: time.Unix(1700000000, 0),
	}
	a := MetadataMarkdown(e)
	b := MetadataMarkdown(e)
	if a != b {
		t.Error("MetadataMarkdown should be deterministic")
	}
	if !strings.Contains(a, "foo.md") || !strings.Contains(a, "42 bytes") {
		t.Errorf("missing fields:\n%s", a)
	}
}

func TestMetadataMarkdownDirectoryLabel(t *testing.T) {
	e := Entry{Name: "bin", Kind: KindDir, AbsPath: "/bin"}
	md := MetadataMarkdown(e)
	if !strings.Contains(md, "directory") {
		t.Errorf("expected directory label, got:\n%s", md)
	}
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// realTempDir is a temp dir by its real path, as Target spells paths: on
// macOS the temp dir itself sits behind a symlink.
func realTempDir(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return dir
}
