// Package plugin is the gmail plugin: the wire half over
// gridwell-plugins/gmail/mailbox. It projects two of Gmail's labels — the
// inbox and the starred mail — as two grids, one context each, read through
// the Gmail API. A message is a text tile that serves a page: its face and
// document are a markdown card about the email, and descending into it opens
// the email itself, as the HTML the sender wrote, through the node's content
// door.
//
// It is a READ-ONLY projection. The token it holds carries the
// gmail.readonly scope and nothing else, and there is no Delete, no
// WriteContent and no write of any kind to Gmail: the trash gesture is
// refused with its reason rather than redefined, because "what does deleting
// an email mean" is the user's decision to make and it has not been made.
//
// The plugin holds no node fact — no id, no layout — only its memory of
// Gmail, in the private directory the node hands it as `state_dir`. Its
// credentials are not there and never will be: they are host-local files the
// user named, because the state directory is disposable and a deleted
// credential is not rewarmed by use.
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

	"github.com/josephburnett/gridwell-plugins/gmail/mailbox"
	pluginv1 "github.com/josephburnett/gridwell/api/gen/plugin/v1"
)

// Kind is the plugin's declared kind, and the suffix of its binary name.
const Kind = "gmail"

// displayName is the plugin's own name for itself. The name the user sees is
// server.yaml's label; this is the fallback when none is configured.
const displayName = "gmail"

// DefaultRefresh bounds how often memory catches up with Gmail. The node
// lists a context on every GetGrid and GetTile, and a descent must feel
// instant rather than cost a round trip to Google each time.
const DefaultRefresh = time.Minute

// SweepEvery is how old the last full walk may grow before a refresh walks
// every collection again instead of reading history. History carries every
// change, but a catch-up only sees labels it watches and messages it could
// read, so once a day the cold path runs as a consistency pass.
const SweepEvery = 24 * time.Hour

// DefaultFirstAnswer bounds how long a cold List — one the memory has no
// answer for — waits on a refresh in flight before answering what memory
// holds so far. A cold refresh is a label listing plus a metadata read per
// new message; waiting for all of it would show the user "loading" the whole
// time, and the node's refresh paints the rest in when it lands.
const DefaultFirstAnswer = 2 * time.Second

// DefaultMaxMessages bounds one collection's grid. A mailbox has no end, and
// a grid with a hundred thousand tiles on it is not a place. The newest N are
// what a person is looking at; older mail is what search is for.
//
// It is not a truncation the plugin hides: a read that stops here is not a
// whole read, and mailbox.Memory's watermark keeps everything below it rather
// than retiring tiles the read never reached.
const DefaultMaxMessages = 500

// Source is Gmail, as much of it as this plugin reads. *gmailapi.Client is
// the production implementation; a test fakes it without any HTTP at all.
type Source interface {
	// Label answers the ids one label (or intersection of labels) holds,
	// newest first, up to limit, and whether the read reached the end.
	Label(ctx context.Context, labelIDs []string, limit int) (ids []string, whole bool, err error)
	// Headers answers one message's record and the labels it carries now. An
	// error with codes.NotFound means Gmail no longer has the message.
	Headers(ctx context.Context, id string) (mailbox.Message, []string, error)
	// HTML answers one message's body and its media type. An empty body is
	// not an error: some messages have none.
	HTML(ctx context.Context, id string) (body []byte, mediaType string, err error)
	// HistoryID answers the account's current history id.
	HistoryID(ctx context.Context) (uint64, error)
	// History answers every change since the history id that names one of
	// labels, or mailbox.ErrHistoryExpired when the id is too old.
	History(ctx context.Context, since uint64, labels []string) (mailbox.Delta, error)
}

