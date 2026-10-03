package plugin

import (
	"context"
	"slices"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/josephburnett/gridwell-plugins/hey/mail"
	pluginv1 "github.com/josephburnett/gridwell/api/gen/plugin/v1"
)

// Everything lists each thread every box holds, once, as the thread's own
// tile; a box lists a link to that tile per thread it holds.
func TestEverythingIsEveryThreadOnceAndBoxesLinkToIt(t *testing.T) {
	f := newFake()
	f.boxes["imbox"] = []mail.Thread{th(1, "lunch", "2026-01-05T14:00:00Z")}
	f.boxes["laterbox"] = []mail.Thread{th(1, "lunch", "2026-01-05T14:00:00Z"), th(2, "invoice", "2026-01-04T09:00:00Z")}
	f.boxes["feedbox"] = []mail.Thread{th(3, "digest", "2026-01-03T09:00:00Z")}
	p := stable(t, f, Options{})
	ctx := context.Background()

	all, err := p.List(ctx, &pluginv1.ListRequest{Context: mail.EverythingContext})
	if err != nil {
		t.Fatal(err)
	}
	var keys []string
	for _, e := range all.Entries {
		keys = append(keys, e.Key)
		if e.GetLinkTarget() != nil {
			t.Errorf("everything's %s is a link; it is the thread's one home", e.Key)
		}
	}
	if want := []string{"thread:3", "thread:2", "thread:1"}; !slices.Equal(keys, want) {
		t.Fatalf("everything = %v, want %v", keys, want)
	}
	if all.Authoritative {
		t.Error("everything listed authoritatively; absence is Probe's answer")
	}

	later, err := p.List(ctx, &pluginv1.ListRequest{Context: mail.ReplyLaterContext})
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range later.Entries {
		if lt := e.GetLinkTarget(); lt == nil || lt.Context != mail.EverythingContext || lt.Key != e.Key {
			t.Errorf("reply later's %s links to %+v, want the thread in everything", e.Key, lt)
		}
	}
}

// One box that cannot be read costs everything nothing it remembers: every
// other box's threads still list, and the failure is the listing's
// unreachable reason, as it is the failing box's. A refused everything would
// cost the node every email's page, since each one is a tile in it.
func TestOneFailingBoxLeavesEverythingServing(t *testing.T) {
	f := newFake()
	f.boxes["imbox"] = []mail.Thread{th(1, "lunch", "2026-01-05T14:00:00Z")}
	f.boxes["laterbox"] = []mail.Thread{th(2, "invoice", "2026-01-04T09:00:00Z")}
	f.boxErr["feedbox"] = status.Error(codes.NotFound, "hey plugin: box view feedbox: no box of kind feedbox")
	p := stable(t, f, Options{})
	ctx := context.Background()

	all, err := p.List(ctx, &pluginv1.ListRequest{Context: mail.EverythingContext})
	if err != nil {
		t.Fatalf("one failing box refused everything: %v", err)
	}
	var keys []string
	for _, e := range all.Entries {
		keys = append(keys, e.Key)
	}
	if want := []string{"thread:2", "thread:1"}; !slices.Equal(keys, want) {
		t.Fatalf("everything = %v, want %v", keys, want)
	}
	if !strings.Contains(all.Unreachable, "no box of kind feedbox") {
		t.Errorf("everything's unreachable = %q, want the feed's failure", all.Unreachable)
	}
	feed, err := p.List(ctx, &pluginv1.ListRequest{Context: mail.FeedContext})
	if err != nil || !strings.Contains(feed.Unreachable, "no box of kind feedbox") {
		t.Errorf("the failing box = %+v, %v; want memory and its reason", feed, err)
	}
	imbox, err := p.List(ctx, &pluginv1.ListRequest{Context: mail.ImboxContext})
	if err != nil || imbox.Unreachable != "" {
		t.Errorf("a box that reads = %+v, %v; want no reason", imbox, err)
	}

	f.mu.Lock()
	delete(f.boxErr, "feedbox")
	f.mu.Unlock()
	p.flights.Rewalk(mail.FeedContext)
	idle(t, p)
	if all, err := p.List(ctx, &pluginv1.ListRequest{Context: mail.EverythingContext}); err != nil || all.Unreachable != "" {
		t.Errorf("everything after the box reads again = %q, %v", all.GetUnreachable(), err)
	}
}

// Probe answers for the context the node asks about: a box for its own
// membership, everything for whether HEY still has the thread at all.
func TestProbeAnswersForTheContextAsked(t *testing.T) {
	f := newFake()
	f.boxes["imbox"] = []mail.Thread{th(1, "lunch", "2026-01-05T14:00:00Z")}
	f.whole["laterbox"] = false
	f.threadErr[9] = status.Error(codes.NotFound, "hey plugin: thread read: resource not found")
	f.threadErr[7] = status.Error(codes.Unavailable, "hey plugin: thread read: network")
	f.html[8] = "<!doctype html>"
	p := stable(t, f, Options{})
	ctx := context.Background()
	if _, err := p.List(ctx, &pluginv1.ListRequest{Context: mail.EverythingContext}); err != nil {
		t.Fatal(err)
	}
	probe := func(context, key string) pluginv1.ProbeResponse_Presence {
		t.Helper()
		got, err := p.Probe(ctx, &pluginv1.ProbeRequest{Context: context, Key: key})
		if err != nil {
			t.Fatal(err)
		}
		return got.Presence
	}
	for _, c := range []struct {
		context, key string
		want         pluginv1.ProbeResponse_Presence
	}{
		{mail.ImboxContext, "thread:1", pluginv1.ProbeResponse_PRESENCE_PRESENT},
		{mail.ImboxContext, "thread:2", pluginv1.ProbeResponse_PRESENCE_GONE},             // the imbox was read whole
		{mail.ReplyLaterContext, "thread:2", pluginv1.ProbeResponse_PRESENCE_UNSPECIFIED}, // a capped read says nothing
		{mail.EverythingContext, "thread:1", pluginv1.ProbeResponse_PRESENCE_PRESENT},
		{mail.EverythingContext, "thread:8", pluginv1.ProbeResponse_PRESENCE_PRESENT},     // in no box, but HEY has it
		{mail.EverythingContext, "thread:9", pluginv1.ProbeResponse_PRESENCE_GONE},        // HEY says it does not exist
		{mail.EverythingContext, "thread:7", pluginv1.ProbeResponse_PRESENCE_UNSPECIFIED}, // HEY cannot say right now
		{"box:spambox", "thread:1", pluginv1.ProbeResponse_PRESENCE_GONE},                 // a context never listed
	} {
		if got := probe(c.context, c.key); got != c.want {
			t.Errorf("Probe(%s, %s) = %v, want %v", c.context, c.key, got, c.want)
		}
	}
	before := f.count("thread")
	probe(mail.EverythingContext, "thread:1")
	if f.count("thread") != before {
		t.Error("a thread a box holds cost a CLI run to probe")
	}
}
