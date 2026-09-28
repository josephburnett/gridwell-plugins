// Package heycli is the HEY half of the hey plugin: it runs the official HEY
// CLI (https://www.hey.com/agents) once per read and turns what it prints
// into mail.Thread records and email HTML. It holds no credentials and never
// looks for any: `hey` keeps its own, in its own config directory or the
// host's keyring, and this package only spawns it.
//
// The exact command surface it is built against — hey 1.4.1 — is:
//
//	hey box view <box> --json --all     one box's threads, as a JSON envelope
//	hey thread read <topic-id> --html   one thread as an HTML5 document
//	hey watch --events <mail changes>   the live feed, one JSON line per change
//
// where <box> is one of the CLI's own named box selectors: imbox, laterbox
// (Reply Later) and asidebox (Set Aside). README.md records the shapes those
// three commands answer with, so a CLI change is diagnosable from this
// repository alone.
//
// Everything that runs a process goes through Runner, so the parsing above it
// is tested without spawning anything and Exec is tested against a script
// that answers canned output.
package heycli

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/josephburnett/gridwell-plugins/hey/mail"
)

// DefaultBinary is the CLI the plugin runs when config names no path: found
// on PATH, the way the HEY installer leaves it.
const DefaultBinary = "hey"

// DefaultTimeout bounds one CLI run end to end. A `hey box view --all` on a
// large box reads many pages, and a HEY that stalls mid-read would otherwise
// park the plugin's sweep forever — the shared flight then holds every reader
// on that one hung process, and the grid says "loading" for the life of the
// process with no error to surface. A timeout turns the stall into
// Unavailable, "not right now", and the node serves its remembered listing.
const DefaultTimeout = 2 * time.Minute

// Runner runs the CLI once and reports what it printed. err is only for a
// failure to RUN — the binary is missing, the context ended — never for a
// command that ran and refused: that is a non-zero code with output to read.
type Runner interface {
	Run(ctx context.Context, args ...string) (stdout []byte, stderr string, code int, err error)
	// Stream runs the CLI until it exits or ctx ends, handing onLine each
	// stdout line as it arrives. err is Run's.
	Stream(ctx context.Context, onLine func([]byte), args ...string) (stderr string, code int, err error)
}

// Exec is the production Runner: one subprocess per call.
type Exec struct {
	// Binary is the CLI to run. Empty means DefaultBinary on PATH.
	Binary string
	// Timeout bounds one run. Zero means DefaultTimeout.
	Timeout time.Duration
}

// Run spawns the CLI with no stdin. HEY_NONINTERACTIVE stops the CLI offering
// to sign the user in: a prompt with nothing to read it would hang the sweep,
// and signing in is the user's own gesture at their own terminal, never
// something a plugin does on their behalf.
func (e Exec) Run(ctx context.Context, args ...string) ([]byte, string, int, error) {
	bin := strings.TrimSpace(e.Binary)
	if bin == "" {
		bin = DefaultBinary
	}
	timeout := e.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Env = append(os.Environ(), "HEY_NONINTERACTIVE=1")
	cmd.Stdin = nil
	var errBuf strings.Builder
	cmd.Stderr = &errBuf
	out, err := cmd.Output()
	if err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			return out, errBuf.String(), exit.ExitCode(), nil
		}
		return out, errBuf.String(), 0, err
	}
	return out, errBuf.String(), 0, nil
}

// maxLine bounds one line of the live feed. A line is one posting, a few
// kilobytes with its contacts and avatars; a longer one is not a line the
// contract describes.
const maxLine = 16 << 20

// Stream spawns the CLI as Run does, with no timeout: the live feed runs
// until ctx ends. The child is tied to this process (dieWithParent), because
// a plugin is stopped by being killed and a feed nothing reads must not
// outlive it.
func (e Exec) Stream(ctx context.Context, onLine func([]byte), args ...string) (string, int, error) {
	bin := strings.TrimSpace(e.Binary)
	if bin == "" {
		bin = DefaultBinary
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Env = append(os.Environ(), "HEY_NONINTERACTIVE=1")
	cmd.Stdin = nil
	cmd.WaitDelay = time.Second
	dieWithParent(cmd)
	var errBuf strings.Builder
	cmd.Stderr = &errBuf
	out, err := cmd.StdoutPipe()
	if err != nil {
		return "", 0, err
	}
	if err := cmd.Start(); err != nil {
		return "", 0, err
	}
	sc := bufio.NewScanner(out)
	sc.Buffer(make([]byte, 64<<10), maxLine)
	for sc.Scan() {
		onLine(sc.Bytes())
	}
	scanErr := sc.Err()
	if scanErr != nil {
		cancel()
	}
	err = cmd.Wait()
	if scanErr != nil {
		return errBuf.String(), 0, scanErr
	}
	if err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) && ctx.Err() == nil {
			return errBuf.String(), exit.ExitCode(), nil
		}
		return errBuf.String(), 0, err
	}
	return errBuf.String(), 0, nil
}

