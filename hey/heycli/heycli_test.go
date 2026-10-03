package heycli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/josephburnett/gridwell-plugins/hey/mail"
)

// fake is a Runner that answers canned output, and remembers the argv it was
// asked for: the flags ARE the contract, so the test pins them.
type fake struct {
	args   [][]string
	out    string
	errOut string
	code   int
	err    error
}

func (f *fake) Run(_ context.Context, args ...string) ([]byte, string, int, error) {
	f.args = append(f.args, args)
	return []byte(f.out), f.errOut, f.code, f.err
}

// Stream hands out's lines one at a time, then exits as Run would.
func (f *fake) Stream(_ context.Context, onLine func([]byte), args ...string) (string, int, error) {
	f.args = append(f.args, args)
	for _, l := range strings.Split(f.out, "\n") {
		onLine([]byte(l))
	}
	return f.errOut, f.code, f.err
}

const imboxJSON = `{"ok":true,"data":{"id":1,"kind":"imbox","name":"Imbox","postings":[
 {"id":900,"topic_id":101,"kind":"topic","name":"Lunch plans","summary":"Are you free friday?","seen":false,
  "created_at":"2026-01-05T14:03:00Z","creator":{"name":"Alice","email_address":"alice@example.com"}},
 {"id":901,"kind":"bundle","name":"News","summary":"three unseen","seen":true,
  "created_at":"2026-01-05T09:00:00Z","creator":{"name":"News","email_address":"news@example.com"}}
]}}`

func TestBoxReadsPostingsAndPinsTheCommand(t *testing.T) {
	f := &fake{out: imboxJSON}
	threads, whole, err := New(f).Box(context.Background(), "imbox")
	if err != nil {
		t.Fatalf("Box: %v", err)
	}
	want := []string{"box", "view", "imbox", "--json", "--all"}
	if len(f.args) != 1 || strings.Join(f.args[0], " ") != strings.Join(want, " ") {
		t.Fatalf("ran %v, want %v", f.args, want)
	}
	// The bundle row names no thread, so it is not a tile.
	if len(threads) != 1 {
		t.Fatalf("got %d threads, want 1: %+v", len(threads), threads)
	}
	got := threads[0]
	if got.TopicID != 101 || got.PostingID != 900 {
		t.Errorf("ids = %d/%d, want 101/900", got.TopicID, got.PostingID)
	}
	if got.Subject != "Lunch plans" || got.FromName != "Alice" || got.FromEmail != "alice@example.com" {
		t.Errorf("thread = %+v", got)
	}
	if got.Seen {
		t.Error("an unseen posting read as seen")
	}
	if got.CreatedAt.UTC().Format("2006-01-02 15:04") != "2026-01-05 14:03" {
		t.Errorf("created at %v", got.CreatedAt)
	}
	if !whole {
		t.Error("a listing with no next_page did not read as whole")
	}
}

// A cursor left over means the read was capped: what came back is what was
// seen, never the box's whole membership.
func TestBoxReportsACappedRead(t *testing.T) {
	f := &fake{out: `{"ok":true,"data":{"postings":[],"next_page":"cursor"}}`}
	_, whole, err := New(f).Box(context.Background(), "imbox")
	if err != nil {
		t.Fatalf("Box: %v", err)
	}
	if whole {
		t.Fatal("a capped listing read as whole")
	}
}

func TestThreadHTMLPinsTheCommand(t *testing.T) {
	f := &fake{out: "<!doctype html><html></html>"}
	out, err := New(f).ThreadHTML(context.Background(), 101)
	if err != nil {
		t.Fatalf("ThreadHTML: %v", err)
	}
	want := "thread read 101 --html"
	if len(f.args) != 1 || strings.Join(f.args[0], " ") != want {
		t.Fatalf("ran %v, want %q", f.args, want)
	}
	if string(out) != "<!doctype html><html></html>" {
		t.Errorf("html = %q", out)
	}
}

