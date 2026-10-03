package mail

import "slices"

// CacheFile is the plugin's cache file in the private directory the node
// hands it as `state_dir` (memo.File owns its contract). It holds this
// plugin's memory of ITS SOURCE, never a node fact.
const CacheFile = "mail.json"

// Snapshot is the memory as the cache file holds it: every thread the memory
// has seen, each collection's membership, and the strays not yet asked
// about.
type Snapshot struct {
	Threads     []Thread                `json:"threads"`
	Collections map[string]CollectionIn `json:"collections"`
	Strays      []int64                 `json:"strays,omitempty"`
}

// CollectionIn is one collection's remembered membership: what it held, and
// whether the walk that read it reached the end of the box.
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
	strays := make([]int64, 0, len(m.strays))
	for id := range m.strays {
		strays = append(strays, id)
	}
	slices.Sort(strays)
	return Snapshot{Threads: threads, Collections: cols, Strays: strays}
}

// Restore folds a snapshot into the memory. It is the boot path only: the
// membership it carries is taken as read, because it is this plugin's own
// last word about the same boxes.
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
	for _, id := range s.Strays {
		m.strays[id] = true
	}
}
