// Package plugin is the hey plugin: the wire half over
// gridwell-plugins/hey/mail. It projects HEY's six boxes as six grids, one
// context each, walked through the official HEY CLI and kept current by its
// live feed while a Watch stream is open (watch.go), and everything, their
// union, as a seventh. A thread is one url tile in everything that serves a
// page, the email itself as HEY's own HTML through the node's content door;
// a box lists links to it, so a thread that moves between boxes keeps its
// one tile.
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
	"fmt"
	"log"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/josephburnett/gridwell-plugins/hey/mail"
	"github.com/josephburnett/gridwell-plugins/memo"
	pluginv1 "github.com/josephburnett/gridwell/api/gen/plugin/v1"
)

// Kind is the plugin's declared kind, and the suffix of its binary name.
const Kind = "hey"

// displayName is the plugin's own name for itself. The name the user sees is
// server.yaml's label; this is the fallback when none is configured.
const displayName = "hey"

// DefaultRefresh is how long a landed walk answers reads while the live feed
// is not live; while it is, the feed keeps memory and no clock re-walks
// (fresh).
const DefaultRefresh = time.Minute

// DefaultFirstAnswer bounds how long a cold List waits on its walk. A cold
// walk is a whole box read through a subprocess; waiting for all of it would
// show the user "loading" the whole time, and the node's refresh paints the
// rest in when it lands.
const DefaultFirstAnswer = 2 * time.Second

// cacheVersion is the shape of cache; memo.File refuses any other.
const cacheVersion = 2

// cache is what the file in state_dir holds: memory, and when each
// collection's walk landed, so a restart inside the refresh window answers
// without running the CLI.
type cache struct {
	Memory   mail.Snapshot        `json:"memory"`
	WalkedAt map[string]time.Time `json:"walkedAt"`
}

// feedUnit is the plugin's one unit of background work: the account-wide
// live feed, which every context needs.
const feedUnit = "feed"

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
	src     Source
	mem     *mail.Memory
	life    *memo.Life
	file    *memo.File[cache]
	flights *memo.Flights
	changes *memo.Changes
	refresh time.Duration
	clock   memo.Clock
	logf    func(format string, args ...any)

	mu sync.Mutex
	// live is true from the feed's ready until it disconnects or ends.
	// liveGen counts readies, and caughtUp holds, per collection, the ready
	// whose catch-up walk landed: memory is current while both agree. asked
	// counts the re-reads the feed asked of each collection (a resync, a
	// delete memory could not map), so a walk begun before the latest ask
	// catches nothing up.
	live     bool
	liveGen  int
	caughtUp map[string]catchUp
	asked    map[string]int
	// watchErr is the verdict the feed last ended on — not signed in —
	// until a feed reaches ready. With the feed down, memory is only as
	// current as the walks, and every read says why.
	watchErr error
	// effects are what each landed walk changed, held for flights' Landed to
	// announce once the landing is recorded.
	effects map[string]mail.Effect
	// failing holds the collections whose last walk failed. Only the edges
	// repaint: the first failure, so an open grid shows the reason, and the
	// landing after it, so the reason leaves.
	failing map[string]bool

	watchBackoff time.Duration
	recoverAfter time.Duration

	ready func() error
	// readied latches the first Info ready passed: a CLI that goes missing
	// later is a source gone dark, which every read reports.
	readied atomic.Bool
}

// catchUp is the walk that last caught one collection up with the feed: the
// ready it followed, and whether it read the whole box.
type catchUp struct {
	gen   int
	whole bool
}

