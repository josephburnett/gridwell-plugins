// Package plugin is the gitlab todos plugin: the wire half over
// plugins/gitlab/todos. The root context, "todos", lists weeks; a week,
// "week:<monday>", lists the todos created that week as markdown text tiles.
// Keys are GitLab's todo ids, stable forever. Listings are non-authoritative
// and Probe never answers GONE: a todo never disappears from the grid, it
// changes state when refreshed, and both the node's read-through cache and
// this plugin's own cache file remember it across restarts. The one write is
// Delete, which here means mark-as-done: the trash gesture resolves the todo
// at GitLab rather than removing anything. The plugin holds
// no node fact — no id, no layout — only its memory of GitLab, in the private
// directory the node hands it as `state_dir`.
package plugin

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/josephburnett/gridwell-plugins/gitlab/todos"
	pluginv1 "github.com/josephburnett/gridwell/api/gen/plugin/v1"
)

// Kind is the plugin's declared kind, and the suffix of its binary name.
const Kind = "gitlab"

// displayName is the plugin's own name for itself. The name the user sees is
// server.yaml's `name`, the registry label; this is the fallback when none is
// configured, and the root grid's source label.
const displayName = "gitlab todos"

// DefaultRefresh bounds how often one context re-walks GitLab. The node lists
// a context on every GetGrid and GetTile, and a descent must feel instant
// rather than cost a round of API pages each time.
const DefaultRefresh = 30 * time.Second

// DefaultFirstAnswer bounds how long a List waits on a walk in flight before
// answering what memory holds so far. GitLab pages newest-first, so the first
// answer is the most recent weeks; the walk streams on behind, and the node's
// refresh paints the rest in as pages land. A real cold walk runs minutes —
// waiting for all of it showed the user "loading" the whole time.
const DefaultFirstAnswer = time.Second

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
	firstAnswer time.Duration
	now         func() time.Time
	// cache is the memory's file in the state directory, "" when the node
	// handed no state_dir — then the plugin runs as it always did, walking
	// GitLab from cold at every start.
	cache string
	// logf is the plugin's one log door: the walk's narration, and what must
	// not be swallowed and must not fail a read — a cache the plugin could
	// not read or write, a walk that failed.
	logf func(format string, args ...any)

	mu       sync.Mutex
	syncedAt map[string]time.Time // context → last successful walk
	// flights are the walks in progress, by context. A List that finds one
	// waits for it instead of starting its own, because the node lists a
	// context on every GetGrid and GetTile and a burst of reads must cost
	// GitLab one walk, not one per reader.
	flights map[string]*flight
}

// flight is one walk in progress; done closes when err is final.
type flight struct {
	done chan struct{}
	err  error
}

// Options tunes a plugin. Zero values take the defaults.
type Options struct {
	Refresh     time.Duration
	FirstAnswer time.Duration
	Now         func() time.Time
	// Marker is the mark-as-done writer. Nil means read-only: Delete answers
	// Unimplemented and everything else works as before.
	Marker Marker
	// StateDir is the private directory the node hands the plugin. Empty
	// means no cache: the plugin keeps everything in memory for its process
	// lifetime, as it did before the node handed one out.
	StateDir string
	// Logf takes every line the plugin writes: the walk's narration, and the
	// failures that must not be swallowed and must not fail a read. It
	// defaults to the standard logger, which the node captures from the
	// subprocess's stderr.
	Logf func(format string, args ...any)
}

// New builds a plugin over src. Whether there is a source is decided before
// this point: FromConfig refuses a missing token, and both doors stop the
// launch with its reason. A state directory holding a cache file is loaded
// here, before the plugin serves its first request, so the first listing is
// answered from what the last process walked.
func New(src todos.Source, o Options) *Plugin {
	p := &Plugin{
		src:         retrying{src: src, attempts: pageAttempts, backoff: pageBackoff},
		marker:      o.Marker,
		mem:         todos.NewMemory(),
		refresh:     o.Refresh,
		firstAnswer: o.FirstAnswer,
		now:         o.Now,
		logf:        o.Logf,
		syncedAt:    map[string]time.Time{},
		flights:     map[string]*flight{},
	}
	if p.refresh <= 0 {
		p.refresh = DefaultRefresh
	}
	if p.firstAnswer <= 0 {
		p.firstAnswer = DefaultFirstAnswer
	}
	if p.now == nil {
		p.now = time.Now
	}
	if p.logf == nil {
		p.logf = log.Printf
	}
	if dir := strings.TrimSpace(o.StateDir); dir != "" {
		p.cache = filepath.Join(dir, todos.CacheFile)
		p.loadCache()
	}
	return p
}