// Plugin implements pluginv1.PluginServer.
type Plugin struct {
	pluginv1.UnimplementedPluginServer
	src         Source
	mem         *mailbox.Memory
	refresh     time.Duration
	firstAnswer time.Duration
	max         int
	now         func() time.Time
	// cache is the memory's file in the state directory, "" when the node
	// handed no state_dir — then the plugin runs cold at every start.
	cache string
	// logf is the plugin's one log door: the refresh's narration, and what
	// must not be swallowed and must not fail a read — a cache it could not
	// read or write, a metadata fetch that failed.
	logf func(format string, args ...any)

	// A refresh is of the whole account, never of one collection: Gmail's
	// history is account-wide, and one history id is current to every
	// collection at once.
	mu       sync.Mutex
	syncedAt time.Time // last refresh that landed
	sweptAt  time.Time // last full walk that landed
	// flight is the refresh in progress. A List that finds one joins it
	// instead of starting its own, because the node lists a context on every
	// GetGrid and GetTile and a burst of reads must cost Gmail one refresh,
	// not one per reader.
	flight *flight
	// failed is the last refresh's error until a refresh lands. A warm read
	// answers it, having not waited to hear it.
	failed error
}

// flight is one refresh in progress; done closes when err is final.
type flight struct {
	done chan struct{}
	err  error
}

// Options tunes a plugin. Zero values take the defaults.
type Options struct {
	Refresh     time.Duration
	FirstAnswer time.Duration
	MaxMessages int
	Now         func() time.Time
	// StateDir is the private directory the node hands the plugin. Empty
	// means no cache: the plugin keeps everything in memory for its process
	// lifetime.
	StateDir string
	// Logf takes every line the plugin writes. It defaults to the standard
	// logger, which the node captures from the subprocess's stderr.
	Logf func(format string, args ...any)
}

// New builds a plugin over src. A state directory holding a cache file is
// loaded here, before the plugin serves its first request, so the first
// listing is answered from what the last process walked.
func New(src Source, o Options) *Plugin {
	p := &Plugin{
		src:         src,
		mem:         mailbox.NewMemory(),
		refresh:     o.Refresh,
		firstAnswer: o.FirstAnswer,
		max:         o.MaxMessages,
		now:         o.Now,
		logf:        o.Logf,
	}
	if p.refresh <= 0 {
		p.refresh = DefaultRefresh
	}
	if p.firstAnswer <= 0 {
		p.firstAnswer = DefaultFirstAnswer
	}
	if p.max <= 0 {
		p.max = DefaultMaxMessages
	}
	if p.now == nil {
		p.now = time.Now
	}
	if p.logf == nil {
		p.logf = log.Printf
	}
	if dir := strings.TrimSpace(o.StateDir); dir != "" {
		p.cache = filepath.Join(dir, mailbox.CacheFile)
		p.loadCache()
	}
	return p
}

// loadCache folds the last process's memory in, with when its last refresh
// and full walk landed: a refresh is fresh for the refresh window whichever
// process ran it, so a restart inside that window answers every listing from
// the file without touching Gmail, and one past it catches up from the
// remembered history id instead of walking. A missing file is the first boot,
// which is not news; anything else is reported and the plugin starts cold,
// because a cache is disposable and a walk rebuilds it, but a cache that
// cannot be read must not vanish in silence.
func (p *Plugin) loadCache() {
	snap, err := mailbox.LoadCache(p.cache)
	switch {
	case err == nil:
		p.mem.Restore(snap)
		p.syncedAt, p.sweptAt = snap.SyncedAt, snap.SweptAt
	case errors.Is(err, fs.ErrNotExist):
	default:
		p.logf("gmail plugin: cache: %v (starting cold)", err)
	}
}

// saveCache writes memory back after a refresh lands. A failure is reported
// and nothing else: the refresh succeeded, the answer is good, and only the
// next restart pays for the lost write.
func (p *Plugin) saveCache() {
	if p.cache == "" {
		return
	}
	snap := p.mem.Snapshot()
	p.mu.Lock()
	snap.SyncedAt, snap.SweptAt = p.syncedAt, p.sweptAt
	p.mu.Unlock()
	if err := mailbox.SaveCache(p.cache, snap); err != nil {
		p.logf("gmail plugin: cache: %v", err)
	}
}

