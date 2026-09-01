package todos

import (
	"context"
	"log"
	"sort"
	"sync"
	"time"
)

// Source pages GitLab's todo list for one state, NEWEST FIRST (GitLab
// orders /todos by id descending, and ids rise with creation). Page
// numbers start at 1; more is false on the last page. The ordering is
// verified per page, never assumed — see Sync.
type Source interface {
	Page(ctx context.Context, state string, page int) (todos []Todo, more bool, err error)
}

// Memory is everything the plugin has seen, keyed by todo id. A todo that
// vanishes from GitLab keeps its record here and shows as done; nothing is
// ever removed. It survives a restart through the cache file in the plugin's
// state directory — see Snapshot and store.go — which holds this plugin's
// memory of ITS SOURCE and never a node fact. The node keeps its own
// read-through cache of what the plugin last said; this one only saves the
// walk.
type Memory struct {
	mu    sync.Mutex
	todos map[int64]*Todo
	// doneComplete records that some walk — this process's, or one whose
	// snapshot Restore folded back in — reached the end of the done list.
	// Only then does a page of already-known done todos prove every older one
	// is known: a targeted week walk stops at the week boundary having
	// absorbed one page past it, so until a walk has run to the end, a
	// fully-known page proves nothing about the rest.
	doneComplete bool
	// resumes is where a failed walk stopped, by the window it was walking.
	// A walk that succeeds leaves none.
	resumes map[string]*resumePoint
}

// resumePoint is a failed walk's mark: the phase that failed, and the page the
// next walk over the same window starts at — one before the failure. The
// overlap is free, because absorb is idempotent, and it re-reads the page the
// list may have shifted items onto while the walk was down.
type resumePoint struct {
	state string // StatePending or StateDone
	page  int
}

// NewMemory builds an empty memory.
func NewMemory() *Memory {
	return &Memory{todos: map[int64]*Todo{}, resumes: map[string]*resumePoint{}}
}

// Sync refreshes the memory from src. A zero since walks everything: every
// pending page, then done pages to the end. Once a walk has reached that end,
// later walks stop at the first page that carries nothing new, because a page
// of already-known done todos then means every older one is known too, since
// done todos only enter at their own position. A non-zero since is the
// targeted walk for one week: both states stop as soon as a page reaches todos
// created before since.
//
// Completion is derived: a remembered pending todo the pending walk did not
// see, within the walk's coverage, is done, whether it was marked done or
// deleted with its target. That derivation is the only place a todo's state
// changes without GitLab saying so, and it is only safe when the walk really
// covered the todo's creation time. A page that is not newest-first therefore
// disables the early stop, and the walk runs to the end, rather than risk
// marking live todos done.
//
// A walk that fails keeps everything it absorbed and leaves a mark: the next
// walk over the same window starts one page before the failure instead of at
// the first page again, so a lid closing mid-walk costs a page rather than
// dozens. A resumed pending walk does NOT derive completion — it never saw
// the pages the failed attempt covered, and the list may have shifted items
// across the seam in between, so absence proves nothing. Only a walk that
// started at the first pending page judges absence; the next one does, one
// refresh later.
func (m *Memory) Sync(ctx context.Context, src Source, since time.Time) error {
	pendingFrom, doneFrom := 1, 1
	if r := m.takeResume(since); r != nil {
		if r.state == StatePending {
			pendingFrom = r.page
		} else {
			// The pending half finished, and judged absence, in the attempt
			// that went on to fail in the done list. Walking it again would
			// only re-page GitLab for what is already known.
			pendingFrom, doneFrom = 0, r.page
		}
	}

	if pendingFrom > 0 {
		seenPending := map[int64]bool{}
		// coverage is the oldest creation time the pending walk provably
		// enumerated past; zero means everything.
		coverage := since
		fullWalk := false
		for page := pendingFrom; ; page++ {
			pageStart := time.Now()
			todos, more, err := src.Page(ctx, StatePending, page)
			logPage(StatePending, page, len(todos), more, time.Since(pageStart), err)
			if err != nil {
				m.keepResume(since, &resumePoint{state: StatePending, page: rewind(page)})
				return err
			}
			m.absorb(todos)
			for i := range todos {
				seenPending[todos[i].ID] = true
			}
			if !more {
				fullWalk = true
				break
			}
			if since.IsZero() || !descending(todos) {
				continue
			}
			if len(todos) > 0 && todos[len(todos)-1].CreatedAt.Before(since) {
				break
			}
		}
		if fullWalk {
			coverage = time.Time{}
		}
		if pendingFrom == 1 {
			m.deriveDone(seenPending, coverage)
		}
	}

	for page := doneFrom; ; page++ {
		pageStart := time.Now()
		todos, more, err := src.Page(ctx, StateDone, page)
		logPage(StateDone, page, len(todos), more, time.Since(pageStart), err)
		if err != nil {
			m.keepResume(since, &resumePoint{state: StateDone, page: rewind(page)})
			return err
		}
		unknown := m.absorb(todos)
		if !more {
			m.mu.Lock()
			m.doneComplete = true
			m.mu.Unlock()
			break
		}
		m.mu.Lock()
		complete := m.doneComplete
		m.mu.Unlock()
		if complete && unknown == 0 {
			break
		}
		if since.IsZero() || !descending(todos) {
			continue
		}
		if len(todos) > 0 && todos[len(todos)-1].CreatedAt.Before(since) {
			break
		}
	}
	return nil
}

