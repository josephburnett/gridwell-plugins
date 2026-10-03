// Package gitlabapi is the thin HTTP half of the gitlab todos plugin:
// one pager over GET /api/v4/todos. It knows the wire (token header,
// per_page, X-Next-Page, X-Total-Pages) and nothing about weeks, memory, or
// tiles.
package gitlabapi

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
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
	base  string // "https://gitlab.com", with no trailing slash
	token string
	http  *http.Client
}

// DefaultTimeout bounds one request end to end. http.DefaultClient has no
// timeout, and a GitLab response that stalls mid-body would park the shared
// walk forever, with no failure to report. A timeout turns the stall into
// Unavailable, "not right now", and memory answers with that reason until a
// walk lands.
const DefaultTimeout = 30 * time.Second

// New builds a client. A nil httpClient gets a default with DefaultTimeout.
func New(base, token string, httpClient *http.Client) *Client {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: DefaultTimeout}
	}
	return &Client{base: strings.TrimRight(base, "/"), token: token, http: httpClient}
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
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+"/api/v4/todos?"+q.Encode(), nil)
	if err != nil {
		return todos.Reply{}, status.Errorf(codes.InvalidArgument, "gitlab: %v", err)
	}
	req.Header.Set("PRIVATE-TOKEN", c.token)
	resp, err := c.http.Do(req)
	if err != nil {
		return todos.Reply{}, status.Errorf(codes.Unavailable, "gitlab: %v", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return todos.Reply{}, status.Errorf(codes.Unavailable, "gitlab: read: %v", err)
	}
	switch {
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return todos.Reply{}, status.Errorf(codes.PermissionDenied, "gitlab: %s (check the token's read_api scope)", resp.Status)
	case resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500:
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

// MarkDone marks one todo done: POST /api/v4/todos/:id/mark_as_done, the
// plugin's only write, and the only call that needs the token's api scope —
// the reads get by on read_api, so the permission verdict names the scope the
// user must add. The other codes map like Page's: a network failure, a 5xx or
// a 429 is Unavailable, and GitLab not knowing the id is NotFound.
func (c *Client) MarkDone(ctx context.Context, id int64) error {
	u := fmt.Sprintf("%s/api/v4/todos/%d/mark_as_done", c.base, id)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, nil)
	if err != nil {
		return status.Errorf(codes.InvalidArgument, "gitlab: %v", err)
	}
	req.Header.Set("PRIVATE-TOKEN", c.token)
	resp, err := c.http.Do(req)
	if err != nil {
		return status.Errorf(codes.Unavailable, "gitlab: %v", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return status.Errorf(codes.Unavailable, "gitlab: read: %v", err)
	}
	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		return nil
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return status.Errorf(codes.PermissionDenied, "gitlab: %s (marking done needs the token's api scope)", resp.Status)
	case resp.StatusCode == http.StatusNotFound:
		return status.Errorf(codes.NotFound, "gitlab: %s", resp.Status)
	case resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500:
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
