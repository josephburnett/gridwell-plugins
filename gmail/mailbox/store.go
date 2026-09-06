package mailbox

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// CacheFile is the plugin's cache file, inside the private directory the node
// hands the plugin as `state_dir`. It holds one Snapshot and nothing else:
// the plugin's memory of ITS SOURCE, never a node fact and never a
// credential. It is disposable under the same contract as the node's own
// cache — deleting it is always safe, and the next walk rewarms it.
const CacheFile = "gmail.json"

// snapshotVersion stamps the file so a later format change can refuse an
// older one rather than misread it. A refused file is a cold start, which
// costs one walk.
const snapshotVersion = 1

// Snapshot is what the cache file holds: every message the memory has seen,
// the unread set, and each collection's membership.
type Snapshot struct {
	Version     int                     `json:"version"`
	Messages    []Message               `json:"messages"`
	Unread      []string                `json:"unread"`
	Collections map[string]CollectionIn `json:"collections"`
}

// CollectionIn is one collection's remembered membership: what it held, and
// whether the walk that read it produced a usable membership. WalkedAt is the
// PLUGIN's fact — when that walk landed, so a restart inside the refresh
// window answers from the file without calling Gmail at all. Memory neither
// sets nor reads it; the plugin stamps it on the way out and takes it back on
// the way in.
type CollectionIn struct {
	Complete bool      `json:"complete"`
	WalkedAt time.Time `json:"walkedAt,omitempty"`
	IDs      []string  `json:"ids"`
}

// Snapshot copies out everything the memory holds, messages oldest first and
// the unread set in the same order, so two snapshots of the same memory are
// byte-identical.
func (m *Memory) Snapshot() Snapshot {
	m.mu.Lock()
	defer m.mu.Unlock()
	msgs := make([]Message, 0, len(m.messages))
	for _, msg := range m.messages {
		msgs = append(msgs, *msg)
	}
	sortMessages(msgs)
	unread := make([]string, 0, len(m.unread))
	for _, msg := range msgs {
		if m.unread[msg.ID] {
			unread = append(unread, msg.ID)
		}
	}
	cols := make(map[string]CollectionIn, len(m.members))
	for key, ids := range m.members {
		out := make([]string, len(ids))
		copy(out, ids)
		cols[key] = CollectionIn{Complete: m.complete[key], IDs: out}
	}
	return Snapshot{Version: snapshotVersion, Messages: msgs, Unread: unread, Collections: cols}
}

// Restore folds a snapshot into the memory. It is the boot path only: the
// membership it carries is taken as read, because it is this plugin's own
// last word about the same collections.
func (m *Memory) Restore(s Snapshot) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := range s.Messages {
		msg := s.Messages[i]
		m.messages[msg.ID] = &msg
	}
	for _, id := range s.Unread {
		m.unread[id] = true
	}
	for key, in := range s.Collections {
		ids := make([]string, len(in.IDs))
		copy(ids, in.IDs)
		m.members[key] = ids
		if in.Complete {
			m.complete[key] = true
		}
	}
}

func sortMessages(ms []Message) {
	views := make([]View, len(ms))
	for i := range ms {
		views[i] = View{Message: ms[i]}
	}
	sortViews(views)
	for i := range views {
		ms[i] = views[i].Message
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
// missing, so a state directory deleted by hand comes back on the next walk.
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
