package plugin

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/josephburnett/gridwell-plugins/gitlab/todos"
	pluginv1 "github.com/josephburnett/gridwell/api/gen/plugin/v1"
)

// A config the plugin cannot run on is FromConfig's error, the sentence
// guest.Main answers Info with and the node shows on the plugin's row.
func TestFromConfigRefusesBadConfig(t *testing.T) {
	cases := map[string]map[string]string{
		"token_file not configured":              {},
		"cannot be read: no such file":           {"token_file": filepath.Join(t.TempDir(), "missing")},
		"is empty":                               {"token_file": writeTemp(t, "  \n")},
		"refresh \"soon\" is not a duration":     {"token_file": writeTemp(t, "tok"), "refresh": "soon"},
		"full_refresh \"-1m\" is not a duration": {"token_file": writeTemp(t, "tok"), "full_refresh": "-1m"},
	}
	for want, cfg := range cases {
		impl, err := FromConfig(cfg)
		if impl != nil || err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("cfg %v → %v, %v; want a refusal containing %q", cfg, impl, err, want)
		}
	}
}

func TestFromConfigComposesTheClient(t *testing.T) {
	// A GitLab that takes the token, so Info's check passes.
	gl := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v4/user" || r.Header.Get("PRIVATE-TOKEN") != "tok" {
			w.WriteHeader(401)
			return
		}
		w.Write([]byte(`{"id":1}`))
	}))
	defer gl.Close()
	impl, err := FromConfig(map[string]string{"token_file": writeTemp(t, "tok\n"), "refresh": "5m", "full_refresh": "2h", "url": gl.URL + "/"})
	if err != nil {
		t.Fatal(err)
	}
	p := impl.(*Plugin)
	if p.src == nil || p.token == nil || p.refresh != 5*time.Minute || p.fullRefresh != 2*time.Hour {
		t.Errorf("plugin = src %v token %v refresh %v full_refresh %v", p.src, p.token, p.refresh, p.fullRefresh)
	}
	// state_dir is the node's key, beside uuid and kind. A node that hands
	// none is no error: the plugin then keeps its memory in process.
	if p.file.Path() != "" {
		t.Errorf("cache path = %q with no state_dir in the config", p.file.Path())
	}
	dir := t.TempDir()
	impl, err = FromConfig(map[string]string{"token_file": writeTemp(t, "tok"), "state_dir": dir})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := impl.(*Plugin).file.Path(), filepath.Join(dir, todos.CacheFile); got != want {
		t.Errorf("cache path = %q, want %q", got, want)
	}
	// The user-facing name is server.yaml's `name` (the registry label);
	// the plugin's own DisplayName is only the fallback.
	info, err := p.Info(context.Background(), &pluginv1.InfoRequest{})
	if err != nil || info.DisplayName != displayName || info.Kind != Kind {
		t.Errorf("info = (%v, %v)", info, err)
	}
}

func writeTemp(t *testing.T, s string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(p, []byte(s), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}
