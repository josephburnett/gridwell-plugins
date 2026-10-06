package todos

import (
	"context"
	"errors"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/josephburnett/gridwell-plugins/memo/calendar"
)

func at(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return t
}

func TestWeekStartIsMondayUTC(t *testing.T) {
	cases := map[string]string{
		"2026-08-24T00:00:00Z":      "2026-08-24", // a Monday
		"2026-08-30T23:59:59Z":      "2026-08-24", // the Sunday after
		"2026-08-23T23:59:59Z":      "2026-08-17", // the Sunday before
		"2026-08-24T03:00:00+05:00": "2026-08-17", // 22:00 UTC the Sunday before
	}
	for in, want := range cases {
		got := WeekKey(WeekStart(at(in)))
		if got != WeekPrefix+want {
			t.Errorf("WeekStart(%s) = %s, want %s", in, got, want)
		}
	}
	if _, ok := ParseWeekKey("week:2026-08-25"); ok {
		t.Error("a Tuesday parsed as a week key")
	}
	if s, ok := ParseWeekKey("week:2026-08-24"); !ok || !s.Equal(at("2026-08-24T00:00:00Z")) {
		t.Errorf("ParseWeekKey = %v, %v", s, ok)
	}
}

// A todo's hint is the shared calendar's cell for its creation time, so it
// reads only the todo: a todo listed earlier in the same week moves nobody.
func TestTodoHintsAreAFunctionOfTheTodo(t *testing.T) {
	start := at("2026-08-17T00:00:00Z")
	later := []Todo{
		mk(2, "2026-08-18T12:00:00Z", StateDone),
		mk(3, "2026-08-18T12:30:00Z", StatePending),
		mk(4, "2026-08-23T10:00:00Z", StatePending),
	}
	hints := func(ts []Todo) map[string][4]int64 {
		out := map[string][4]int64{}
		for _, e := range WeekEntries(start, ts) {
			h := e.PlacementHint
			out[e.Key] = [4]int64{h.X, h.Y, h.W, h.H}
		}
		return out
	}
	before := hints(later)
	after := hints(append([]Todo{mk(1, "2026-08-18T09:00:00Z", StatePending)}, later...))
	for k, h := range before {
		if after[k] != h {
			t.Errorf("%s moved from %v to %v when an earlier todo arrived", k, h, after[k])
		}
	}
	for _, td := range later {
		x, y := calendar.Cell(td.CreatedAt, TodoTileW)
		if want := [4]int64{x, y, TodoTileW, 1}; before[td.Key()] != want {
			t.Errorf("%s hint = %v, want the calendar cell %v", td.Key(), before[td.Key()], want)
		}
	}
}

func TestWeekHintsAreTheSharedWeekCell(t *testing.T) {
	weeks := []WeekSummary{{Start: at("2026-08-24T00:00:00Z")}, {Start: at("2026-07-27T00:00:00Z")}}
	for i, e := range RootEntries(weeks) {
		x, y := calendar.WeekCell(weeks[i].Start)
		if h := e.PlacementHint; h.X != x || h.Y != y || h.W != 1 || h.H != 1 {
			t.Errorf("%s hint = %v, want (%d,%d)", e.Key, h, x, y)
		}
	}
}

func TestLabelAndRef(t *testing.T) {
	var mr Todo
	mr.TargetType, mr.Target.IID, mr.Target.Title, mr.State = "MergeRequest", 42, "Fix it", StatePending
	if mr.Label() != "!42 Fix it" {
		t.Errorf("label = %q", mr.Label())
	}
	var commit Todo
	commit.TargetType, commit.ActionName, commit.Body = "Commit", "build_failed", "pipeline exploded\nmore"
	if commit.Label() != "pipeline exploded" {
		t.Errorf("commit label = %q", commit.Label())
	}
	commit.Body = ""
	if commit.Label() != "build failed Commit" {
		t.Errorf("bare label = %q", commit.Label())
	}
	if id, ok := ParseKey("todo:7"); !ok || id != 7 {
		t.Errorf("ParseKey = %d %v", id, ok)
	}
	if _, ok := ParseKey("week:2026-08-24"); ok {
		t.Error("a week key parsed as a todo key")
	}
}

// fakeSource is a paged GitLab: pages of at most per, newest first
// unless ascending, recording every page it served. It is safe for a walk's
// concurrent fetches.
type fakeSource struct {
	pending, done []Todo
	per           int
	ascending     bool
	calls         []string
	err           error
	// failOn is one page — "pending/3" — that fails once, the way a lid
	// closing mid-walk does.
	failOn string
	// totals has every reply name the list's length in pages, as GitLab's
	// X-Total-Pages does; without it the walk pages serially.
	totals bool
	// delay is how long each page takes; inFlight and maxInFlight count the
	// pages being served at once.
	delay                 time.Duration
	inFlight, maxInFlight int

	mu sync.Mutex
}

