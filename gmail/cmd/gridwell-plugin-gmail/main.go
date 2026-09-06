// gridwell-plugin-gmail is the Gmail plugin binary: a read-only projection of
// one Gmail account's inbox and starred mail, read through the Gmail API. The
// config vocabulary is plugin.FromConfig's, the one derivation every door
// shares. It holds no node fact — only a cache file, in the private directory
// the node hands it as state_dir, holding the walk it would otherwise repeat.
//
// It has one other mode, which the node never runs:
//
//	gridwell-plugin-gmail -auth -credentials <client.json> -token <token.json>
//
// That is the one-time authorization. It listens on 127.0.0.1, prints a
// Google consent URL for you to open, takes the redirect, and writes the
// refresh token to -token as 0600. Point the plugin's `token:` at that same
// path. See README.md for where the credentials file comes from.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"

	"github.com/josephburnett/gridwell-plugins/gmail/gmailauth"
	"github.com/josephburnett/gridwell-plugins/gmail/plugin"
	"github.com/josephburnett/gridwell-plugins/guest"
)

func main() {
	auth := flag.Bool("auth", false, "run the one-time Google authorization and write the token file")
	credentials := flag.String("credentials", "", "path to the OAuth client JSON from the Google Cloud console (with -auth)")
	token := flag.String("token", "", "path to write the token file to, 0600 (with -auth)")
	flag.Parse()

	if !*auth {
		// The node's door: config arrives in the environment, never in argv.
		guest.Main(plugin.FromConfig)
		return
	}
	if *credentials == "" || *token == "" {
		fmt.Fprintln(os.Stderr, "gridwell-plugin-gmail -auth needs -credentials <client.json> and -token <token.json>")
		os.Exit(2)
	}
	if err := gmailauth.Run(context.Background(), *credentials, *token, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