// Options tunes a plugin. Zero values take the defaults.
type Options struct {
	Refresh     time.Duration
	FirstAnswer time.Duration
	Now         func() time.Time
	// Life is what walks and the feed run under. Nil starts one.
	Life *memo.Life
	// Linger is how long the feed outlives the last Watch stream (memo's).
	Linger time.Duration
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

// clock is memo's clock over a test's Now.
type clock struct{ now func() time.Time }

func (c clock) Now() time.Time                         { return c.now() }
func (c clock) After(d time.Duration) <-chan time.Time { return time.After(d) }

// New builds a plugin over src. A cache file in the state directory is
// loaded here, before the plugin serves its first request, so the first
// listing is answered from what the last process walked. Nothing runs until
// a call or a Watch stream asks.
func New(src Source, o Options) *Plugin {
	p := &Plugin{
		src:          src,
		mem:          mail.NewMemory(),
		life:         o.Life,
		refresh:      o.Refresh,
		logf:         o.Logf,
		caughtUp:     map[string]catchUp{},
		asked:        map[string]int{},
		effects:      map[string]mail.Effect{},
		failing:      map[string]bool{},
		watchBackoff: o.WatchBackoff,
		recoverAfter: o.RecoverAfter,
		ready:        o.Ready,
	}
	if p.life == nil {
		p.life = memo.NewLife()
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
	if o.FirstAnswer <= 0 {
		o.FirstAnswer = DefaultFirstAnswer
	}
	p.clock = memo.System
	if o.Now != nil {
		p.clock = clock{o.Now}
	}
	if p.logf == nil {
		p.logf = log.Printf
	}
	p.flights = memo.NewFlights(p.life, memo.FlightOptions{
		Name:        "hey plugin",
		Walk:        p.walk,
		Fresh:       p.fresh,
		FirstAnswer: o.FirstAnswer,
		Landed:      p.landed,
		Clock:       p.clock,
		Logf:        p.logf,
	})
	p.changes = memo.NewChanges(p.life, memo.ChangeOptions{
		Unscoped: contexts(),
		Work:     func(string) []string { return []string{feedUnit} },
		Do:       func(ctx context.Context, _ string) { p.watch(ctx) },
		Linger:   o.Linger,
		Clock:    p.clock,
	})
	p.file = memo.NewFile[cache](strings.TrimSpace(o.StateDir), mail.CacheFile, cacheVersion, p.logf)
	if c, ok := p.file.Load(); ok {
		p.mem.Restore(c.Memory)
		p.flights.Restore(c.WalkedAt)
	}
	return p
}

// contexts is every context the plugin lists: the boxes, then everything.
func contexts() []string {
	out := make([]string, 0, len(mail.Collections)+1)
	for _, c := range mail.Collections {
		out = append(out, c.Key)
	}
	return append(out, mail.EverythingContext)
}

// save writes memory back. A failure is memo.File's to log; the answer is
// good, and only the next restart pays for the lost write.
func (p *Plugin) save() {
	_ = p.file.Save(func() cache {
		return cache{Memory: p.mem.Snapshot(), WalkedAt: p.flights.WalkedAt()}
	})
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

// fresh says whether memory is current for a collection, as memo.Flights
// asks it. While the feed is live, that is whether the collection's catch-up
// walk landed: the feed keeps it from there, and no clock re-walks it.
// Otherwise it is whether the collection was walked within the refresh
// window.
func (p *Plugin) fresh(key string, walkedAt time.Time) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.live {
		return p.caughtUpLocked(key)
	}
	return memo.Within(p.clock.Now(), walkedAt, p.refresh)
}

// caughtUpLocked says the collection's last landed walk followed the feed's
// latest ready and every re-read the feed asked of it since. The caller
// holds p.mu.
func (p *Plugin) caughtUpLocked(key string) bool {
	cu, ok := p.caughtUp[key]
	return ok && cu.gen == p.liveGen
}

// authoritative says a box's listing is definitive (rule 4): its catch-up
// walk read the whole box, and the live feed has kept it since. A capped
// walk, a feed that is down, or a re-read the feed asked for and no walk has
// answered is silence, and absence is never inferred from silence.
func (p *Plugin) authoritative(key string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.live && p.caughtUpLocked(key) && p.caughtUp[key].whole
}

// ask marks a collection as needing a re-read the feed cannot give, and
// starts it: until a walk begun after now lands, the collection is neither
// current nor definitive.
func (p *Plugin) ask(key string) {
	p.mu.Lock()
	p.asked[key]++
	delete(p.caughtUp, key)
	p.mu.Unlock()
	p.flights.Rewalk(key)
}

// walk is one detached walk of one collection, run by memo.Flights under the
// plugin's lifetime: no reader's hangup ends it, and the CLI run's own
// timeout bounds it. A walk that began after a ready is that ready's
// catch-up.
func (p *Plugin) walk(ctx context.Context, key string) error {
	c, ok := mail.LookupCollection(key)
	if !ok {
		return status.Errorf(codes.InvalidArgument, "hey plugin: unknown context %q", key)
	}
	p.mu.Lock()
	gen, ask := p.liveGen, p.asked[key]
	p.mu.Unlock()
	p.mem.BeginWalk(key)
	threads, whole, err := p.src.Box(ctx, c.Box)
	if err != nil {
		p.mem.EndWalk(key)
		return err
	}
	eff := p.mem.Absorb(key, threads, whole)
	p.mu.Lock()
	if p.asked[key] == ask {
		p.caughtUp[key] = catchUp{gen: gen, whole: whole}
	}
	p.effects[key] = merge(p.effects[key], eff)
	p.mu.Unlock()
	return nil
}

// landed is memo.Flights' Landed: the cache lands before any waiting reader
// is released, so a listing that waited is one a restart repeats, and then
// the change goes out, so the node's re-list finds the landing recorded.
func (p *Plugin) landed(key string, err error) {
	p.mu.Lock()
	edge := p.failing[key] != (err != nil)
	if err != nil {
		p.failing[key] = true
	} else {
		delete(p.failing, key)
	}
	eff := p.effects[key]
	delete(p.effects, key)
	p.mu.Unlock()
	if err == nil {
		p.save()
	}
	if edge {
		eff.Changed, eff.Everything = true, true
	}
	p.publish(key, eff)
}

func merge(a, b mail.Effect) mail.Effect {
	return mail.Effect{Changed: a.Changed || b.Changed, Everything: a.Everything || b.Everything, Rewalk: a.Rewalk || b.Rewalk}
}

// publish tells every Watch stream what one change moved: the box's listing,
// everything's, or both.
func (p *Plugin) publish(box string, eff mail.Effect) {
	var keys []string
	if eff.Changed {
		keys = append(keys, box)
	}
	if eff.Everything {
		keys = append(keys, mail.EverythingContext)
	}
	p.changes.Publish(keys...)
}

// read makes one collection answerable (memo.Flights.Read): a read memory
// has a listing for never waits. A refresh that failed is not the read's
// failure: memory answers, and unreachable says why (rule 7), the walk's
// failure first and then the feed's verdict. err is only the caller hanging
// up.
func (p *Plugin) read(ctx context.Context, key string) (unreachable string, err error) {
	reason, err := p.flights.Read(ctx, key, p.mem.Shows(key))
	if err != nil {
		if ctx.Err() != nil {
			return "", err
		}
		reason = memo.Reason(err)
	}
	if reason == "" {
		p.mu.Lock()
		reason = memo.Reason(p.watchErr)
		p.mu.Unlock()
	}
	return reason, nil
}

// List answers one collection from memory, refreshed first when it is not
// current. A failed refresh is the listing's unreachable reason, which the
// node reports as the source's health while it keeps serving the rows.
//
// A box is authoritative when its listing is definitive (authoritative): the
// node then retires its link to a thread that left, which costs nothing, since
// the thread's one tile is in everything. Everything never is: a thread in no
// box may still be in HEY, which only Probe asks.
//
// A box lists links into everything (mail.BoxEntries); everything lists each
// thread once, and is as current as every box is.
func (p *Plugin) List(ctx context.Context, req *pluginv1.ListRequest) (*pluginv1.ListResponse, error) {
	if req.Context == mail.EverythingContext {
		reason, err := p.readAll(ctx)
		if err != nil {
			return nil, err
		}
		threads := p.mem.Everything()
		return listing(mail.EverythingLabel, threads, mail.CollectionEntries(threads), reason, false), nil
	}
	c, ok := mail.LookupCollection(req.Context)
	if !ok {
		return nil, status.Errorf(codes.InvalidArgument, "hey plugin: unknown context %q", req.Context)
	}
	reason, err := p.read(ctx, c.Key)
	if err != nil {
		return nil, err
	}
	definitive := reason == "" && p.authoritative(c.Key)
	threads := p.mem.Collection(c.Key)
	return listing(c.Label, threads, mail.BoxEntries(threads), reason, definitive), nil
}

func listing(label string, threads []mail.Thread, entries []*pluginv1.Entry, unreachable string, authoritative bool) *pluginv1.ListResponse {
	unseen := 0
	for i := range threads {
		if !threads[i].Seen {
			unseen++
		}
	}
	return &pluginv1.ListResponse{
		Entries:       entries,
		Authoritative: authoritative,
		SourceLabel:   fmt.Sprintf("%s · %d threads · %d unseen", label, len(threads), unseen),
		Unreachable:   unreachable,
	}
}

// readAll is read over every box at once. Everything is the union of what
// memory holds for each, so a box that cannot be read costs it that box's
// news, never the listing: its reason is everything's, the first in box
// order.
func (p *Plugin) readAll(ctx context.Context) (unreachable string, err error) {
	reasons := make([]string, len(mail.Collections))
	errs := make([]error, len(mail.Collections))
	var wg sync.WaitGroup
	for i, c := range mail.Collections {
		wg.Go(func() { reasons[i], errs[i] = p.read(ctx, c.Key) })
	}
	wg.Wait()
	for i := range errs {
		if errs[i] != nil {
			return "", errs[i]
		}
	}
	for _, r := range reasons {
		if r != "" {
			return r, nil
		}
	}
	return "", nil
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
	html, err := p.src.ThreadHTML(stream.Context(), id)
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

// Probe is the one place that decides a thread has left, and it answers for
// the context asked. A box: PRESENT while it holds the thread, GONE once a
// whole walk did not list it or the feed moved it out (mail.InBox). The
// thread is usually in another box, and its tile in everything stays.
// Everything: PRESENT while some box holds it, else HEY's own word on the
// thread — GONE only when `thread read` says it does not exist. No context
// (a node from before contexts): PRESENT while some box holds it, GONE once
// every box has been read to its end and none does. UNSPECIFIED, meaning
// "cannot say", otherwise: a half-swept memory must never cost the user a
// tile.
func (p *Plugin) Probe(ctx context.Context, req *pluginv1.ProbeRequest) (*pluginv1.ProbeResponse, error) {
	id, ok := mail.ParseKey(req.Key)
	if !ok {
		return presence(pluginv1.ProbeResponse_PRESENCE_GONE), nil
	}
	switch req.Context {
	case "":
		switch {
		case p.mem.Member(id):
			return presence(pluginv1.ProbeResponse_PRESENCE_PRESENT), nil
		case p.mem.Swept():
			return presence(pluginv1.ProbeResponse_PRESENCE_GONE), nil
		}
	case mail.EverythingContext:
		if p.mem.Member(id) {
			return presence(pluginv1.ProbeResponse_PRESENCE_PRESENT), nil
		}
		_, err := p.src.ThreadHTML(ctx, id)
		switch {
		case err == nil:
			return presence(pluginv1.ProbeResponse_PRESENCE_PRESENT), nil
		case status.Code(err) == codes.NotFound:
			return presence(pluginv1.ProbeResponse_PRESENCE_GONE), nil
		}
	default:
		if _, ok := mail.LookupCollection(req.Context); !ok {
			return presence(pluginv1.ProbeResponse_PRESENCE_GONE), nil // a context this plugin never lists
		}
		switch p.mem.InBox(req.Context, id) {
		case mail.Present:
			return presence(pluginv1.ProbeResponse_PRESENCE_PRESENT), nil
		case mail.Gone:
			return presence(pluginv1.ProbeResponse_PRESENCE_GONE), nil
		}
	}
	return presence(pluginv1.ProbeResponse_PRESENCE_UNSPECIFIED), nil
}

func presence(p pluginv1.ProbeResponse_Presence) *pluginv1.ProbeResponse {
	return &pluginv1.ProbeResponse{Presence: p}
}

// Search matches the query against the subject, preview and sender of every
// thread a collection still holds; each result is the thread's one tile, in
// everything. It reads memory only — no CLI run — so it answers what the
// grids show and nothing the user cannot navigate to.
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
			ContextPath: []string{mail.EverythingContext},
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