func (f *fakeSource) Page(_ context.Context, state string, page int) (Reply, error) {
	key := state + "/" + strconv.Itoa(page)
	f.mu.Lock()
	f.calls = append(f.calls, key)
	f.inFlight++
	f.maxInFlight = max(f.maxInFlight, f.inFlight)
	f.mu.Unlock()
	time.Sleep(f.delay)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.inFlight--
	if f.err != nil {
		return Reply{}, f.err
	}
	if f.failOn == key {
		f.failOn = ""
		return Reply{}, errors.New("connection reset")
	}
	src := f.pending
	if state == StateDone {
		src = f.done
	}
	ordered := make([]Todo, len(src))
	copy(ordered, src)
	// newest first by default
	for i := 0; i < len(ordered); i++ {
		for j := i + 1; j < len(ordered); j++ {
			if (ordered[j].CreatedAt.After(ordered[i].CreatedAt)) != f.ascending {
				ordered[i], ordered[j] = ordered[j], ordered[i]
			}
		}
	}
	var pages int
	if f.totals {
		pages = (len(ordered) + f.per - 1) / f.per
	}
	start := (page - 1) * f.per
	if start >= len(ordered) {
		return Reply{Pages: pages}, nil
	}
	end := start + f.per
	if end > len(ordered) {
		end = len(ordered)
	}
	return Reply{Todos: ordered[start:end], More: end < len(ordered), Pages: pages}, nil
}

func mk(id int64, created string, state string) Todo {
	var t Todo
	t.ID, t.CreatedAt, t.State = id, at(created), state
	t.TargetType, t.Target.IID, t.Target.Title = "Issue", id, "t"+strconv.FormatInt(id, 10)
	return t
}

func TestSyncAllWalksPendingFullyAndDoneUntilNothingNew(t *testing.T) {
	src := &fakeSource{per: 2,
		pending: []Todo{mk(1, "2026-08-10T10:00:00Z", StatePending), mk(3, "2026-08-18T10:00:00Z", StatePending), mk(5, "2026-08-25T10:00:00Z", StatePending)},
		done:    []Todo{mk(2, "2026-08-11T10:00:00Z", StateDone), mk(4, "2026-08-19T10:00:00Z", StateDone), mk(6, "2026-08-25T11:00:00Z", StateDone)},
	}
	m := NewMemory()
	if err := m.Sync(context.Background(), src, time.Time{}); err != nil {
		t.Fatal(err)
	}
	if len(m.All()) != 6 {
		t.Fatalf("remembered %d, want 6", len(m.All()))
	}
	// The outset walks everything: 2 pending pages, 2 done pages.
	if got := strings.Join(src.calls, " "); got != "pending/1 pending/2 done/1 done/2" {
		t.Errorf("calls = %s", got)
	}
	// A second full sync: pending again in full, done stops at page 1
	// (nothing unknown there).
	src.calls = nil
	if err := m.Sync(context.Background(), src, time.Time{}); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(src.calls, " "); got != "pending/1 pending/2 done/1" {
		t.Errorf("resync calls = %s", got)
	}
}

func TestSyncDerivesDoneFromAbsenceInPending(t *testing.T) {
	src := &fakeSource{per: 10, pending: []Todo{mk(1, "2026-08-10T10:00:00Z", StatePending), mk(2, "2026-08-24T10:00:00Z", StatePending)}}
	m := NewMemory()
	if err := m.Sync(context.Background(), src, time.Time{}); err != nil {
		t.Fatal(err)
	}
	// Todo 1 is marked done in GitLab AND vanishes (its target deleted):
	// it is in neither list any more.
	src.pending = src.pending[1:]
	if err := m.Sync(context.Background(), src, time.Time{}); err != nil {
		t.Fatal(err)
	}
	got, _ := m.Get(1)
	if !got.Done() {
		t.Errorf("todo 1 state = %s, want done", got.State)
	}
	if two, _ := m.Get(2); two.Done() {
		t.Error("todo 2 flipped without cause")
	}
	// Restored in GitLab: GitLab's record wins again.
	src.pending = append(src.pending, mk(1, "2026-08-10T10:00:00Z", StatePending))
	_ = m.Sync(context.Background(), src, time.Time{})
	if one, _ := m.Get(1); one.Done() {
		t.Error("a restored todo stayed done")
	}
}

func TestSyncSinceStopsAtTheWeekAndFlipsOnlyWithinCoverage(t *testing.T) {
	week := at("2026-08-17T00:00:00Z")
	src := &fakeSource{per: 1,
		pending: []Todo{
			mk(10, "2026-06-01T10:00:00Z", StatePending),
			mk(1, "2026-07-01T10:00:00Z", StatePending), mk(2, "2026-07-08T10:00:00Z", StatePending),
			mk(3, "2026-08-18T10:00:00Z", StatePending), mk(4, "2026-08-20T10:00:00Z", StatePending),
			mk(5, "2026-08-25T10:00:00Z", StatePending),
		},
		done: []Todo{mk(6, "2026-07-02T10:00:00Z", StateDone), mk(7, "2026-08-19T10:00:00Z", StateDone), mk(8, "2026-08-26T10:00:00Z", StateDone)},
	}
	m := NewMemory()
	if err := m.Sync(context.Background(), src, time.Time{}); err != nil {
		t.Fatal(err)
	}
	// Todo 3 (in the week) and todo 1 (before the week) both leave pending.
	src.pending = []Todo{src.pending[0], src.pending[2], src.pending[4], src.pending[5]}
	src.calls = nil
	if err := m.Sync(context.Background(), src, week); err != nil {
		t.Fatal(err)
	}
	// Pending: pages {5}, {4}, {2} — 2 is before the week, stop with
	// pages left. Done: page 1 = {8}, nothing unknown, stop.
	if got := strings.Join(src.calls, " "); got != "pending/1 pending/2 pending/3 done/1" {
		t.Errorf("targeted calls = %s", got)
	}
	if three, _ := m.Get(3); !three.Done() {
		t.Error("todo 3 (inside the walked window, absent from pending) must be done")
	}
	if one, _ := m.Get(1); one.Done() {
		t.Error("todo 1 is outside the walk's coverage: the targeted sync must not judge it")
	}
}