// MinRefresherInterval is the fastest the background refresher runs, whatever
// the refresh window says. The refresher is a warmer, not a poller: a window
// shorter than a refresh would leave it always refreshing, hammering Gmail.
// Reads still refresh on the configured window — a tiny one is how a test
// says "refresh on every read", and that keeps working.
const MinRefresherInterval = time.Second

func (p *Plugin) refresherInterval() time.Duration {
	if p.refresh < MinRefresherInterval {
		return MinRefresherInterval
	}
	return p.refresh
}

// Run keeps the memory warm until ctx is done: one goroutine refreshing on
// the refresher's interval, so the refresh has happened before a read asks
// rather than because one did. It shares the flight and the freshness window
// with the reads, so a tick that lands on a memory a read has just refreshed
// costs Gmail nothing.
func (p *Plugin) Run(ctx context.Context) {
	t := time.NewTicker(p.refresherInterval())
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			// No verdict to read: the refresh logs its own start and finish.
			// The refresher's whole job is to make sure a refresh happens.
			p.kick()
		}
	}
}

func (p *Plugin) Info(context.Context, *pluginv1.InfoRequest) (*pluginv1.InfoResponse, error) {
	return &pluginv1.InfoResponse{
		Kind:        Kind,
		DisplayName: displayName,
		MenuEntries: mailbox.MenuEntries(),
		// These grids PROJECT a mail account that lives outside Gridwell, so
		// their rows are summaries the node cannot re-arrange across contexts
		// and the client draws them with the host treatment. It is a
		// declaration, never inferred: the node has no list of which kinds
		// are host-backed.
		HostContent: true,
	}, nil
}

// withinLocked reports whether t is less than d ago. A stamp in the FUTURE is
// not within: it can come from the cache file, and a clock that has since
// stepped back would otherwise freeze the plugin on a stale memory. The
// caller holds p.mu.
func (p *Plugin) withinLocked(t time.Time, d time.Duration) bool {
	if t.IsZero() {
		return false
	}
	age := p.now().Sub(t)
	return age >= 0 && age < d
}

// kick makes sure memory is fresh or a refresh is on its way, and answers the
// flight to wait on — nil when memory is fresh — with the last refresh's
// error. No refresh belongs to its starter: it runs detached, so no reader's
// patience or hangup can kill or restart it.
func (p *Plugin) kick() (*flight, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.withinLocked(p.syncedAt, p.refresh) {
		return nil, nil
	}
	if p.flight == nil {
		p.flight = &flight{done: make(chan struct{})}
		go p.refreshFlight(p.flight)
	}
	return p.flight, p.failed
}

