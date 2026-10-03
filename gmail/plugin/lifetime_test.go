package plugin

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/josephburnett/gridwell-plugins/gmail/mailbox"
	pluginv1 "github.com/josephburnett/gridwell/api/gen/plugin/v1"
)

// A token Google refuses is a config the plugin cannot serve: Info says so in
// a sentence that names the fix, and passes once it is fixed, with no
// restart. After the first pass Info asks Google nothing: a token revoked
// later is weather for the reads to report, not a verdict on the plugin.
func TestInfoRefusesARefusedTokenThenLatches(t *testing.T) {
	f := newFake()
	f.profileErr = status.Error(codes.PermissionDenied, "gmail plugin: profile: Request had invalid authentication credentials.")
	p := stable(f, Options{Reauth: "gridwell-plugin-gmail -auth -credentials c.json -token t.json"})
	ctx := context.Background()

	_, err := p.Info(ctx, &pluginv1.InfoRequest{})
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("Info with a refused token = %v, want FailedPrecondition", err)
	}
	msg := status.Convert(err).Message()
	if !strings.Contains(msg, "refused") || !strings.Contains(msg, "-auth -credentials c.json -token t.json") {
		t.Errorf("refusal = %q, want a sentence that names the fix", msg)
	}

	f.mu.Lock()
	f.profileErr = nil
	f.mu.Unlock()
	if _, err := p.Info(ctx, &pluginv1.InfoRequest{}); err != nil {
		t.Fatalf("Info once the token works = %v", err)
	}
	asked := f.count("profile")
	f.mu.Lock()
	f.profileErr = status.Error(codes.PermissionDenied, "revoked")
	f.mu.Unlock()
	if _, err := p.Info(ctx, &pluginv1.InfoRequest{}); err != nil {
		t.Fatalf("Info after it passed = %v; it latches", err)
	}
	if f.count("profile") != asked {
		t.Error("Info asked Google again after it had passed")
	}
}

// Google out of reach at the first Info is not a verdict on the config: Info
// passes and latches, and the reads say the source is dark.
func TestATransportFailureAtFirstInfoPassesAndReadsDark(t *testing.T) {
	f := newFake()
	f.err = status.Error(codes.Unavailable, "gmail plugin: profile: connection refused")
	p := stable(f, Options{FirstAnswer: 10 * time.Millisecond})
	ctx := context.Background()
	if _, err := p.Info(ctx, &pluginv1.InfoRequest{}); err != nil {
		t.Fatalf("Info with Google out of reach = %v, want a pass", err)
	}
	asked := f.count("profile")
	if _, err := p.Info(ctx, &pluginv1.InfoRequest{}); err != nil || f.count("profile") != asked {
		t.Fatalf("the second Info = %v after %d profile reads; it latched on the first", err, f.count("profile")-asked)
	}
	landed(t, p)
	if _, err := p.List(ctx, &pluginv1.ListRequest{Context: mailbox.InboxContext}); status.Code(err) != codes.Unavailable {
		t.Fatalf("a cold read with Google out of reach = %v, want Unavailable", err)
	}
}

// The email is fetched for the request that asked for it, so a hangup ends
// the fetch.
func TestServeContentTakesTheRequestsContext(t *testing.T) {
	f := newFake()
	f.hold("INBOX", msg("a", "lunch", "2026-01-05T14:00:00Z"))
	p := stable(f, Options{})
	ctx, cancel := context.WithCancel(context.Background())
	if err := p.ServeContent(&pluginv1.ServeContentRequest{Key: "msg:a"}, &server{ctx: ctx}); err != nil {
		t.Fatal(err)
	}
	cancel()
	if f.ctxOf("html").Err() == nil {
		t.Error("the email was fetched under a context the request's hangup does not end")
	}
	if _, err := p.Probe(ctx, &pluginv1.ProbeRequest{Context: mailbox.AllMailContext, Key: "msg:a"}); err != nil {
		t.Fatal(err)
	}
	if f.ctxOf("headers").Err() == nil {
		t.Error("a probe's lookup was made under a context the request's hangup does not end")
	}
}