func TestSyncSinceRefusesEarlyStopOnUnorderedPages(t *testing.T) {
	week := at("2026-08-17T00:00:00Z")
	src := &fakeSource{per: 2, ascending: true,
		pending: []Todo{mk(1, "2026-07-01T10:00:00Z", StatePending), mk(2, "2026-07-08T10:00:00Z", StatePending), mk(3, "2026-08-18T10:00:00Z", StatePending), mk(4, "2026-08-20T10:00:00Z", StatePending)},
	}
	m := NewMemory()
	if err := m.Sync(context.Background(), src, week); err != nil {
		t.Fatal(err)
	}
	// Ascending pages: page 1's last item is older than the week, but
	// the early stop must not fire — the walk runs to the end.
	if got := strings.Join(src.calls, " "); got != "pending/1 pending/2 done/1" {
		t.Errorf("calls = %s", got)
	}
	for _, id := range []int64{1, 2, 3, 4} {
		if got, ok := m.Get(id); !ok || got.Done() {
			t.Errorf("todo %d: ok=%v done=%v — a live todo was judged done", id, ok, got.Done())
		}
	}
}

// A todo's name is its title, done or open; only a done todo carries a
// status, and it is the one mark.
func TestDoneIsTheStatusNotTheName(t *testing.T) {
	start := at("2026-08-17T00:00:00Z")
	open := mk(1, "2026-08-18T10:00:00Z", StatePending)
	done := open
	done.State = StateDone
	o, d := WeekEntries(start, []Todo{open})[0], WeekEntries(start, []Todo{done})[0]
	if o.Label != "#1 t1" || d.Label != o.Label {
		t.Errorf("labels: open %q, done %q", o.Label, d.Label)
	}
	if o.StatusDetail != "" || d.StatusDetail != DoneMark {
		t.Errorf("status: open %q, done %q", o.StatusDetail, d.StatusDetail)
	}
	if w := RootEntries([]WeekSummary{{Start: start, Open: 1}})[0].Label; w != RootEntries([]WeekSummary{{Start: start, Done: 1}})[0].Label {
		t.Errorf("a week's name moved with its counts: %q", w)
	}
}

// Every text entry declares its presentation: the body is markdown, so both.
func TestEveryTextEntryDeclaresItsPresentation(t *testing.T) {
	start := at("2026-08-17T00:00:00Z")
	ts := []Todo{mk(1, "2026-08-18T10:00:00Z", StatePending), mk(2, "2026-08-19T10:00:00Z", StateDone)}
	entries := append(WeekEntries(start, ts), RootEntries([]WeekSummary{{Start: start}})...)
	for _, e := range entries {
		if e.Kind == "text" && e.TextPresentation != TextPresentation {
			t.Errorf("%s declares %q", e.Key, e.TextPresentation)
		}
	}
	if TextPresentation != "both" {
		t.Errorf("a markdown body presents both, not %q", TextPresentation)
	}
}

func TestSyncErrorLeavesMemoryUntouched(t *testing.T) {
	src := &fakeSource{per: 10, pending: []Todo{mk(1, "2026-08-10T10:00:00Z", StatePending)}}
	m := NewMemory()
	_ = m.Sync(context.Background(), src, time.Time{})
	src.err = errors.New("boom")
	if err := m.Sync(context.Background(), src, time.Time{}); err == nil {
		t.Fatal("expected the source error")
	}
	if one, ok := m.Get(1); !ok || one.Done() {
		t.Error("a failed walk must not flip anything")
	}
}

func TestWeeksAndEntries(t *testing.T) {
	m := NewMemory()
	m.absorb([]Todo{
		mk(1, "2026-08-18T10:00:00Z", StatePending), // Tue, week of 08-17
		mk(2, "2026-08-18T12:00:00Z", StateDone),    // Tue, same day, second row
		mk(3, "2026-08-23T10:00:00Z", StateDone),    // Sun
		mk(4, "2026-08-25T10:00:00Z", StatePending), // week of 08-24
	}, 0)
	weeks := m.Weeks()
	if len(weeks) != 2 || !weeks[0].Start.Equal(at("2026-08-24T00:00:00Z")) || weeks[1].Open != 1 || weeks[1].Done != 2 {
		t.Fatalf("weeks = %+v", weeks)
	}
	root := RootEntries(weeks)
	if root[0].Key != "week:2026-08-24" || root[0].ChildContext != root[0].Key || root[0].PlacementHint.X != 3 || root[0].PlacementHint.Y != 0 || root[1].PlacementHint.X != 2 || root[1].PlacementHint.Y != 0 {
		t.Errorf("root entries = %v", root)
	}
	if root[1].Label != "2026-08-17" {
		t.Errorf("week label = %q", root[1].Label)
	}
	wk := WeekEntries(at("2026-08-17T00:00:00Z"), m.Week(at("2026-08-17T00:00:00Z")))
	if len(wk) != 3 {
		t.Fatalf("week entries = %d", len(wk))
	}
	if wk[0].ServesPage || wk[0].Kind != "text" {
		t.Errorf("entry facts = %v", wk[0])
	}
}

