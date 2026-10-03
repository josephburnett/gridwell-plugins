package gitlabapi

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/josephburnett/gridwell-plugins/gitlab/todos"
)

func TestPageSpeaksTheGitLabWire(t *testing.T) {
	var got *http.Request
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r
		if r.Header.Get("PRIVATE-TOKEN") != "tok" {
			w.WriteHeader(401)
			return
		}
		if r.URL.Query().Get("page") == "1" {
			w.Header().Set("X-Next-Page", "2")
			w.Header().Set("X-Total-Pages", "2")
		}
		w.Write([]byte(`[{"id":7,"state":"pending","created_at":"2026-08-18T10:00:00Z","target_type":"Issue","target":{"iid":3,"title":"x"}}]`))
	}))
	defer srv.Close()
	c := New(srv.URL+"/", "tok", nil)
	r, err := c.Page(context.Background(), "pending", 1)
	if err != nil || len(r.Todos) != 1 || r.Todos[0].ID != 7 || r.Todos[0].Target.IID != 3 || !r.More || r.Pages != 2 {
		t.Fatalf("page 1 = %+v %v", r, err)
	}
	if got.URL.Path != "/api/v4/todos" || got.URL.Query().Get("state") != "pending" || got.URL.Query().Get("per_page") != "100" {
		t.Errorf("request = %s", got.URL)
	}
	if r, _ := c.Page(context.Background(), "pending", 2); r.More || r.Pages != 0 {
		t.Errorf("page 2 = %+v: no X-Next-Page is the last page, no X-Total-Pages is an unknown length", r)
	}
	if _, err := New(srv.URL, "wrong", nil).Page(context.Background(), "pending", 1); status.Code(err) != codes.PermissionDenied {
		t.Errorf("401 → %v, want PermissionDenied", err)
	}
}

func TestPageMapsOutagesToUnavailable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(503) }))
	if _, err := New(srv.URL, "t", nil).Page(context.Background(), "done", 1); status.Code(err) != codes.Unavailable {
		t.Errorf("503 → %v", err)
	}
	srv.Close()
	if _, err := New(srv.URL, "t", nil).Page(context.Background(), "done", 1); status.Code(err) != codes.Unavailable {
		t.Errorf("refused connection → %v", err)
	}
}

// TestDefaultClientHasATimeout pins the guard against the wedge that hung a
// real node: http.DefaultClient never times out, so one GitLab response that
// stalls mid-body parks the walk forever, and the plugin's shared flight
// turns that one hung request into every reader waiting on it — the UI shows
// "loading" for the life of the process, with no error to surface.
func TestDefaultClientHasATimeout(t *testing.T) {
	c := New("https://gitlab.example", "tok", nil)
	if c.http.Timeout <= 0 {
		t.Fatal("the default HTTP client must carry a timeout: a stalled response wedges the walk forever")
	}
}

// TestStalledResponseIsUnavailable: a request that exceeds the client timeout
// answers Unavailable — "not right now", transport-shaped — so memory answers
// with that reason instead of every reader waiting forever.
func TestStalledResponseIsUnavailable(t *testing.T) {
	stall := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(2 * time.Second)
	}))
	defer stall.Close()
	c := New(stall.URL, "tok", &http.Client{Timeout: 50 * time.Millisecond})
	_, err := c.Page(context.Background(), "pending", 1)
	if status.Code(err) != codes.Unavailable {
		t.Fatalf("stalled page = %v, want Unavailable", err)
	}
}

func TestMarkDoneSpeaksTheGitLabWire(t *testing.T) {
	var got *http.Request
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r
		if r.Header.Get("PRIVATE-TOKEN") != "tok" {
			w.WriteHeader(403)
			return
		}
		w.WriteHeader(201)
	}))
	defer srv.Close()
	if err := New(srv.URL, "tok", nil).MarkDone(context.Background(), 42); err != nil {
		t.Fatal(err)
	}
	if got.Method != http.MethodPost || got.URL.Path != "/api/v4/todos/42/mark_as_done" {
		t.Errorf("request = %s %s", got.Method, got.URL)
	}
	err := New(srv.URL, "wrong", nil).MarkDone(context.Background(), 42)
	if status.Code(err) != codes.PermissionDenied || !strings.Contains(err.Error(), "api scope") {
		t.Errorf("403 → %v; want PermissionDenied naming the api scope, which reads get by without", err)
	}
}

func TestMarkDoneMapsTheVerdicts(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(404) }))
	if err := New(srv.URL, "t", nil).MarkDone(context.Background(), 7); status.Code(err) != codes.NotFound {
		t.Errorf("404 → %v, want NotFound", err)
	}
	srv.Close()
	if err := New(srv.URL, "t", nil).MarkDone(context.Background(), 7); status.Code(err) != codes.Unavailable {
		t.Errorf("refused connection → %v, want Unavailable", err)
	}
}

// The client and the walk across their seam: a GitLab that names its length
// in X-Total-Pages is walked several pages at once, and the walk still
// absorbs every page.
func TestAWalkOverTheWireFetchesPagesConcurrently(t *testing.T) {
	const pages = 8
	var mu sync.Mutex
	inFlight, maxInFlight := 0, 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		inFlight++
		maxInFlight = max(maxInFlight, inFlight)
		mu.Unlock()
		defer func() { mu.Lock(); inFlight--; mu.Unlock() }()
		time.Sleep(30 * time.Millisecond)
		if r.URL.Query().Get("state") != "pending" {
			w.Write([]byte(`[]`))
			return
		}
		page, _ := strconv.Atoi(r.URL.Query().Get("page"))
		w.Header().Set("X-Total-Pages", strconv.Itoa(pages))
		if page < pages {
			w.Header().Set("X-Next-Page", strconv.Itoa(page+1))
		}
		created := time.Date(2026, 8, 25, 10, 0, 0, 0, time.UTC).AddDate(0, 0, -page).Format(time.RFC3339)
		fmt.Fprintf(w, `[{"id":%d,"state":"pending","created_at":%q,"target_type":"Issue","target":{"iid":1,"title":"x"}}]`, page, created)
	}))
	defer srv.Close()
	m := todos.NewMemory()
	if err := m.Sync(context.Background(), New(srv.URL, "tok", nil), time.Time{}); err != nil {
		t.Fatal(err)
	}
	if n := len(m.All()); n != pages {
		t.Errorf("remembered %d todos, want %d", n, pages)
	}
	if maxInFlight < 2 {
		t.Errorf("at most %d requests in flight: the walk paged serially", maxInFlight)
	}
}
