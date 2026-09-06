package gmailauth

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/oauth2"
)

// The credentials file is the one Google's console hands out. Reading it is
// where a typo in a path or a file from the wrong client type is caught, so
// the refusal has to name the file.
func TestConfigReadsGooglesClientFile(t *testing.T) {
	cfg, err := Config(filepath.Join("testdata", "credentials.json"), "http://127.0.0.1:4321/")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(cfg.ClientID, "1234567890-") || cfg.ClientSecret == "" {
		t.Errorf("config = %+v", cfg)
	}
	if cfg.RedirectURL != "http://127.0.0.1:4321/" {
		t.Errorf("redirect = %q", cfg.RedirectURL)
	}
	if len(cfg.Scopes) != 1 || cfg.Scopes[0] != Scope {
		t.Errorf("scopes = %v; a read-only projection asks for read-only", cfg.Scopes)
	}
	if _, err := Config(filepath.Join(t.TempDir(), "absent.json"), ""); err == nil {
		t.Error("a missing credentials file was accepted")
	}
	bad := filepath.Join(t.TempDir(), "bad.json")
	if err := os.WriteFile(bad, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Config(bad, ""); err == nil {
		t.Error("a credentials file with no client was accepted")
	}
}

// The token file is the plugin's whole credential: 0600, atomic, and refused
// on the way in if it carries no refresh token — an access token alone is an
// hour of Gmail and then a plugin that stopped for no visible reason.
func TestTokenFileRoundTripsAt0600(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "token.json")
	tok := &oauth2.Token{AccessToken: "at", RefreshToken: "rt", TokenType: "Bearer",
		Expiry: time.Now().Add(time.Hour).Round(time.Second)}
	if err := SaveToken(path, tok); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("token mode = %v; a credential must not be readable by anyone else on the host", info.Mode().Perm())
	}
	back, err := LoadToken(path)
	if err != nil {
		t.Fatal(err)
	}
	if back.AccessToken != "at" || back.RefreshToken != "rt" || !back.Expiry.Equal(tok.Expiry) {
		t.Fatalf("token = %+v", back)
	}

	noRefresh := filepath.Join(t.TempDir(), "token.json")
	if err := SaveToken(noRefresh, &oauth2.Token{AccessToken: "at"}); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadToken(noRefresh); err == nil || !strings.Contains(err.Error(), "refresh token") {
		t.Fatalf("a token with no refresh half = %v", err)
	}
}

// A refreshed access token is written back to the same file, so the file
// stays the one owner of the credential and a restart never goes back to
// Google for something it already had.
func TestTokenSourcePersistsARefresh(t *testing.T) {
	google := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"access_token":"fresh","refresh_token":"rt","token_type":"Bearer","expires_in":3600}`))
	}))
	defer google.Close()

	path := filepath.Join(t.TempDir(), "token.json")
	expired := &oauth2.Token{AccessToken: "stale", RefreshToken: "rt", Expiry: time.Now().Add(-time.Hour)}
	if err := SaveToken(path, expired); err != nil {
		t.Fatal(err)
	}
	cfg := &oauth2.Config{ClientID: "id", ClientSecret: "secret",
		Endpoint: oauth2.Endpoint{TokenURL: google.URL, AuthURL: google.URL + "/auth"}}

	var errs []error
	ts := TokenSource(context.Background(), cfg, path, expired, func(err error) { errs = append(errs, err) })
	tok, err := ts.Token()
	if err != nil {
		t.Fatal(err)
	}
	if tok.AccessToken != "fresh" {
		t.Fatalf("token = %+v", tok)
	}
	if len(errs) != 0 {
		t.Fatalf("errors = %v", errs)
	}
	back, err := LoadToken(path)
	if err != nil {
		t.Fatal(err)
	}
	if back.AccessToken != "fresh" || back.RefreshToken != "rt" {
		t.Fatalf("the refresh was not written back: %+v", back)
	}
	// A second read inside the window changes nothing, so the file is not
	// rewritten on every call.
	before, _ := os.Stat(path)
	if _, err := ts.Token(); err != nil {
		t.Fatal(err)
	}
	after, _ := os.Stat(path)
	if !before.ModTime().Equal(after.ModTime()) {
		t.Error("an unchanged token rewrote the file")
	}
}

// The whole flow, over a real loopback listener and a fake Google: the URL
// the user is given, the redirect back, the exchange, and the token file at
// the end. A unit test on either side of the redirect would not catch a state
// value that never travels or a redirect URL that names a port nothing is
// listening on.
func TestRunTakesTheRedirectAndWritesTheToken(t *testing.T) {
	google := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Error(err)
		}
		if got := r.Form.Get("code"); got != "the-code" {
			t.Errorf("exchanged code = %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"access_token":"at","refresh_token":"rt","token_type":"Bearer","expires_in":3600}`))
	}))
	defer google.Close()

	tokenPath := filepath.Join(t.TempDir(), "token.json")
	var out syncBuf
	done := make(chan error, 1)
	go func() { done <- flow(google.URL, tokenPath, &out) }()

	consent := waitForURL(t, &out)
	// The consent URL is what the user is asked to open: it must carry this
	// client, the read-only scope, offline access, and a loopback redirect
	// this process is actually listening on.
	if consent.Query().Get("scope") != Scope {
		t.Errorf("scope = %q", consent.Query().Get("scope"))
	}
	if consent.Query().Get("access_type") != "offline" {
		t.Errorf("access_type = %q; without it Google issues no refresh token", consent.Query().Get("access_type"))
	}
	redirect := consent.Query().Get("redirect_uri")
	if !strings.HasPrefix(redirect, "http://127.0.0.1:") {
		t.Fatalf("redirect_uri = %q", redirect)
	}
	state := consent.Query().Get("state")
	if state == "" {
		t.Fatal("no state travelled; any page the user visits could then drive this listener")
	}

	// A callback carrying the wrong state must not end the flow.
	if resp, err := http.Get(redirect + "?state=other&code=stolen"); err != nil {
		t.Fatal(err)
	} else {
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("a wrong-state callback = %d", resp.StatusCode)
		}
	}
	select {
	case err := <-done:
		t.Fatalf("a wrong-state callback ended the flow: %v", err)
	case <-time.After(50 * time.Millisecond):
	}

	resp, err := http.Get(redirect + "?state=" + state + "&code=the-code")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("the redirect answered %d", resp.StatusCode)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}

	tok, err := LoadToken(tokenPath)
	if err != nil {
		t.Fatal(err)
	}
	if tok.RefreshToken != "rt" {
		t.Fatalf("token = %+v", tok)
	}
	if !strings.Contains(out.String(), tokenPath) {
		t.Errorf("the flow did not say where it wrote the token:\n%s", out.String())
	}
}