func TestMarkdownCarriesTheEssentials(t *testing.T) {
	td := mk(9, "2026-08-18T10:00:00Z", StatePending)
	td.Target.Title = "Fix the widget"
	td.ActionName = "review_requested"
	td.Author.Name, td.Author.Username = "Ada Lovelace", "ada"
	td.Project.PathWithNamespace = "g/p"
	td.Body = "Could you   look at\nthis one?   " + strings.Repeat("x", 400)
	td.TargetURL = "https://gitlab.example/g/p/-/issues/9"
	got := string(Markdown(&td))
	for _, want := range []string{
		"# #9 Fix the widget",
		"review requested — from Ada Lovelace (@ada) · g/p · 2026-08-18",
		"> Could you look at this one? xxx",
		"[Open #9 in GitLab](https://gitlab.example/g/p/-/issues/9)",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("markdown lacks %q:\n%s", want, got)
		}
	}
	if !strings.Contains(got, "…") || len([]rune(td.Snippet())) > SnippetRunes+1 {
		t.Errorf("the snippet must be bounded: %d runes", len([]rune(td.Snippet())))
	}
	if td.Label() != "Ada Lovelace: #9 Fix the widget" {
		t.Errorf("label = %q", td.Label())
	}
	td.State = StateDone
	if got := string(Markdown(&td)); !strings.HasPrefix(got, "# "+DoneMark+" #9") || !strings.Contains(got, "· done") {
		t.Errorf("done must show in the heading and the line:\n%s", got)
	}
	if !strings.Contains(string(GoneMarkdown("todo:<1>")), "todo:<1>") {
		t.Error("the gone notice names the key")
	}
}

// A targeted week walk absorbs one done page PAST the week boundary
// (the page that proves the boundary was crossed). A later FULL walk
// then found done page 1 fully known and stopped — "nothing unknown
// means every older one is known" is only true once a walk has reached
// the END of the done list, and no walk had.
func TestSyncFullAfterTargetedWalksDoneToTheEnd(t *testing.T) {
	week := at("2026-08-17T00:00:00Z")
	src := &fakeSource{per: 2,
		pending: []Todo{mk(1, "2026-08-18T10:00:00Z", StatePending)},
		done: []Todo{
			mk(9, "2026-08-19T10:00:00Z", StateDone), mk(8, "2026-08-18T12:00:00Z", StateDone), // in the week
			mk(7, "2026-08-10T10:00:00Z", StateDone), mk(6, "2026-08-09T10:00:00Z", StateDone), // the page past the boundary
			mk(5, "2026-07-01T10:00:00Z", StateDone), mk(4, "2026-06-01T10:00:00Z", StateDone), // never walked by the week
		},
	}
	m := NewMemory()
	if err := m.Sync(context.Background(), src, week); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(src.calls, " "); got != "pending/1 done/1 done/2" {
		t.Fatalf("targeted calls = %s", got)
	}
	if n := len(m.All()); n != 5 {
		t.Fatalf("after the week walk remembered %d, want 5", n)
	}
	src.calls = nil
	if err := m.Sync(context.Background(), src, time.Time{}); err != nil {
		t.Fatal(err)
	}
	if n := len(m.All()); n != 7 {
		t.Errorf("after the full walk remembered %d, want 7 (calls %s)", n, strings.Join(src.calls, " "))
	}
	// Now the done list HAS been walked to its end: the next full walk
	// may stop at the first fully-known page.
	src.calls = nil
	_ = m.Sync(context.Background(), src, time.Time{})
	if got := strings.Join(src.calls, " "); got != "pending/1 done/1" {
		t.Errorf("resync calls = %s", got)
	}
}

// A walk that dies mid-list keeps what it absorbed and starts the next one
// ONE PAGE BACK, not at the beginning: a lid closing costs a page, not the
// dozens already paid for. The overlap re-reads the page the list may have
// shifted items onto while the walk was down.
func TestAFailedPendingWalkResumesOnePageBack(t *testing.T) {
	src := &fakeSource{per: 2, failOn: "pending/3", pending: []Todo{
		mk(1, "2026-08-10T10:00:00Z", StatePending), mk(2, "2026-08-11T10:00:00Z", StatePending),
		mk(3, "2026-08-12T10:00:00Z", StatePending), mk(4, "2026-08-13T10:00:00Z", StatePending),
		mk(5, "2026-08-14T10:00:00Z", StatePending), mk(6, "2026-08-15T10:00:00Z", StatePending),
	}}
	m := NewMemory()
	if err := m.Sync(context.Background(), src, time.Time{}); err == nil {
		t.Fatal("expected the page failure to fail the walk")
	}
	if got := strings.Join(src.calls, " "); got != "pending/1 pending/2 pending/3" {
		t.Fatalf("failed walk = %s", got)
	}
	if n := len(m.All()); n != 4 {
		t.Errorf("the failed walk kept %d todos, want the 4 it absorbed", n)
	}
	src.calls = nil
	if err := m.Sync(context.Background(), src, time.Time{}); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(src.calls, " "); got != "pending/2 pending/3 done/1" {
		t.Errorf("resumed walk = %s, want it to restart at pending/2", got)
	}
	if n := len(m.All()); n != 6 {
		t.Errorf("after the resumed walk remembered %d, want 6", n)
	}
	// The mark is spent: the walk after a successful one starts over.
	src.calls = nil
	if err := m.Sync(context.Background(), src, time.Time{}); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(src.calls, " "); got != "pending/1 pending/2 pending/3 done/1" {
		t.Errorf("the walk after a success = %s, want a fresh start", got)
	}
}