// The exit status is the whole error vocabulary: weather degrades to the
// remembered listing, a verdict surfaces. Getting this backwards either hides
// "you are not signed in" or throws the grid away over a dropped packet.
func TestExitCodesMapToTheNodesVocabulary(t *testing.T) {
	cases := []struct {
		code int
		want codes.Code
	}{
		{1, codes.InvalidArgument},
		{2, codes.NotFound},
		{3, codes.PermissionDenied},
		{4, codes.PermissionDenied},
		{5, codes.Unavailable},
		{6, codes.Unavailable},
		{7, codes.Unavailable},
		{8, codes.InvalidArgument},
	}
	for _, c := range cases {
		// The envelope rides stderr, where the CLI prints it.
		f := &fake{code: c.code, errOut: "warning: system keyring unavailable\n" +
			`{"ok":false,"error":"Not logged in","code":"auth","hint":"Run: hey auth login"}`}
		_, _, err := New(f).Box(context.Background(), "imbox")
		if got := status.Code(err); got != c.want {
			t.Errorf("exit %d = %v, want %v", c.code, got, c.want)
		}
		if !strings.Contains(err.Error(), "Not logged in") || !strings.Contains(err.Error(), "hey auth login") {
			t.Errorf("exit %d lost the reason: %v", c.code, err)
		}
	}
}

// A refusal with no envelope to read still has to say something — `--html`
// carries none — and a bare exit number is not something. The CLI's warnings
// are not the reason and must not bury it.
func TestRefusalWithNoEnvelopeReadsStderr(t *testing.T) {
	f := &fake{code: 7, errOut: "warning: system keyring unavailable\nError: the server said no\n"}
	_, _, err := New(f).Box(context.Background(), "imbox")
	if got := err.Error(); !strings.HasSuffix(got, "the server said no") {
		t.Fatalf("error = %v", got)
	}
}

// Nothing to read at all: the exit code is still an answer.
func TestASilentRefusalStillSaysSomething(t *testing.T) {
	f := &fake{code: 7}
	_, _, err := New(f).Box(context.Background(), "imbox")
	if !strings.Contains(err.Error(), "exit 7") {
		t.Fatalf("error = %v", err)
	}
}

// An envelope on stdout is read too: the CLI's own docs put it there, and a
// release that moved it back must not lose the reason.
func TestAnEnvelopeOnStdoutIsReadToo(t *testing.T) {
	f := &fake{code: 3, out: `{"ok":false,"error":"Not logged in","hint":"Run: hey auth login"}`}
	_, _, err := New(f).Box(context.Background(), "imbox")
	if !strings.Contains(err.Error(), "Not logged in (Run: hey auth login)") {
		t.Fatalf("error = %v", err)
	}
}

// A CLI that cannot be run is weather, not a verdict: a read runs only after
// Info's Installed check passed, which is the verdict on a missing CLI, and a
// verdict here would refuse every grid the node could still serve rows for.
func TestAMissingBinaryIsUnavailable(t *testing.T) {
	e := Exec{Binary: filepath.Join(t.TempDir(), "no-such-hey")}
	_, _, err := New(e).Box(context.Background(), "imbox")
	if got := status.Code(err); got != codes.Unavailable {
		t.Fatalf("code = %v, want Unavailable (%v)", got, err)
	}
}

// ── the real Exec, against the fake CLI ────────────────────────────────

func fakeCLI(t *testing.T) Exec {
	t.Helper()
	path, err := filepath.Abs(filepath.Join("testdata", "fake-hey"))
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&0o111 == 0 {
		t.Fatalf("%s is not executable; the CLI contract must be runnable", path)
	}
	return Exec{Binary: path}
}

