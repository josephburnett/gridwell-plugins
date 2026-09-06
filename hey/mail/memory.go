package mail

import (
	"sort"
	"sync"
)

// Memory is everything the plugin has seen: the record of every thread, and
// which threads each collection last held. It survives a restart through the
// cache file in the plugin's state directory — see Snapshot and store.go —
// which holds this plugin's memory of ITS SOURCE and never a node fact.
//
// A collection's membership is REPLACED by a walk that read the whole box,
// and only merged into by one that did not: a box listing is a live
// membership, so a thread archived at HEY must leave the grid, but a walk
// that stopped short proves nothing about what it did not reach. The thread
// record itself is never removed, so a tile whose thread has left still reads
// as the email it was.
type Memory struct {
	mu      sync.Mutex
	threads map[int64]*Thread
	members map[string][]int64 // collection key → topic ids
	// complete records that some walk of that collection — this process's, or
	// one whose snapshot Restore folded back in — read the box to its end.
	// Until every collection has had one, nothing is ever GONE: a thread
	// missing from a partial sweep is unseen, not archived.
	complete map[string]bool
}

// NewMemory builds an empty memory.
func NewMemory() *Memory {
	return &Memory{
		threads:  map[int64]*Thread{},
		members:  map[string][]int64{},
		complete: map[string]bool{},
	}
}

// Absorb folds one walk of one collection in. read is what the walk saw,
// whole is whether it reached the end of the box. A whole walk replaces the
// collection's membership; a partial one adds to it, because absence in a
// partial read is not evidence.
func (m *Memory) Absorb(collection string, read []Thread, whole bool) {
	m.mu.Lock()
	defer m.mu.Unlock()

	seen := make([]int64, 0, len(read))
	for i := range read {
		t := read[i]
		t.Collection = collection
		m.threads[t.TopicID] = &t
		seen = append(seen, t.TopicID)
	}
	if whole {
		m.members[collection] = seen
		m.complete[collection] = true
		return
	}
	m.members[collection] = union(m.members[collection], seen)
}

// union appends the ids of b that a does not already hold, keeping a's order.
func union(a, b []int64) []int64 {
	have := make(map[int64]bool, len(a))
	out := make([]int64, 0, len(a)+len(b))
	for _, id := range a {
		have[id] = true
		out = append(out, id)
	}
	for _, id := range b {
		if !have[id] {
			have[id] = true
			out = append(out, id)
		}
	}
	return out
}

// Collection answers one collection's threads, oldest first — the order the
// placement hints are derived from, so two nodes lay the same mail out the
// same way. A thread whose record is missing is skipped rather than invented.
func (m *Memory) Collection(key string) []Thread {
	m.mu.Lock()
	defer m.mu.Unlock()
	ids := m.members[key]
	out := make([]Thread, 0, len(ids))
	for _, id := range ids {
		if t, ok := m.threads[id]; ok {
			out = append(out, *t)
		}
	}
	sortThreads(out)
	return out
}

// Get answers one thread's record, whatever collection it is in and whether
// or not it still is in one.
func (m *Memory) Get(topicID int64) (Thread, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	t, ok := m.threads[topicID]
	if !ok {
		return Thread{}, false
	}
	return *t, true
}

// Member reports whether some collection currently holds the thread.
func (m *Memory) Member(topicID int64) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, ids := range m.members {
		for _, id := range ids {
			if id == topicID {
				return true
			}
		}
	}
	return false
}

// Swept reports whether every collection has been read to its end at least
// once. It is the one gate on answering GONE: only a complete sweep of all
// three can say a thread is in none of them.
func (m *Memory) Swept() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, c := range Collections {
		if !m.complete[c.Key] {
			return false
		}
	}
	return true
}

// All answers every remembered thread, oldest first.
func (m *Memory) All() []Thread {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Thread, 0, len(m.threads))
	for _, t := range m.threads {
		out = append(out, *t)
	}
	sortThreads(out)
	return out
}

// sortThreads orders by arrival, then by id, so the order is total: two
// threads that landed in the same instant must not swap between two reads.
func sortThreads(ts []Thread) {
	sort.Slice(ts, func(i, j int) bool {
		if !ts[i].CreatedAt.Equal(ts[j].CreatedAt) {
			return ts[i].CreatedAt.Before(ts[j].CreatedAt)
		}
		return ts[i].TopicID < ts[j].TopicID
	})
}
