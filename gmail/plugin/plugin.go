// Package plugin is the gmail plugin: the wire half over
// gridwell-plugins/gmail/mailbox. It projects two of Gmail's labels — the
// inbox and the starred mail — as two grids, one context each, read through
// the Gmail API, and all mail, their union, as a third. A message is one url
// tile in all mail that serves a page, the email itself as the HTML the
// sender wrote, through the node's content door; a label lists links to it,
// so a message starred out of the inbox keeps its one tile.
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
	"log"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/josephburnett/gridwell-plugins/gmail/mailbox"
	"github.com/josephburnett/gridwell-plugins/memo"
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

// DefaultMaxMessages bounds one walk of one label: a mailbox has no end, and
// the newest N are what a person is looking at. It bounds the WALK, not the
// grid: a read that stops here is not a whole read, and mailbox.Memory's
// watermark keeps every message remembered below it rather than retiring
// tiles the read never reached.
const DefaultMaxMessages = 500

// MinPollInterval is the fastest a watched plugin polls Gmail's history,
// whatever the refresh window says: a window shorter than a refresh would
// leave it always refreshing. Reads still refresh on the configured window —
// a tiny one is how a test says "refresh on every read".
const MinPollInterval = time.Second

// account is the one key every refresh runs under. Gmail's history is
// account-wide, and one history id is current to every context at once, so a
// refresh is of the whole account, never of one context.
const account = "account"

// cacheFile is the memory's file in the state directory.
const cacheFile = "gmail.json"

// cacheVersion is bumped whenever cache changes shape.
const cacheVersion = 1

// cache is what the cache file holds: the memory, and the plugin's own
// stamps — when each refresh and the last full walk landed — so a restart
// inside the refresh window answers from the file without calling Gmail, and
// the consistency pass keeps its schedule across restarts.
type cache struct {
	Memory   mailbox.Snapshot     `json:"memory"`
	WalkedAt map[string]time.Time `json:"walkedAt,omitempty"`
	SweptAt  time.Time            `json:"sweptAt,omitempty"`
}

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
	src     Source
	mem     *mailbox.Memory
	refresh time.Duration
	max     int
	clock   memo.Clock
	// logf is the plugin's one log door: what must not be swallowed and must
	// not fail a read — a cache it could not read or write, a metadata fetch
	// that failed — once per episode (episodes).
	logf     func(format string, args ...any)
	episodes episodes
	// reauth is the command that writes a token Google accepts, which Info's
	// refusal names.
	reauth string
	// served latches the first Info that passed: a token refused later is a
	// source gone dark, which the reads report.
	served atomic.Bool

	life    *memo.Life
	file    *memo.File[cache]
	flights *memo.Flights
	changes *memo.Changes
	// landing counts refreshes walked and not yet landed (saved and
	// published), so a test can wait until the plugin is still.
	landing atomic.Int32

	mu      sync.Mutex
	sweptAt time.Time // last full walk that landed
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
	// Reauth is the command that writes a new token, which Info names when
	// Google refuses the one it has.
	Reauth string
	// Linger is how long history is polled after the last stream needing it
	// leaves: memo.DefaultLinger when zero, at once when negative.
	Linger time.Duration
}

// New builds a plugin over src. A state directory holding a cache file is
// loaded here, before the plugin serves its first request, so the first
// listing is answered from what the last process walked.
func New(src Source, o Options) *Plugin {
	p := &Plugin{
		src:     src,
		mem:     mailbox.NewMemory(),
		refresh: o.Refresh,
		max:     o.MaxMessages,
		clock:   memo.System,
		logf:    o.Logf,
		reauth:  o.Reauth,
		life:    memo.NewLife(),
	}
	if p.refresh <= 0 {
		p.refresh = DefaultRefresh
	}
	if p.max <= 0 {
		p.max = DefaultMaxMessages
	}
	if o.Now != nil {
		p.clock = clock{now: o.Now}
	}
	if o.FirstAnswer <= 0 {
		o.FirstAnswer = DefaultFirstAnswer
	}
	if p.logf == nil {
		p.logf = log.Printf
	}
	p.episodes.logf = p.logf
	p.file = memo.NewFile[cache](strings.TrimSpace(o.StateDir), cacheFile, cacheVersion, p.logf)
	p.flights = memo.NewFlights(p.life, memo.FlightOptions{
		Name:        "gmail plugin",
		Walk:        p.walkAccount,
		Window:      p.refresh,
		FirstAnswer: o.FirstAnswer,
		Landed:      p.landed,
		Clock:       p.clock,
		Logf:        p.logf,
	})
	p.changes = memo.NewChanges(p.life, memo.ChangeOptions{
		Unscoped: mailbox.Contexts(),
		Work:     work,
		Do:       p.poll,
		Linger:   o.Linger,
		Clock:    p.clock,
	})
	if c, ok := p.file.Load(); ok {
		p.mem.Restore(c.Memory)
		p.flights.Restore(c.WalkedAt)
		p.sweptAt = c.SweptAt
	}
	return p
}

