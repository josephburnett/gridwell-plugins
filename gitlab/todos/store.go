package todos

import "time"

// CacheFile names the memory's file inside the private directory the node
// hands the plugin as `state_dir`. It holds one Snapshot: the plugin's memory
// of ITS SOURCE's data, never a node fact, under cache.db's contract.
const CacheFile = "todos.json"

// CacheVersion is the Snapshot's shape, bumped whenever it changes so an
// older file is a cold start rather than a misreading.
const CacheVersion = 1

// Snapshot is what the cache file holds. Memory owns two of its facts: every
// todo it has seen, with the derived state each carries, and whether some walk
// has reached the end of GitLab's done list — the records to answer listings
// from, and the high-water mark that lets the next done walk stop at the first
// page carrying nothing unknown. WalkedAt is the plugin's flights' fact, when
// each context's last walk landed, so a restart inside the full-refresh window
// answers from the file without walking at all; Memory neither sets nor reads
// it.
type Snapshot struct {
	WalkedAt     map[string]time.Time `json:"walkedAt,omitempty"`
	DoneComplete bool                 `json:"doneComplete"`
	Todos        []Todo               `json:"todos"`
}

// Snapshot copies out everything the memory holds, oldest first, so two
// snapshots of the same memory are byte-identical.
func (m *Memory) Snapshot() Snapshot {
	m.mu.Lock()
	complete := m.doneComplete
	m.mu.Unlock()
	return Snapshot{DoneComplete: complete, Todos: m.All()}
}

// Restore folds a snapshot into the memory. The records absorb exactly as a
// walked page does, one asked for before any mark, and doneComplete only ever
// rises: a memory that has already reached the end of the done list does not
// forget it because the file was written before that walk. A restore is no
// change to announce: it is what the last process already answered.
func (m *Memory) Restore(s Snapshot) {
	m.absorb(s.Todos, 0)
	m.TakeChanges()
	if s.DoneComplete {
		m.mu.Lock()
		m.doneComplete = true
		m.mu.Unlock()
	}
}
