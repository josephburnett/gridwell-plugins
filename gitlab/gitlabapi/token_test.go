package gitlabapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// CheckToken asks GET /api/v4/user with the token: a 200 passes, a 401 or 403
// is PermissionDenied with a sentence naming the token file and the scope, and
// GitLab not answering is Unavailable, not a verdict on the token.
func TestCheckTokenSpeaksTheGitLabWire(t *testing.T) {
	var path string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		if r.Header.Get("PRIVATE-TOKEN") != "tok" {
			w.WriteHeader(401)
			return
		}
		w.Write([]byte(`{"id":1,"username":"ada"}`))
	}))
	file := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(file, []byte("tok\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := NewWithTokenFile(srv.URL, file, nil).CheckToken(context.Background()); err != nil || path != "/api/v4/user" {
		t.Fatalf("a good token = %v asking %s", err, path)
	}
	err := New(srv.URL, "wrong", nil).CheckToken(context.Background())
	if status.Code(err) != codes.PermissionDenied || !strings.Contains(err.Error(), "read_api") {
		t.Errorf("401 → %v, want PermissionDenied naming the scope", err)
	}
	if err := os.WriteFile(file, []byte("wrong\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := NewWithTokenFile(srv.URL, file, nil).CheckToken(context.Background()); !strings.Contains(err.Error(), file) {
		t.Errorf("401 over a token file → %v, want the sentence to name the file", err)
	}
	srv.Close()
	if err := New(srv.URL, "tok", nil).CheckToken(context.Background()); status.Code(err) != codes.Unavailable {
		t.Errorf("refused connection → %v, want Unavailable", err)
	}
}

// The token file is read at every request, so a token written there after a
// refusal is the one the next request carries, with no restart.
func TestTheTokenFileIsReadAtEveryRequest(t *testing.T) {
	var got []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = append(got, r.Header.Get("PRIVATE-TOKEN"))
		w.Write([]byte(`[]`))
	}))
	defer srv.Close()
	file := filepath.Join(t.TempDir(), "token")
	c := NewWithTokenFile(srv.URL, file, nil)
	if _, err := c.Page(context.Background(), "pending", 1); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("a missing token file → %v, want FailedPrecondition", err)
	}
	for _, tok := range []string{"old", "new"} {
		if err := os.WriteFile(file, []byte(tok+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := c.Page(context.Background(), "pending", 1); err != nil {
			t.Fatal(err)
		}
	}
	if strings.Join(got, ",") != "old,new" {
		t.Errorf("requests carried %v, want each the file's token at the time", got)
	}
}
