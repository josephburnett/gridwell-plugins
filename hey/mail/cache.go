package mail

import (
	"cmp"
	"slices"
	"sort"
	"sync"
	"time"
)

// Cache is everything the plugin has seen: the record of every thread, and
// which threads each grid last held. It survives a restart through the
// cache file in the plugin's state directory — see Snapshot and store.go —
// which holds this plugin's cache of ITS SOURCE and never a node fact.
//
// A grid's membership is REPLACED by a walk that read the whole box,
// and only merged into by one that did not: a box listing is a live
// membership, so a thread archived at HEY must leave the grid, but a walk
// that stopped short proves nothing about what it did not reach. Between
// walks, the CLI's live feed moves it one posting at a time (Apply). A
// thread's record outlives its membership, so a tile whose thread has left
// every box still reads as the email it was, until HEY says the thread is
// gone (Forget).
type Cache struct {
	mu      sync.Mutex
	threads map[int64]*Thread
	members map[string][]int64 // grid key → topic ids
	// postings maps each member of a grid to the box item that holds it
	// there, because a deleted line on the live feed names only the posting.
	// A member a restored snapshot brought back has no entry until the next
	// walk of its box.
	postings map[string]map[int64]int64 // grid key → topic id → posting id
	// complete records that some walk of that grid — this process's, or
	// one whose snapshot Restore folded back in — read the box to its end.
	// Until every grid has had one, nothing is ever GONE: a thread
	// missing from a partial sweep is unseen, not archived.
	complete map[string]bool
	// journal holds, per grid with a walk open, every event applied
	// since the walk began. The walk's answer predates them, so Absorb
	// replays them over it.
	journal map[string][]Event
	// left holds, per grid, the threads the feed said left it: a
	// delete of the posting that held them. It is how a box no walk reads
	// whole (a capped one) can still say a thread is not in it.
	left map[string]map[int64]bool
	// strays are threads that left every box: the candidates to forget, once
	// HEY says it no longer has them. Each maps to its place in the queue
	// TakeStrays serves, smallest first; next is the place after the last.
	strays map[int64]uint64
	next   uint64
	// moved holds the threads whose stamp moved since the last effect.
	moved map[int64]bool
}

// NewCache builds an empty cache.
func NewCache() *Cache {
	return &Cache{
		threads:  map[int64]*Thread{},
		members:  map[string][]int64{},
		postings: map[string]map[int64]int64{},
		complete: map[string]bool{},
		journal:  map[string][]Event{},
		left:     map[string]map[int64]bool{},
		strays:   map[int64]uint64{},
		moved:    map[int64]bool{},
	}
}

// BeginWalk opens a read of one grid's box: from here until its Absorb
// or EndWalk, every event applied to the grid is kept and replayed over
// the read's answer. One walk per grid at a time.
func (m *Cache) BeginWalk(grid string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.journal[grid] = []Event{}
}

// EndWalk closes a read that brought no answer.
func (m *Cache) EndWalk(grid string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.journal, grid)
}

// Absorb folds one walk of one grid in and closes it. read is what the
// walk saw, whole is whether it reached the end of the box. A whole walk
// replaces the grid's membership; a partial one adds to it, because
// absence in a partial read is not evidence. The events applied while the
// walk was open are then replayed in order, so the feed's word stands over a
// read that began before it.
func (m *Cache) Absorb(grid string, read []Thread, whole bool) Effect {
	m.mu.Lock()
	defer m.mu.Unlock()
	before, beforeAll := m.gridLocked(grid), m.everythingLocked()

	seen := make([]int64, 0, len(read))
	posts := m.postings[grid]
	if whole || posts == nil {
		posts = map[int64]int64{}
	}
	for i := range read {
		t := read[i]
		t.Grid = grid
		m.putLocked(&t)
		seen = append(seen, t.TopicID)
		posts[t.TopicID] = t.PostingID
	}
	m.postings[grid] = posts
	if whole {
		m.members[grid] = seen
		m.complete[grid] = true
		delete(m.left, grid)
	} else {
		m.members[grid] = union(m.members[grid], seen)
		for _, id := range seen {
			delete(m.left[grid], id)
		}
	}
	for _, ev := range m.journal[grid] {
		m.applyLocked(grid, ev)
	}
	delete(m.journal, grid)
	return m.effectLocked(grid, before, beforeAll)
}

