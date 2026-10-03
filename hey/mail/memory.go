package mail

import (
	"slices"
	"sort"
	"sync"
	"time"
)

// Memory is everything the plugin has seen: the record of every thread, and
// which threads each collection last held. It survives a restart through the
// cache file in the plugin's state directory — see Snapshot and store.go —
// which holds this plugin's memory of ITS SOURCE and never a node fact.
//
// A collection's membership is REPLACED by a walk that read the whole box,
// and only merged into by one that did not: a box listing is a live
// membership, so a thread archived at HEY must leave the grid, but a walk
// that stopped short proves nothing about what it did not reach. Between
// walks, the CLI's live feed moves it one posting at a time (Apply). A
// thread's record outlives its membership, so a tile whose thread has left
// every box still reads as the email it was, until HEY says the thread is
// gone (Forget).
type Memory struct {
	mu      sync.Mutex
	threads map[int64]*Thread
	members map[string][]int64 // collection key → topic ids
	// postings maps each member of a collection to the box item that holds it
	// there, because a deleted line on the live feed names only the posting.
	// A member a restored snapshot brought back has no entry until the next
	// walk of its box.
	postings map[string]map[int64]int64 // collection key → topic id → posting id
	// complete records that some walk of that collection — this process's, or
	// one whose snapshot Restore folded back in — read the box to its end.
	// Until every collection has had one, nothing is ever GONE: a thread
	// missing from a partial sweep is unseen, not archived.
	complete map[string]bool
	// journal holds, per collection with a walk open, every event applied
	// since the walk began. The walk's answer predates them, so Absorb
	// replays them over it.
	journal map[string][]Event
	// left holds, per collection, the threads the feed said left it: a
	// delete of the posting that held them. It is how a box no walk reads
	// whole (a capped one) can still say a thread is not in it.
	left map[string]map[int64]bool
	// strays are threads that left every box and have not been asked about
	// since: the candidates to forget, once HEY says it no longer has them.
	strays map[int64]bool
}

// NewMemory builds an empty memory.
func NewMemory() *Memory {
	return &Memory{
		threads:  map[int64]*Thread{},
		members:  map[string][]int64{},
		postings: map[string]map[int64]int64{},
		complete: map[string]bool{},
		journal:  map[string][]Event{},
		left:     map[string]map[int64]bool{},
		strays:   map[int64]bool{},
	}
}

// BeginWalk opens a read of one collection's box: from here until its Absorb
// or EndWalk, every event applied to the collection is kept and replayed over
// the read's answer. One walk per collection at a time.
func (m *Memory) BeginWalk(collection string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.journal[collection] = []Event{}
}

// EndWalk closes a read that brought no answer.
func (m *Memory) EndWalk(collection string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.journal, collection)
}

// Absorb folds one walk of one collection in and closes it. read is what the
// walk saw, whole is whether it reached the end of the box. A whole walk
// replaces the collection's membership; a partial one adds to it, because
// absence in a partial read is not evidence. The events applied while the
// walk was open are then replayed in order, so the feed's word stands over a
// read that began before it.
func (m *Memory) Absorb(collection string, read []Thread, whole bool) Effect {
	m.mu.Lock()
	defer m.mu.Unlock()
	before, beforeAll := m.collectionLocked(collection), m.everythingLocked()

	seen := make([]int64, 0, len(read))
	posts := m.postings[collection]
	if whole || posts == nil {
		posts = map[int64]int64{}
	}
	for i := range read {
		t := read[i]
		t.Collection = collection
		m.threads[t.TopicID] = &t
		seen = append(seen, t.TopicID)
		posts[t.TopicID] = t.PostingID
	}
	m.postings[collection] = posts
	if whole {
		m.members[collection] = seen
		m.complete[collection] = true
		delete(m.left, collection)
	} else {
		m.members[collection] = union(m.members[collection], seen)
		for _, id := range seen {
			delete(m.left[collection], id)
		}
	}
	for _, ev := range m.journal[collection] {
		m.applyLocked(collection, ev)
	}
	delete(m.journal, collection)
	return m.effectLocked(collection, before, beforeAll)
}

// Apply folds one added, updated or deleted event about one collection in.
// A collection no walk has listed yet takes no membership from an event —
// the listing a first walk brings is the whole one, and an event's thread
// alone is not — but the thread's record is kept, and the event is replayed
// over that walk if one is open.
func (m *Memory) Apply(collection string, ev Event) Effect {
	m.mu.Lock()
	defer m.mu.Unlock()
	before, beforeAll := m.collectionLocked(collection), m.everythingLocked()
	rewalk := m.applyLocked(collection, ev)
	if j, open := m.journal[collection]; open {
		m.journal[collection] = append(j, ev)
	}
	eff := m.effectLocked(collection, before, beforeAll)
	eff.Rewalk = rewalk
	return eff
}

