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

// DefaultRefresh bounds how often one collection is re-walked. The node lists
// a context on every GetGrid and GetTile, and a descent must feel instant
// rather than cost a round trip to Google each time.
const DefaultRefresh = time.Minute

// DefaultFirstAnswer bounds how long a List waits on a walk in flight before
// answering what memory holds so far. A cold walk is a label listing plus a
// metadata read per new message; waiting for all of it would show the user
// "loading" the whole time, and the node's refresh paints the rest in when it
// lands.
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
	// Headers answers one message's record.
	Headers(ctx context.Context, id string) (mailbox.Message, error)
	// HTML answers one message's body and its media type. An empty body is
	// not an error: some messages have none.
	HTML(ctx context.Context, id string) (body []byte, mediaType string, err error)
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
	// logf is the plugin's one log door: the walk's narration, and what must
	// not be swallowed and must not fail a read — a cache it could not read
	// or write, a metadata fetch that failed.
	logf func(format string, args ...any)

	mu       sync.Mutex
	walkedAt map[string]time.Time // collection key → last successful walk
	// flights are the walks in progress, by collection. A List that finds one
	// waits for it instead of starting its own, because the node lists a
	// context on every GetGrid and GetTile and a burst of reads must cost
	// Gmail one walk, not one per reader.
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
		walkedAt:    map[string]time.Time{},
		flights:     map[string]*flight{},
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

// loadCache folds the last process's walk into memory, each collection's
// landing time included: a walk is fresh for the refresh window whichever
// process ran it, so a restart inside that window answers every listing from
// the file without touching Gmail. A missing file is the first boot, which is
// not news; anything else is reported and the plugin starts cold, because a
// cache is disposable and a walk rebuilds it, but a cache that cannot be read
// must not vanish in silence.
func (p *Plugin) loadCache() {
	snap, err := mailbox.LoadCache(p.cache)
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
		p.logf("gmail plugin: cache: %v (starting cold)", err)
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
	if err := mailbox.SaveCache(p.cache, snap); err != nil {
		p.logf("gmail plugin: cache: %v", err)
	}
}

// MinRefresherInterval is the fastest the background refresher runs, whatever
// the refresh window says. The refresher is a warmer, not a poller: a window
// shorter than a walk would leave it always walking, hammering Gmail. Reads
// still walk on the configured window — a tiny one is how a test says "walk
// on every read", and that keeps working.
const MinRefresherInterval = time.Second

func (p *Plugin) refresherInterval() time.Duration {
	if p.refresh < MinRefresherInterval {
		return MinRefresherInterval
	}
	return p.refresh
}

// Run keeps the memory warm until ctx is done: one goroutine walking both
// collections on the refresher's interval, so the walk has happened before a
// read asks rather than because one did. It shares the flights and the
// freshness window with the reads, so a tick that lands on a memory a read has
// just refreshed costs Gmail nothing. Walking both together is also what lets
// Probe ever answer GONE: a message is gone only when no collection holds it,
// and that takes a pass over every one.
func (p *Plugin) Run(ctx context.Context) {
	t := time.NewTicker(p.refresherInterval())
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			for _, c := range mailbox.Collections {
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

// freshLocked reports whether the collection was walked within the refresh
// window. A walk stamped in the FUTURE is not fresh: the stamp can come from
// the cache file, and a clock that has since stepped back would otherwise
// freeze the plugin on a stale memory. The caller holds p.mu.
func (p *Plugin) freshLocked(key string) bool {
	t, ok := p.walkedAt[key]
	if !ok {
		return false
	}
	d := p.now().Sub(t)
	return d >= 0 && d < p.refresh
}

// sync makes one collection answerable: fresh memory as-is, else a walk. A
// walk already in flight for it is shared — one pass per burst of readers —
// and no walk belongs to its starter: it runs detached, so no reader's
// patience or hangup can kill or restart it. The caller waits at most
// firstAnswer, then answers what memory holds so far.
func (p *Plugin) sync(ctx context.Context, c mailbox.Collection) error {
	p.mu.Lock()
	if p.freshLocked(c.Key) {
		p.mu.Unlock()
		return nil
	}
	f, running := p.flights[c.Key]
	if !running {
		f = &flight{done: make(chan struct{})}
		p.flights[c.Key] = f
		go p.walkFlight(c, f)
	}
	p.mu.Unlock()

	select {
	case <-f.done:
		return f.err
	case <-time.After(p.firstAnswer):
		p.logf("gmail plugin: %q answering with memory so far; the walk runs on", c.Key)
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// walkFlight is one detached walk: it owns its flight and outlives every
// reader. Its context is the plugin's lifetime — every Gmail call is bounded
// by the client's own timeout, so a dead source ends the walk with its error
// rather than hanging it.
func (p *Plugin) walkFlight(c mailbox.Collection, f *flight) {
	p.logf("gmail plugin: walk %q starting", c.Key)
	start := time.Now()
	n, err := p.walk(context.Background(), c)
	p.logf("gmail plugin: walk %q finished in %s: %d messages, err=%v",
		c.Key, time.Since(start).Round(time.Millisecond), n, err)
	p.mu.Lock()
	if err == nil {
		p.walkedAt[c.Key] = p.now()
	}
	delete(p.flights, c.Key)
	p.mu.Unlock()
	// The cache lands before the flight closes: a listing that waited for the
	// walk is one a restart can repeat.
	if err == nil {
		p.saveCache()
	}
	f.err = err
	close(f.done)
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
		m, err := p.src.Headers(ctx, id)
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