// Apply folds one added, updated or deleted event about one grid in.
// A grid no walk has listed yet takes no membership from an event —
// the listing a first walk brings is the whole one, and an event's thread
// alone is not — but the thread's record is kept, and the event is replayed
// over that walk if one is open.
func (m *Cache) Apply(grid string, ev Event) Effect {
	m.mu.Lock()
	defer m.mu.Unlock()
	before, beforeAll := m.gridLocked(grid), m.everythingLocked()
	rewalk := m.applyLocked(grid, ev)
	if j, open := m.journal[grid]; open {
		m.journal[grid] = append(j, ev)
	}
	eff := m.effectLocked(grid, before, beforeAll)
	eff.Rewalk = rewalk
	return eff
}

// effectLocked is what a change to one grid did, measured against
// the listings from before it. A thread that left everything becomes a
// stray; one that came back is none.
func (m *Cache) effectLocked(grid string, before, beforeAll []Thread) Effect {
	all := m.everythingLocked()
	in := make(map[int64]bool, len(all))
	for i := range all {
		in[all[i].TopicID] = true
		delete(m.strays, all[i].TopicID)
	}
	for i := range beforeAll {
		if id := beforeAll[i].TopicID; !in[id] {
			m.strayLocked(id)
		}
	}
	eff := Effect{Changed: !sameListing(before, m.gridLocked(grid)),
		Everything: !sameListing(beforeAll, all)}
	for id := range m.moved {
		eff.Moved = append(eff.Moved, id)
	}
	slices.Sort(eff.Moved)
	clear(m.moved)
	return eff
}

// putLocked records t. A thread whose known stamp moved to another known one
// changed in place, a reply landed on it, and is noted moved; a stamp first
// learned, or a read that does not say, is no change to the page anyone saw.
func (m *Cache) putLocked(t *Thread) {
	if was, ok := m.threads[t.TopicID]; ok && !was.ActiveAt.IsZero() && !t.ActiveAt.IsZero() && !was.ActiveAt.Equal(t.ActiveAt) {
		m.moved[t.TopicID] = true
	}
	m.threads[t.TopicID] = t
}

// TakeStrays hands out the n threads that left every box and have waited
// longest to be asked about. The caller asks HEY whether each still exists
// and answers with Forget or Stray.
func (m *Cache) TakeStrays(n int) []int64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := m.strayOrderLocked()
	out = out[:min(n, len(out))]
	for _, id := range out {
		delete(m.strays, id)
	}
	return out
}

// Stray hands a thread back to the end of the queue, to be asked about again.
func (m *Cache) Stray(topicID int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, known := m.threads[topicID]; known && !m.memberLocked(topicID) {
		m.strayLocked(topicID)
	}
}

func (m *Cache) strayLocked(topicID int64) {
	if _, queued := m.strays[topicID]; !queued {
		m.strays[topicID] = m.next
		m.next++
	}
}

func (m *Cache) strayOrderLocked() []int64 {
	out := make([]int64, 0, len(m.strays))
	for id := range m.strays {
		out = append(out, id)
	}
	slices.SortFunc(out, func(a, b int64) int { return cmp.Compare(m.strays[a], m.strays[b]) })
	return out
}

// Forget drops a thread's record once HEY said it no longer has it. A thread
// some box has listed again since is kept.
func (m *Cache) Forget(topicID int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.memberLocked(topicID) {
		delete(m.threads, topicID)
		delete(m.strays, topicID)
	}
}

