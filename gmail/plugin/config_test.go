package plugin

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/josephburnett/gridwell-plugins/gmail/mailbox"
	pluginv1 "github.com/josephburnett/gridwell/api/gen/plugin/v1"
)

// credentials is the recorded OAuth client file, which lives beside the
// package that parses it so there is one copy of that shape.
func credentials() string { return filepath.Join("..", "gmailauth", "testdata", "credentials.json") }

func tokenFile(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "token.json")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func aToken(t *testing.T) string {
	t.Helper()
	raw, err := json.Marshal(map[string]any{
		"access_token": "at", "refresh_token": "rt", "token_type": "Bearer",
		"expiry": time.Now().Add(time.Hour).Format(time.RFC3339),
	})
	if err != nil {
		t.Fatal(err)
	}
	return tokenFile(t, string(raw))
}

// A config the plugin cannot run on is FromConfig's error — the one verdict
// both doors turn into a launch that stops with the reason, rather than a
// plugin row serving an empty grid forever.
func TestFromConfigRefusesBadConfig(t *testing.T) {
	ok := aToken(t)
	cases := []struct {
		want string
		cfg  map[string]string
	}{
		{"credentials not configured", map[string]string{}},
		{"token not configured", map[string]string{"credentials": credentials()}},
		{"credentials", map[string]string{"credentials": filepath.Join(t.TempDir(), "absent.json"), "token": ok}},
		{"token", map[string]string{"credentials": credentials(), "token": filepath.Join(t.TempDir(), "absent.json")}},
		{"refresh token", map[string]string{"credentials": credentials(), "token": tokenFile(t, `{"access_token":"at"}`)}},
		{"not a duration", map[string]string{"credentials": credentials(), "token": ok, "refresh": "soon"}},
		{"not a positive number", map[string]string{"credentials": credentials(), "token": ok, "max_messages": "lots"}},
		{"not a positive number", map[string]string{"credentials": credentials(), "token": ok, "max_messages": "0"}},
	}
	for _, c := range cases {
		impl, err := FromConfig(c.cfg)
		if impl != nil || err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("cfg %v → %v, %v; want a refusal containing %q", c.cfg, impl, err, c.want)
		}
	}
}

// The endpoint knob is the address of the service this plugin reads, and the
// only proof it is wired is a read that lands somewhere else: the client
// FromConfig composed — credential and all — asking a fake Gmail for the
// inbox, and sending the token it was configured with.
func TestFromConfigReadsTheConfiguredEndpoint(t *testing.T) {
	var mu sync.Mutex
	auth := ""
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		auth = r.Header.Get("Authorization")
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json; charset=UTF-8")
		if strings.HasSuffix(r.URL.Path, "/messages") {
			if strings.Join(r.URL.Query()["labelIds"], "+") == "INBOX" {
				w.Write([]byte(`{"messages":[{"id":"a1","threadId":"a1"}]}`))
				return
			}
			w.Write([]byte(`{"resultSizeEstimate":0}`))
			return
		}
		w.Write([]byte(`{"id":"a1","threadId":"a1","internalDate":"1767621780000","labelIds":["INBOX"],` +
			`"payload":{"headers":[{"name":"Subject","value":"Lunch plans"}]}}`))
	}))
	defer srv.Close()

	impl, err := FromConfig(map[string]string{
		"credentials": credentials(), "token": aToken(t),
		"state_dir": t.TempDir(), "refresh": "1h", "endpoint": srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	resp, err := impl.List(context.Background(), &pluginv1.ListRequest{Context: mailbox.InboxContext})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(resp.Entries) != 1 || !strings.Contains(resp.Entries[0].Label, "Lunch plans") {
		t.Fatalf("entries = %+v; the walk did not read the configured endpoint", resp.Entries)
	}
	// The credential still travels: an endpoint override changes where the
	// plugin reads, never whether it authenticates.
	mu.Lock()
	defer mu.Unlock()
	if auth != "Bearer at" {
		t.Errorf("Authorization = %q, want the configured token", auth)
	}
}

// A token that will not run the plugin says so at launch, and says how to fix
// it: the fix is a command, and a user staring at a dead plugin row should not
// have to find it in a README.
func TestARefusedTokenNamesTheAuthCommand(t *testing.T) {
	_, err := FromConfig(map[string]string{
		"credentials": credentials(), "token": filepath.Join(t.TempDir(), "absent.json")})
	if err == nil || !strings.Contains(err.Error(), "-auth") {
		t.Fatalf("err = %v", err)
	}
}

func TestFromConfigComposesTheClient(t *testing.T) {
	dir := t.TempDir()
	// A long refresh: FromConfig starts the refresher, and this test has no
	// Gmail for it to walk.
	impl, err := FromConfig(map[string]string{
		"credentials": credentials(), "token": aToken(t),
		"state_dir": dir, "refresh": "1h", "max_messages": "50"})
	if err != nil {
		t.Fatal(err)
	}
	p := impl.(*Plugin)
	if p.src == nil || p.refresh != time.Hour || p.max != 50 {
		t.Errorf("plugin = src %v refresh %v max %d", p.src, p.refresh, p.max)
	}
	if got, want := p.cache, filepath.Join(dir, mailbox.CacheFile); got != want {
		t.Errorf("cache path = %q, want %q", got, want)
	}

	// state_dir is the node's key, beside uuid and kind. A node that hands
	// none is no error: the plugin then keeps its memory in process.
	impl, err = FromConfig(map[string]string{
		"credentials": credentials(), "token": aToken(t), "refresh": "1h"})
	if err != nil {
		t.Fatal(err)
	}
	if got := impl.(*Plugin).cache; got != "" {
		t.Errorf("cache path = %q with no state_dir in the config", got)
	}
	// The user-facing name is server.yaml's label; the plugin's own
	// DisplayName is only the fallback.
	info, _ := impl.Info(context.Background(), &pluginv1.InfoRequest{})
	if info.DisplayName != displayName || info.Kind != Kind {
		t.Errorf("info = %v", info)
	}
}