// The seam the unit tests above cannot cross: argv, the environment, the exit
// code and the stdout/stderr split, through a real process.
func TestExecRunsTheRealCLI(t *testing.T) {
	c := New(fakeCLI(t))
	threads, whole, err := c.Box(context.Background(), "imbox")
	if err != nil {
		t.Fatalf("Box: %v", err)
	}
	if len(threads) != 1 || threads[0].TopicID != 101 || threads[0].FromName != "Alice" {
		t.Fatalf("threads = %+v", threads)
	}
	if !whole {
		t.Error("imbox did not read as whole")
	}
	// The CLI's keyring warning goes to stderr, and stderr is not data.
	if strings.Contains(threads[0].Subject, "warning") {
		t.Error("stderr leaked into the parsed listing")
	}

	if _, whole, err = c.Box(context.Background(), "laterbox"); err != nil {
		t.Fatalf("Box laterbox: %v", err)
	} else if whole {
		t.Error("a box that reported a cursor read as whole")
	}

	html, err := c.ThreadHTML(context.Background(), 101)
	if err != nil {
		t.Fatalf("ThreadHTML: %v", err)
	}
	if !strings.HasPrefix(string(html), "<!doctype html>") || !strings.Contains(string(html), "data-entry-id") {
		t.Fatalf("html = %q", html)
	}

	// A refusal prints on STDERR, envelope and all, after the keyring
	// warning — and --html prints no envelope at all. The reason is what
	// reaches the user, not the transcript around it.
	_, _, err = c.Box(context.Background(), "lockedbox")
	if status.Code(err) != codes.PermissionDenied {
		t.Errorf("a logged-out box = %v, want PermissionDenied", err)
	}
	if !strings.Contains(err.Error(), "Not logged in (Run: hey auth login)") {
		t.Errorf("the envelope was not read: %v", err)
	}
	if strings.Contains(err.Error(), "keyring") || strings.Contains(err.Error(), "\"ok\"") {
		t.Errorf("the whole stderr transcript rode the error: %v", err)
	}
	_, err = c.ThreadHTML(context.Background(), 404)
	if status.Code(err) != codes.NotFound {
		t.Errorf("a missing thread = %v, want NotFound", err)
	}
	if !strings.Contains(err.Error(), "Thread not found") || strings.Contains(err.Error(), "keyring") {
		t.Errorf("an envelope-less refusal read as %v", err)
	}
}

// watchFixture is the live feed's contract: every line shape `hey watch`
// prints, as hey 1.4.1 prints it.
func watchFixture(t *testing.T) [][]byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "watch.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	var lines [][]byte
	for _, l := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		lines = append(lines, []byte(l))
	}
	return lines
}

func TestParseWatchLinesPinsTheFeed(t *testing.T) {
	var got []mail.Event
	for _, l := range watchFixture(t) {
		ev, err := ParseWatchLine(l)
		if err != nil {
			t.Fatalf("%s: %v", l, err)
		}
		got = append(got, ev)
	}
	if len(got) != 8 {
		t.Fatalf("parsed %d lines", len(got))
	}
	if got[0].Change != mail.ChangeReady || got[0].Box != "" {
		t.Errorf("ready = %+v", got[0])
	}
	// The thread is the line's thread_id: the posting inside carries no
	// topic_id, unlike a `box view` row.
	added := got[1]
	if added.Change != mail.ChangeAdded || added.Box != "imbox" || added.PostingID != 930 {
		t.Errorf("added = %+v", added)
	}
	if th := added.Thread; th.TopicID != 103 || th.PostingID != 930 || th.Subject != "Board games" ||
		th.Summary != "Thursday at mine?" || th.FromName != "Erin" || th.FromEmail != "erin@example.com" ||
		th.Seen || th.CreatedAt.IsZero() {
		t.Errorf("added thread = %+v", th)
	}
	if !got[2].Thread.Seen || got[2].Change != mail.ChangeUpdated {
		t.Errorf("updated = %+v", got[2])
	}
	if got[3].Box != "feedbox" {
		t.Errorf("a box outside the projection = %+v", got[3])
	}
	// A deleted line names only the posting and its box.
	if d := got[4]; d.Change != mail.ChangeDeleted || d.Box != "imbox" || d.PostingID != 930 || d.Thread.TopicID != 0 {
		t.Errorf("deleted = %+v", d)
	}
	if r := got[5]; r.Change != mail.ChangeResync || r.Box != "laterbox" {
		t.Errorf("resync = %+v", r)
	}
	if got[6].Change != mail.ChangeDisconnected || got[7].Change != mail.ChangeReady {
		t.Errorf("feed words = %+v, %+v", got[6], got[7])
	}
}

