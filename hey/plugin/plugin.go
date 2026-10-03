// Package plugin is the hey plugin: the wire half over
// gridwell-plugins/hey/mail. It projects three of HEY's stacks — the Imbox,
// Reply Later and Set Aside — as three grids, one context each, walked
// through the official HEY CLI and kept current by its live feed (watch.go).
// A thread is a text tile that serves a page:
// its face and document are a markdown card about the email, and descending
// into it opens the email itself, as HEY's own HTML, through the node's
// content door.
//
// It is a READ-ONLY projection. There is no Delete, no WriteContent and no
// write of any kind to HEY: the trash gesture is refused with its reason
// rather than redefined, because "what does deleting an email mean" is the
// user's decision to make and it has not been made.
//
// The plugin holds no node fact — no id, no layout — only its memory of HEY,
// in the private directory the node hands it as `state_dir`. It holds no
// credential either, and looks for none: the CLI keeps its own.
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
	"sync/atomic"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/josephburnett/gridwell-plugins/hey/mail"
	pluginv1 "github.com/josephburnett/gridwell/api/gen/plugin/v1"
)

// Kind is the plugin's declared kind, and the suffix of its binary name.
const Kind = "hey"

// displayName is the plugin's own name for itself. The name the user sees is
// server.yaml's label; this is the fallback when none is configured.
const displayName = "hey"

// DefaultRefresh bounds how often one collection is re-walked while the live
// feed is not live; while it is, the feed keeps memory and nothing is walked
// on a clock (freshLocked).
const DefaultRefresh = time.Minute

// DefaultFirstAnswer bounds how long a cold List — one the memory has no
// listing for — waits on a walk in flight before answering what memory holds
// so far. A cold walk is a whole box read through
// a subprocess; waiting for all of it would show the user "loading" the whole
// time, and the node's refresh paints the rest in when it lands.
const DefaultFirstAnswer = 2 * time.Second

// Source is HEY, as much of it as this plugin reads: one box's threads, one
// thread's HTML, and the live feed of changes. *heycli.Client is the
// production implementation; a test fakes it without spawning anything.
type Source interface {
	Box(ctx context.Context, box string) (threads []mail.Thread, whole bool, err error)
	ThreadHTML(ctx context.Context, topicID int64) ([]byte, error)
	// Watch runs the feed until ctx ends, handing on each line, and answers
	// why it stopped.
	Watch(ctx context.Context, on func(mail.Event, error)) error
}

// Plugin implements pluginv1.PluginServer.
type Plugin struct {
	pluginv1.UnimplementedPluginServer
	src         Source
	mem         *mail.Memory
	refresh     time.Duration
	firstAnswer time.Duration
	now         func() time.Time
	// cache is the memory's file in the state directory, "" when the node
	// handed no state_dir — then the plugin runs cold at every start.
	cache string
	// logf is the plugin's one log door: the sweep's narration, and what must
	// not be swallowed and must not fail a read — a cache it could not read
	// or write, a walk that failed.
	logf func(format string, args ...any)

	mu       sync.Mutex
	walkedAt map[string]time.Time // collection key → last successful walk
	// live is true from the feed's ready until it disconnects or ends.
	// liveGen counts readies, and caughtUp holds, per collection, the ready
	// whose catch-up walk landed: memory is current while both agree.
	live     bool
	liveGen  int
	caughtUp map[string]int
	// failed is the last walk's error, by collection, until one lands; a
	// warm read answers it, having not waited to hear it.
	failed map[string]error
	// watchErr is the verdict the feed last ended on — not signed in, no
	// CLI — until a feed reaches ready. Every read answers it: with the feed
	// down, memory is only as current as the walks.
	watchErr error
	// flights are the walks in progress, by collection. A List that finds one
	// waits for it instead of starting its own, because the node lists a
	// context on every GetGrid and GetTile and a burst of reads must cost HEY
	// one CLI run, not one per reader.
	flights map[string]*flight
	// again marks a collection whose box must be read once more after the
	// walk in flight: the feed asked for a read, and the one running may have
	// begun before what it asked about.
	again map[string]bool

	watchBackoff time.Duration
	recoverAfter time.Duration
	// changes is where every listing change goes out to Watch subscribers.
	changes fanout

	ready func() error
	// readied latches the first Info ready passed: a CLI that goes missing
	// later is a source gone dark, which every read reports.
	readied atomic.Bool
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
	// StateDir is the private directory the node hands the plugin. Empty
	// means no cache: the plugin keeps everything in memory for its process
	// lifetime.
	StateDir string
	// Logf takes every line the plugin writes. It defaults to the standard
	// logger, which the node captures from the subprocess's stderr.
	Logf func(format string, args ...any)
	// WatchBackoff is the first wait before restarting a feed that ended; it
	// doubles to MaxWatchBackoff. Zero means DefaultWatchBackoff.
	WatchBackoff time.Duration
	// RecoverAfter is how long a disconnected feed may take to say ready
	// again before it is restarted. Zero means DefaultRecoverAfter.
	RecoverAfter time.Duration
	// Ready is asked on every Info until it passes, and its error is Info's
	// refusal. Nil means the source is always servable, as a test's fake is.
	Ready func() error
}

