package plugin

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/josephburnett/gridwell-plugins/gitlab/gitlabapi"
	pluginv1 "github.com/josephburnett/gridwell/api/gen/plugin/v1"
)

// DefaultURL is the GitLab instance when config names none.
const DefaultURL = "https://gitlab.com"

// FromConfig builds the production plugin from the shared config vocabulary
// and starts its refresher. It is the one owner of the config-to-plugin
// derivation, so the subprocess main and a bundled binary compose exactly the
// same plugin. A missing or unreadable token is a refusal: the error is the
// verdict, and both doors turn it into a launch that stops with the reason
// instead of a plugin serving an empty grid.
func FromConfig(cfg map[string]string) (pluginv1.PluginServer, error) {
	base := strings.TrimSpace(cfg["url"])
	if base == "" {
		base = DefaultURL
	}
	// state_dir is the private directory the node mints for this plugin and
	// hands over beside uuid and kind. The plugin caches its walk there. A
	// node that hands none — an older one, or a hand-launched binary — is no
	// error: the plugin then keeps its memory for its process lifetime.
	opts := Options{StateDir: cfg["state_dir"]}
	if r := strings.TrimSpace(cfg["refresh"]); r != "" {
		d, err := time.ParseDuration(r)
		if err != nil || d <= 0 {
			return nil, fmt.Errorf("gitlab plugin: refresh %q is not a duration (e.g. 30s, 5m)", r)
		}
		opts.Refresh = d
	}
	tokenFile := strings.TrimSpace(cfg["token_file"])
	if tokenFile == "" {
		return nil, errors.New("gitlab plugin: token_file not configured (a file holding a read_api personal access token)")
	}
	raw, err := os.ReadFile(tokenFile)
	if err != nil {
		return nil, fmt.Errorf("gitlab plugin: token_file: %v", err)
	}
	token := strings.TrimSpace(string(raw))
	if token == "" {
		return nil, fmt.Errorf("gitlab plugin: token_file %s is empty", tokenFile)
	}
	api := gitlabapi.New(base, token, nil)
	// The API client is both halves: the pager the walk reads, and the
	// mark-as-done writer the trash gesture becomes.
	opts.Marker = api
	p := New(api, opts)
	// The refresher lives as long as the process does: a plugin subprocess is
	// stopped by the node killing it, and there is nothing else to unwind.
	go p.Run(context.Background())
	return p, nil
}