// A resumed pending walk must not judge absence. It never saw the pages the
// failed attempt covered, and the list may have shifted items across the seam
// in between — deriving done from that would mark live todos done, which is
// the one thing the walk may never get wrong. The next walk starts at page one
// and judges then.
func TestAResumedPendingWalkDoesNotJudgeAbsence(t *testing.T) {
	all := []Todo{
		mk(5, "2026-08-15T10:00:00Z", StatePending), mk(4, "2026-08-14T10:00:00Z", StatePending),
		mk(3, "2026-08-13T10:00:00Z", StatePending), mk(2, "2026-08-12T10:00:00Z", StatePending),
		mk(1, "2026-08-11T10:00:00Z", StatePending),
	}
	src := &fakeSource{per: 1, pending: all}
	m := NewMemory()
	if err := m.Sync(context.Background(), src, time.Time{}); err != nil {
		t.Fatal(err)
	}
	// The walk dies on page 4, so the next starts at page 3 — and by then
	// todo 5, which lives on page 1, has left GitLab's pending list.
	src.failOn = "pending/4"
	if err := m.Sync(context.Background(), src, time.Time{}); err == nil {
		t.Fatal("expected the page failure")
	}
	src.pending = all[1:]
	if err := m.Sync(context.Background(), src, time.Time{}); err != nil {
		t.Fatal(err)
	}
	if five, _ := m.Get(5); five.Done() {
		t.Error("the resumed walk judged a todo it never walked past")
	}
	// The next walk starts at page one, sees the whole pending list, and
	// only then flips it.
	if err := m.Sync(context.Background(), src, time.Time{}); err != nil {
		t.Fatal(err)
	}
	if five, _ := m.Get(5); !five.Done() {
		t.Error("the fresh walk after it must judge absence")
	}
	for _, id := range []int64{1, 2, 3, 4} {
		if got, _ := m.Get(id); got.Done() {
			t.Errorf("todo %d, still pending in GitLab, was flipped", id)
		}
	}
}

// A walk that fails in the DONE list resumes there: the pending half already
// finished, and judged absence, in the attempt that failed. Walking it again
// would re-page GitLab for what is already known.
func TestAFailedDoneWalkResumesWithoutRepeatingPending(t *testing.T) {
	src := &fakeSource{per: 2, failOn: "done/3",
		pending: []Todo{mk(9, "2026-08-20T10:00:00Z", StatePending)},
		done: []Todo{
			mk(1, "2026-08-10T10:00:00Z", StateDone), mk(2, "2026-08-11T10:00:00Z", StateDone),
			mk(3, "2026-08-12T10:00:00Z", StateDone), mk(4, "2026-08-13T10:00:00Z", StateDone),
			mk(5, "2026-08-14T10:00:00Z", StateDone), mk(6, "2026-08-15T10:00:00Z", StateDone),
		},
	}
	m := NewMemory()
	if err := m.Sync(context.Background(), src, time.Time{}); err == nil {
		t.Fatal("expected the page failure")
	}
	if m.Walked() {
		t.Error("a walk that never reached the end of the done list must not claim it did")
	}
	src.calls = nil
	if err := m.Sync(context.Background(), src, time.Time{}); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(src.calls, " "); got != "done/2 done/3" {
		t.Errorf("resumed walk = %s, want the done list from page 2 and no pending pages", got)
	}
	if !m.Walked() || len(m.All()) != 7 {
		t.Errorf("after the resume: walked=%v remembered=%d, want true and 7", m.Walked(), len(m.All()))
	}
}

// One window's failure does not send another window's walk to the wrong page:
// a week's pages and the root's are different lists.
func TestAResumeBelongsToItsWindow(t *testing.T) {
	week := at("2026-08-17T00:00:00Z")
	src := &fakeSource{per: 2, failOn: "pending/3", pending: []Todo{
		mk(1, "2026-08-17T10:00:00Z", StatePending), mk(2, "2026-08-18T10:00:00Z", StatePending),
		mk(3, "2026-08-19T10:00:00Z", StatePending), mk(4, "2026-08-20T10:00:00Z", StatePending),
		mk(5, "2026-08-21T10:00:00Z", StatePending), mk(6, "2026-08-22T10:00:00Z", StatePending),
	}}
	m := NewMemory()
	if err := m.Sync(context.Background(), src, time.Time{}); err == nil {
		t.Fatal("expected the page failure")
	}
	src.calls = nil
	if err := m.Sync(context.Background(), src, week); err != nil {
		t.Fatal(err)
	}
	if got := src.calls[0]; got != "pending/1" {
		t.Errorf("the week's walk started at %s; the root's mark is not its", got)
	}
	// The root's mark is untouched and still spends on the root's next walk.
	src.calls = nil
	if err := m.Sync(context.Background(), src, time.Time{}); err != nil {
		t.Fatal(err)
	}
	if got := src.calls[0]; got != "pending/2" {
		t.Errorf("the root's walk started at %s, want pending/2", got)
	}
}

