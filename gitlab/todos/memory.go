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
// numbers start at 1. The ordering is verified per page, never assumed — see
// Sync.
type Source interface {
	Page(ctx context.Context, state string, page int) (Reply, error)
}

// Reply is one page of a Source's list.
type Reply struct {
	Todos []Todo
	// More is false on the last page.
	More bool
	// Pages is how many pages the list held when this page was served, zero
	// when the source does not say — GitLab omits it on very large lists. A
	// walk that knows it fetches pages concurrently; one that does not pages
	// serially.
	Pages int
}

// walkConcurrency is how many pages a walk fetches at once: GitLab answers
// one page in seconds, and pages are independent, so the walk is bound by
// round trips rather than bandwidth — while a handful keeps one walk from
// crowding the API's rate limit.
const walkConcurrency = 4

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
	// changedWeeks and rootChanged are what the memory changed since the last
	// TakeChanges; see note.
	changedWeeks map[time.Time]bool
	rootChanged  bool
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
	return &Memory{todos: map[int64]*Todo{}, resumes: map[string]*resumePoint{}, changedWeeks: map[time.Time]bool{}}
}

// Changes is what the memory changed since it was last asked: the weeks whose
// listing moved, newest first, and whether the root's did.
type Changes struct {
	Root  bool
	Weeks []time.Time
}

// Contexts names the contexts whose listings moved: the root first, then each
// week, newest first.
func (c Changes) Contexts() []string {
	var out []string
	if c.Root {
		out = append(out, RootContext)
	}
	for _, w := range c.Weeks {
		out = append(out, WeekKey(w))
	}
	return out
}

// TakeChanges answers what the memory changed since the last call and forgets
// it, so each change is answered once.
func (m *Memory) TakeChanges() Changes {
	m.mu.Lock()
	defer m.mu.Unlock()
	c := Changes{Root: m.rootChanged}
	for w := range m.changedWeeks {
		c.Weeks = append(c.Weeks, w)
	}
	sort.Slice(c.Weeks, func(i, j int) bool { return c.Weeks[i].After(c.Weeks[j]) })
	m.changedWeeks, m.rootChanged = map[time.Time]bool{}, false
	return c
}

// noteLocked records that a todo's record went from was (nil when new) to now.
// A week's listing shows every field of its todos; the root's shows only the
// weeks and their open and done counts, so it moves when a todo arrives, flips
// state, or changes week. The caller holds m.mu.
func (m *Memory) noteLocked(was *Todo, now *Todo) {
	if was != nil && sameRecord(*was, *now) {
		return
	}
	week := WeekStart(now.CreatedAt)
	m.changedWeeks[week] = true
	if was == nil || was.State != now.State {
		m.rootChanged = true
	}
	if was != nil {
		if old := WeekStart(was.CreatedAt); !old.Equal(week) {
			m.changedWeeks[old] = true
			m.rootChanged = true
		}
	}
}

// sameRecord compares two records field by field, instants by Equal: a
// time.Time's == also compares its location and monotonic reading, which
// differ between a decoded cache file and a decoded page for the same instant.
func sameRecord(a, b Todo) bool {
	if !a.CreatedAt.Equal(b.CreatedAt) || !a.UpdatedAt.Equal(b.UpdatedAt) {
		return false
	}
	a.CreatedAt, a.UpdatedAt, b.CreatedAt, b.UpdatedAt = time.Time{}, time.Time{}, time.Time{}, time.Time{}
	return a == b
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
// Pages are fetched concurrently when GitLab names the list's length, but
// absorbed in page order (see walkPages), so everything below reads as for a
// serial walk: "the failure" is the first page that failed.
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
		failed, err := walkPages(ctx, src, StatePending, pendingFrom, func(r Reply) bool {
			m.absorb(r.Todos)
			for i := range r.Todos {
				seenPending[r.Todos[i].ID] = true
			}
			if !r.More {
				coverage = time.Time{}
				return true
			}
			return pastSince(r.Todos, since)
		})
		if err != nil {
			m.keepResume(since, &resumePoint{state: StatePending, page: rewind(failed)})
			return err
		}
		if pendingFrom == 1 {
			m.deriveDone(seenPending, coverage)
		}
	}

	failed, err := walkPages(ctx, src, StateDone, doneFrom, func(r Reply) bool {
		unknown := m.absorb(r.Todos)
		m.mu.Lock()
		defer m.mu.Unlock()
		if !r.More {
			m.doneComplete = true
			return true
		}
		if m.doneComplete && unknown == 0 {
			return true
		}
		return pastSince(r.Todos, since)
	})
	if err != nil {
		m.keepResume(since, &resumePoint{state: StateDone, page: rewind(failed)})
		return err
	}
	return nil
}

