package mailbox

import (
	"sort"
	"sync"
	"time"
)

// Memory is everything the plugin has seen: the record of every message, the
// unread set, and which messages each collection last held. It survives a
// restart through the cache file in the plugin's state directory — see
// Snapshot and store.go — which holds this plugin's memory of ITS SOURCE and
// never a node fact.
//
// It is the one owner of every fact about a message that can change. A
// Message record holds only what cannot: the id, the subject, the sender, the
// date. Read/unread and starred are read from here, so a message that is in
// both grids reads the same in each and its tile body does not flip with the
// order the walks ran in.
//
// The thing that makes a capped read usable is the WATERMARK. Gmail lists
// newest first, so a read that stopped at 500 messages still says exactly
// which of the newest 500 the label holds — it says nothing about older ones.
// Absorb therefore replaces the membership above the oldest message it read
// and keeps everything remembered below it. An email archived this morning
// leaves the grid; one from last year that the cap never reached stays.
type Memory struct {
	mu       sync.Mutex
	messages map[string]*Message
	unread   map[string]bool
	members  map[string][]string // collection key → message ids
	// complete records that some walk of that collection — this process's, or
	// one whose snapshot Restore folded back in — produced a usable
	// membership. Until every collection has had one, nothing is ever GONE: a
	// message missing from a memory nothing has walked is unseen, not deleted.
	complete map[string]bool
}

// NewMemory builds an empty memory.
func NewMemory() *Memory {
	return &Memory{
		messages: map[string]*Message{},
		unread:   map[string]bool{},
		members:  map[string][]string{},
		complete: map[string]bool{},
	}
}

// Missing answers which of ids the memory holds no record for: the delta one
// walk must fetch metadata for. Everything else it already knows, and a
// message's subject, sender and date do not change once Gmail has it.
func (m *Memory) Missing(ids []string) []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []string
	for _, id := range ids {
		if _, ok := m.messages[id]; !ok {
			out = append(out, id)
		}
	}
	return out
}

// Absorb folds one walk of one collection in.
//
//	ids      what the label held, newest first, as far as the read reached
//	unread   the ids of those that also carry Gmail's UNREAD label
//	fetched  the records read for ids the memory did not have
//	whole    the read reached the end of the label
//
// A whole read replaces the membership outright. A capped one replaces it
// above the watermark — the date of the oldest message read — and keeps every
// remembered member older than that, because the read proves nothing about
// what it did not reach.
func (m *Memory) Absorb(collection string, ids []string, unread map[string]bool, fetched []Message, whole bool) {
	m.mu.Lock()
	defer m.mu.Unlock()

	for i := range fetched {
		msg := fetched[i]
		m.messages[msg.ID] = &msg
	}
	for _, id := range ids {
		if unread[id] {
			m.unread[id] = true
		} else {
			delete(m.unread, id)
		}
	}

	if whole {
		m.members[collection] = append([]string(nil), ids...)
		m.complete[collection] = true
		return
	}

	mark, ok := m.watermarkLocked(ids)
	if !ok {
		// A capped read with no dated message in it is no evidence at all:
		// merge, and do not claim the membership is usable.
		m.members[collection] = union(m.members[collection], ids)
		return
	}
	kept := append([]string(nil), ids...)
	have := make(map[string]bool, len(ids))
	for _, id := range ids {
		have[id] = true
	}
	for _, id := range m.members[collection] {
		if have[id] {
			continue
		}
		// Below the watermark the read said nothing, so what was remembered
		// stands. A member with no record has no date to place, and losing it
		// would cost the user a tile on no evidence, so it stays too.
		msg, known := m.messages[id]
		if !known || msg.Date.Before(mark) {
			kept = append(kept, id)
		}
	}
	m.members[collection] = kept
	m.complete[collection] = true
}

// watermarkLocked is the date of the oldest message in ids the memory has a
// record for: the line below which a capped read is silent. The caller holds
// m.mu.
func (m *Memory) watermarkLocked(ids []string) (time.Time, bool) {
	var mark time.Time
	for _, id := range ids {
		msg, ok := m.messages[id]
		if !ok || msg.Date.IsZero() {
			continue
		}
		if mark.IsZero() || msg.Date.Before(mark) {
			mark = msg.Date
		}
	}
	return mark, !mark.IsZero()
}

// union appends the ids of b that a does not already hold, keeping a's order.
func union(a, b []string) []string {
	have := make(map[string]bool, len(a))
	out := make([]string, 0, len(a)+len(b))
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

// Collection answers one collection's messages, oldest first — the order the
// placement hints are derived from, so two nodes lay the same mail out the
// same way. A member whose record is missing is skipped rather than invented;
// it keeps its place in the membership and gets a tile on the next walk that
// reads its metadata.
func (m *Memory) Collection(key string) []View {
	m.mu.Lock()
	defer m.mu.Unlock()
	ids := m.members[key]
	out := make([]View, 0, len(ids))
	for _, id := range ids {
		if v, ok := m.viewLocked(id); ok {
			out = append(out, v)
		}
	}
	sortViews(out)
	return out
}

// View answers one message as a grid shows it, whatever collection holds it
// and whether or not one still does.
func (m *Memory) View(id string) (View, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.viewLocked(id)
}

func (m *Memory) viewLocked(id string) (View, bool) {
	msg, ok := m.messages[id]
	if !ok {
		return View{}, false
	}
	return View{Message: *msg, Unread: m.unread[id], Starred: m.memberLocked(StarredContext, id)}, true
}

// Member reports whether some collection currently holds the message.
func (m *Memory) Member(id string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, c := range Collections {
		if m.memberLocked(c.Key, id) {
			return true
		}
	}
	return false
}

func (m *Memory) memberLocked(collection, id string) bool {
	for _, have := range m.members[collection] {
		if have == id {
			return true
		}
	}
	return false
}

// Swept reports whether every collection has produced a usable membership at
// least once. It is the one gate on answering GONE: only a pass over all of
// them can say a message is in none.
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

// All answers every remembered message, oldest first.
func (m *Memory) All() []View {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]View, 0, len(m.messages))
	for id := range m.messages {
		if v, ok := m.viewLocked(id); ok {
			out = append(out, v)
		}
	}
	sortViews(out)
	return out
}

// sortViews orders by date, then by id, so the order is total: two messages
// that landed in the same instant must not swap between two reads.
func sortViews(vs []View) {
	sort.Slice(vs, func(i, j int) bool {
		if !vs[i].Date.Equal(vs[j].Date) {
			return vs[i].Date.Before(vs[j].Date)
		}
		return vs[i].ID < vs[j].ID
	})
}