// loadCache folds the last process's walk into memory, its landing time
// included: a walk is fresh for the refresh window whichever process ran it,
// so a restart inside that window answers every listing from the file without
// touching GitLab. A missing file is the first boot, which is not news;
// anything else is reported and the plugin starts cold, because a cache is
// disposable and a walk rebuilds it, but a cache that cannot be read must not
// vanish in silence.
func (p *Plugin) loadCache() {
	snap, err := todos.LoadCache(p.cache)
	switch {
	case err == nil:
		p.mem.Restore(snap)
		if !snap.WalkedAt.IsZero() {
			p.syncedAt[todos.RootContext] = snap.WalkedAt
		}
	case errors.Is(err, fs.ErrNotExist):
	default:
		p.logf("gitlab plugin: cache: %v (starting cold)", err)
	}
}

// saveCache writes memory back after a successful walk, stamped with when the
// last root walk landed. A failure is reported and nothing else: the walk
// succeeded, the answer is good, and only the next restart pays for the lost
// write.
func (p *Plugin) saveCache(walkedAt time.Time) {
	if p.cache == "" {
		return
	}
	snap := p.mem.Snapshot()
	snap.WalkedAt = walkedAt
	if err := todos.SaveCache(p.cache, snap); err != nil {
		p.logf("gitlab plugin: cache: %v", err)
	}
}

// MinRefresherInterval is the fastest the background refresher runs, whatever
// the refresh window says. The refresher is a warmer, not a poller: a window
// shorter than a walk would leave it always walking, hammering GitLab and
// answering every read from a walk that started before the read arrived.
// Reads still walk on the configured window — a tiny one is how a test says
// "walk on every read", and that keeps working.
const MinRefresherInterval = time.Second

// refresherInterval is how often the warmer walks: the refresh window, floored.
func (p *Plugin) refresherInterval() time.Duration {
	if p.refresh < MinRefresherInterval {
		return MinRefresherInterval
	}
	return p.refresh
}

// Run keeps the memory warm until ctx is done: one goroutine walking the root
// context on the refresher's interval, so the walk has happened before a read
// asks rather than because one did. It shares the flights and the freshness
// window with the reads, so a tick that lands on a memory a read has just
// refreshed costs GitLab nothing. FromConfig starts it; a walk that fails is
// reported and the next tick tries again, one page back.
func (p *Plugin) Run(ctx context.Context) {
	t := time.NewTicker(p.refresherInterval())
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			// No verdict to read: the walk logs its own start and finish, and
			// sync answers the first-answer bound rather than the walk's end.
			// The refresher's whole job is to make sure a walk happens.
			_ = p.sync(ctx, todos.RootContext, time.Time{})
		}
	}
}

func (p *Plugin) Info(context.Context, *pluginv1.InfoRequest) (*pluginv1.InfoResponse, error) {
	return &pluginv1.InfoResponse{
		Kind:        Kind,
		DisplayName: displayName,
		RootContext: todos.RootContext,
	}, nil
}

// freshLocked reports whether ctxKey was walked within the refresh window. A
// root walk covers every week, so a week is fresh under either. A walk stamped
// in the FUTURE is not fresh: the root stamp can come from the cache file, and
// a clock that has since stepped back would otherwise freeze the plugin on a
// stale memory. The caller holds p.mu.
func (p *Plugin) freshLocked(ctxKey string) bool {
	now := p.now()
	for _, k := range []string{ctxKey, todos.RootContext} {
		if t, ok := p.syncedAt[k]; ok {
			if d := now.Sub(t); d >= 0 && d < p.refresh {
				return true
			}
		}
	}
	return false
}