// logPage narrates one page of a walk: which list, how far in, what it
// carried, how long GitLab took, and the error when there is one. The walk is
// the plugin's only slow work and its only network dependency, so when a grid
// sits on "loading" this line is the difference between a stall, a crawl, and
// a loop.
func logPage(state string, page, n int, more bool, took time.Duration, err error) {
	if err != nil {
		log.Printf("gitlab plugin: %s page %d failed after %s: %v", state, page, took.Round(time.Millisecond), err)
		return
	}
	log.Printf("gitlab plugin: %s page %d: %d todos, more=%v, %s", state, page, n, more, took.Round(time.Millisecond))
}

// deriveDone marks every remembered pending todo the walk did not see, within
// the coverage it proved, as done. It is the one place a todo's state changes
// without GitLab saying so.
func (m *Memory) deriveDone(seenPending map[int64]bool, coverage time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, t := range m.todos {
		if t.State != StatePending || seenPending[t.ID] {
			continue
		}
		if coverage.IsZero() || !t.CreatedAt.Before(coverage) {
			t.State = StateDone
		}
	}
}

// takeResume removes and returns where the last walk over this window failed.
// Removing it is the point: a walk that fails again leaves a fresh mark, and
// a walk that succeeds leaves none, so the walk after it starts at page one
// and may judge absence again.
func (m *Memory) takeResume(since time.Time) *resumePoint {
	m.mu.Lock()
	defer m.mu.Unlock()
	k := resumeKey(since)
	r := m.resumes[k]
	delete(m.resumes, k)
	return r
}

func (m *Memory) keepResume(since time.Time, r *resumePoint) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.resumes[resumeKey(since)] = r
}

// resumeKey names the window a walk covered. A week's walk and the root's
// cover different pages, so one cannot resume the other.
func resumeKey(since time.Time) string { return since.UTC().Format(time.RFC3339Nano) }

// rewind is the page a failed walk restarts at: one before the failure, never
// before the first.
func rewind(page int) int {
	if page <= 1 {
		return 1
	}
	return page - 1
}

// absorb records a page, with GitLab's record replacing the remembered one,
// its state included, so a restored todo goes back to pending. It returns how
// many were new.
func (m *Memory) absorb(todos []Todo) (unknown int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := range todos {
		t := todos[i]
		if _, ok := m.todos[t.ID]; !ok {
			unknown++
		}
		m.todos[t.ID] = &t
	}
	return unknown
}

// descending reports whether a page is ordered newest-first.
func descending(todos []Todo) bool {
	for i := 1; i < len(todos); i++ {
		if todos[i].CreatedAt.After(todos[i-1].CreatedAt) {
			return false
		}
	}
	return true
}

// Walked reports whether some walk, in this process or a previous one whose
// snapshot was restored, has run to the end of GitLab's lists, so
// absence from this memory means something: an unknown todo is gone, rather
// than not yet seen. It is the same fact the done walk's early stop keys on.
func (m *Memory) Walked() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.doneComplete
}

// Get answers one remembered todo (a copy).
func (m *Memory) Get(id int64) (Todo, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	t, ok := m.todos[id]
	if !ok {
		return Todo{}, false
	}
	return *t, true
}

// All answers every remembered todo, oldest first (ties by id).
func (m *Memory) All() []Todo {
	m.mu.Lock()
	out := make([]Todo, 0, len(m.todos))
	for _, t := range m.todos {
		out = append(out, *t)
	}
	m.mu.Unlock()
	sort.Slice(out, func(i, j int) bool {
		if !out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].CreatedAt.Before(out[j].CreatedAt)
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// Week answers the remembered todos created in the week starting at
// start, oldest first.
func (m *Memory) Week(start time.Time) []Todo {
	end := start.AddDate(0, 0, 7)
	var out []Todo
	for _, t := range m.All() {
		if !t.CreatedAt.Before(start) && t.CreatedAt.Before(end) {
			out = append(out, t)
		}
	}
	return out
}

// WeekSummary is one week of the root listing.
type WeekSummary struct {
	Start      time.Time
	Open, Done int
}

// Weeks answers every week that holds a remembered todo, newest first.
func (m *Memory) Weeks() []WeekSummary {
	byStart := map[time.Time]*WeekSummary{}
	for _, t := range m.All() {
		s := WeekStart(t.CreatedAt)
		w := byStart[s]
		if w == nil {
			w = &WeekSummary{Start: s}
			byStart[s] = w
		}
		if t.Done() {
			w.Done++
		} else {
			w.Open++
		}
	}
	out := make([]WeekSummary, 0, len(byStart))
	for _, w := range byStart {
		out = append(out, *w)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Start.After(out[j].Start) })
	return out
}
