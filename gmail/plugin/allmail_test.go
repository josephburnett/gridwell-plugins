package plugin

import (
	"context"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/josephburnett/gridwell-plugins/gmail/mailbox"
	pluginv1 "github.com/josephburnett/gridwell/api/gen/plugin/v1"
)

// All mail lists every message a label holds, each once as the one page it
// is; a label lists a link to that page, so a message starred out of the
// inbox is one tile, not two.
func TestAllMailIsEveryMessageOnceAndLabelsLinkToIt(t *testing.T) {
	f := newFake()
	a, b, c := msg("a", "one", "2026-01-05T09:00:00Z"), msg("b", "two", "2026-01-05T10:00:00Z"), msg("c", "three", "2026-01-04T10:00:00Z")
	f.hold("INBOX", a, b)
	f.hold("STARRED", c, b)
	p := stable(f, Options{})
	listAll(t, p)
	ctx := context.Background()

	info, err := p.Info(ctx, &pluginv1.InfoRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if n := len(info.Grids); n != 3 || info.Grids[2].Grid != mailbox.AllMailGrid {
		t.Fatalf("menu = %+v, want the labels and then all mail", info.Grids)
	}

	all, err := p.List(ctx, &pluginv1.ListRequest{Grid: mailbox.AllMailGrid})
	if err != nil {
		t.Fatal(err)
	}
	if got := entryKeys(all.Entries); got != "msg:c,msg:a,msg:b" {
		t.Fatalf("all mail = %s, want every labelled message once", got)
	}
	for _, e := range all.Entries {
		if e.LinkTarget != nil || !e.ServesPage {
			t.Errorf("all mail's %s = %+v, want the page itself", e.Key, e)
		}
	}
	for label, want := range map[string]string{mailbox.InboxGrid: "msg:a,msg:b", mailbox.StarredGrid: "msg:c,msg:b"} {
		resp, err := p.List(ctx, &pluginv1.ListRequest{Grid: label})
		if err != nil {
			t.Fatal(err)
		}
		if got := entryKeys(resp.Entries); got != want {
			t.Errorf("%s = %s, want %s", label, got, want)
		}
		for _, e := range resp.Entries {
			if e.LinkTarget.GetGrid() != mailbox.AllMailGrid || e.LinkTarget.GetKey() != e.Key {
				t.Errorf("%s's %s links to %+v, want its all mail page", label, e.Key, e.LinkTarget)
			}
		}
	}
}

// Probe answers for the grid it names. A label after a whole read: its
// membership. All mail: a labelled message at once, else Gmail's own word,
// and only NotFound is gone. A capped label, or a Gmail that cannot answer,
// cannot say.
func TestProbeAnswersForTheGridAsked(t *testing.T) {
	f := newFake()
	a, b := msg("a", "one", "2026-01-05T09:00:00Z"), msg("b", "two", "2026-01-05T10:00:00Z")
	f.hold("INBOX", a)
	f.hold("STARRED", b)
	f.recs["x"] = msg("x", "archived", "2026-01-02T10:00:00Z") // Gmail has it, in no label walked
	p := stable(f, Options{})
	listAll(t, p)
	ctx := context.Background()

	probe := func(p *Plugin, gridKey, key string) pluginv1.ProbeResponse_Presence {
		t.Helper()
		got, err := p.Probe(ctx, &pluginv1.ProbeRequest{Grid: gridKey, Key: key})
		if err != nil {
			t.Fatal(err)
		}
		return got.Presence
	}
	present, gone, unsure := pluginv1.ProbeResponse_PRESENCE_PRESENT, pluginv1.ProbeResponse_PRESENCE_GONE, pluginv1.ProbeResponse_PRESENCE_UNSPECIFIED
	headers := f.count("headers")
	for _, c := range []struct {
		gridKey, key string
		want         pluginv1.ProbeResponse_Presence
	}{
		{mailbox.InboxGrid, "msg:a", present},
		{mailbox.InboxGrid, "msg:b", gone}, // starred, not in the inbox: leaving a label is not gone
		{mailbox.StarredGrid, "msg:b", present},
		{mailbox.AllMailGrid, "msg:a", present},
		{mailbox.AllMailGrid, "msg:b", present},
		{"label:SPAM", "msg:a", gone}, // a gridKey this plugin never lists
	} {
		if got := probe(p, c.gridKey, c.key); got != c.want {
			t.Errorf("Probe(%s, %s) = %v, want %v", c.gridKey, c.key, got, c.want)
		}
	}
	if f.count("headers") != headers {
		t.Error("a labelled message cost a Gmail read to probe")
	}
	if got := probe(p, mailbox.AllMailGrid, "msg:x"); got != present {
		t.Errorf("an archived message Gmail still has probed %v in all mail", got)
	}
	if got := probe(p, mailbox.AllMailGrid, "msg:z"); got != gone {
		t.Errorf("a message Gmail says is not found probed %v in all mail", got)
	}
	f.mu.Lock()
	f.headerErr = status.Error(codes.Unavailable, "gmail is down")
	f.mu.Unlock()
	if got := probe(p, mailbox.AllMailGrid, "msg:x"); got != unsure {
		t.Errorf("a Gmail that cannot answer probed %v, want cannot say", got)
	}

	// A capped read says nothing about what it did not reach.
	g := newFake()
	g.hold("INBOX", a, b)
	q := stable(g, Options{MaxMessages: 1})
	listAll(t, q)
	if got := probe(q, mailbox.InboxGrid, "msg:a"); got != unsure {
		t.Errorf("a message below a capped read probed %v, want cannot say", got)
	}
}

// A label listing is authoritative when it is definitive: a whole read, every
// member read, and memory current to Gmail's history. Anything less, and All
// mail always, leaves absence to Probe.
func TestALabelListingIsAuthoritativeOnlyWhenDefinitive(t *testing.T) {
	for name, c := range map[string]struct {
		setup func(f *fakeGmail)
		o     Options
		want  bool
	}{
		"a whole read, current":      {want: true},
		"a capped read":              {o: Options{MaxMessages: 1}},
		"a member it could not read": {setup: func(f *fakeGmail) { delete(f.recs, "b") }},
		"no history id":              {setup: func(f *fakeGmail) { f.profileErr = status.Error(codes.Unavailable, "no profile") }},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFake()
			f.history = true
			f.hold("INBOX", msg("a", "one", "2026-01-05T09:00:00Z"), msg("b", "two", "2026-01-05T10:00:00Z"))
			if c.setup != nil {
				c.setup(f)
			}
			p := stable(f, c.o)
			listAll(t, p)
			resp, err := p.List(context.Background(), &pluginv1.ListRequest{Grid: mailbox.InboxGrid})
			if err != nil {
				t.Fatal(err)
			}
			if resp.Authoritative != c.want {
				t.Errorf("authoritative = %v, want %v", resp.Authoritative, c.want)
			}
			all, _ := p.List(context.Background(), &pluginv1.ListRequest{Grid: mailbox.AllMailGrid})
			if all.Authoritative {
				t.Error("all mail listed authoritatively; it is a union, and leaving it is Probe's to say")
			}
		})
	}

	t.Run("a refresh that failed", func(t *testing.T) {
		f := newFake()
		f.hold("INBOX", msg("a", "one", "2026-01-05T09:00:00Z"))
		p, clock := warmHistory(t, f, Options{})
		f.mu.Lock()
		f.err = status.Error(codes.Unavailable, "gmail is down")
		f.mu.Unlock()
		refreshed(t, p, clock, 2*time.Minute)
		resp, _ := p.List(context.Background(), &pluginv1.ListRequest{Grid: mailbox.InboxGrid})
		if resp.GetAuthoritative() {
			t.Error("a listing after a failed refresh claimed authority")
		}
	})
}