// A refresh outlives every reader, and the plugin: it runs under the plugin's
// lifetime, so ending the plugin ends it.
func TestTheWalkRunsUnderThePluginsLifetime(t *testing.T) {
	f := newFake()
	f.block = make(chan struct{})
	f.hold("INBOX", msg("a", "lunch", "2026-01-05T14:00:00Z"))
	p := stable(f, Options{FirstAnswer: time.Millisecond})
	reader, hangup := context.WithCancel(context.Background())
	if _, err := p.List(reader, &pluginv1.ListRequest{Context: mailbox.InboxContext}); err != nil {
		t.Fatal(err)
	}
	hangup()
	walk := f.waitCtx(t, "label")
	if walk.Err() != nil {
		t.Fatal("the reader's hangup ended the shared walk")
	}
	ended := make(chan struct{})
	go func() { p.life.End(); close(ended) }()
	select {
	case <-walk.Done():
	case <-time.After(5 * time.Second):
		t.Error("ending the plugin did not end its walk")
	}
	close(f.block)
	<-ended
}

// A failing refresh is logged when it starts failing, not on every refresh,
// and again only after one has landed between.
func TestARefreshFailureLogsOncePerEpisode(t *testing.T) {
	f := newFake()
	f.hold("INBOX", msg("a", "one", "2026-01-05T09:00:00Z"))
	var mu sync.Mutex
	var lines []string
	p, clock := warmHistory(t, f, Options{Logf: func(format string, args ...any) {
		mu.Lock()
		lines = append(lines, fmt.Sprintf(format, args...))
		mu.Unlock()
	}})
	said := func() int {
		mu.Lock()
		defer mu.Unlock()
		n := 0
		for _, l := range lines {
			if strings.Contains(l, "gmail is down") {
				n++
			}
		}
		return n
	}
	fail := func(err error) {
		f.mu.Lock()
		f.err = err
		f.mu.Unlock()
	}
	if n := len(lines); n != 0 {
		t.Fatalf("a refresh that worked said %d lines: %q", n, lines)
	}
	fail(status.Error(codes.Unavailable, "gmail is down"))
	for range 3 {
		refreshed(t, p, clock, 2*time.Minute)
	}
	if n := said(); n != 1 {
		t.Fatalf("three failed refreshes said it %d times, want once", n)
	}
	fail(nil)
	refreshed(t, p, clock, 2*time.Minute)
	fail(status.Error(codes.Unavailable, "gmail is down"))
	refreshed(t, p, clock, 2*time.Minute)
	if n := said(); n != 2 {
		t.Errorf("a failure after a landing said it %d times in all, want twice", n)
	}

	// One unreadable message is its own episode: said once, however many
	// walks fail to read it.
	g := newFake()
	g.hold("INBOX", msg("a", "one", "2026-01-05T09:00:00Z"), msg("b", "two", "2026-01-05T10:00:00Z"))
	delete(g.recs, "b")
	lines = nil
	gclock := at("2026-01-06T12:00:00Z")
	q := stable(g, Options{Refresh: time.Minute, Now: func() time.Time { return gclock }, Logf: func(format string, args ...any) {
		mu.Lock()
		lines = append(lines, fmt.Sprintf(format, args...))
		mu.Unlock()
	}})
	listAll(t, q)
	refreshed(t, q, &gclock, 2*time.Minute)
	refreshed(t, q, &gclock, 2*time.Minute)
	mu.Lock()
	n := 0
	for _, l := range lines {
		if strings.HasPrefix(l, "gmail plugin: message b:") {
			n++
		}
	}
	mu.Unlock()
	if n != 1 {
		t.Errorf("an unreadable message was said %d times over three walks, want once: %q", n, lines)
	}
}