// sync makes ctxKey answerable: fresh memory as-is, else a walk. since is
// zero for the root, meaning everything, and the Monday for a week. A walk
// already in flight for the context, or for the root, which covers every
// week, is shared — one walk per burst of readers — and no walk belongs to
// its starter: it runs detached, so no reader's patience or hangup can kill
// or restart it. The caller waits at most firstAnswer, then answers what
// memory holds so far: pages land newest-first, so a partial answer is the
// most recent weeks, and the node's refresh paints in the rest.
func (p *Plugin) sync(ctx context.Context, ctxKey string, since time.Time) error {
	p.mu.Lock()
	if p.freshLocked(ctxKey) {
		p.mu.Unlock()
		return nil
	}
	var f *flight
	for _, k := range []string{ctxKey, todos.RootContext} {
		if ex, ok := p.flights[k]; ok {
			f = ex
			break
		}
	}
	if f == nil {
		f = &flight{done: make(chan struct{})}
		p.flights[ctxKey] = f
		go p.walk(ctxKey, since, f)
	}
	p.mu.Unlock()

	select {
	case <-f.done:
		return f.err
	case <-time.After(p.firstAnswer):
		p.logf("gitlab plugin: %q answering with memory so far; the walk streams on", ctxKey)
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// walk is one detached walk: it owns its flight and outlives every reader.
// Its context is the plugin's lifetime — each page REQUEST is bounded by the
// API client's own timeout and each PAGE is retried in place a bounded number
// of times, so a dead source ends the walk with its error rather than hanging
// it.
func (p *Plugin) walk(ctxKey string, since time.Time, f *flight) {
	p.logf("gitlab plugin: walk %q starting (since=%s)", ctxKey, since.Format("2006-01-02"))
	start := time.Now()
	err := p.mem.Sync(context.Background(), p.src, since)
	p.logf("gitlab plugin: walk %q finished in %s: err=%v", ctxKey, time.Since(start).Round(time.Millisecond), err)
	p.mu.Lock()
	if err == nil {
		p.syncedAt[ctxKey] = p.now()
	}
	rootWalk := p.syncedAt[todos.RootContext]
	delete(p.flights, ctxKey)
	p.mu.Unlock()
	// The cache lands before the flight closes: a listing that waited for the
	// walk is one a restart can repeat, and a listing answered early on the
	// firstAnswer bound becomes repeatable as soon as the walk behind it
	// lands.
	if err == nil {
		p.saveCache(rootWalk)
	}
	f.err = err
	close(f.done)
}

// List answers the root, listing weeks, or one week, listing todos. A walk
// failure with a transport-shaped code degrades at the node to the remembered
// listing, stamped stale; a verdict such as a bad token surfaces.
func (p *Plugin) List(ctx context.Context, req *pluginv1.ListRequest) (*pluginv1.ListResponse, error) {
	switch {
	case req.Context == todos.RootContext:
		if err := p.sync(ctx, req.Context, time.Time{}); err != nil {
			return nil, err
		}
		weeks := p.mem.Weeks()
		open, done := 0, 0
		for _, w := range weeks {
			open += w.Open
			done += w.Done
		}
		// The totals ride the grid's source label, so the root says at a
		// glance what the walk found.
		return &pluginv1.ListResponse{Entries: todos.RootEntries(weeks), Authoritative: false,
			SourceLabel: fmt.Sprintf("%s · %d open · %d done", displayName, open, done)}, nil
	default:
		start, ok := todos.ParseWeekKey(req.Context)
		if !ok {
			return nil, status.Errorf(codes.InvalidArgument, "gitlab plugin: unknown context %q", req.Context)
		}
		if err := p.sync(ctx, req.Context, start); err != nil {
			return nil, err
		}
		return &pluginv1.ListResponse{Entries: todos.WeekEntries(start, p.mem.Week(start)), Authoritative: false, SourceLabel: req.Context}, nil
	}
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
		// not "gone" — and it must answer Unavailable, transport-shaped, so
		// the node's cache serves the remembered body instead of storing a
		// Gone body over it while the walk is still running.
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
// flip lands in memory and the cache file only after GitLab accepted the
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
	// The flip is worth a restart: save under the standing walk stamp, not a
	// fresh one — marking done is not a walk and must not extend the window.
	p.mu.Lock()
	walked := p.syncedAt[todos.RootContext]
	p.mu.Unlock()
	p.saveCache(walked)
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
		if len(p.mem.Week(start)) > 0 {
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