// history is n pending todos, one a day back from 2026-08-25 — n pages at
// per 1 — and one done todo.
func history(n int) (pending, done []Todo) {
	day := at("2026-08-25T10:00:00Z")
	for i := 0; i < n; i++ {
		pending = append(pending, mk(int64(100+i), day.AddDate(0, 0, -i).Format(time.RFC3339), StatePending))
	}
	return pending, []Todo{mk(1, "2026-01-05T10:00:00Z", StateDone)}
}

// sortedCalls is the pages a walk asked for, in an order that does not depend
// on which concurrent fetch reached the source first.
func sortedCalls(src *fakeSource) string {
	c := append([]string(nil), src.calls...)
	sort.Strings(c)
	return strings.Join(c, " ")
}

// A walk whose first page names the list's length fetches the rest
// walkConcurrency at a time: GitLab answers a page in seconds, and a real
// history of a dozen pages walked one after another outlasted the refresh
// window. What it remembers is exactly what the serial walk remembers.
func TestAConcurrentWalkTakesRoundTripsNotPages(t *testing.T) {
	const pages, delay = 24, 50 * time.Millisecond
	pending, done := history(pages)
	serial := &fakeSource{per: 1, pending: pending, done: done}
	want := NewMemory()
	if err := want.Sync(context.Background(), serial, time.Time{}); err != nil {
		t.Fatal(err)
	}

	src := &fakeSource{per: 1, pending: pending, done: done, totals: true, delay: delay}
	m := NewMemory()
	start := time.Now()
	if err := m.Sync(context.Background(), src, time.Time{}); err != nil {
		t.Fatal(err)
	}
	took := time.Since(start)
	// Serially: 24 pending pages and a done page, 25 delays. Concurrently:
	// the first page, then 23 more four at a time, then the done page.
	if limit := (pages + 1) * delay / 2; took > limit {
		t.Errorf("a %d-page walk took %v, want under %v", pages, took, limit)
	}
	if src.maxInFlight > walkConcurrency || src.maxInFlight < 2 {
		t.Errorf("at most %d pages in flight, want 2..%d", src.maxInFlight, walkConcurrency)
	}
	if got := len(src.calls); got != pages+1 {
		t.Errorf("the walk asked for %d pages, want %d", got, pages+1)
	}
	if !reflect.DeepEqual(m.Snapshot(), want.Snapshot()) || !m.Walked() {
		t.Error("the concurrent walk remembers something other than the serial one")
	}
}

// A page that fails mid-walk leaves the same mark a serial walk does — one
// page before the lowest page that failed — and nothing past it is absorbed,
// so the next walk resumes there and completes.
func TestAConcurrentWalkFailureResumesOnePageBack(t *testing.T) {
	pending, done := history(8)
	src := &fakeSource{per: 1, pending: pending, done: done, totals: true, failOn: "pending/5"}
	m := NewMemory()
	if err := m.Sync(context.Background(), src, time.Time{}); err == nil {
		t.Fatal("expected the page failure to fail the walk")
	}
	if n := len(m.All()); n != 4 {
		t.Errorf("the failed walk absorbed %d todos, want pages 1-4's 4", n)
	}
	src.calls = nil
	if err := m.Sync(context.Background(), src, time.Time{}); err != nil {
		t.Fatal(err)
	}
	if src.calls[0] != "pending/4" {
		t.Errorf("the resumed walk started at %s, want pending/4", src.calls[0])
	}
	if got := sortedCalls(src); got != "done/1 pending/4 pending/5 pending/6 pending/7 pending/8" {
		t.Errorf("resumed walk = %s", got)
	}
	if n := len(m.All()); n != 9 {
		t.Errorf("after the resume remembered %d, want 9", n)
	}
}

// Only a walk that started at the first pending page and saw every page
// judges absence, however its pages were fetched.
func TestAConcurrentWalkJudgesAbsenceOnlyWhenComplete(t *testing.T) {
	all, _ := history(6)
	src := &fakeSource{per: 1, pending: all, totals: true}
	m := NewMemory()
	if err := m.Sync(context.Background(), src, time.Time{}); err != nil {
		t.Fatal(err)
	}
	// The next walk dies on page 5; by the resumed walk, the newest todo
	// (page 1) has left GitLab's pending list.
	src.failOn = "pending/5"
	if err := m.Sync(context.Background(), src, time.Time{}); err == nil {
		t.Fatal("expected the page failure")
	}
	src.pending = all[1:]
	if err := m.Sync(context.Background(), src, time.Time{}); err != nil {
		t.Fatal(err)
	}
	if gone, _ := m.Get(all[0].ID); gone.Done() {
		t.Error("the resumed walk judged a todo it never walked past")
	}
	if err := m.Sync(context.Background(), src, time.Time{}); err != nil {
		t.Fatal(err)
	}
	if gone, _ := m.Get(all[0].ID); !gone.Done() {
		t.Error("the complete walk after it must judge absence")
	}
	for _, td := range all[1:] {
		if got, _ := m.Get(td.ID); got.Done() {
			t.Errorf("todo %d, still pending in GitLab, was flipped", td.ID)
		}
	}
}

