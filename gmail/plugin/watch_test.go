package plugin

import (
	"context"
	"testing"
	"time"

	"github.com/josephburnett/gridwell-plugins/gmail/mailbox"
	pluginv1 "github.com/josephburnett/gridwell/api/gen/plugin/v1"
)

// Gmail cannot tell, so the plugin asks its history on a clock, and only
// while a Watch stream has one of its contexts in scope: the stream is the
// node saying someone is looking. With none, or one scoped to nothing this
// plugin lists, Gmail is asked nothing.
func TestHistoryPollsOnlyWhileAContextIsInScope(t *testing.T) {
	f := newFake()
	f.history = true
	f.hold("INBOX", msg("a", "one", "2026-01-05T09:00:00Z"))
	p := New(f, Options{Refresh: 10 * time.Millisecond, Linger: -1, Logf: func(string, ...any) {}})
	t.Cleanup(p.life.End)
	listAll(t, p)
	quiet := func(why string) {
		t.Helper()
		before := f.count("history")
		time.Sleep(MinPollInterval + 500*time.Millisecond)
		if got := f.count("history") - before; got != 0 {
			t.Errorf("%s: history was asked %d times", why, got)
		}
	}

	quiet("no stream open")
	closeOther := openWatch(t, p, "label:SPAM")
	quiet("a stream scoped to nothing this plugin lists")
	closeOther()

	closeInbox := openWatch(t, p, mailbox.InboxContext)
	deadline := time.Now().Add(3 * MinPollInterval)
	for f.count("history") == 0 {
		if time.Now().After(deadline) {
			t.Fatal("a stream showing the inbox never had history asked")
		}
		time.Sleep(10 * time.Millisecond)
	}
	closeInbox()
	landed(t, p)
	quiet("after the last stream left")
}

// openWatch opens a Watch scoped to contexts and waits for its header; the
// returned func hangs up and waits for Watch to return.
func openWatch(t *testing.T, p *Plugin, contexts ...string) func() {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	w := &watchStream{ctx: ctx, sent: make(chan string, 64)}
	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := p.Watch(&pluginv1.WatchRequest{Contexts: contexts}, w); err != nil {
			t.Error(err)
		}
	}()
	for deadline := time.Now().Add(5 * time.Second); !w.header.Load(); time.Sleep(time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("Watch sent no header")
		}
	}
	return func() {
		cancel()
		<-done
	}
}
