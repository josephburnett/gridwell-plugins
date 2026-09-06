package plugin

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/josephburnett/gridwell-plugins/hey/heycli"
	pluginv1 "github.com/josephburnett/gridwell/api/gen/plugin/v1"
)

// FromConfig builds the production plugin from the shared config vocabulary
// and starts its refresher. It is the one owner of the config-to-plugin
// derivation, so the subprocess main and any other door compose exactly the
// same plugin.
//
// There is nothing required to configure: the HEY CLI holds the account and
// its credentials, and this plugin never asks for either. The two optional
// keys are
//
//	binary   the hey CLI to run (default: "hey" on PATH)
//	refresh  how often a collection is re-walked (default: 1m)
//
// A missing or unusable CLI is not refused here. It is a fact about the host
// that can change while the node runs — the user installs it, or signs in —
// and refusing at launch would leave a dead plugin row until the node is
// restarted. Every read says so instead, with the reason.
func FromConfig(cfg map[string]string) (pluginv1.PluginServer, error) {
	opts := Options{
		// state_dir is the private directory the node mints for this plugin
		// and hands over beside uuid and kind. The plugin caches its sweep
		// there. A node that hands none — an older one, or a hand-launched
		// binary — is no error: the plugin then keeps its memory for its
		// process lifetime.
		StateDir: cfg["state_dir"],
	}
	if r := strings.TrimSpace(cfg["refresh"]); r != "" {
		d, err := time.ParseDuration(r)
		if err != nil || d <= 0 {
			return nil, fmt.Errorf("hey plugin: refresh %q is not a duration (e.g. 30s, 5m)", r)
		}
		opts.Refresh = d
	}
	src := heycli.New(heycli.Exec{Binary: strings.TrimSpace(cfg["binary"])})
	p := New(src, opts)
	// The refresher lives as long as the process does: a plugin subprocess is
	// stopped by the node killing it, and there is nothing else to unwind.
	go p.Run(context.Background())
	return p, nil
}
