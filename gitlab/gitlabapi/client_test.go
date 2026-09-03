package gitlabapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
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
		}
		w.Write([]byte(`[{"id":7,"state":"pending","created_at":"2026-08-18T10:00:00Z","target_type":"Issue","target":{"iid":3,"title":"x"}}]`))
	}))
	defer srv.Close()
	c := New(srv.URL+"/", "tok", nil)
	todos, more, err := c.Page(context.Background(), "pending", 1)
	if err != nil || len(todos) != 1 || todos[0].ID != 7 || todos[0].Target.IID != 3 || !more {
		t.Fatalf("page 1 = %v %v %v", todos, more, err)
	}
	if got.URL.Path != "/api/v4/todos" || got.URL.Query().Get("state") != "pending" || got.URL.Query().Get("per_page") != "100" {
		t.Errorf("request = %s", got.URL)
	}
	if _, more, _ := c.Page(context.Background(), "pending", 2); more {
		t.Error("no X-Next-Page must mean the last page")
	}
	if _, _, err := New(srv.URL, "wrong", nil).Page(context.Background(), "pending", 1); status.Code(err) != codes.PermissionDenied {
		t.Errorf("401 → %v, want PermissionDenied", err)
	}
}

func TestPageMapsOutagesToUnavailable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(503) }))
	if _, _, err := New(srv.URL, "t", nil).Page(context.Background(), "done", 1); status.Code(err) != codes.Unavailable {
		t.Errorf("503 → %v", err)
	}
	srv.Close()
	if _, _, err := New(srv.URL, "t", nil).Page(context.Background(), "done", 1); status.Code(err) != codes.Unavailable {
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
// answers Unavailable — "not right now", transport-shaped — so the node
// degrades to its remembered listing instead of waiting forever.
func TestStalledResponseIsUnavailable(t *testing.T) {
	stall := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(2 * time.Second)
	}))
	defer stall.Close()
	c := New(stall.URL, "tok", &http.Client{Timeout: 50 * time.Millisecond})
	_, _, err := c.Page(context.Background(), "pending", 1)
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