// New builds a plugin over src. A state directory holding a cache file is
// loaded here, before the plugin serves its first request, so the first
// listing is answered from what the last process walked.
func New(src Source, o Options) *Plugin {
	p := &Plugin{
		src:          src,
		mem:          mail.NewMemory(),
		refresh:      o.Refresh,
		firstAnswer:  o.FirstAnswer,
		now:          o.Now,
		logf:         o.Logf,
		walkedAt:     map[string]time.Time{},
		caughtUp:     map[string]int{},
		failed:       map[string]error{},
		flights:      map[string]*flight{},
		again:        map[string]bool{},
		watchBackoff: o.WatchBackoff,
		recoverAfter: o.RecoverAfter,
		changes:      fanout{subs: map[*subscriber]struct{}{}},
		ready:        o.Ready,
	}
	if p.watchBackoff <= 0 {
		p.watchBackoff = DefaultWatchBackoff
	}
	if p.recoverAfter <= 0 {
		p.recoverAfter = DefaultRecoverAfter
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
		p.cache = filepath.Join(dir, mail.CacheFile)
		p.loadCache()
	}
	return p
}

// loadCache folds the last process's sweep into memory, each collection's
// landing time included: a walk is fresh for the refresh window whichever
// process ran it, so until the feed is live a restart inside that window
// answers every listing from the file without running the CLI. A missing file is the first boot, which
// is not news; anything else is reported and the plugin starts cold, because
// a cache is disposable and a sweep rebuilds it, but a cache that cannot be
// read must not vanish in silence.
func (p *Plugin) loadCache() {
	snap, err := mail.LoadCache(p.cache)
	switch {
	case err == nil:
		p.mem.Restore(snap)
		for key, in := range snap.Collections {
			if !in.WalkedAt.IsZero() {
				p.walkedAt[key] = in.WalkedAt
			}
		}
	case errors.Is(err, fs.ErrNotExist):
	default:
		p.logf("hey plugin: cache: %v (starting cold)", err)
	}
}

// saveCache writes memory back after a successful walk, each collection
// stamped with when its own walk landed. A failure is reported and nothing
// else: the walk succeeded, the answer is good, and only the next restart
// pays for the lost write.
func (p *Plugin) saveCache() {
	if p.cache == "" {
		return
	}
	snap := p.mem.Snapshot()
	p.mu.Lock()
	for key, in := range snap.Collections {
		in.WalkedAt = p.walkedAt[key]
		snap.Collections[key] = in
	}
	p.mu.Unlock()
	if err := mail.SaveCache(p.cache, snap); err != nil {
		p.logf("hey plugin: cache: %v", err)
	}
}

// MinRefresherInterval is the fastest the background refresher runs, whatever
// the refresh window says. The refresher is a warmer, not a poller: a window
// shorter than a sweep would leave it always sweeping, spawning CLI runs back
// to back. Reads still walk on the configured window — a tiny one is how a
// test says "walk on every read", and that keeps working.
const MinRefresherInterval = time.Second

