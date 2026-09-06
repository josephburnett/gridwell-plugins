package mail

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// CacheFile is the plugin's cache file, inside the private directory the node
// hands the plugin as `state_dir`. It holds one Snapshot and nothing else:
// the plugin's memory of ITS SOURCE, never a node fact. It is disposable
// under the same contract as the node's own cache — deleting it is always
// safe, and the next sweep rewarms it.
const CacheFile = "mail.json"

// snapshotVersion stamps the file so a later format change can refuse an
// older one rather than misread it. A refused file is a cold start, which
// costs one sweep.
const snapshotVersion = 1

// Snapshot is what the cache file holds: every thread the memory has seen,
// each collection's membership and whether it has been read to its end, and
// when the last sweep landed. SweptAt is the plugin's own fact, so a restart
// inside the refresh window answers from the file without running the CLI at
// all; Memory neither sets nor reads it.
type Snapshot struct {
	Version     int                     `json:"version"`
	SweptAt     time.Time               `json:"sweptAt,omitempty"`
	Threads     []Thread                `json:"threads"`
	Collections map[string]CollectionIn `json:"collections"`
}

// CollectionIn is one collection's remembered membership.
type CollectionIn struct {
	Complete bool    `json:"complete"`
	TopicIDs []int64 `json:"topicIds"`
}

// Snapshot copies out everything the memory holds, threads oldest first, so
// two snapshots of the same memory are byte-identical.
func (m *Memory) Snapshot() Snapshot {
	threads := m.All()
	m.mu.Lock()
	defer m.mu.Unlock()
	cols := make(map[string]CollectionIn, len(m.members))
	for key, ids := range m.members {
		out := make([]int64, len(ids))
		copy(out, ids)
		cols[key] = CollectionIn{Complete: m.complete[key], TopicIDs: out}
	}
	return Snapshot{Version: snapshotVersion, Threads: threads, Collections: cols}
}

// Restore folds a snapshot into the memory. It is the boot path only: the
// membership it carries is taken as read, because it is this plugin's own
// last word about the same three collections.
func (m *Memory) Restore(s Snapshot) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := range s.Threads {
		t := s.Threads[i]
		m.threads[t.TopicID] = &t
	}
	for key, in := range s.Collections {
		ids := make([]int64, len(in.TopicIDs))
		copy(ids, in.TopicIDs)
		m.members[key] = ids
		if in.Complete {
			m.complete[key] = true
		}
	}
}

// LoadCache reads the snapshot at path. A missing file wraps fs.ErrNotExist,
// which the caller reads as "no cache yet"; anything else is a real failure
// to report, and the caller starts cold rather than serving a half-read
// memory.
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
// synced, then renamed over it. A reader — this plugin's next boot — sees the
// whole old file or the whole new one, never a half-written one, and a crash
// mid-write costs the update, not the cache. The directory is minted if it is
// missing, so a state directory deleted by hand comes back on the next sweep.
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
