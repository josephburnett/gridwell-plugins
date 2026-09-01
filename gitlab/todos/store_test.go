package todos

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A restart is a Save then a Load into a fresh Memory: every listing answers
// from the file, with the DERIVED state each todo carried, and the done
// high-water mark comes back so the next walk stops early instead of paging
// the whole done list again.
func TestCacheFileCarriesMemoryAcrossARestart(t *testing.T) {
	src := &fakeSource{per: 2,
		pending: []Todo{mk(1, "2026-08-10T10:00:00Z", StatePending), mk(3, "2026-08-18T10:00:00Z", StatePending)},
		done:    []Todo{mk(2, "2026-08-11T10:00:00Z", StateDone), mk(4, "2026-08-19T10:00:00Z", StateDone), mk(6, "2026-08-25T11:00:00Z", StateDone)},
	}
	m := NewMemory()
	if err := m.Sync(context.Background(), src, time.Time{}); err != nil {
		t.Fatal(err)
	}
	// Todo 3 leaves pending: the derived done state is what must survive.
	src.pending = src.pending[:1]
	if err := m.Sync(context.Background(), src, time.Time{}); err != nil {
		t.Fatal(err)
	}

	path := filepath.Join(t.TempDir(), CacheFile)
	if err := SaveCache(path, m.Snapshot()); err != nil {
		t.Fatal(err)
	}
	snap, err := LoadCache(path)
	if err != nil {
		t.Fatal(err)
	}
	restored := NewMemory()
	restored.Restore(snap)

	if got, want := len(restored.All()), len(m.All()); got != want {
		t.Fatalf("restored %d todos, want %d", got, want)
	}
	if three, ok := restored.Get(3); !ok || !three.Done() {
		t.Errorf("todo 3 restored as %+v; the derived done state must survive the restart", three)
	}
	if !restored.Walked() {
		t.Error("doneComplete did not survive: the restart would re-page the whole done list")
	}
	if got, want := restored.Weeks(), m.Weeks(); len(got) != len(want) || !got[0].Start.Equal(want[0].Start) || got[0].Done != want[0].Done {
		t.Errorf("weeks after restore = %+v, want %+v", got, want)
	}
	// The restored memory keeps the high-water stop: a walk over the same
	// GitLab pages nothing it already knows past the first done page.
	src.calls = nil
	if err := restored.Sync(context.Background(), src, time.Time{}); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(src.calls, " "); got != "pending/1 done/1" {
		t.Errorf("warm walk = %s, want the pending set in full plus one done page", got)
	}
}

// Restore never lowers the high-water mark: a file written before the first
// full done walk must not un-complete a memory that has since finished one.
func TestRestoreOnlyRaisesDoneComplete(t *testing.T) {
	m := NewMemory()
	m.Restore(Snapshot{DoneComplete: true, Todos: []Todo{mk(1, "2026-08-10T10:00:00Z", StatePending)}})
	if !m.Walked() {
		t.Fatal("a snapshot with doneComplete must restore it")
	}
	m.Restore(Snapshot{Todos: []Todo{mk(2, "2026-08-11T10:00:00Z", StatePending)}})
	if !m.Walked() {
		t.Error("an incomplete snapshot un-completed the memory")
	}
	if _, ok := m.Get(2); !ok {
		t.Error("the second snapshot's todos were dropped")
	}
}

// A save leaves the directory holding the cache and nothing else: no temp
// file survives, on the way in or over an existing cache.
func TestSaveCacheReplacesInPlaceAndLeavesNoTemp(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "gitlab-state") // not yet minted
	path := filepath.Join(dir, CacheFile)
	first := Snapshot{Version: snapshotVersion, Todos: []Todo{mk(1, "2026-08-10T10:00:00Z", StatePending)}}
	if err := SaveCache(path, first); err != nil {
		t.Fatal(err)
	}
	second := Snapshot{Version: snapshotVersion, DoneComplete: true, Todos: []Todo{mk(2, "2026-08-11T10:00:00Z", StateDone)}}
	if err := SaveCache(path, second); err != nil {
		t.Fatal(err)
	}
	names, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(names) != 1 || names[0].Name() != CacheFile {
		got := []string{}
		for _, n := range names {
			got = append(got, n.Name())
		}
		t.Errorf("state dir holds %v, want just %s", got, CacheFile)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("cache mode = %v, want 0600", info.Mode().Perm())
	}
	snap, err := LoadCache(path)
	if err != nil {
		t.Fatal(err)
	}
	if !snap.DoneComplete || len(snap.Todos) != 1 || snap.Todos[0].ID != 2 {
		t.Errorf("loaded %+v, want the second save", snap)
	}
}

// A missing cache is not a failure to report — it is the first boot. A
// corrupt or future-version one is, and neither is served half-read.
func TestLoadCacheSeparatesMissingFromUnreadable(t *testing.T) {
	dir := t.TempDir()
	if _, err := LoadCache(filepath.Join(dir, CacheFile)); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("missing cache = %v, want fs.ErrNotExist", err)
	}
	bad := filepath.Join(dir, "bad.json")
	if err := os.WriteFile(bad, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadCache(bad); err == nil || errors.Is(err, fs.ErrNotExist) {
		t.Errorf("corrupt cache = %v, want a reportable error", err)
	}
	future := filepath.Join(dir, "future.json")
	if err := os.WriteFile(future, []byte(`{"version":99,"todos":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadCache(future); err == nil || !strings.Contains(err.Error(), "version 99") {
		t.Errorf("future cache = %v, want a refusal naming the version", err)
	}
}