// applyLocked is Apply without the bookkeeping; it reports whether only a
// walk can apply the event.
func (m *Cache) applyLocked(grid string, ev Event) (rewalk bool) {
	switch ev.Change {
	case ChangeAdded, ChangeUpdated:
		if ev.Thread.TopicID == 0 {
			return false
		}
		t := ev.Thread
		t.Grid = grid
		m.putLocked(&t)

		ids, listed := m.members[grid]
		if !listed {
			return false
		}
		m.members[grid] = union(ids, []int64{t.TopicID})
		delete(m.left[grid], t.TopicID)
		if m.postings[grid] == nil {
			m.postings[grid] = map[int64]int64{}
		}
		m.postings[grid][t.TopicID] = t.PostingID
	case ChangeDeleted:
		posts := m.postings[grid]
		for topic, posting := range posts {
			if posting == ev.PostingID {
				delete(posts, topic)
				m.members[grid] = without(m.members[grid], topic)
				if m.left[grid] == nil {
					m.left[grid] = map[int64]bool{}
				}
				m.left[grid][topic] = true
				return false
			}
		}
		// Not a posting this cache maps. If every member is mapped, it is
		// not one the listing shows — a bundle — and nothing changes;
		// otherwise it may be a restored member, and only a read can say.
		for _, id := range m.members[grid] {
			if _, mapped := posts[id]; !mapped {
				return true
			}
		}
	}
	return false
}

// Shows reports whether the cache has a listing of the grid to
// answer with — a walk, or the snapshot of one, even of an empty box. It is
// the one owner of "warm".
func (m *Cache) Shows(grid string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.members[grid]
	return ok
}

// without drops id from ids, keeping order.
func without(ids []int64, id int64) []int64 {
	out := make([]int64, 0, len(ids))
	for _, x := range ids {
		if x != id {
			out = append(out, x)
		}
	}
	return out
}

// sameListing reports whether two listings show the same thing. Grid
// is left out: it is where a thread was last seen, which a thread held by
// two boxes flips between, and no listing shows it.
func sameListing(a, b []Thread) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		x, y := a[i], b[i]
		if !x.CreatedAt.Equal(y.CreatedAt) {
			return false
		}
		x.Grid, y.Grid = "", ""
		x.CreatedAt, y.CreatedAt = time.Time{}, time.Time{}
		if x != y {
			return false
		}
	}
	return true
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

// Grid answers one grid's threads, oldest first — the order the
// placement hints are derived from, so two nodes lay the same mail out the
// same way. A thread whose record is missing is skipped rather than invented.
func (m *Cache) Grid(key string) []Thread {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.gridLocked(key)
}

func (m *Cache) gridLocked(key string) []Thread {
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

// Everything answers every thread some grid holds, each once, oldest
// first.
func (m *Cache) Everything() []Thread {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.everythingLocked()
}

func (m *Cache) everythingLocked() []Thread {
	var ids []int64
	for _, c := range Grids {
		ids = union(ids, m.members[c.Key])
	}
	out := make([]Thread, 0, len(ids))
	for _, id := range ids {
		if t, ok := m.threads[id]; ok {
			out = append(out, *t)
		}
	}
	sortThreads(out)
	return out
}

// Presence is what the cache can say about a thread in one grid.
type Presence int

const (
	Unknown Presence = iota
	Present
	Gone
)

// InBox says whether a grid holds a thread: Present while it does,
// Gone once a whole walk did not list it or the feed deleted the posting
// that held it, and Unknown otherwise.
func (m *Cache) InBox(grid string, topicID int64) Presence {
	m.mu.Lock()
	defer m.mu.Unlock()
	switch {
	case slices.Contains(m.members[grid], topicID):
		return Present
	case m.complete[grid] || m.left[grid][topicID]:
		return Gone
	}
	return Unknown
}

// Get answers one thread's record, whatever grid it is in and whether
// or not it still is in one.
func (m *Cache) Get(topicID int64) (Thread, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	t, ok := m.threads[topicID]
	if !ok {
		return Thread{}, false
	}
	return *t, true
}

// Member reports whether some grid currently holds the thread.
func (m *Cache) Member(topicID int64) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.memberLocked(topicID)
}

func (m *Cache) memberLocked(topicID int64) bool {
	for _, ids := range m.members {
		if slices.Contains(ids, topicID) {
			return true
		}
	}
	return false
}

// Swept reports whether every grid has been read to its end at least
// once. It is the one gate on answering GONE: only a complete sweep of all
// boxes can say a thread is in none of them.
func (m *Cache) Swept() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, c := range Grids {
		if !m.complete[c.Key] {
			return false
		}
	}
	return true
}

// All answers every cached thread, oldest first.
func (m *Cache) All() []Thread {
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