func (p *Plugin) refresherInterval() time.Duration {
	if p.refresh < MinRefresherInterval {
		return MinRefresherInterval
	}
	return p.refresh
}

// Run keeps the memory warm until ctx is done: the live feed (watch.go), and
// one goroutine sweeping all three collections on the refresher's interval,
// so the walk has happened before a read asks rather than because one did. It shares the flights and
// the freshness window with the reads, so a tick that lands on a memory a
// read has just refreshed costs HEY nothing. Sweeping all three together is
// also what lets Probe ever answer GONE: a thread is archived only when no
// collection holds it, and that takes a complete pass over every one.
func (p *Plugin) Run(ctx context.Context) {
	go p.watch(ctx)
	t := time.NewTicker(p.refresherInterval())
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			for _, c := range mail.Collections {
				// No verdict to read: the walk logs its own start and finish,
				// and sync answers the first-answer bound rather than the
				// walk's end. The refresher's whole job is to make sure a walk
				// happens.
				_ = p.sync(ctx, c)
			}
		}
	}
}

func (p *Plugin) Info(context.Context, *pluginv1.InfoRequest) (*pluginv1.InfoResponse, error) {
	if p.ready != nil && !p.readied.Load() {
		if err := p.ready(); err != nil {
			return nil, status.Error(codes.FailedPrecondition, err.Error())
		}
		p.readied.Store(true)
	}
	return &pluginv1.InfoResponse{
		Kind:        Kind,
		DisplayName: displayName,
		MenuEntries: mail.MenuEntries(),
		// These grids PROJECT a mail account that lives outside Gridwell, so
		// their rows are summaries the node cannot re-arrange across contexts
		// and the client draws them with the host treatment. It is a
		// declaration, never inferred: the node has no list of which kinds
		// are host-backed.
		HostContent: true,
		Watch:       true,
	}, nil
}

// freshLocked reports whether memory is current for the collection. While the
// feed is live, that is whether the collection's catch-up walk landed: the
// feed keeps it from there, and no clock re-walks it. Otherwise it is whether
// the collection was walked within the refresh window. A walk stamped in the
// FUTURE is not fresh: the stamp can come from the cache file, and a clock
// that has since stepped back would otherwise freeze the plugin on a stale
// memory. The caller holds p.mu.
func (p *Plugin) freshLocked(key string) bool {
	if p.live {
		return p.caughtUp[key] == p.liveGen
	}
	t, ok := p.walkedAt[key]
	if !ok {
		return false
	}
	d := p.now().Sub(t)
	return d >= 0 && d < p.refresh
}

