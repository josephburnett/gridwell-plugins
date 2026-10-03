package plugin

import (
	"fmt"
	"strings"
	"time"

	"github.com/josephburnett/gridwell-plugins/hey/heycli"
	"github.com/josephburnett/gridwell-plugins/memo"
	pluginv1 "github.com/josephburnett/gridwell/api/gen/plugin/v1"
)

// FromConfig builds the production plugin from the shared config vocabulary.
// It is the one owner of the config-to-plugin derivation, so the subprocess
// main and any other door compose exactly the same plugin. It starts nothing:
// a walk runs for a call, and the live feed while a Watch stream is open.
//
// There is nothing required to configure: the HEY CLI holds the account and
// its credentials, and this plugin never asks for either. The two optional
// keys are
//
//	binary   the hey CLI to run (default: "hey" on PATH)
//	refresh  how long a walk answers reads while the live feed is not live
//	         (default: 1m)
//
// A missing CLI is not refused here but at Info until it is found, because
// it is a fact about the host that can change while the node runs: the user
// installs it and the plugin comes back without a restart. A CLI that is not
// signed in is only learned by running it, so every read says that, with the
// reason.
func FromConfig(cfg map[string]string) (pluginv1.PluginServer, error) {
	opts := Options{
		// state_dir is the private directory the node mints for this plugin
		// and hands over beside uuid and kind. A node that hands none — an
		// older one, or a hand-launched binary — is no error: the plugin then
		// keeps its memory for its process lifetime.
		StateDir: cfg["state_dir"],
		// A plugin subprocess ends by being killed, so nothing ends its life.
		Life: memo.NewLife(),
	}
	if r := strings.TrimSpace(cfg["refresh"]); r != "" {
		d, err := time.ParseDuration(r)
		if err != nil || d <= 0 {
			return nil, fmt.Errorf("hey plugin: refresh %q is not a duration (e.g. 30s, 5m)", r)
		}
		opts.Refresh = d
	}
	cli := heycli.Exec{Binary: strings.TrimSpace(cfg["binary"])}
	opts.Ready = cli.Installed
	return New(heycli.New(cli), opts), nil
}