// clock is memo's clock with the time a test sets.
type clock struct{ now func() time.Time }

func (c clock) Now() time.Time                         { return c.now() }
func (c clock) After(d time.Duration) <-chan time.Time { return time.After(d) }

// work names the background work a context in scope needs: every context
// this plugin lists is read from the one account-wide history, so they share
// one poll; any other context needs none.
func work(context string) []string {
	for _, c := range mailbox.Contexts() {
		if c == context {
			return []string{account}
		}
	}
	return nil
}

// poll asks Gmail's history on the refresh window while some stream needs
// it: Gmail cannot tell, so the clock is the plugin's. A refresh a read has
// just landed is fresh, and the tick costs nothing.
func (p *Plugin) poll(ctx context.Context, _ string) {
	memo.Poll(ctx, p.clock, max(p.refresh, MinPollInterval), func(ctx context.Context) {
		_, _ = p.flights.Read(ctx, account, true) // a warm read: it starts the refresh and never waits on it
	})
}

// walkAccount is one refresh, flights' Walk: it runs detached under the
// plugin's lifetime, shared by every reader, and announces what it changed. A
// failed refresh can still have changed memory — a full walk that read the
// inbox and then failed — so the announcement does not wait on its error.
func (p *Plugin) walkAccount(ctx context.Context, _ string) error {
	p.landing.Add(1)
	before := p.faces()
	swept, err := p.catchUpOrSweep(ctx)
	if err == nil && swept {
		p.mu.Lock()
		p.sweptAt = p.clock.Now()
		p.mu.Unlock()
	}
	p.changes.Publish(p.changedSince(before)...)
	return err
}

// landed saves memory once a refresh's outcome is recorded and before any
// reader waiting on it is released: a listing that waited is one a restart
// repeats.
func (p *Plugin) landed(string, error) {
	defer p.landing.Add(-1)
	_ = p.file.Save(p.snapshot) // a failure is the File's to log; it costs the next restart a walk
}

func (p *Plugin) snapshot() cache {
	p.mu.Lock()
	swept := p.sweptAt
	p.mu.Unlock()
	return cache{Memory: p.mem.Snapshot(), WalkedAt: p.flights.WalkedAt(), SweptAt: swept}
}

// Info asks Google for the account's profile until it has once answered, and
// refuses while Google refuses the token: that config cannot serve, and the
// fix is a command. Anything else — Google out of reach — passes, and the
// reads say the source is dark. After the first pass Info asks nothing.
func (p *Plugin) Info(ctx context.Context, _ *pluginv1.InfoRequest) (*pluginv1.InfoResponse, error) {
	if !p.served.Load() {
		if _, err := p.src.HistoryID(ctx); status.Code(err) == codes.PermissionDenied {
			return nil, status.Errorf(codes.FailedPrecondition,
				"gmail plugin: Google refused the stored token (%s); run: %s", memo.Reason(err), p.reauth)
		}
		p.served.Store(true)
	}
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
		Watch:       true,
	}, nil
}

