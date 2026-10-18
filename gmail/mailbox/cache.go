package mailbox

import (
	"sort"
	"sync"
	"time"
)

// Cache is everything the plugin has seen: the record of every message, the
// unread set, and which messages each grid last held. It survives a
// restart as a Snapshot in the plugin's cache file, which holds this
// plugin's cache of ITS SOURCE and never a node fact.
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
// and keeps everything cached below it. An email archived this morning
// leaves the grid; one from last year that the cap never reached stays.
type Cache struct {
	mu       sync.Mutex
	messages map[string]*Message
	unread   map[string]bool
	members  map[string][]string // grid key → message ids
	// complete records that some walk of that grid — this process's, or
	// one whose snapshot Restore folded back in — produced a usable
	// membership. Until every grid has had one, nothing is ever GONE: a
	// message missing from a cache nothing has walked is unseen, not deleted.
	complete map[string]bool
	// whole records that a grid's last walk read the label to its end,
	// so its membership is the label's and not only its newest part. History
	// keeps a membership exact, so only a walk changes it.
	whole map[string]bool
	// historyID is the Gmail history id the cache is current to: every
	// change Gmail recorded after it is still to be applied. Zero means none
	// is known, and only a full walk of every grid mints one.
	historyID uint64
}

// NewCache builds an empty cache.
func NewCache() *Cache {
	return &Cache{
		messages: map[string]*Message{},
		unread:   map[string]bool{},
		members:  map[string][]string{},
		complete: map[string]bool{},
		whole:    map[string]bool{},
	}
}

// Missing answers which of ids the cache holds no record for: the delta one
// walk must fetch metadata for. Everything else it already knows, and a
// message's subject, sender and date do not change once Gmail has it.
func (m *Cache) Missing(ids []string) []string {
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

// Absorb folds one walk of one grid in.
//
//	ids      what the label held, newest first, as far as the read reached
//	unread   the ids of those that also carry Gmail's UNREAD label
//	fetched  the records read for ids the cache did not have
//	whole    the read reached the end of the label
//
// A whole read replaces the membership outright. A capped one replaces it
// above the watermark — the date of the oldest message read — and keeps every
// cached member older than that, because the read proves nothing about
// what it did not reach.
func (m *Cache) Absorb(grid string, ids []string, unread map[string]bool, fetched []Message, whole bool) {
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

	m.whole[grid] = whole
	if whole {
		m.members[grid] = append([]string(nil), ids...)
		m.complete[grid] = true
		return
	}

	mark, ok := m.watermarkLocked(ids)
	if !ok {
		// A capped read with no dated message in it is no evidence at all:
		// merge, and do not claim the membership is usable.
		m.members[grid] = union(m.members[grid], ids)
		return
	}
	kept := append([]string(nil), ids...)
	have := make(map[string]bool, len(ids))
	for _, id := range ids {
		have[id] = true
	}
	for _, id := range m.members[grid] {
		if have[id] {
			continue
		}
		// Below the watermark the read said nothing, so what was cached
		// stands. A member with no record has no date to place, and losing it
		// would cost the user a tile on no evidence, so it stays too.
		msg, known := m.messages[id]
		if !known || msg.Date.Before(mark) {
			kept = append(kept, id)
		}
	}
	m.members[grid] = kept
	m.complete[grid] = true
}

// Apply folds a history catch-up in. Each fetched message is placed by the
// labels it carries now: a member of every grid whose labels it has,
// of none it lacks, and unread exactly when it carries UNREAD. Each deleted
// message leaves every grid and the cache: Gmail said it is gone. Applying
// the same catch-up twice changes nothing, which is what lets a refresh that
// read only part of it keep the old history id and read it all again.
func (m *Cache) Apply(fetched []Labelled, deleted []string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := range fetched {
		msg := fetched[i].Message
		m.messages[msg.ID] = &msg
		has := make(map[string]bool, len(fetched[i].Labels))
		for _, l := range fetched[i].Labels {
			has[l] = true
		}
		if has[UnreadLabel] {
			m.unread[msg.ID] = true
		} else {
			delete(m.unread, msg.ID)
		}
		for _, c := range Grids {
			in := true
			for _, l := range c.LabelIDs {
				in = in && has[l]
			}
			m.members[c.Key] = placed(m.members[c.Key], msg.ID, in)
		}
	}
	for _, id := range deleted {
		for _, c := range Grids {
			m.members[c.Key] = placed(m.members[c.Key], id, false)
		}
		m.forgetLocked(id)
	}
}

// Forget drops a message Gmail says it no longer has, unless some grid
// still holds it: a membership is the label's own word, newer than a lookup
// that missed. It reports whether the message was forgotten.
func (m *Cache) Forget(id string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.forgetLocked(id)
}

func (m *Cache) forgetLocked(id string) bool {
	for _, c := range Grids {
		if m.memberLocked(c.Key, id) {
			return false
		}
	}
	delete(m.messages, id)
	delete(m.unread, id)
	return true
}

// placed is ids holding id exactly when in is true, otherwise unchanged.
func placed(ids []string, id string, in bool) []string {
	for i, have := range ids {
		if have == id {
			if in {
				return ids
			}
			return append(ids[:i:i], ids[i+1:]...)
		}
	}
	if in {
		return append(ids, id)
	}
	return ids
}

// HistoryID answers the Gmail history id the cache is current to.
func (m *Cache) HistoryID() uint64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.historyID
}