// effectLocked is what a change to one collection did, measured against
// the listings from before it. A thread that left everything becomes a
// stray; one that came back is none.
func (m *Memory) effectLocked(collection string, before, beforeAll []Thread) Effect {
	all := m.everythingLocked()
	in := make(map[int64]bool, len(all))
	for i := range all {
		in[all[i].TopicID] = true
		delete(m.strays, all[i].TopicID)
	}
	for i := range beforeAll {
		if id := beforeAll[i].TopicID; !in[id] {
			m.strays[id] = true
		}
	}
	return Effect{Changed: !sameListing(before, m.collectionLocked(collection)),
		Everything: !sameListing(beforeAll, all)}
}

// TakeStrays hands out at most n threads that left every box and have not
// been asked about since. The caller asks HEY whether each still exists and
// answers with Forget or Stray.
func (m *Memory) TakeStrays(n int) []int64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]int64, 0, min(n, len(m.strays)))
	for id := range m.strays {
		if len(out) == n {
			break
		}
		out = append(out, id)
		delete(m.strays, id)
	}
	slices.Sort(out)
	return out
}

// Stray hands a thread back to be asked about again: the answer was doubt.
func (m *Memory) Stray(topicID int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, known := m.threads[topicID]; known && !m.memberLocked(topicID) {
		m.strays[topicID] = true
	}
}

// Forget drops a thread's record once HEY said it no longer has it. A thread
// some box has listed again since is kept.
func (m *Memory) Forget(topicID int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.memberLocked(topicID) {
		delete(m.threads, topicID)
		delete(m.strays, topicID)
	}
}

// applyLocked is Apply without the bookkeeping; it reports whether only a
// walk can apply the event.
func (m *Memory) applyLocked(collection string, ev Event) (rewalk bool) {
	switch ev.Change {
	case ChangeAdded, ChangeUpdated:
		if ev.Thread.TopicID == 0 {
			return false
		}
		t := ev.Thread
		t.Collection = collection
		m.threads[t.TopicID] = &t
		ids, listed := m.members[collection]
		if !listed {
			return false
		}
		m.members[collection] = union(ids, []int64{t.TopicID})
		delete(m.left[collection], t.TopicID)
		if m.postings[collection] == nil {
			m.postings[collection] = map[int64]int64{}
		}
		m.postings[collection][t.TopicID] = t.PostingID
	case ChangeDeleted:
		posts := m.postings[collection]
		for topic, posting := range posts {
			if posting == ev.PostingID {
				delete(posts, topic)
				m.members[collection] = without(m.members[collection], topic)
				if m.left[collection] == nil {
					m.left[collection] = map[int64]bool{}
				}
				m.left[collection][topic] = true
				return false
			}
		}
		// Not a posting this memory maps. If every member is mapped, it is
		// not one the listing shows — a bundle — and nothing changes;
		// otherwise it may be a restored member, and only a read can say.
		for _, id := range m.members[collection] {
			if _, mapped := posts[id]; !mapped {
				return true
			}
		}
	}
	return false
}

// Shows reports whether the memory has a listing of the collection to
// answer with — a walk, or the snapshot of one, even of an empty box. It is
// the one owner of "warm".
func (m *Memory) Shows(collection string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.members[collection]
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

// sameListing reports whether two listings show the same thing. Collection
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
		x.Collection, y.Collection = "", ""
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

// Collection answers one collection's threads, oldest first — the order the
// placement hints are derived from, so two nodes lay the same mail out the
// same way. A thread whose record is missing is skipped rather than invented.
func (m *Memory) Collection(key string) []Thread {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.collectionLocked(key)
}

func (m *Memory) collectionLocked(key string) []Thread {
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

// Everything answers every thread some collection holds, each once, oldest
// first.
func (m *Memory) Everything() []Thread {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.everythingLocked()
}

func (m *Memory) everythingLocked() []Thread {
	var ids []int64
	for _, c := range Collections {
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

// Presence is what memory can say about a thread in one collection.
type Presence int

const (
	Unknown Presence = iota
	Present
	Gone
)

// InBox says whether a collection holds a thread: Present while it does,
// Gone once a whole walk did not list it or the feed deleted the posting
// that held it, and Unknown otherwise.
func (m *Memory) InBox(collection string, topicID int64) Presence {
	m.mu.Lock()
	defer m.mu.Unlock()
	switch {
	case slices.Contains(m.members[collection], topicID):
		return Present
	case m.complete[collection] || m.left[collection][topicID]:
		return Gone
	}
	return Unknown
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
	return m.memberLocked(topicID)
}

func (m *Memory) memberLocked(topicID int64) bool {
	for _, ids := range m.members {
		if slices.Contains(ids, topicID) {
			return true
		}
	}
	return false
}

// Swept reports whether every collection has been read to its end at least
// once. It is the one gate on answering GONE: only a complete sweep of all
// boxes can say a thread is in none of them.
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