// catchUpOrSweep is the one rule for how memory catches up. A full walk of
// every collection when memory has no history id, has not seen every
// collection, or last walked SweepEvery ago; else Gmail's history since the
// id memory is current to, and a full walk after all when Gmail says that id
// is too old. swept reports that the full walk ran.
func (p *Plugin) catchUpOrSweep(ctx context.Context) (swept bool, err error) {
	p.mu.Lock()
	due := p.mem.HistoryID() == 0 || !p.mem.Swept() || !memo.Within(p.clock.Now(), p.sweptAt, SweepEvery)
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
// again by the next catch-up, and reading a change twice is harmless. A
// history id Gmail would not give costs the next refresh a walk, not this one
// its answer: the id is how memory keeps up cheaply, never what it shows.
func (p *Plugin) sweep(ctx context.Context) error {
	id, err := p.src.HistoryID(ctx)
	p.episodes.note("history id", err, "gmail plugin: history id: %v (each refresh walks until Gmail gives one)", err)
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
// again. The source failing every read is the refresh failing (sourceDown).
func (p *Plugin) catchUp(ctx context.Context) error {
	d, err := p.src.History(ctx, p.mem.HistoryID(), mailbox.WatchedLabels())
	if err != nil {
		return err
	}
	fetched := make([]mailbox.Labelled, 0, len(d.Touched))
	deleted := append([]string(nil), d.Deleted...)
	var down error
	failed := 0
	for _, id := range d.Touched {
		m, labels, err := p.src.Headers(ctx, id)
		if status.Code(err) != codes.NotFound {
			p.episodes.note("message "+id, err, "gmail plugin: message %s: %v", id, err)
		}
		switch {
		case status.Code(err) == codes.NotFound:
			deleted = append(deleted, id)
		case err != nil:
			down = sourceDown(down, err)
			failed++
		default:
			fetched = append(fetched, mailbox.Labelled{Message: m, Labels: labels})
		}
	}
	if down != nil && failed == len(d.Touched) {
		return down
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
	var down error
	for _, id := range missing {
		m, _, err := p.src.Headers(ctx, id)
		p.episodes.note("message "+id, err, "gmail plugin: message %s: %v", id, err)
		if err != nil {
			// One message the metadata read could not reach costs a tile this
			// pass, not the walk: the id stays in the membership, so nothing
			// calls it gone, and the next walk reads it. It is never silent.
			down = sourceDown(down, err)
			continue
		}
		fetched = append(fetched, m)
	}
	if down != nil && len(fetched) == 0 {
		return 0, down
	}
	p.mem.Absorb(c.Key, ids, unread, fetched, whole)
	return len(ids), nil
}

// sourceDown is down, else err when err says the source failed rather than
// one message: Gmail out of reach or the token refused. Every read failing
// that way is the walk failing, which must surface with its reason; a
// message Gmail will not answer for (NotFound, a body it cannot give) costs
// its own tile only, however few messages a walk has left to read.
func sourceDown(down, err error) error {
	if down != nil {
		return down
	}
	switch status.Code(err) {
	case codes.Unavailable, codes.PermissionDenied, codes.Unauthenticated, codes.DeadlineExceeded, codes.ResourceExhausted, codes.Canceled:
		return err
	}
	return nil
}

// shows reports whether memory has an answer for a context: all mail has one
// once any label does.
func (p *Plugin) shows(key string) bool {
	if key != mailbox.AllMailContext {
		return p.mem.Shows(key)
	}
	for _, c := range mailbox.Collections {
		if p.mem.Shows(c.Key) {
			return true
		}
	}
	return false
}

// read makes a context answerable: fresh memory as it is, else a refresh,
// joined if one is in flight (memo.Flights.Read). A read memory can answer
// never waits and never fails on Gmail: unreachable is why the last refresh
// failed, "" while it lands, and memory answers either way — a token revoked
// after Info included, which costs the user no tile. A cold read waits at
// most the first-answer bound, and fails only when its refresh failed with
// nothing remembered. current reports that memory is current to Gmail: no
// refresh has failed since one landed inside the window, and memory knows
// the history id it is current to, so the next refresh reads every change
// since.
func (p *Plugin) read(ctx context.Context, key string) (unreachable string, current bool, err error) {
	unreachable, err = p.flights.Read(ctx, account, p.shows(key))
	if err != nil {
		return "", false, err
	}
	return unreachable, unreachable == "" && p.flights.Fresh(account) && p.mem.HistoryID() != 0, nil
}

// List answers one context.
//
// A label lists links into all mail (mailbox.LabelEntries), and is
// authoritative only when it is definitive: its last walk read the whole
// label, every member was read, and memory is current. A capped read proves
// nothing below its oldest message, and a memory behind Gmail proves nothing
// about what left since. All mail is never authoritative: a message that
// left every label is usually archived, not gone, and Probe asks Gmail.
func (p *Plugin) List(ctx context.Context, req *pluginv1.ListRequest) (*pluginv1.ListResponse, error) {
	if req.Context == mailbox.AllMailContext {
		unreachable, _, err := p.read(ctx, req.Context)
		if err != nil {
			return nil, err
		}
		views := p.mem.AllMail()
		return listing(mailbox.AllMailLabel, views, mailbox.CollectionEntries(views), false, unreachable), nil
	}
	c, ok := mailbox.LookupCollection(req.Context)
	if !ok {
		return nil, status.Errorf(codes.InvalidArgument, "gmail plugin: unknown context %q", req.Context)
	}
	unreachable, current, err := p.read(ctx, c.Key)
	if err != nil {
		return nil, err
	}
	views := p.mem.Collection(c.Key)
	return listing(c.Label, views, mailbox.LabelEntries(c.Key, views), p.mem.Definitive(c.Key) && current, unreachable), nil
}

func listing(label string, views []mailbox.View, entries []*pluginv1.Entry, authoritative bool, unreachable string) *pluginv1.ListResponse {
	unread := 0
	for _, v := range views {
		if v.Unread {
			unread++
		}
	}
	return &pluginv1.ListResponse{
		Entries:       entries,
		Authoritative: authoritative,
		SourceLabel:   fmt.Sprintf("%s · %d messages · %d unread", label, len(views), unread),
		Unreachable:   unreachable,
	}
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
	body, media, err := p.src.HTML(stream.Context(), id)
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

// Probe is the one place that decides a message has left, and it answers for
// the context asked. A label: PRESENT while it holds the message, GONE once a
// whole read of it did not (mailbox.InLabel); the message is usually in
// another label or archived, and its tile in all mail stays. All mail:
// PRESENT while some label holds it, else Gmail's own word on the message —
// GONE only when Gmail says it does not have it, and then memory forgets it
// too. No context (a node from before contexts): PRESENT while some label
// holds it, GONE once every label has been read and none does. UNSPECIFIED,
// meaning "cannot say", otherwise: a half-walked memory must never cost the
// user a tile.
func (p *Plugin) Probe(ctx context.Context, req *pluginv1.ProbeRequest) (*pluginv1.ProbeResponse, error) {
	id, ok := mailbox.ParseKey(req.Key)
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
	case mailbox.AllMailContext:
		if p.mem.Member(id) {
			return presence(pluginv1.ProbeResponse_PRESENCE_PRESENT), nil
		}
		_, _, err := p.src.Headers(ctx, id)
		switch {
		case err == nil:
			return presence(pluginv1.ProbeResponse_PRESENCE_PRESENT), nil
		case status.Code(err) == codes.NotFound:
			p.mem.Forget(id)
			return presence(pluginv1.ProbeResponse_PRESENCE_GONE), nil
		}
	default:
		if _, ok := mailbox.LookupCollection(req.Context); !ok {
			return presence(pluginv1.ProbeResponse_PRESENCE_GONE), nil // a context this plugin never lists
		}
		switch p.mem.InLabel(req.Context, id) {
		case mailbox.Present:
			return presence(pluginv1.ProbeResponse_PRESENCE_PRESENT), nil
		case mailbox.Gone:
			return presence(pluginv1.ProbeResponse_PRESENCE_GONE), nil
		}
	}
	return presence(pluginv1.ProbeResponse_PRESENCE_UNSPECIFIED), nil
}

func presence(p pluginv1.ProbeResponse_Presence) *pluginv1.ProbeResponse {
	return &pluginv1.ProbeResponse{Presence: p}
}

// Search matches the query against the subject, snippet and sender of every
// message a label still holds; each result is the message's one tile, in all
// mail. It reads memory only — no Gmail call — so it answers what the grids
// show and nothing the user cannot navigate to. Gmail's own search is a
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
	all := p.mem.AllMail()
	for i := len(all) - 1; i >= 0 && len(resp.Results) < limit; i-- { // newest first
		v := all[i]
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
			ContextPath: []string{mailbox.AllMailContext},
			Snippet:     v.Preview(),
			Score:       1,
		})
	}
	return resp, nil
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