// Google issuing no refresh token is refused rather than written: a token
// file that works for an hour and then stops is a much worse thing to debug
// than a refusal here.
func TestAuthorizeRefusesATokenWithNoRefreshHalf(t *testing.T) {
	google := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"access_token":"at","token_type":"Bearer","expires_in":3600}`))
	}))
	defer google.Close()

	tokenPath := filepath.Join(t.TempDir(), "token.json")
	var out syncBuf
	done := make(chan error, 1)
	go func() { done <- flow(google.URL, tokenPath, &out) }()
	consent := waitForURL(t, &out)
	resp, err := http.Get(consent.Query().Get("redirect_uri") + "?state=" + consent.Query().Get("state") + "&code=c")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	err = <-done
	if err == nil || !strings.Contains(err.Error(), "no refresh token") {
		t.Fatalf("err = %v", err)
	}
	if _, statErr := os.Stat(tokenPath); statErr == nil {
		t.Error("a token with no refresh half was written anyway")
	}
}

// Google refusing at the consent screen is the user's answer, and it ends the
// flow with what Google said rather than hanging until the timeout.
func TestAuthorizeSurfacesGooglesRefusal(t *testing.T) {
	var out syncBuf
	done := make(chan error, 1)
	go func() { done <- flow("http://127.0.0.1:1/token", filepath.Join(t.TempDir(), "t.json"), &out) }()
	consent := waitForURL(t, &out)
	resp, err := http.Get(consent.Query().Get("redirect_uri") + "?state=" + consent.Query().Get("state") + "&error=access_denied")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if err := <-done; err == nil || !strings.Contains(err.Error(), "access_denied") {
		t.Fatalf("err = %v", err)
	}
}

// syncBuf is a buffer the flow writes from its own goroutine while the test
// reads it from this one.
type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// flow runs the PRODUCTION flow with only Google replaced: the listener, the
// state, the redirect URL, the exchange and the write are all Run's own, so a
// Run that drifts from this cannot pass.
func flow(tokenURL, tokenPath string, out *syncBuf) error {
	return run(context.Background(), filepath.Join("testdata", "credentials.json"), tokenPath, out,
		&oauth2.Endpoint{AuthURL: "https://accounts.example/o/oauth2/auth", TokenURL: tokenURL})
}

// waitForURL pulls the consent URL out of what the flow printed.
func waitForURL(t *testing.T, out *syncBuf) *url.URL {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		for _, line := range strings.Fields(out.String()) {
			if strings.HasPrefix(line, "https://accounts.example/") {
				u, err := url.Parse(line)
				if err != nil {
					t.Fatal(err)
				}
				return u
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("the flow printed no consent URL:\n%s", out.String())
	return nil
}

// The token file is JSON oauth2 itself round-trips, so a token written by one
// release is read by the next.
func TestTheTokenFileIsOauth2sOwnShape(t *testing.T) {
	path := filepath.Join(t.TempDir(), "token.json")
	if err := SaveToken(path, &oauth2.Token{AccessToken: "at", RefreshToken: "rt"}); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatal(err)
	}
	if fields["refresh_token"] != "rt" || fields["access_token"] != "at" {
		t.Fatalf("token file = %s", raw)
	}
}