// A week walk still stops at the week, judges only within the coverage it
// reached, and fetches at most walkConcurrency pages past the stop.
func TestAConcurrentWeekWalkStopsAtTheWeek(t *testing.T) {
	all, _ := history(16) // 2026-08-25 back to 2026-08-10
	week := at("2026-08-17T00:00:00Z")
	src := &fakeSource{per: 1, pending: all, totals: true}
	m := NewMemory()
	if err := m.Sync(context.Background(), src, time.Time{}); err != nil {
		t.Fatal(err)
	}
	// One todo inside the week and one before it leave pending.
	inWeek, before := all[5], all[12] // 08-20, 08-13
	src.pending = nil
	for _, td := range all {
		if td.ID != inWeek.ID && td.ID != before.ID {
			src.pending = append(src.pending, td)
		}
	}
	src.calls = nil
	if err := m.Sync(context.Background(), src, week); err != nil {
		t.Fatal(err)
	}
	// Page 9 (08-16) is the first before the week.
	if n := len(src.calls); n > 9+walkConcurrency+1 {
		t.Errorf("the week walk asked for %d pages: %s", n, sortedCalls(src))
	}
	if got, _ := m.Get(inWeek.ID); !got.Done() {
		t.Error("a todo inside the walked window, absent from pending, must be done")
	}
	if got, _ := m.Get(before.ID); got.Done() {
		t.Error("a todo outside the walk's coverage must not be judged")
	}
}

// A list that grows mid-walk runs past the length its first page named: the
// walk pages on, one at a time, until a page says it is the last.
func TestAListLongerThanItsFirstPageSaidIsWalkedToTheEnd(t *testing.T) {
	pending, done := history(6)
	src := &growing{fakeSource: fakeSource{per: 1, pending: pending, done: done, totals: true}, said: 3}
	m := NewMemory()
	if err := m.Sync(context.Background(), src, time.Time{}); err != nil {
		t.Fatal(err)
	}
	if n := len(m.All()); n != 7 {
		t.Errorf("remembered %d, want all 7", n)
	}
}

// growing is a fakeSource whose replies name a length of said pages, as a
// list that grew after the walk began would.
type growing struct {
	fakeSource
	said int
}

func (g *growing) Page(ctx context.Context, state string, page int) (Reply, error) {
	r, err := g.fakeSource.Page(ctx, state, page)
	if r.Pages > 0 {
		r.Pages = g.said
	}
	return r, err
}

// A memory answers what a walk changed, context by context, and nothing for a
// walk that changed nothing: that diff is what the plugin announces to the
// node, so a change it misses never repaints and a change it invents
// repaints for nothing.
func TestChangesNameExactlyTheListingsThatMoved(t *testing.T) {
	src := &fakeSource{per: 10, pending: []Todo{mk(1, "2026-08-10T10:00:00Z", StatePending), mk(2, "2026-08-18T10:00:00Z", StatePending)}}
	m := NewMemory()
	sync := func() []string {
		t.Helper()
		if err := m.Sync(context.Background(), src, time.Time{}); err != nil {
			t.Fatal(err)
		}
		return m.TakeChanges().Contexts()
	}
	if got := sync(); !reflect.DeepEqual(got, []string{RootContext, "week:2026-08-17", "week:2026-08-10"}) {
		t.Errorf("first walk changed %v", got)
	}
	if got := sync(); got != nil {
		t.Errorf("an unchanged walk changed %v", got)
	}
	// Todo 1 leaves pending: its week and the root's counts move.
	src.pending = src.pending[1:]
	if got := sync(); !reflect.DeepEqual(got, []string{RootContext, "week:2026-08-10"}) {
		t.Errorf("a derived done changed %v", got)
	}
	// A retitled todo moves its week's listing and not the root's counts.
	src.pending[0].Target.Title = "renamed"
	if got := sync(); !reflect.DeepEqual(got, []string{"week:2026-08-17"}) {
		t.Errorf("a retitle changed %v", got)
	}
	// Mark-done is a change like any other; a second one is none.
	m.MarkDone(2)
	if got := m.TakeChanges().Contexts(); !reflect.DeepEqual(got, []string{RootContext, "week:2026-08-17"}) {
		t.Errorf("mark-done changed %v", got)
	}
	m.MarkDone(2)
	if got := m.TakeChanges().Contexts(); got != nil {
		t.Errorf("a repeated mark-done changed %v", got)
	}
	// A restore is what was already answered: nothing to announce.
	fresh := NewMemory()
	fresh.Restore(m.Snapshot())
	if got := fresh.TakeChanges().Contexts(); got != nil {
		t.Errorf("a restore changed %v", got)
	}
}

// A todo whose body changed while its name, state and week stayed is that
// todo's change, not its week's: the listing did not move, the bytes did.
// Its body's stamp moves with them, and a walk that changes nothing names no
// todo.
func TestABodyChangedInPlaceIsItsTodoNotItsWeek(t *testing.T) {
	src := &fakeSource{per: 10, pending: []Todo{mk(1, "2026-08-10T10:00:00Z", StatePending), mk(2, "2026-08-18T10:00:00Z", StatePending)}}
	m := NewMemory()
	sync := func() Changes {
		t.Helper()
		if err := m.Sync(context.Background(), src, time.Time{}); err != nil {
			t.Fatal(err)
		}
		return m.TakeChanges()
	}
	if got := sync(); len(got.Todos) != 0 {
		t.Errorf("a first walk named %d todos changed in place; new ones have no body anyone holds", len(got.Todos))
	}
	before, _ := m.Get(2)
	src.pending[1].Body = "a new note"
	got := sync()
	if got.Contexts() != nil {
		t.Errorf("a body change moved listings %v", got.Contexts())
	}
	if len(got.Todos) != 1 || got.Todos[0].ID != 2 || got.Todos[0].Body != "a new note" {
		t.Fatalf("a body change named %v, want todo 2 as it is now", got.Todos)
	}
	if Stamp(Markdown(&before)) == Stamp(Markdown(&got.Todos[0])) {
		t.Error("the body moved and its stamp did not")
	}
	if got := sync(); got.Contexts() != nil || len(got.Todos) != 0 {
		t.Errorf("an unchanged walk changed %v and %d todos", got.Contexts(), len(got.Todos))
	}
	// Done shows in the body and in the listing: both move.
	m.MarkDone(1)
	if got := m.TakeChanges(); len(got.Todos) != 1 || got.Todos[0].ID != 1 || got.Contexts() == nil {
		t.Errorf("mark-done changed %v and %v; want todo 1 and its listings", got.Contexts(), got.Todos)
	}
}