// sync makes one collection answerable: fresh memory as-is, else a walk. A
// walk already in flight for it is shared — one CLI run per burst of readers
// — and no walk belongs to its starter: it runs detached, so no reader's
// patience or hangup can kill or restart it. A read the memory already Shows
// a listing for answers at once, with the last failure if there is one; only
// a cold read waits, at most firstAnswer, then answers what memory holds so
// far.
func (p *Plugin) sync(ctx context.Context, c mail.Collection) error {
	warm := p.mem.Shows(c.Key)
	p.mu.Lock()
	last := p.failed[c.Key]
	if last == nil {
		last = p.watchErr
	}
	if p.freshLocked(c.Key) {
		p.mu.Unlock()
		return last
	}
	f, running := p.flights[c.Key]
	if !running {
		f = p.startLocked(c)
	}
	p.mu.Unlock()
	if warm {
		return last
	}

	select {
	case <-f.done:
		return f.err
	case <-time.After(p.firstAnswer):
		p.logf("hey plugin: %q answering with memory so far; the walk runs on", c.Key)
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// rewalk reads one collection's box whatever its freshness: the feed said
// memory cannot know it without a read. A walk already in flight may have
// begun before the change, so it is followed by one more.
func (p *Plugin) rewalk(c mail.Collection) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, running := p.flights[c.Key]; running {
		p.again[c.Key] = true
		return
	}
	p.startLocked(c)
}

// startLocked starts a detached walk of c under a new flight. The caller
// holds p.mu.
func (p *Plugin) startLocked(c mail.Collection) *flight {
	f := &flight{done: make(chan struct{})}
	p.flights[c.Key] = f
	go p.walk(c, f, p.liveGen)
	return f
}

// walk is one detached walk: it owns its flight and outlives every reader.
// Its context is the plugin's lifetime — the CLI run is bounded by the
// runner's own timeout, so a dead source ends the walk with its error rather
// than hanging it. A walk that changed the listing says so to every Watch
// subscriber, whichever door asked for it.
//
// gen is the feed's ready count when the walk started: a walk that began
// after a ready is that ready's catch-up.
func (p *Plugin) walk(c mail.Collection, f *flight, gen int) {
	p.logf("hey plugin: walk %q starting", c.Key)
	start := time.Now()
	p.mem.BeginWalk(c.Key)
	threads, whole, err := p.src.Box(context.Background(), c.Box)
	p.logf("hey plugin: walk %q finished in %s: %d threads, whole=%v, err=%v",
		c.Key, time.Since(start).Round(time.Millisecond), len(threads), whole, err)
	changed := false
	if err == nil {
		changed = p.mem.Absorb(c.Key, threads, whole)
	} else {
		p.mem.EndWalk(c.Key)
	}
	p.mu.Lock()
	if err == nil {
		p.walkedAt[c.Key] = p.now()
		p.caughtUp[c.Key] = gen
		delete(p.failed, c.Key)
	} else {
		p.failed[c.Key] = err
	}
	delete(p.flights, c.Key)
	if p.again[c.Key] {
		delete(p.again, c.Key)
		p.startLocked(c)
	}
	p.mu.Unlock()
	// The cache lands before the flight closes: a listing that waited for the
	// walk is one a restart can repeat.
	if err == nil {
		p.saveCache()
	}
	if changed {
		p.changes.publish(c.Key)
	}
	f.err = err
	close(f.done)
}

// List answers one collection. A walk failure with a transport-shaped code
// degrades at the node to the remembered listing, stamped stale; a verdict
// such as "not signed in" surfaces.
//
// The listing is NOT authoritative even after a whole walk. Absence is the
// node's question to settle through Probe, which is the one place that knows
// whether every collection has been read: a thread missing from the Imbox is
// usually in Reply Later, and an authoritative Imbox would retire its id and
// lose the user's placement on the way past.
func (p *Plugin) List(ctx context.Context, req *pluginv1.ListRequest) (*pluginv1.ListResponse, error) {
	c, ok := mail.LookupCollection(req.Context)
	if !ok {
		return nil, status.Errorf(codes.InvalidArgument, "hey plugin: unknown context %q", req.Context)
	}
	if err := p.sync(ctx, c); err != nil {
		return nil, err
	}
	threads := p.mem.Collection(c.Key)
	unseen := 0
	for i := range threads {
		if !threads[i].Seen {
			unseen++
		}
	}
	return &pluginv1.ListResponse{
		Entries:       mail.CollectionEntries(threads),
		Authoritative: false,
		SourceLabel:   fmt.Sprintf("%s · %d threads · %d unseen", c.Label, len(threads), unseen),
	}, nil
}

// ReadContent answers the thread's markdown card: the tile's face and its
// read-only document. The email itself is ServeContent's answer, not this
// one. An unknown key reads as a one-line notice.
func (p *Plugin) ReadContent(req *pluginv1.ReadContentRequest, stream pluginv1.Plugin_ReadContentServer) error {
	id, ok := mail.ParseKey(req.Key)
	if !ok {
		return stream.Send(&pluginv1.ContentChunk{}) // not one of ours: no body
	}
	t, known := p.mem.Get(id)
	if !known {
		// Before any collection has been swept, "not in memory" means "not
		// yet", not "gone" — and it must answer Unavailable, transport-shaped,
		// so the node's cache serves the remembered body instead of storing a
		// gone body over it while the sweep is still running.
		if !p.mem.Swept() {
			return status.Error(codes.Unavailable, "hey plugin: the first sweep has not completed")
		}
		return stream.Send(&pluginv1.ContentChunk{Data: mail.GoneMarkdown(req.Key), MediaType: "text/markdown"})
	}
	return stream.Send(&pluginv1.ContentChunk{Data: mail.Markdown(&t), MediaType: "text/markdown"})
}

// ServeContent is the email. Subpath "" is the thread, as mail.Page presents
// the HTML HEY served; any other subpath is a resource named by a relative
// URL, and those are paths in HEY's own app that this door cannot serve, so it
// is an ordinary 404 rather than an error. The document is
// fetched on descent and never remembered: a mailbox listing is small and a
// mailbox's bodies are not.
func (p *Plugin) ServeContent(req *pluginv1.ServeContentRequest, stream pluginv1.Plugin_ServeContentServer) error {
	id, ok := mail.ParseKey(req.Key)
	if !ok || req.Subpath != "" {
		return stream.Send(&pluginv1.ServeContentChunk{
			Status:    404,
			MediaType: "text/plain; charset=utf-8",
			Data:      []byte("not found"),
		})
	}
	html, err := p.src.ThreadHTML(context.Background(), id)
	if err != nil {
		// The reason travels: the node turns a coded failure into an answer
		// the user can read, and a silent blank page would look like an email
		// with nothing in it.
		return err
	}
	if len(html) == 0 {
		t, known := p.mem.Get(id)
		title := req.Key
		if known {
			title = t.Title()
		}
		html = mail.NoticeHTML(title, "HEY served no body for this thread.")
	}
	return stream.Send(&pluginv1.ServeContentChunk{
		Status:    200,
		MediaType: "text/html; charset=utf-8",
		Data:      mail.Page(html, id),
	})
}

// Probe is the one place that decides a thread has left. PRESENT while some
// collection holds it. GONE only once every collection has been read to its
// end and none of them does — the thread was archived, trashed or filed
// somewhere this plugin does not project, and the node may retire its id.
// UNSPECIFIED, meaning "cannot say", until then: a half-swept memory must
// never cost the user a tile.
func (p *Plugin) Probe(_ context.Context, req *pluginv1.ProbeRequest) (*pluginv1.ProbeResponse, error) {
	id, ok := mail.ParseKey(req.Key)
	if !ok {
		return &pluginv1.ProbeResponse{Presence: pluginv1.ProbeResponse_PRESENCE_GONE}, nil
	}
	switch {
	case p.mem.Member(id):
		return &pluginv1.ProbeResponse{Presence: pluginv1.ProbeResponse_PRESENCE_PRESENT}, nil
	case p.mem.Swept():
		return &pluginv1.ProbeResponse{Presence: pluginv1.ProbeResponse_PRESENCE_GONE}, nil
	default:
		return &pluginv1.ProbeResponse{Presence: pluginv1.ProbeResponse_PRESENCE_UNSPECIFIED}, nil
	}
}

// Search matches the query against the subject, preview and sender of every
// thread a collection still holds; each result's path is that collection. It
// reads memory only — no CLI run — so it answers what the grids show and
// nothing the user cannot navigate to.
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
		if !p.mem.Member(t.TopicID) {
			continue
		}
		hay := strings.ToLower(strings.Join([]string{t.Subject, t.Summary, t.FromName, t.FromEmail}, "\n"))
		if !strings.Contains(hay, q) {
			continue
		}
		entries := mail.CollectionEntries([]mail.Thread{t})
		if len(entries) == 0 {
			continue
		}
		resp.Results = append(resp.Results, &pluginv1.SearchResult{
			Entry:       entries[0],
			ContextPath: []string{t.Collection},
			Snippet:     t.Snippet(),
			Score:       1,
		})
	}
	return resp, nil
}

// Delete is refused rather than left to the embedded Unimplemented, so the
// reason travels. This plugin is a read-only projection: what the trash
// gesture should mean for an email — archive it, trash it, mark it done — is
// a decision about the user's mail, and it has not been made. A gesture that
// silently did nothing would look like one that failed to stick.
func (p *Plugin) Delete(context.Context, *pluginv1.DeleteRequest) (*pluginv1.DeleteResponse, error) {
	return nil, status.Error(codes.Unimplemented,
		"hey plugin: this is a read-only projection of HEY; nothing here writes to your mail")
}