// A find hit is the message's one tile, in all mail.
func TestFindLandsOnTheAllMailTile(t *testing.T) {
	f := newFake()
	m := msg("a", "lunch", "2026-01-05T09:00:00Z")
	f.hold("INBOX", m)
	f.hold("STARRED", m)
	p := stable(f, Options{})
	listAll(t, p)
	res, err := p.Find(context.Background(), &pluginv1.FindRequest{Query: "lunch"})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Results) != 1 {
		t.Fatalf("results = %+v, want the one message once", res.Results)
	}
	r := res.Results[0]
	if r.Entry.Key != "msg:a" || r.Entry.LinkTarget != nil || len(r.GridPath) != 1 || r.GridPath[0] != mailbox.AllMailGrid {
		t.Errorf("hit = %+v at %v, want all mail's page", r.Entry, r.GridPath)
	}
}

// Memory forgets a message only once no label holds it AND Gmail says it is
// gone. Archived is not gone: Gmail still has it, and so does memory.
func TestMemoryForgetsAMessageOnlyWhenGmailSaysItIsGone(t *testing.T) {
	f := newFake()
	f.hold("INBOX", msg("a", "one", "2026-01-05T09:00:00Z"), msg("b", "two", "2026-01-05T10:00:00Z"))
	p, clock := warmHistory(t, f, Options{})

	f.hold("INBOX", msg("b", "two", "2026-01-05T10:00:00Z")) // a is archived
	f.changed("a")
	refreshed(t, p, clock, 2*time.Minute)
	if _, known := p.mem.View("a"); !known {
		t.Fatal("an archived message was forgotten; Gmail still has it")
	}
	f.deleted("a")
	got, _ := p.Probe(context.Background(), &pluginv1.ProbeRequest{Grid: mailbox.AllMailGrid, Key: "msg:a"})
	if got.Presence != pluginv1.ProbeResponse_PRESENCE_GONE {
		t.Fatalf("a deleted message probed %v", got.Presence)
	}
	if _, known := p.mem.View("a"); known {
		t.Error("memory kept a message Gmail says is gone and no label holds")
	}

	f.deleted("b") // deleted out of the inbox: history says so
	refreshed(t, p, clock, 2*time.Minute)
	if _, known := p.mem.View("b"); known {
		t.Error("memory kept a message history says was deleted")
	}
}

func entryKeys(es []*pluginv1.Entry) string {
	keys := make([]string, 0, len(es))
	for _, e := range es {
		keys = append(keys, e.Key)
	}
	return strings.Join(keys, ",")
}
