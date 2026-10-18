package mail

// CacheFile is the plugin's cache file in the private directory the node
// hands it as `state_dir` (memo.File owns its contract). It holds this
// plugin's memory of ITS SOURCE, never a node fact.
const CacheFile = "mail.json"

// Snapshot is the memory as the cache file holds it: every thread the memory
// has seen, each grid's membership, and the strays in the order they
// wait to be asked about.
type Snapshot struct {
	Threads []Thread          `json:"threads"`
	Grids   map[string]GridIn `json:"grids"`
	Strays  []int64           `json:"strays,omitempty"`
}

// GridIn is one grid's remembered membership: what it held, and
// whether the walk that read it reached the end of the box.
type GridIn struct {
	Complete bool    `json:"complete"`
	TopicIDs []int64 `json:"topicIds"`
}

// Snapshot copies out everything the memory holds, threads oldest first, so
// two snapshots of the same memory are byte-identical.
func (m *Memory) Snapshot() Snapshot {
	threads := m.All()
	m.mu.Lock()
	defer m.mu.Unlock()
	cols := make(map[string]GridIn, len(m.members))
	for key, ids := range m.members {
		out := make([]int64, len(ids))
		copy(out, ids)
		cols[key] = GridIn{Complete: m.complete[key], TopicIDs: out}
	}
	return Snapshot{Threads: threads, Grids: cols, Strays: m.strayOrderLocked()}
}

// Restore folds a snapshot into the memory. It is the boot path only: the
// membership it carries is taken as read, because it is this plugin's own
// last word about the same boxes. A remembered thread in no box that the
// strays do not name joins the back of the queue, so no record is kept
// without ever being asked about.
func (m *Memory) Restore(s Snapshot) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := range s.Threads {
		t := s.Threads[i]
		m.threads[t.TopicID] = &t
	}
	for key, in := range s.Grids {
		ids := make([]int64, len(in.TopicIDs))
		copy(ids, in.TopicIDs)
		m.members[key] = ids
		if in.Complete {
			m.complete[key] = true
		}
	}
	for _, id := range s.Strays {
		m.strayLocked(id)
	}
	for _, t := range s.Threads {
		if !m.memberLocked(t.TopicID) {
			m.strayLocked(t.TopicID)
		}
	}
}