// sync makes one collection answerable. A read the memory already Shows
// something for answers at once, with the last failed refresh's error if
// there is one: waiting on the refresh would tax every read past the refresh
// window for an answer memory already has. Only a cold read waits, at most
// firstAnswer, then answers what memory holds so far.
func (p *Plugin) sync(ctx context.Context, c mailbox.Collection) error {
	warm := p.mem.Shows(c.Key)
	f, last := p.kick()
	if f == nil {
		return nil
	}
	if warm {
		return last
	}
	select {
	case <-f.done:
		return f.err
	case <-time.After(p.firstAnswer):
		p.logf("gmail plugin: %q answering with memory so far; the refresh runs on", c.Key)
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// refreshFlight is one detached refresh: it owns its flight and outlives
// every reader. Its context is the plugin's lifetime — every Gmail call is
// bounded by the client's own timeout, so a dead source ends the refresh
// with its error rather than hanging it.
func (p *Plugin) refreshFlight(f *flight) {
	start := time.Now()
	swept, err := p.catchUpOrSweep(context.Background())
	how := "history"
	if swept {
		how = "full walk"
	}
	p.logf("gmail plugin: refresh (%s) finished in %s: err=%v", how, time.Since(start).Round(time.Millisecond), err)
	p.mu.Lock()
	if err == nil {
		p.syncedAt = p.now()
		if swept {
			p.sweptAt = p.syncedAt
		}
	}
	p.failed = err
	p.flight = nil
	p.mu.Unlock()
	// The cache lands before the flight closes: a listing that waited for the
	// refresh is one a restart can repeat, and a listing answered without
	// waiting becomes repeatable as soon as the refresh behind it lands.
	if err == nil {
		p.saveCache()
	}
	f.err = err
	close(f.done)
}

// catchUpOrSweep is the one rule for how memory catches up. A full walk of
// every collection when memory has no history id, has not seen every
// collection, or last walked SweepEvery ago; else Gmail's history since the
// id memory is current to, and a full walk after all when Gmail says that id
// is too old. swept reports that the full walk ran.
func (p *Plugin) catchUpOrSweep(ctx context.Context) (swept bool, err error) {
	p.mu.Lock()
	due := p.mem.HistoryID() == 0 || !p.mem.Swept() || !p.withinLocked(p.sweptAt, SweepEvery)
	p.mu.Unlock()
	if !due {
		err := p.catchUp(ctx)
		if !errors.Is(err, mailbox.ErrHistoryExpired) {
			return false, err
		}
		p.logf("gmail plugin: %v; walking every collection", err)
	}
	return true, p.sweep(ctx)
}

// sweep is the full walk: every collection, from the history id Gmail stood
// at before the first listing. A change that lands during the walk is read
// again by the next catch-up, and reading a change twice is harmless.
func (p *Plugin) sweep(ctx context.Context) error {
	id, err := p.src.HistoryID(ctx)
	if err != nil {
		return err
	}
	for _, c := range mailbox.Collections {
		if _, err := p.walk(ctx, c); err != nil {
			return err
		}
	}
	p.mem.SetHistoryID(id)
	return nil
}

// catchUp applies Gmail's history since the id memory is current to: each
// message whose watched labels changed is read as it stands now and placed
// by its labels, and each deleted one, or one Gmail no longer answers for,
// leaves. A message whose read failed costs its change this refresh, not the
// others: the id stays where it was, so the next refresh reads that change
// again. Every read failing is the refresh failing, with its reason.
func (p *Plugin) catchUp(ctx context.Context) error {
	d, err := p.src.History(ctx, p.mem.HistoryID(), mailbox.WatchedLabels())
	if err != nil {
		return err
	}
	fetched := make([]mailbox.Labelled, 0, len(d.Touched))
	deleted := append([]string(nil), d.Deleted...)
	var firstErr error
	failed := 0
	for _, id := range d.Touched {
		m, labels, err := p.src.Headers(ctx, id)
		switch {
		case status.Code(err) == codes.NotFound:
			deleted = append(deleted, id)
		case err != nil:
			if firstErr == nil {
				firstErr = err
			}
			failed++
			p.logf("gmail plugin: history: message %s: %v", id, err)
		default:
			fetched = append(fetched, mailbox.Labelled{Message: m, Labels: labels})
		}
	}
	if failed > 0 && failed == len(d.Touched) {
		return firstErr
	}
	p.mem.Apply(fetched, deleted)
	if failed == 0 {
		p.mem.SetHistoryID(d.HistoryID)
	}
	return nil
}

// walk is one pass over one collection, and it is a DELTA: two cheap id
// listings — the label, and the label intersected with UNREAD — and then a
// metadata read for only the ids the memory has never seen. A message's
// subject, sender and date do not change once Gmail has it, so re-reading a
// known message would buy nothing; its read/unread state does change, and
// that is what the second listing is for.
func (p *Plugin) walk(ctx context.Context, c mailbox.Collection) (int, error) {
	ids, whole, err := p.src.Label(ctx, c.LabelIDs, p.max)
	if err != nil {
		return 0, err
	}
	unreadIDs, _, err := p.src.Label(ctx, append(append([]string(nil), c.LabelIDs...), mailbox.UnreadLabel), p.max)
	if err != nil {
		return 0, err
	}
	unread := make(map[string]bool, len(unreadIDs))
	for _, id := range unreadIDs {
		unread[id] = true
	}

	missing := p.mem.Missing(ids)
	fetched := make([]mailbox.Message, 0, len(missing))
	var firstErr error
	for _, id := range missing {
		m, _, err := p.src.Headers(ctx, id)
		if err != nil {
			// One message the metadata read could not reach costs a tile this
			// pass, not the walk: the id stays in the membership, so nothing
			// calls it gone, and the next walk reads it. It is never silent.
			if firstErr == nil {
				firstErr = err
			}
			p.logf("gmail plugin: %q message %s: %v", c.Key, id, err)
			continue
		}
		fetched = append(fetched, m)
	}
	// Every read failing is not "a message was skipped", it is the walk
	// failing — a revoked token, or Gmail down — and it must surface with its
	// reason instead of leaving an empty grid with nothing said.
	if firstErr != nil && len(fetched) == 0 && len(missing) > 0 {
		return 0, firstErr
	}
	p.mem.Absorb(c.Key, ids, unread, fetched, whole)
	return len(ids), nil
}

// List answers one collection. A walk failure with a transport-shaped code
// degrades at the node to the remembered listing, stamped stale; a verdict
// such as "this token was refused" surfaces.
//
// The listing is NOT authoritative. Absence is the node's question to settle
// through Probe, which is the one place that knows whether every collection
// has been read: a message missing from the inbox is often still starred, and
// an authoritative inbox would retire its id and lose the user's placement on
// the way past.
func (p *Plugin) List(ctx context.Context, req *pluginv1.ListRequest) (*pluginv1.ListResponse, error) {
	c, ok := mailbox.LookupCollection(req.Context)
	if !ok {
		return nil, status.Errorf(codes.InvalidArgument, "gmail plugin: unknown context %q", req.Context)
	}
	if err := p.sync(ctx, c); err != nil {
		return nil, err
	}
	views := p.mem.Collection(c.Key)
	unread := 0
	for _, v := range views {
		if v.Unread {
			unread++
		}
	}
	return &pluginv1.ListResponse{
		Entries:       mailbox.CollectionEntries(views),
		Authoritative: false,
		SourceLabel:   fmt.Sprintf("%s · %d messages · %d unread", c.Label, len(views), unread),
	}, nil
}

// ReadContent answers the message's markdown card: the tile's face and its
// read-only document. The email itself is ServeContent's answer, not this
// one. An unknown key reads as a one-line notice.
func (p *Plugin) ReadContent(req *pluginv1.ReadContentRequest, stream pluginv1.Plugin_ReadContentServer) error {
	id, ok := mailbox.ParseKey(req.Key)
	if !ok {
		return stream.Send(&pluginv1.ContentChunk{}) // not one of ours: no body
	}
	v, known := p.mem.View(id)
	if !known {
		// Before any collection has been walked, "not in memory" means "not
		// yet", not "gone" — and it must answer Unavailable, transport-shaped,
		// so the node's cache serves the remembered body instead of storing a
		// gone body over it while the walk is still running.
		if !p.mem.Swept() {
			return status.Error(codes.Unavailable, "gmail plugin: the first walk has not completed")
		}
		return stream.Send(&pluginv1.ContentChunk{Data: mailbox.GoneMarkdown(req.Key), MediaType: "text/markdown"})
	}
	return stream.Send(&pluginv1.ContentChunk{Data: mailbox.Markdown(v), MediaType: "text/markdown"})
}

// ServeContent is the email. Subpath "" is the message, as the HTML the
// sender wrote; any other subpath is a resource the email named by a relative
// URL, and there are none — an email's own images and links are absolute,
// embedded, or `cid:` attachments this plugin does not serve — so it is an
// ordinary 404 rather than an error. The document is fetched on descent and
// never remembered: a mailbox listing is small and a mailbox's bodies are not.
func (p *Plugin) ServeContent(req *pluginv1.ServeContentRequest, stream pluginv1.Plugin_ServeContentServer) error {
	id, ok := mailbox.ParseKey(req.Key)
	if !ok || req.Subpath != "" {
		return stream.Send(&pluginv1.ServeContentChunk{
			Status:    404,
			MediaType: "text/plain; charset=utf-8",
			Data:      []byte("not found"),
		})
	}
	body, media, err := p.src.HTML(context.Background(), id)
	if err != nil {
		// The reason travels: the node turns a coded failure into an answer
		// the user can read, and a silent blank page would look like an email
		// with nothing in it.
		return err
	}
	if len(body) == 0 {
		title := req.Key
		if v, known := p.mem.View(id); known {
			title = v.Title()
		}
		body = mailbox.NoticeHTML(title, "This message has no text or HTML body — it may be attachments only.")
		media = "text/html; charset=utf-8"
	}
	return stream.Send(&pluginv1.ServeContentChunk{Status: 200, MediaType: media, Data: body})
}

// Probe is the one place that decides a message has left. PRESENT while some
// collection holds it. GONE only once every collection has produced a usable
// membership and none of them does — the message was archived, deleted or
// filed somewhere this plugin does not project, and the node may retire its
// id. UNSPECIFIED, meaning "cannot say", until then: a half-walked memory
// must never cost the user a tile.
func (p *Plugin) Probe(_ context.Context, req *pluginv1.ProbeRequest) (*pluginv1.ProbeResponse, error) {
	id, ok := mailbox.ParseKey(req.Key)
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

// Search matches the query against the subject, snippet and sender of every
// message a collection still holds; each result's path is the collection that
// holds it. It reads memory only — no Gmail call — so it answers what the
// grids show and nothing the user cannot navigate to. Gmail's own search is a
// bigger thing and would answer mail that has no tile.
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
	holder := p.holders()
	all := p.mem.All()
	for i := len(all) - 1; i >= 0 && len(resp.Results) < limit; i-- { // newest first
		v := all[i]
		held, ok := holder[v.ID]
		if !ok {
			continue
		}
		hay := strings.ToLower(strings.Join([]string{v.Subject, v.Snippet, v.FromName, v.FromEmail}, "\n"))
		if !strings.Contains(hay, q) {
			continue
		}
		entries := mailbox.CollectionEntries([]mailbox.View{v})
		if len(entries) == 0 {
			continue
		}
		resp.Results = append(resp.Results, &pluginv1.SearchResult{
			Entry:       entries[0],
			ContextPath: []string{held},
			Snippet:     v.Preview(),
			Score:       1,
		})
	}
	return resp, nil
}

// holders maps each message to a context that currently holds it, read once
// per search. The collections are visited in the order they are declared, so
// a starred message that is also in the inbox is navigated to in the inbox —
// where a message is looked for first.
func (p *Plugin) holders() map[string]string {
	out := map[string]string{}
	for _, c := range mailbox.Collections {
		for _, v := range p.mem.Collection(c.Key) {
			if _, already := out[v.ID]; !already {
				out[v.ID] = c.Key
			}
		}
	}
	return out
}

// Delete is refused rather than left to the embedded Unimplemented, so the
// reason travels. This plugin is a read-only projection: what the trash
// gesture should mean for an email — archive it, trash it, unstar it — is a
// decision about the user's mail, and it has not been made. A gesture that
// silently did nothing would look like one that failed to stick. The token
// could not do it either: gmail.readonly is the only scope it carries.
func (p *Plugin) Delete(context.Context, *pluginv1.DeleteRequest) (*pluginv1.DeleteResponse, error) {
	return nil, status.Error(codes.Unimplemented,
		"gmail plugin: this is a read-only projection of Gmail; nothing here writes to your mail")
}
