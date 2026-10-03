package memo

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

type snap struct {
	Items    []string             `json:"items"`
	WalkedAt map[string]time.Time `json:"walkedAt"`
}

// TestFileLoad: a file this version wrote comes back whole; anything else on
// disk is a cold start, logged unless it is simply absent.
func TestFileLoad(t *testing.T) {
	stamp := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	want := snap{Items: []string{"a", "b"}, WalkedAt: map[string]time.Time{"inbox": stamp}}
	for _, tc := range []struct {
		name    string
		on      func(t *testing.T, path string)
		wantOK  bool
		wantLog int
	}{
		{"missing is a quiet cold start", func(*testing.T, string) {}, false, 0},
		{"what Save wrote round-trips", func(t *testing.T, path string) {
			f := NewFile[snap](filepath.Dir(path), filepath.Base(path), 2, nil)
			if err := f.Save(func() snap { return want }); err != nil {
				t.Fatal(err)
			}
		}, true, 0},
		{"corrupt is a logged cold start", func(t *testing.T, path string) {
			write(t, path, `{"version":2,"data":{"items":[`)
		}, false, 1},
		{"another version is a logged cold start", func(t *testing.T, path string) {
			write(t, path, `{"version":1,"data":{"items":["old"]}}`)
		}, false, 1},
		{"a file from before memo is a logged cold start", func(t *testing.T, path string) {
			write(t, path, `{"version":2,"threads":[]}`)
		}, false, 1},
		{"a directory in its place is a logged cold start", func(t *testing.T, path string) {
			if err := os.Mkdir(path, 0o700); err != nil {
				t.Fatal(err)
			}
		}, false, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "memory.json")
			tc.on(t, path)
			var l logs
			got, ok := NewFile[snap](dir, "memory.json", 2, l.logf).Load()
			if ok != tc.wantOK {
				t.Fatalf("Load ok = %v, want %v", ok, tc.wantOK)
			}
			if ok && !reflect.DeepEqual(got, want) {
				t.Errorf("Load = %+v, want %+v", got, want)
			}
			if !ok && !reflect.DeepEqual(got, snap{}) {
				t.Errorf("a cold Load answered %+v, want the zero memory", got)
			}
			if l.count() != tc.wantLog {
				t.Errorf("logged %v, want %d lines", l.lines, tc.wantLog)
			}
		})
	}
}

// TestFileWithNoStateDirIsAlwaysCold: a node that hands no state_dir gets a
// memory that lives as long as the process and writes nothing.
func TestFileWithNoStateDirIsAlwaysCold(t *testing.T) {
	f := NewFile[snap]("", "memory.json", 1, nil)
	if err := f.Save(func() snap { return snap{Items: []string{"x"}} }); err != nil {
		t.Fatal(err)
	}
	if _, ok := f.Load(); ok {
		t.Error("Load with no state_dir answered a memory")
	}
	if f.Path() != "" {
		t.Errorf("Path = %q, want none", f.Path())
	}
}

// TestFileSave: the file is private, its directory is minted if deleted, and
// no temp file is left beside it.
func TestFileSave(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "plugins", "p1")
	f := NewFile[snap](dir, "memory.json", 1, nil)
	if err := f.Save(func() snap { return snap{Items: []string{"x"}} }); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(f.Path())
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v, want 0600", info.Mode().Perm())
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Errorf("state dir holds %d files, want the one", len(entries))
	}
}

// TestFileSaveLogsOncePerEpisode: a save that keeps failing is one line; one
// that lands ends the episode, and the next failure is a new line.
func TestFileSaveLogsOncePerEpisode(t *testing.T) {
	root := t.TempDir()
	blocker := filepath.Join(root, "state")
	var l logs
	f := NewFile[snap](blocker, "memory.json", 1, l.logf)
	save := func() error { return f.Save(func() snap { return snap{} }) }

	write(t, blocker, "a file where the directory should be")
	for range 3 {
		if save() == nil {
			t.Fatal("Save into a file succeeded")
		}
	}
	if l.count() != 1 {
		t.Fatalf("three failed saves logged %d lines, want 1", l.count())
	}
	if err := os.Remove(blocker); err != nil {
		t.Fatal(err)
	}
	if err := save(); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(blocker); err != nil {
		t.Fatal(err)
	}
	write(t, blocker, "again")
	if save() == nil {
		t.Fatal("Save into a file succeeded")
	}
	if l.count() != 2 {
		t.Errorf("a second episode logged %d lines in all, want 2", l.count())
	}
}

func write(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}
