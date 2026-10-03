package plugin

import (
	"fmt"
	"strings"
	"time"

	"google.golang.org/grpc/status"

	"github.com/josephburnett/gridwell-plugins/gitlab/gitlabapi"
	pluginv1 "github.com/josephburnett/gridwell/api/gen/plugin/v1"
)

// DefaultURL is the GitLab instance when config names none.
const DefaultURL = "https://gitlab.com"

// FromConfig builds the production plugin from the shared config
// vocabulary. It is the one owner of the config-to-plugin
// derivation, so the subprocess main and a bundled binary compose exactly the
// same plugin. A missing or unreadable token is a refusal: the error is the
// verdict, the node shows the plugin broken with it instead of serving an
// empty grid, and guest.Main asks again until the token file is fixed. A
// token GitLab refuses is Info's refusal (see Plugin.Info).
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
	for key, into := range map[string]*time.Duration{"refresh": &opts.Refresh, "full_refresh": &opts.FullRefresh} {
		r := strings.TrimSpace(cfg[key])
		if r == "" {
			continue
		}
		d, err := time.ParseDuration(r)
		if err != nil || d <= 0 {
			return nil, fmt.Errorf("gitlab plugin: %s %q is not a duration (e.g. 30s, 10m)", key, r)
		}
		*into = d
	}
	tokenFile := strings.TrimSpace(cfg["token_file"])
	if tokenFile == "" {
		return nil, fmt.Errorf("gitlab plugin: token_file not configured (a file holding a read_api personal access token)")
	}
	if _, err := gitlabapi.ReadToken(tokenFile); err != nil {
		return nil, fmt.Errorf("gitlab plugin: %s", status.Convert(err).Message())
	}
	api := gitlabapi.NewWithTokenFile(base, tokenFile, nil)
	// The API client is all three halves: the pager the walk reads, the
	// mark-as-done writer the trash gesture becomes, and the token check
	// Info makes.
	opts.Marker, opts.Token = api, api
	return New(api, opts), nil
}