// pastSince reports whether a week walk may stop after this page: it reached
// todos created before since, on a page proven newest-first.
func pastSince(todos []Todo, since time.Time) bool {
	if since.IsZero() || len(todos) == 0 || !descending(todos) {
		return false
	}
	return todos[len(todos)-1].CreatedAt.Before(since)
}

// walkPages pages one state from page from, handing each page to visit IN
// PAGE ORDER until visit says stop or a page fails, which it answers with the
// page number. The first page names the list's length; the pages after it,
// up to that length, are fetched walkConcurrency at a time, at most that many
// ahead of the page visit has reached, so an early stop wastes little. Pages
// past the named length — the list grew mid-walk — or a list of unknown length
// are paged one after another.
func walkPages(ctx context.Context, src Source, state string, from int, visit func(Reply) (stop bool)) (failed int, err error) {
	fetch := func(ctx context.Context, page int) (Reply, error) {
		start := time.Now()
		r, err := src.Page(ctx, state, page)
		if ctx.Err() == nil || err == nil {
			// A page the walk abandoned is not news; one it asked for is.
			logPage(state, page, len(r.Todos), r.More, time.Since(start), err)
		}
		return r, err
	}
	r, err := fetch(ctx, from)
	if err != nil {
		return from, err
	}
	if visit(r) || !r.More {
		return 0, nil
	}
	next := from + 1
	if last := r.Pages; last >= next {
		stop, failed, err := walkAhead(ctx, fetch, next, last, visit)
		if err != nil || stop {
			return failed, err
		}
		next = last + 1
	}
	for page := next; ; page++ {
		r, err := fetch(ctx, page)
		if err != nil {
			return page, err
		}
		if visit(r) || !r.More {
			return 0, nil
		}
	}
}

// walkAhead is walkPages over pages first..last with the fetches running
// ahead of the visits. It returns once every fetch it started has returned,
// so no request outlives the walk. stop is false only when every page was
// visited and the last one said there is more.
func walkAhead(ctx context.Context, fetch func(context.Context, int) (Reply, error), first, last int,
	visit func(Reply) bool) (stop bool, failed int, err error) {
	ctx, cancel := context.WithCancel(ctx)
	var wg sync.WaitGroup
	defer wg.Wait()
	defer cancel()

	type answer struct {
		r   Reply
		err error
	}
	answers := make([]chan answer, last-first+1)
	for i := range answers {
		answers[i] = make(chan answer, 1)
	}
	// slots holds one token per page fetched and not yet visited.
	slots := make(chan struct{}, walkConcurrency)
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := range answers {
			select {
			case slots <- struct{}{}:
			case <-ctx.Done():
				return
			}
			if ctx.Err() != nil {
				return
			}
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				r, err := fetch(ctx, first+i)
				answers[i] <- answer{r, err}
			}(i)
		}
	}()

	for i := range answers {
		a := <-answers[i]
		<-slots
		if a.err != nil {
			return true, first + i, a.err
		}
		if visit(a.r) || !a.r.More {
			return true, 0, nil
		}
	}
	return false, 0, nil
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
			was := *t
			t.State = StateDone
			m.noteLocked(&was, t)
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
		was, ok := m.todos[t.ID]
		if !ok {
			unknown++
		}
		m.noteLocked(was, &t)
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

// MarkDone records GitLab's acceptance of a mark-as-done: the remembered todo
// flips to done. It is the write-side sibling of deriveDone's absence rule —
// the only other place state changes without a walk saying so — and it is only
// called after GitLab itself accepted the write, so the next walk's record
// agrees with it. False when the todo is unknown.
func (m *Memory) MarkDone(id int64) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	t, ok := m.todos[id]
	if !ok {
		return false
	}
	was := *t
	t.State = StateDone
	m.noteLocked(&was, t)
	return true
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

// Shows reports whether the memory has anything to answer a listing with: any
// todo at all for a zero week, else any todo created in the week starting
// there. It is the line between a warm read, answered at once, and a cold one,
// which waits on the walk.
func (m *Memory) Shows(week time.Time) bool {
	if week.IsZero() {
		m.mu.Lock()
		defer m.mu.Unlock()
		return len(m.todos) > 0
	}
	return len(m.Week(week)) > 0
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
