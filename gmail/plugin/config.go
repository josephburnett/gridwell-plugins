package plugin

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strconv"
	"strings"
	"time"

	"github.com/josephburnett/gridwell-plugins/gmail/gmailapi"
	"github.com/josephburnett/gridwell-plugins/gmail/gmailauth"
	pluginv1 "github.com/josephburnett/gridwell/api/gen/plugin/v1"
)

// FromConfig builds the production plugin from the shared config vocabulary
// and starts its refresher. It is the one owner of the config-to-plugin
// derivation, so the subprocess main and any other door compose exactly the
// same plugin.
//
// The config carries PATHS and never secrets:
//
//	credentials  the OAuth client JSON from the Google Cloud console (required)
//	token        the token file `-auth` wrote (required)
//	refresh      how often a collection is re-walked (default: 1m)
//	max_messages how many of the newest messages a grid holds (default: 500)
//	endpoint     the Gmail API base URL (default: Gmail's own)
//
// endpoint is the same ordinary knob gitlab's url is — the address of the
// service this plugin reads. A node points it at a recorded Gmail to exercise
// the plugin without an account; nothing else about the plugin changes, and
// the credential is still required and still sent.
//
// Both files are the user's own, on the user's host, and neither belongs in
// the plugin's state directory: that directory is disposable, and a deleted
// credential is not rewarmed by use. There is deliberately no default for
// either path — a plugin that guessed where a credential lives would be
// guessing about the one thing it must not.
//
// A missing or unusable credential IS refused here, unlike a missing CLI: it
// is a fact about the configuration and not about the host's weather, and the
// node turns the refusal into a launch that stops naming the reason instead of
// a plugin row that serves an empty grid forever.
func FromConfig(cfg map[string]string) (pluginv1.PluginServer, error) {
	opts := Options{
		// state_dir is the private directory the node mints for this plugin
		// and hands over beside uuid and kind. The plugin caches its walk
		// there. A node that hands none — an older one, or a hand-launched
		// binary — is no error: the plugin then keeps its memory for its
		// process lifetime.
		StateDir: cfg["state_dir"],
	}
	if r := strings.TrimSpace(cfg["refresh"]); r != "" {
		d, err := time.ParseDuration(r)
		if err != nil || d <= 0 {
			return nil, fmt.Errorf("gmail plugin: refresh %q is not a duration (e.g. 30s, 5m)", r)
		}
		opts.Refresh = d
	}
	if m := strings.TrimSpace(cfg["max_messages"]); m != "" {
		n, err := strconv.Atoi(m)
		if err != nil || n <= 0 {
			return nil, fmt.Errorf("gmail plugin: max_messages %q is not a positive number", m)
		}
		opts.MaxMessages = n
	}

	credentials := strings.TrimSpace(cfg["credentials"])
	if credentials == "" {
		return nil, errors.New("gmail plugin: credentials not configured (the path to the OAuth client JSON from the Google Cloud console)")
	}
	tokenPath := strings.TrimSpace(cfg["token"])
	if tokenPath == "" {
		return nil, errors.New("gmail plugin: token not configured (the path gridwell-plugin-gmail -auth writes the token to)")
	}
	// The redirect URL is the auth flow's business and means nothing to a
	// running plugin: refreshing a token never redirects anywhere.
	oauthCfg, err := gmailauth.Config(credentials, "")
	if err != nil {
		return nil, err
	}
	tok, err := gmailauth.LoadToken(tokenPath)
	if err != nil {
		return nil, fmt.Errorf("gmail plugin: token: %v (run: gridwell-plugin-gmail -auth -credentials %s -token %s)",
			err, credentials, tokenPath)
	}

	ctx := context.Background()
	ts := gmailauth.TokenSource(ctx, oauthCfg, tokenPath, tok, func(err error) {
		// A refresh that could not be written back is not a read failure —
		// Gmail is readable — but it must not be silent: unwritten, the next
		// restart goes back to Google for a token it already had.
		log.Printf("gmail plugin: token: %v", err)
	})
	src, err := gmailapi.New(ctx, ts, strings.TrimSpace(cfg["endpoint"]))
	if err != nil {
		return nil, err
	}
	p := New(src, opts)
	// The refresher lives as long as the process does: a plugin subprocess is
	// stopped by the node killing it, and there is nothing else to unwind.
	go p.Run(ctx)
	return p, nil
}