// Client reads one HEY account through the CLI.
type Client struct{ run Runner }

// New builds a client over a runner.
func New(r Runner) *Client { return &Client{run: r} }

// ── the JSON the CLI answers with ──────────────────────────────────────

// envelope is the CLI's response envelope, the same shape for every command
// that returns data and for every failure.
type envelope struct {
	OK    bool            `json:"ok"`
	Data  json.RawMessage `json:"data"`
	Error string          `json:"error"`
	Code  string          `json:"code"`
	Hint  string          `json:"hint"`
}

// boxData is `hey box view`'s payload: HEY's own box object with the
// listing's postings over the top of it. next_page is the cursor a further
// read would continue from, so its ABSENCE is how the plugin knows the box
// was read to its end.
type boxData struct {
	ID       int64     `json:"id"`
	Kind     string    `json:"kind"`
	Name     string    `json:"name"`
	Postings []posting `json:"postings"`
	NextPage string    `json:"next_page"`
}

// posting is the subset of HEY's posting the plugin reads. topic_id is the
// CLI's own addition beside HEY's fields, and it is the thread the row opens:
// a bundle row — one sender's unseen threads grouped — names no topic and
// answers zero.
type posting struct {
	ID        int64     `json:"id"`
	TopicID   int64     `json:"topic_id"`
	Kind      string    `json:"kind"`
	Name      string    `json:"name"`
	Summary   string    `json:"summary"`
	Seen      bool      `json:"seen"`
	CreatedAt time.Time `json:"created_at"`
	Creator   struct {
		Name         string `json:"name"`
		EmailAddress string `json:"email_address"`
	} `json:"creator"`
}

// ── reads ──────────────────────────────────────────────────────────────

// Box reads one box to its end. whole is false when the CLI capped the read
// and reported a cursor to continue from: the caller must then treat what
// came back as "what was seen", never as the box's whole membership.
//
// A row that names no thread — a bundle — is skipped rather than shown: it
// has no content to open, and inventing a key for it would put a tile on the
// grid that nothing can read.
func (c *Client) Box(ctx context.Context, box string) (threads []mail.Thread, whole bool, err error) {
	out, err := c.read(ctx, "box view "+box, "box", "view", box, "--json", "--all")
	if err != nil {
		return nil, false, err
	}
	var env envelope
	if err := json.Unmarshal(out, &env); err != nil {
		return nil, false, status.Errorf(codes.Internal, "hey plugin: box view %s: %v", box, err)
	}
	var data boxData
	if len(env.Data) > 0 {
		if err := json.Unmarshal(env.Data, &data); err != nil {
			return nil, false, status.Errorf(codes.Internal, "hey plugin: box view %s: %v", box, err)
		}
	}
	for _, p := range data.Postings {
		if p.TopicID == 0 {
			continue
		}
		threads = append(threads, p.thread(p.TopicID))
	}
	return threads, data.NextPage == "", nil
}

// thread is the posting as the thread it opens. topicID is passed in because
// the two commands name it in different places: `box view` beside HEY's
// fields, `watch` on the line around the posting.
func (p *posting) thread(topicID int64) mail.Thread {
	return mail.Thread{
		TopicID:   topicID,
		PostingID: p.ID,
		Subject:   p.Name,
		Summary:   p.Summary,
		FromName:  p.Creator.Name,
		FromEmail: p.Creator.EmailAddress,
		CreatedAt: p.CreatedAt,
		Seen:      p.Seen,
	}
}

// watchLine is one line of `hey watch`: no envelope, one object per line. A
// deleted line carries no posting and no thread_id; ready and disconnected
// carry no box.
type watchLine struct {
	Change string `json:"change"`
	Box    struct {
		Kind string `json:"kind"`
	} `json:"box"`
	PostingID int64    `json:"posting_id"`
	ThreadID  int64    `json:"thread_id"`
	Posting   *posting `json:"posting"`
}

// ParseWatchLine reads one line of the live feed.
func ParseWatchLine(line []byte) (mail.Event, error) {
	var w watchLine
	if err := json.Unmarshal(line, &w); err != nil {
		return mail.Event{}, fmt.Errorf("hey plugin: watch line: %v", err)
	}
	if w.Change == "" {
		return mail.Event{}, fmt.Errorf("hey plugin: watch line names no change: %.80q", line)
	}
	ev := mail.Event{Change: w.Change, Box: w.Box.Kind, PostingID: w.PostingID}
	if w.Posting != nil {
		ev.Thread = w.Posting.thread(w.ThreadID)
	}
	return ev, nil
}

// ThreadHTML reads one thread as HEY's own HTML: a whole HTML5 document, one
// <article> per entry oldest first, each holding the message exactly as HEY
// served it. That is the email, which is what descending into the tile
// opens. An empty answer is not an error — HEY serves some threads no body —
// and the caller says so on the page.
func (c *Client) ThreadHTML(ctx context.Context, topicID int64) ([]byte, error) {
	id := strconv.FormatInt(topicID, 10)
	return c.read(ctx, "thread read "+id, "thread", "read", id, "--html")
}

