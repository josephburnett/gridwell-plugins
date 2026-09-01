// gridwell-plugin-gitlab is the gitlab todos plugin binary: a projection of
// one GitLab account's to-do list, serving plugin.v1. The config vocabulary is
// plugin.FromConfig's, the one derivation every door shares. It holds no node
// fact and has no database — only a cache file, in the private directory the
// node hands it as state_dir, holding the walk it would otherwise repeat.
package main

import (
	"github.com/josephburnett/gridwell-plugins/gitlab/plugin"
	"github.com/josephburnett/gridwell-plugins/guest"
)

func main() { guest.Main(plugin.FromConfig) }