// SetHistoryID records that the cache is current to id.
func (m *Cache) SetHistoryID(id uint64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.historyID = id
}

// watermarkLocked is the date of the oldest message in ids the cache has a
// record for: the line below which a capped read is silent. The caller holds
// m.mu.
func (m *Cache) watermarkLocked(ids []string) (time.Time, bool) {
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

// Grid answers one grid's messages, oldest first, so two reads
// list the same mail in the same order. A member whose record is missing is
// skipped rather than invented;
// it keeps its place in the membership and gets a tile on the next walk that
// reads its metadata.
func (m *Cache) Grid(key string) []View {
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

// AllMail answers every message some grid holds, each once, oldest
// first.
func (m *Cache) AllMail() []View {
	m.mu.Lock()
	defer m.mu.Unlock()
	var ids []string
	for _, c := range Grids {
		ids = union(ids, m.members[c.Key])
	}
	out := make([]View, 0, len(ids))
	for _, id := range ids {
		if v, ok := m.viewLocked(id); ok {
			out = append(out, v)
		}
	}
	sortViews(out)
	return out
}

// Presence is what the cache can say about a message in one grid.
type Presence int

const (
	Unknown Presence = iota
	Present
	Gone
)

// InLabel says whether a grid holds a message: Present while it does,
// Gone once its last walk read the whole label without it, Unknown when that
// walk was capped and said nothing about what it did not reach.
func (m *Cache) InLabel(grid, id string) Presence {
	m.mu.Lock()
	defer m.mu.Unlock()
	switch {
	case m.memberLocked(grid, id):
		return Present
	case m.whole[grid]:
		return Gone
	}
	return Unknown
}

// Definitive reports whether a grid's answer is the whole label: its
// last walk read to the end, and the cache holds a record for every member, so
// nothing it holds is missing from Grid.
func (m *Cache) Definitive(grid string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.whole[grid] {
		return false
	}
	for _, id := range m.members[grid] {
		if _, ok := m.messages[id]; !ok {
			return false
		}
	}
	return true
}

// View answers one message as a grid shows it, whatever grid holds it
// and whether or not one still does.
func (m *Cache) View(id string) (View, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.viewLocked(id)
}

func (m *Cache) viewLocked(id string) (View, bool) {
	msg, ok := m.messages[id]
	if !ok {
		return View{}, false
	}
	return View{Message: *msg, Unread: m.unread[id], Starred: m.memberLocked(StarredGrid, id)}, true
}

// Member reports whether some grid currently holds the message.
func (m *Cache) Member(id string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, c := range Grids {
		if m.memberLocked(c.Key, id) {
			return true
		}
	}
	return false
}

func (m *Cache) memberLocked(grid, id string) bool {
	for _, have := range m.members[grid] {
		if have == id {
			return true
		}
	}
	return false
}

// Shows reports whether the cache has an answer for the grid: some
// walk produced a usable membership, or it holds a member. It is the line
// between a warm read, answered at once, and a cold one, which waits on the
// walk.
func (m *Cache) Shows(key string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.complete[key] || len(m.members[key]) > 0
}

// Swept reports whether every grid has produced a usable membership at
// least once. It is the one gate on answering GONE: only a pass over all of
// them can say a message is in none.
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

// All answers every cached message, oldest first.
func (m *Cache) All() []View {
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

// Snapshot is everything the cache holds, as the plugin's cache file keeps
// it: every message seen, the unread set, each grid's membership, and
// the history id all of it is current to.
type Snapshot struct {
	Messages  []Message         `json:"messages"`
	Unread    []string          `json:"unread"`
	Grids     map[string]GridIn `json:"grids"`
	HistoryID uint64            `json:"historyId,omitempty"`
}

// GridIn is one grid's cached membership: what it held,
// whether the walk that read it produced a usable membership, and whether it
// read the whole label.
type GridIn struct {
	Complete bool     `json:"complete"`
	Whole    bool     `json:"whole,omitempty"`
	IDs      []string `json:"ids"`
}

// Snapshot copies out everything the cache holds, messages oldest first and
// the unread set in the same order, so two snapshots of the same cache are
// byte-identical.
func (m *Cache) Snapshot() Snapshot {
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
	cols := make(map[string]GridIn, len(m.members))
	for key, ids := range m.members {
		out := make([]string, len(ids))
		copy(out, ids)
		cols[key] = GridIn{Complete: m.complete[key], Whole: m.whole[key], IDs: out}
	}
	return Snapshot{Messages: msgs, Unread: unread, Grids: cols, HistoryID: m.historyID}
}

// Restore folds a snapshot into the cache. It is the boot path only: the
// membership it carries is taken as read, because it is this plugin's own
// last word about the same grids.
func (m *Cache) Restore(s Snapshot) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := range s.Messages {
		msg := s.Messages[i]
		m.messages[msg.ID] = &msg
	}
	for _, id := range s.Unread {
		m.unread[id] = true
	}
	for key, in := range s.Grids {
		ids := make([]string, len(in.IDs))
		copy(ids, in.IDs)
		m.members[key] = ids
		if in.Complete {
			m.complete[key] = true
		}
		m.whole[key] = in.Whole
	}
	m.historyID = s.HistoryID
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
