// Package plugin is the gitlab todos plugin: the wire half over
// gitlab/todos. The one context, "todos", lists weeks; a week,
// "week:<monday>", lists the todos created that week as markdown text tiles.
// Keys are GitLab's todo ids, stable forever. Listings are non-authoritative
// and Probe never answers GONE: a todo never disappears from the grid, it
// changes state when refreshed. The plugin's memory of GitLab answers every
// listing and survives a restart through its file in `state_dir`; memo's
// flights refresh it for reads, and its changes' work while a grid is shown.
// The one write is Delete, which here means mark-as-done: the trash gesture
// resolves the todo at GitLab rather than removing anything. The plugin holds
// no node fact — no id, no layout.
package plugin

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/josephburnett/gridwell-plugins/gitlab/todos"
	"github.com/josephburnett/gridwell-plugins/memo"
	pluginv1 "github.com/josephburnett/gridwell/api/gen/plugin/v1"
)

// Kind is the plugin's declared kind, and the suffix of its binary name.
const Kind = "gitlab"

// displayName is the plugin's own name for itself. The name the user sees is
// the plugin's `label` in server.yaml; this is the fallback when none is
// configured, and the root grid's source label.
const displayName = "gitlab todos"

// DefaultRefresh is how often the refresher glances at GitLab's newest
// pending page (config `refresh`): the delay before a new todo shows. GitLab
// pushes nothing about a user's todos, so the plugin polls while one of its
// grids is shown, and a glance is one request.
const DefaultRefresh = 30 * time.Second

// DefaultFullRefresh is how long a full walk stays fresh (config
// `full_refresh`): the delay before a todo done or deleted at GitLab shows
// done, since only a full walk sees absence. The node lists a context on
// every GetGrid and GetTile, and inside the window those reads cost GitLab
// nothing.
const DefaultFullRefresh = 10 * time.Minute

// refreshUnit is the one unit of background work. The to-do list is one
// account-wide list, so every context a stream shows needs the same refresher.
const refreshUnit = todos.RootContext

// Marker is the write half of the source: marking one todo done at GitLab.
// It is a separate interface from Source because the walk and the write have
// different lives — everything reads, one gesture writes — and a test fakes
// them separately. *gitlabapi.Client implements both.
type Marker interface {
	MarkDone(ctx context.Context, id int64) error
}

// Plugin implements pluginv1.PluginServer.
type Plugin struct {
	pluginv1.UnimplementedPluginServer
	src         todos.Source
	marker      Marker
	mem         *todos.Memory
	refresh     time.Duration
	fullRefresh time.Duration
	clock       memo.Clock

	life    *memo.Life
	file    *memo.File[todos.Snapshot]
	flights *memo.Flights
	changes *memo.Changes
}

// Options tunes a plugin. Zero values take the defaults.
type Options struct {
	Refresh     time.Duration
	FullRefresh time.Duration
	// FirstAnswer is memo.DefaultFirstAnswer when zero.
	FirstAnswer time.Duration
	// Linger is how long the refresher outlives the last Watch stream:
	// memo.DefaultLinger when zero, none when negative.
	Linger time.Duration
	Now    func() time.Time
	// Marker is the mark-as-done writer. Nil means read-only: Delete answers
	// Unimplemented and everything else works as before.
	Marker Marker
	// StateDir is the private directory the node hands the plugin. Empty
	// means no file: the plugin keeps its memory for its process lifetime.
	StateDir string
	// Logf takes every line the plugin writes. It defaults to the standard
	// logger, which the node captures from the subprocess's stderr.
	Logf func(format string, args ...any)
}

