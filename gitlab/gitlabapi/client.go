// Package gitlabapi is the thin HTTP half of the gitlab todos plugin: the
// pager over GET /api/v4/todos, the mark-as-done write, and the token check
// over GET /api/v4/user. It knows the wire (token header, per_page,
// X-Next-Page, X-Total-Pages) and nothing about weeks, memory, or tiles.
package gitlabapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/josephburnett/gridwell-plugins/gitlab/todos"
)

// PerPage is the page size asked of GitLab (its maximum).
const PerPage = 100

// Client pages one GitLab instance's todo list for one token.
type Client struct {
	base string // "https://gitlab.com", with no trailing slash
	// token answers the token for each request. A token file is read every
	// time, so a token rewritten there is used without a restart.
	token func() (string, error)
	// tokenFile names where the token lives in a refusal, "" for a token
	// given directly.
	tokenFile string
	http      *http.Client
}

// DefaultTimeout bounds one request end to end. http.DefaultClient has no
// timeout, and a GitLab response that stalls mid-body would park the shared
// walk forever, with no failure to report. A timeout turns the stall into
// Unavailable, "not right now", and memory answers with that reason until a
// walk lands.
const DefaultTimeout = 30 * time.Second

// New builds a client over a fixed token. A nil httpClient gets a default
// with DefaultTimeout.
func New(base, token string, httpClient *http.Client) *Client {
	c := newClient(base, httpClient)
	c.token = func() (string, error) { return token, nil }
	return c
}

// NewWithTokenFile builds a client whose token is the trimmed content of
// path, read at every request.
func NewWithTokenFile(base, path string, httpClient *http.Client) *Client {
	c := newClient(base, httpClient)
	c.tokenFile = path
	c.token = func() (string, error) { return ReadToken(path) }
	return c
}

func newClient(base string, httpClient *http.Client) *Client {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: DefaultTimeout}
	}
	return &Client{base: strings.TrimRight(base, "/"), http: httpClient}
}

// ReadToken reads a token file: its content, trimmed. A file that cannot be
// read or holds nothing is FailedPrecondition, a fault in the config.
func ReadToken(path string) (string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		var pe *fs.PathError
		if errors.As(err, &pe) {
			err = pe.Err
		}
		return "", status.Errorf(codes.FailedPrecondition, "token_file %s cannot be read: %v", path, err)
	}
	token := strings.TrimSpace(string(raw))
	if token == "" {
		return "", status.Errorf(codes.FailedPrecondition, "token_file %s is empty", path)
	}
	return token, nil
}

// do sends one request with the token and reads at most limit bytes of the
// answer. A request that never got an answer is Unavailable.
func (c *Client) do(ctx context.Context, method, u string, limit int64) (*http.Response, []byte, error) {
	token, err := c.token()
	if err != nil {
		return nil, nil, err
	}
	req, err := http.NewRequestWithContext(ctx, method, u, nil)
	if err != nil {
		return nil, nil, status.Errorf(codes.InvalidArgument, "gitlab: %v", err)
	}
	req.Header.Set("PRIVATE-TOKEN", token)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, nil, status.Errorf(codes.Unavailable, "gitlab: %v", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, limit))
	if err != nil {
		return nil, nil, status.Errorf(codes.Unavailable, "gitlab: read: %v", err)
	}
	return resp, body, nil
}

// refused reports a 401 or 403: the token is not accepted for this call.
func refused(resp *http.Response) bool {
	return resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden
}

// weather reports a 429 or 5xx: GitLab is there and cannot answer now.
func weather(resp *http.Response) bool {
	return resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500
}

// Page implements todos.Source. A network failure, a 5xx, or a 429 is
// Unavailable, "not right now", which a walk retries; a 401 or 403 is
// PermissionDenied, which it does not. Either way a read memory can answer is
// answered, with the failure as its unreachable reason.
func (c *Client) Page(ctx context.Context, state string, page int) (todos.Reply, error) {
	q := url.Values{}
	q.Set("state", state)
	q.Set("per_page", strconv.Itoa(PerPage))
	q.Set("page", strconv.Itoa(page))
	resp, body, err := c.do(ctx, http.MethodGet, c.base+"/api/v4/todos?"+q.Encode(), 32<<20)
	if err != nil {
		return todos.Reply{}, err
	}
	switch {
	case refused(resp):
		return todos.Reply{}, status.Errorf(codes.PermissionDenied, "gitlab: %s (check the token's read_api scope)", resp.Status)
	case weather(resp):
		return todos.Reply{}, status.Errorf(codes.Unavailable, "gitlab: %s", resp.Status)
	case resp.StatusCode != http.StatusOK:
		return todos.Reply{}, status.Errorf(codes.Internal, "gitlab: %s: %s", resp.Status, trim(body))
	}
	var out []todos.Todo
	if err := json.Unmarshal(body, &out); err != nil {
		return todos.Reply{}, status.Errorf(codes.Internal, "gitlab: decode todos: %v", err)
	}
	// A missing or malformed X-Total-Pages is zero; see todos.Reply.Pages.
	pages, _ := strconv.Atoi(resp.Header.Get("X-Total-Pages"))
	return todos.Reply{Todos: out, More: resp.Header.Get("X-Next-Page") != "", Pages: pages}, nil
}

// CheckToken asks GitLab whose token this is: one cheap request that proves
// the token works before the plugin presents anything. A 401 or 403 is
// PermissionDenied with a sentence saying what to fix; the other failures map
// as Page's do.
func (c *Client) CheckToken(ctx context.Context) error {
	resp, body, err := c.do(ctx, http.MethodGet, c.base+"/api/v4/user", 1<<20)
	if err != nil {
		return err
	}
	switch {
	case resp.StatusCode == http.StatusOK:
		return nil
	case refused(resp):
		where := "the token"
		if c.tokenFile != "" {
			where = "the token in " + c.tokenFile
		}
		return status.Errorf(codes.PermissionDenied,
			"GitLab refused %s (%s): write a personal access token with the read_api scope there", where, resp.Status)
	case weather(resp):
		return status.Errorf(codes.Unavailable, "gitlab: %s", resp.Status)
	default:
		return status.Errorf(codes.Internal, "gitlab: %s: %s", resp.Status, trim(body))
	}
}

// MarkDone marks one todo done: POST /api/v4/todos/:id/mark_as_done, the
// plugin's only write, and the only call that needs the token's api scope —
// the reads get by on read_api, so the permission verdict names the scope the
// user must add. The other codes map like Page's: a network failure, a 5xx or
// a 429 is Unavailable, and GitLab not knowing the id is NotFound.
func (c *Client) MarkDone(ctx context.Context, id int64) error {
	resp, body, err := c.do(ctx, http.MethodPost, fmt.Sprintf("%s/api/v4/todos/%d/mark_as_done", c.base, id), 1<<20)
	if err != nil {
		return err
	}
	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		return nil
	case refused(resp):
		return status.Errorf(codes.PermissionDenied, "gitlab: %s (marking done needs the token's api scope)", resp.Status)
	case resp.StatusCode == http.StatusNotFound:
		return status.Errorf(codes.NotFound, "gitlab: %s", resp.Status)
	case weather(resp):
		return status.Errorf(codes.Unavailable, "gitlab: %s", resp.Status)
	default:
		return status.Errorf(codes.Internal, "gitlab: %s: %s", resp.Status, trim(body))
	}
}

func trim(b []byte) string {
	s := strings.TrimSpace(string(b))
	if len(s) > 200 {
		s = s[:200] + "…"
	}
	return fmt.Sprintf("%q", s)
}