// WatchArgs is the live feed's argv. The events are the mail changes only,
// which also switches the calendars off; every box is followed whatever the
// list says.
var WatchArgs = []string{"watch", "--events", "added,updated,deleted,resync"}

// Watch runs the live feed until ctx ends or the CLI exits, handing on every
// line it prints; a line it cannot read arrives as an error and the feed runs
// on. It answers the reason the feed stopped, coded as a read's refusal: a
// feed has no success, so an exit the CLI chose on its own is Unavailable,
// and so is ctx ending.
func (c *Client) Watch(ctx context.Context, on func(mail.Event, error)) error {
	errText, code, err := c.run.Stream(ctx, func(line []byte) {
		if len(bytes.TrimSpace(line)) == 0 {
			return
		}
		on(ParseWatchLine(line))
	}, WatchArgs...)
	switch {
	case ctx.Err() != nil:
		return status.Errorf(codes.Unavailable, "hey plugin: watch: %v", ctx.Err())
	case err != nil:
		return status.Errorf(codes.FailedPrecondition, "hey plugin: watch: %v", err)
	case code == 0:
		return status.Error(codes.Unavailable, "hey plugin: watch: the CLI ended the feed")
	}
	return refusal("watch", nil, errText, code)
}

// read runs one command and turns a refusal into a coded error. what names
// the command for the message; a plugin's error is read by a person looking
// at a grid that will not fill.
func (c *Client) read(ctx context.Context, what string, args ...string) ([]byte, error) {
	out, errText, code, err := c.run.Run(ctx, args...)
	if err != nil {
		// The CLI could not be run at all: a missing binary is a
		// configuration verdict, and a context that ended is weather.
		if ctx.Err() != nil {
			return nil, status.Errorf(codes.Unavailable, "hey plugin: %s: %v", what, err)
		}
		return nil, status.Errorf(codes.FailedPrecondition, "hey plugin: %s: %v", what, err)
	}
	if code == 0 {
		return out, nil
	}
	return nil, refusal(what, out, errText, code)
}

// refusal maps the CLI's exit status onto the node's error vocabulary. The
// codes are the CLI's own, documented by `hey help exit-codes`:
//
//	1 usage or validation   2 not found        3 auth required
//	4 forbidden             5 rate limited     6 network failed
//	7 server or local       8 ambiguous
//
// Transport-shaped codes mean "not right now", and the node serves what it
// has, stamped stale. The rest are verdicts and surface: a plugin that
// answered "not right now" to "you are not signed in" would leave the user
// staring at an empty grid with nothing said.
func refusal(what string, out []byte, errText string, code int) error {
	env, ok := readEnvelope(out)
	if !ok {
		// A refusal goes to STDERR, envelope and all — stdout stays empty.
		env, ok = readEnvelope([]byte(errText))
	}
	var detail string
	switch {
	case ok && strings.TrimSpace(env.Error) != "":
		detail = strings.TrimSpace(env.Error)
		if hint := strings.TrimSpace(env.Hint); hint != "" {
			detail += " (" + hint + ")"
		}
	default:
		// No envelope: `--html` carries none, in success or in failure. What
		// stderr says IS the reason, minus the lines that are not about it.
		detail = reason(errText)
	}
	if detail == "" {
		detail = fmt.Sprintf("exit %d", code)
	}
	var c codes.Code
	switch code {
	case 2:
		c = codes.NotFound
	case 3, 4:
		c = codes.PermissionDenied
	case 5, 6, 7:
		c = codes.Unavailable
	default: // 1 and 8: the plugin asked for something the CLI will not do
		c = codes.InvalidArgument
	}
	return status.Errorf(c, "hey plugin: %s: %s", what, detail)
}

// readEnvelope finds the response envelope in one stream. It scans for the
// first '{' rather than decoding from byte zero, because the envelope on
// stderr arrives after whatever the CLI has already said there — the keyring
// warning, most often.
func readEnvelope(b []byte) (envelope, bool) {
	i := bytes.IndexByte(b, '{')
	if i < 0 {
		return envelope{}, false
	}
	var env envelope
	if err := json.Unmarshal(b[i:], &env); err != nil {
		return envelope{}, false
	}
	return env, true
}

// reason is what stderr says, minus the CLI's warnings: a warning is a note
// about the host, never the reason a command refused, and pasting the whole
// transcript into the error buries the one line the user needs. "Error: " is
// the CLI's own prefix for that line, and it says nothing a caller who is
// already reporting an error needs repeated.
func reason(errText string) string {
	var keep []string
	for _, line := range strings.Split(errText, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "warning:") {
			continue
		}
		keep = append(keep, strings.TrimPrefix(line, "Error: "))
	}
	return strings.Join(keep, "; ")
}