// New builds a plugin over src. Whether there is a source is decided before
// this point: FromConfig refuses a missing token, and the node shows the
// plugin broken with its reason. The memory's file is loaded here, before the
// plugin serves its first request, so the first listing is answered from what
// the last process walked.
func New(src todos.Source, o Options) *Plugin {
	p := &Plugin{
		src:         retrying{src: src, attempts: pageAttempts, backoff: pageBackoff},
		marker:      o.Marker,
		mem:         todos.NewMemory(),
		refresh:     o.Refresh,
		fullRefresh: o.FullRefresh,
		clock:       memo.System,
		life:        memo.NewLife(),
	}
	if p.refresh <= 0 {
		p.refresh = DefaultRefresh
	}
	if p.fullRefresh <= 0 {
		p.fullRefresh = DefaultFullRefresh
	}
	if o.Now != nil {
		p.clock = nowClock(o.Now)
	}
	logf := o.Logf
	if logf == nil {
		logf = log.Printf
	}
	p.file = memo.NewFile[todos.Snapshot](strings.TrimSpace(o.StateDir), todos.CacheFile, todos.CacheVersion, logf)
	p.flights = memo.NewFlights(p.life, memo.FlightOptions{
		Name:        "gitlab plugin",
		Walk:        p.walk,
		Window:      p.fullRefresh,
		Covers:      covers,
		FirstAnswer: o.FirstAnswer,
		Landed:      p.landed,
		Clock:       p.clock,
		Logf:        logf,
	})
	p.changes = memo.NewChanges(p.life, memo.ChangeOptions{
		Unscoped: []string{todos.RootContext},
		Work:     func(string) []string { return []string{refreshUnit} },
		Do:       p.refresher,
		Linger:   o.Linger,
		Clock:    p.clock,
	})
	if snap, ok := p.file.Load(); ok {
		p.mem.Restore(snap)
		p.flights.Restore(snap.WalkedAt)
	}
	return p
}

// nowClock is a memo.Clock whose Now a test sets; its waits are real.
type nowClock func() time.Time

func (c nowClock) Now() time.Time                       { return c() }
func (nowClock) After(d time.Duration) <-chan time.Time { return time.After(d) }

// covers is the root walk answering for every week: it reads every page a
// week walk would.
func covers(key string) []string {
	if key == todos.RootContext {
		return nil
	}
	return []string{todos.RootContext}
}

// since is key's walk window: zero for the root, meaning everything, and the
// Monday for a week.
func since(key string) time.Time {
	start, _ := todos.ParseWeekKey(key)
	return start
}

// walk is one flight's refresh. Each page REQUEST is bounded by the API
// client's own timeout and each PAGE is retried in place a bounded number of
// times, so a dead source ends the walk with its error rather than hanging it.
func (p *Plugin) walk(ctx context.Context, key string) error {
	return p.mem.Sync(ctx, p.src, since(key))
}

// landed announces what a walk moved, a failed one included, since the pages
// it absorbed are what reads now answer, and saves what it learned.
func (p *Plugin) landed(_ string, err error) {
	if p.announce() || err == nil {
		p.save()
	}
}

// save writes memory and the flights' walk stamps to the file. memo.File logs
// its own failure, once per episode.
func (p *Plugin) save() {
	_ = p.file.Save(func() todos.Snapshot {
		s := p.mem.Snapshot()
		s.WalkedAt = p.flights.WalkedAt()
		return s
	})
}

// MinRefresherInterval is the fastest the background refresher ticks, whatever
// `refresh` says: a tick is a request to GitLab, and a tick faster than the
// request would leave the refresher always asking. Reads still walk on the
// full-refresh window — a tiny one is how a test says "walk on every read",
// and that keeps working.
const MinRefresherInterval = time.Second

// refresherInterval is how often the refresher ticks: `refresh`, floored.
func (p *Plugin) refresherInterval() time.Duration {
	if p.refresh < MinRefresherInterval {
		return MinRefresherInterval
	}
	return p.refresh
}

// Close ends the plugin's lifetime: the refresher and every walk stop.
func (p *Plugin) Close() { p.life.End() }

// refresher is the Changes work. It runs only while a Watch stream shows one
// of these grids: GitLab cannot tell, so the clock is the plugin's, and only
// for as long as someone is looking.
func (p *Plugin) refresher(ctx context.Context, _ string) {
	memo.Poll(ctx, p.clock, p.refresherInterval(), p.tick)
}