func TestParseWatchLineRefusesWhatIsNotALine(t *testing.T) {
	for _, l := range []string{"", "warning: keyring", `{"at":"2026-09-28T18:56:33Z"}`} {
		if _, err := ParseWatchLine([]byte(l)); err == nil {
			t.Errorf("%q parsed", l)
		}
	}
	// A word this plugin does not know is carried, not refused: a newer CLI
	// adding one must not break the feed.
	ev, err := ParseWatchLine([]byte(`{"change":"recording_added","at":"2026-09-28T18:56:33Z"}`))
	if err != nil || ev.Change != "recording_added" {
		t.Errorf("unknown word = %+v, %v", ev, err)
	}
}

func TestWatchPinsTheCommandAndCarriesABadLine(t *testing.T) {
	f := &fake{out: `{"change":"ready","at":"2026-09-28T18:56:33Z"}` + "\n\nnot json\n", code: 3,
		errOut: "warning: keyring\n" + `{"ok":false,"error":"Not logged in","code":"auth","hint":"Run: hey auth login"}`}
	var evs []mail.Event
	var bad []error
	err := New(f).Watch(context.Background(), func(ev mail.Event, err error) {
		if err != nil {
			bad = append(bad, err)
			return
		}
		evs = append(evs, ev)
	})
	if want := "watch --events added,updated,deleted,resync"; len(f.args) != 1 || strings.Join(f.args[0], " ") != want {
		t.Fatalf("ran %v, want %s", f.args, want)
	}
	if len(evs) != 1 || evs[0].Change != mail.ChangeReady {
		t.Errorf("events = %+v", evs)
	}
	if len(bad) != 1 {
		t.Errorf("an unreadable line was not carried: %v", bad)
	}
	// The feed's refusal is a read's refusal: not signed in is a verdict.
	if status.Code(err) != codes.PermissionDenied || !strings.Contains(err.Error(), "Not logged in") {
		t.Errorf("err = %v", err)
	}
}

func TestAFeedTheCLIEndsIsWeather(t *testing.T) {
	err := New(&fake{}).Watch(context.Background(), func(mail.Event, error) {})
	if status.Code(err) != codes.Unavailable {
		t.Errorf("err = %v, want Unavailable", err)
	}
}

// The live feed through a real process: every line of the contract arrives
// as it is printed, the feed runs on after them, and ending ctx ends it.
func TestExecStreamsTheFeedUntilCancelled(t *testing.T) {
	c := New(fakeCLI(t))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	got := make(chan mail.Event, 16)
	done := make(chan error, 1)
	go func() {
		done <- c.Watch(ctx, func(ev mail.Event, err error) {
			if err != nil {
				t.Errorf("line: %v", err)
				return
			}
			got <- ev
		})
	}()
	for i := range watchFixture(t) {
		select {
		case <-got:
		case err := <-done:
			t.Fatalf("the feed ended after %d lines: %v", i, err)
		case <-time.After(10 * time.Second):
			t.Fatalf("line %d never arrived", i)
		}
	}
	select {
	case err := <-done:
		t.Fatalf("the feed ended by itself: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	cancel()
	select {
	case err := <-done:
		if status.Code(err) != codes.Unavailable {
			t.Errorf("a cancelled feed = %v, want Unavailable", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("cancelling did not end the feed")
	}
}

func TestAFeedWithNoCLIIsUnavailable(t *testing.T) {
	err := New(Exec{Binary: filepath.Join(t.TempDir(), "no-such-hey")}).Watch(context.Background(), func(mail.Event, error) {})
	if status.Code(err) != codes.Unavailable {
		t.Errorf("err = %v, want Unavailable", err)
	}
}

// Installed is the check Info makes: a CLI that cannot be found is named in
// the sentence, and one on PATH passes without being run.
func TestInstalledNamesAMissingCLI(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "no-such-hey")
	if err := (Exec{Binary: missing}).Installed(); err == nil || !strings.Contains(err.Error(), missing) {
		t.Errorf("missing path → %v, want a refusal naming it", err)
	}
	t.Setenv("PATH", t.TempDir())
	if err := (Exec{}).Installed(); err == nil || !strings.Contains(err.Error(), `"hey" is not installed`) {
		t.Errorf("no hey on PATH → %v, want a refusal naming it", err)
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "hey"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	if err := (Exec{}).Installed(); err != nil {
		t.Errorf("hey on PATH → %v, want installed", err)
	}
}
