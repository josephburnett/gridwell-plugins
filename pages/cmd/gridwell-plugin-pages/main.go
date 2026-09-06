// gridwell-plugin-pages is the pages plugin binary: a plugin that serves web
// pages it generates itself, over plugin.v1. It has no config, no database and
// no source outside its own code; the node owns this plugin's memory.
package main

import (
	"github.com/josephburnett/gridwell-plugins/guest"
	"github.com/josephburnett/gridwell-plugins/pages/plugin"
)

func main() { guest.Main(plugin.FromConfig) }