// tick is the refresher's one rule: a full walk of the root once the last one
// has aged out of the full-refresh window, else a glance at the newest
// pending page. It shares the flights with the reads, so a tick after a
// read's walk only glances, and a tick while a root walk runs does nothing,
// since that walk reads everything a glance would.
func (p *Plugin) tick(ctx context.Context) {
	switch {
	case p.flights.Busy(todos.RootContext):
	case !p.flights.Fresh(todos.RootContext):
		p.flights.Rewalk(todos.RootContext)
	default:
		p.glance(ctx)
	}
}

// glance absorbs what is new at the top of GitLab's pending list and announces
// it. Its outcome is the root's, as a walk's is, so the next read answers its
// failure; it stamps no freshness, having proved nothing about absence. A
// glance cut short because the refresher stopped heard no verdict from
// GitLab, so it records none.
func (p *Plugin) glance(ctx context.Context) {
	err := p.mem.Glance(ctx, p.src)
	if ctx.Err() == nil {
		p.flights.Outcome(todos.RootContext, err)
	}
	if p.announce() {
		p.save()
	}
}

func (p *Plugin) Info(context.Context, *pluginv1.InfoRequest) (*pluginv1.InfoResponse, error) {
	return &pluginv1.InfoResponse{
		Kind:        Kind,
		DisplayName: displayName,
		// The one collection this plugin serves: the todo list. It declares no
		// label, so the swatch reads as the configured instance.
		MenuEntries: []*pluginv1.MenuEntry{{Id: todos.RootContext, Context: todos.RootContext}},
		Watch:       true,
	}, nil
}

// List answers the root, listing weeks, or one week, listing todos. A read
// memory can answer is answered from memory, with the last refresh's failure
// as unreachable; only a cold read whose walk failed fails, with the walk's
// own code.
func (p *Plugin) List(ctx context.Context, req *pluginv1.ListRequest) (*pluginv1.ListResponse, error) {
	if req.Context != todos.RootContext {
		if _, ok := todos.ParseWeekKey(req.Context); !ok {
			return nil, status.Errorf(codes.InvalidArgument, "gitlab plugin: unknown context %q", req.Context)
		}
	}
	start := since(req.Context)
	unreachable, err := p.flights.Read(ctx, req.Context, p.mem.Shows(start))
	if err != nil {
		return nil, err
	}
	if req.Context != todos.RootContext {
		return &pluginv1.ListResponse{Entries: todos.WeekEntries(start, p.mem.Week(start)),
			SourceLabel: req.Context, Unreachable: unreachable}, nil
	}
	weeks := p.mem.Weeks()
	open, done := 0, 0
	for _, w := range weeks {
		open += w.Open
		done += w.Done
	}
	// The totals ride the grid's source label, so the root says at a glance
	// what the walk found.
	return &pluginv1.ListResponse{Entries: todos.RootEntries(weeks), Unreachable: unreachable,
		SourceLabel: fmt.Sprintf("%s · %d open · %d done", displayName, open, done)}, nil
}

// ReadContent answers the todo's markdown: the text tile's face and rendered
// document, whose target link opens an ephemeral visit. An unknown key reads as
// a one-line notice.
func (p *Plugin) ReadContent(req *pluginv1.ReadContentRequest, stream pluginv1.Plugin_ReadContentServer) error {
	id, ok := todos.ParseKey(req.Key)
	if !ok {
		return stream.Send(&pluginv1.ContentChunk{}) // a week key: no body
	}
	t, known := p.mem.Get(id)
	if !known {
		// Before the first completed walk, "not in memory" means "not yet",
		// not "gone": Unavailable, transport-shaped, reads at the node as the
		// source dark while the walk runs, not as the todo gone.
		if !p.mem.Walked() {
			return status.Error(codes.Unavailable, "gitlab plugin: the first walk has not completed")
		}
		return stream.Send(&pluginv1.ContentChunk{Data: todos.GoneMarkdown(req.Key), MediaType: "text/markdown"})
	}
	return stream.Send(&pluginv1.ContentChunk{Data: todos.Markdown(&t), MediaType: "text/markdown"})
}