// A glance pages newest-first until a page carries a todo memory knows:
// above it everything is new. It never judges absence.
func TestAGlancePagesOnlyToTheFirstKnownTodo(t *testing.T) {
	src := &fakeSource{per: 2, pending: []Todo{mk(1, "2026-08-10T10:00:00Z", StatePending), mk(2, "2026-08-11T10:00:00Z", StatePending)}}
	m := NewMemory()
	if err := m.Sync(context.Background(), src, time.Time{}); err != nil {
		t.Fatal(err)
	}
	m.TakeChanges()
	src.calls = nil
	if err := m.Glance(context.Background(), src); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(src.calls, " "); got != "pending/1" {
		t.Errorf("a quiet glance asked %s", got)
	}
	// Three new todos push the known ones to page 2; todo 1 is gone.
	src.pending = []Todo{mk(2, "2026-08-11T10:00:00Z", StatePending),
		mk(3, "2026-08-24T10:00:00Z", StatePending), mk(4, "2026-08-25T10:00:00Z", StatePending), mk(5, "2026-08-26T10:00:00Z", StatePending)}
	src.calls = nil
	if err := m.Glance(context.Background(), src); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(src.calls, " "); got != "pending/1 pending/2" {
		t.Errorf("a glance over a page of news asked %s", got)
	}
	if len(m.All()) != 5 {
		t.Errorf("remembered %d, want 5", len(m.All()))
	}
	if one, _ := m.Get(1); one.Done() {
		t.Error("a glance judged absence")
	}
	if got := m.TakeChanges().Contexts(); !reflect.DeepEqual(got, []string{RootContext, "week:2026-08-24"}) {
		t.Errorf("the glance changed %v", got)
	}
}

// gate holds one page after its fetch began until the test releases it.
type gate struct{ began, release chan struct{} }

func newGate() gate { return gate{make(chan struct{}), make(chan struct{})} }

// gatedSource holds the pages named in gates, once each: the window in which
// a walk has asked GitLab and not yet heard.
type gatedSource struct {
	*fakeSource
	gates map[string]gate
	once  sync.Map
}

func (g *gatedSource) Page(ctx context.Context, state string, page int) (Reply, error) {
	r, err := g.fakeSource.Page(ctx, state, page)
	key := state + "/" + strconv.Itoa(page)
	if gt, ok := g.gates[key]; ok {
		if _, held := g.once.LoadOrStore(key, true); !held {
			close(gt.began)
			<-gt.release
		}
	}
	return r, err
}

// A mark-done GitLab accepted while a walk's pending page was in flight is not
// undone by that page: the page was fetched before the mark, so its pending is
// stale, and nothing is announced for it. A page fetched after the mark is
// GitLab's word again, a reopen included.
func TestAMarkOutlivesAPageFetchedBeforeIt(t *testing.T) {
	src := &fakeSource{per: 10, pending: []Todo{mk(1, "2026-08-18T10:00:00Z", StatePending)}}
	m := NewMemory()
	if err := m.Sync(context.Background(), src, time.Time{}); err != nil {
		t.Fatal(err)
	}
	m.TakeChanges()

	pending, done := newGate(), newGate()
	g := &gatedSource{fakeSource: src, gates: map[string]gate{"pending/1": pending, "done/1": done}}
	walked := make(chan error)
	go func() { walked <- m.Sync(context.Background(), g, time.Time{}) }()
	<-pending.began
	m.MarkDone(1)
	m.TakeChanges()
	src.mu.Lock()
	src.pending, src.done = nil, []Todo{mk(1, "2026-08-18T10:00:00Z", StateDone)}
	src.mu.Unlock()
	close(pending.release)
	// The stale pending page is absorbed; the done list is not yet read.
	<-done.began
	if got, _ := m.Get(1); got.State != StateDone {
		t.Errorf("a page fetched before the mark flipped the todo back to %q", got.State)
	}
	if got := m.TakeChanges().Contexts(); got != nil {
		t.Errorf("the stale page announced %v", got)
	}
	close(done.release)
	if err := <-walked; err != nil {
		t.Fatal(err)
	}

	// Reopened at GitLab: a page fetched after the mark wins.
	src.mu.Lock()
	src.pending, src.done = []Todo{mk(1, "2026-08-18T10:00:00Z", StatePending)}, nil
	src.mu.Unlock()
	if err := m.Sync(context.Background(), src, time.Time{}); err != nil {
		t.Fatal(err)
	}
	if got, _ := m.Get(1); got.State != StatePending {
		t.Fatalf("a page fetched after the mark says pending, memory says %q", got.State)
	}
}
