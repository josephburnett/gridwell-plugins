package todos

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// CacheFile is the plugin's cache file, inside the private directory the node
// hands the plugin as `state_dir`. It holds one Snapshot and nothing else: the
// plugin's memory of ITS SOURCE's data, never a node fact. It is disposable
// under the same contract as the node's own cache — deleting it is always
// safe, and the next walk rewarms it.
const CacheFile = "todos.json"

// snapshotVersion stamps the file so a later format change can refuse an
// older one rather than misread it. A refused file is a cold start, which
// costs one walk.
const snapshotVersion = 1

// Snapshot is everything a Memory knows: every todo it has seen, with the
// derived state each carries, and whether some walk has reached the end of
// GitLab's done list. Those are the two facts a restart needs — the records
// to answer listings from, and the high-water mark that lets the next done
// walk stop at the first page carrying nothing unknown.
type Snapshot struct {
	Version      int    `json:"version"`
	DoneComplete bool   `json:"doneComplete"`
	Todos        []Todo `json:"todos"`
}

// Snapshot copies out everything the memory holds, oldest first, so two
// snapshots of the same memory are byte-identical.
func (m *Memory) Snapshot() Snapshot {
	m.mu.Lock()
	complete := m.doneComplete
	m.mu.Unlock()
	return Snapshot{Version: snapshotVersion, DoneComplete: complete, Todos: m.All()}
}

// Restore folds a snapshot into the memory. The records absorb exactly as a
// walked page does, and doneComplete only ever rises: a memory that has
// already reached the end of the done list does not forget it because the
// file was written before that walk.
func (m *Memory) Restore(s Snapshot) {
	m.absorb(s.Todos)
	if s.DoneComplete {
		m.mu.Lock()
		m.doneComplete = true
		m.mu.Unlock()
	}
}

// LoadCache reads the snapshot at path. A missing file wraps fs.ErrNotExist,
// which the caller reads as "no cache yet"; anything else is a real failure to
// report, and the caller starts cold rather than serving a half-read memory.
func LoadCache(path string) (Snapshot, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return Snapshot{}, err
	}
	var s Snapshot
	if err := json.Unmarshal(raw, &s); err != nil {
		return Snapshot{}, fmt.Errorf("%s: %v", path, err)
	}
	if s.Version != snapshotVersion {
		return Snapshot{}, fmt.Errorf("%s: cache version %d, want %d", path, s.Version, snapshotVersion)
	}
	return s, nil
}

// SaveCache writes snap to path atomically: a temp file beside the target,
// synced, then renamed over it. A reader — this plugin's next boot — therefore
// sees the whole old file or the whole new one, never a half-written one, and
// a crash mid-write costs the update, not the cache. The directory is minted
// if it is missing, so a state directory deleted by hand comes back on the
// next walk.
func SaveCache(path string, snap Snapshot) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp) // a no-op once the rename has moved it away
	raw, err := json.Marshal(snap)
	if err != nil {
		f.Close()
		return err
	}
	if _, err := f.Write(raw); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