// Delete is what the trash gesture means here: mark the todo done at GitLab.
// The tile does not vanish — a todo never disappears from the grid, it changes
// state — so the next listing shows it done and the week's counts move. The
// flip lands in memory and its file only after GitLab accepted the
// write, so a refused write changes nothing anywhere. A week well refuses:
// one gesture must not resolve a whole week. An already-done todo succeeds
// without a write — the gesture is idempotent, like fs's already-gone path.
func (p *Plugin) Delete(ctx context.Context, req *pluginv1.DeleteRequest) (*pluginv1.DeleteResponse, error) {
	if _, isWeek := todos.ParseWeekKey(req.Key); isWeek {
		return nil, status.Errorf(codes.FailedPrecondition, "gitlab plugin: a week cannot be marked done — mark its todos")
	}
	id, ok := todos.ParseKey(req.Key)
	if !ok {
		return nil, status.Errorf(codes.InvalidArgument, "gitlab plugin: unknown key %q", req.Key)
	}
	if p.marker == nil {
		return nil, status.Error(codes.Unimplemented, "gitlab plugin: no mark-done writer configured")
	}
	t, known := p.mem.Get(id)
	if !known {
		if !p.mem.Walked() {
			return nil, status.Error(codes.Unavailable, "gitlab plugin: the first walk has not completed")
		}
		return nil, status.Errorf(codes.NotFound, "gitlab plugin: no todo %d", id)
	}
	if t.Done() {
		return &pluginv1.DeleteResponse{}, nil
	}
	if err := p.marker.MarkDone(ctx, id); err != nil {
		return nil, err
	}
	p.mem.MarkDone(id)
	p.announce()
	// The flip is worth a restart. Marking done is not a walk: the save
	// carries the standing walk stamps, never a new one.
	p.save()
	return &pluginv1.DeleteResponse{}, nil
}

// Probe never says GONE: a remembered todo is PRESENT, and one this process
// has not seen is UNSPECIFIED, meaning "cannot say", which keeps the node's
// remembered tile.
func (p *Plugin) Probe(_ context.Context, req *pluginv1.ProbeRequest) (*pluginv1.ProbeResponse, error) {
	if id, ok := todos.ParseKey(req.Key); ok {
		if _, known := p.mem.Get(id); known {
			return &pluginv1.ProbeResponse{Presence: pluginv1.ProbeResponse_PRESENCE_PRESENT}, nil
		}
		return &pluginv1.ProbeResponse{Presence: pluginv1.ProbeResponse_PRESENCE_UNSPECIFIED}, nil
	}
	if start, ok := todos.ParseWeekKey(req.Key); ok {
		if p.mem.Shows(start) {
			return &pluginv1.ProbeResponse{Presence: pluginv1.ProbeResponse_PRESENCE_PRESENT}, nil
		}
		return &pluginv1.ProbeResponse{Presence: pluginv1.ProbeResponse_PRESENCE_UNSPECIFIED}, nil
	}
	return &pluginv1.ProbeResponse{Presence: pluginv1.ProbeResponse_PRESENCE_GONE}, nil
}

// Search matches the query against titles, refs, bodies, projects and
// authors of every remembered todo; each result's path is root → week.
func (p *Plugin) Search(_ context.Context, req *pluginv1.SearchRequest) (*pluginv1.SearchResponse, error) {
	q := strings.ToLower(strings.TrimSpace(req.Query))
	if q == "" {
		return &pluginv1.SearchResponse{}, nil
	}
	limit := int(req.Limit)
	if limit <= 0 {
		limit = 50
	}
	resp := &pluginv1.SearchResponse{}
	all := p.mem.All()
	for i := len(all) - 1; i >= 0 && len(resp.Results) < limit; i-- { // newest first
		t := all[i]
		hay := strings.ToLower(strings.Join([]string{t.Label(), t.Body, t.Project.PathWithNamespace, t.Author.Name, t.Author.Username}, "\n"))
		if !strings.Contains(hay, q) {
			continue
		}
		start := todos.WeekStart(t.CreatedAt)
		entries := todos.WeekEntries(start, []todos.Todo{t})
		if len(entries) == 0 {
			continue
		}
		resp.Results = append(resp.Results, &pluginv1.SearchResult{
			Entry:       entries[0],
			ContextPath: []string{todos.RootContext, todos.WeekKey(start)},
			Snippet:     t.Action(),
			Score:       1,
		})
	}
	return resp, nil
}
