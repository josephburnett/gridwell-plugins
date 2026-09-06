package heycli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
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
		f := &fake{code: c.code, out: `{"ok":false,"error":"Not logged in","code":"auth","hint":"Run: hey auth login"}`}
		_, _, err := New(f).Box(context.Background(), "imbox")
		if got := status.Code(err); got != c.want {
			t.Errorf("exit %d = %v, want %v", c.code, got, c.want)
		}
		if !strings.Contains(err.Error(), "Not logged in") || !strings.Contains(err.Error(), "hey auth login") {
			t.Errorf("exit %d lost the reason: %v", c.code, err)
		}
	}
}

// A refusal with no envelope to read still has to say something: stderr is
// the only text there is, and losing it leaves the user a bare exit number.
func TestRefusalFallsBackToStderr(t *testing.T) {
	f := &fake{code: 7, errOut: "hey: the server said no\n"}
	_, _, err := New(f).Box(context.Background(), "imbox")
	if !strings.Contains(err.Error(), "the server said no") {
		t.Fatalf("error = %v", err)
	}
}

// The binary is not there: a configuration verdict, not weather. Answering
// Unavailable would make the node serve a stale grid forever and say nothing.
func TestAMissingBinaryIsAVerdict(t *testing.T) {
	e := Exec{Binary: filepath.Join(t.TempDir(), "no-such-hey")}
	_, _, err := New(e).Box(context.Background(), "imbox")
	if got := status.Code(err); got != codes.FailedPrecondition {
		t.Fatalf("code = %v, want FailedPrecondition (%v)", got, err)
	}
}

// A cancelled context is weather, whatever the run failed with.
func TestACancelledRunIsWeather(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	e := Exec{Binary: filepath.Join(t.TempDir(), "no-such-hey")}
	_, _, err := New(e).Box(ctx, "imbox")
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

	if _, _, err := c.Box(context.Background(), "trailbox"); status.Code(err) != codes.PermissionDenied {
		t.Errorf("a logged-out box = %v, want PermissionDenied", err)
	}
	if _, err := c.ThreadHTML(context.Background(), 404); status.Code(err) != codes.NotFound {
		t.Errorf("a missing thread = %v, want NotFound", err)
	}
}
