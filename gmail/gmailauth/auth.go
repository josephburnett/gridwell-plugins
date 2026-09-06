// Package gmailauth is the gmail plugin's credential half: the OAuth client
// it is configured with, the one-time flow that mints a refresh token, and
// the token source the running plugin reads Gmail through.
//
// SECRETS ARE HOST-LOCAL FILES. Nothing here takes a client secret or a token
// as a config VALUE, and nothing writes either anywhere but the path the user
// named. Both files are the user's, on the user's host: `server.yaml` carries
// their paths and never their contents, and the token file is written 0600.
//
// Neither file belongs in the plugin's state directory. That directory is
// disposable — the node's contract says deleting it is always safe — and a
// deleted credential is not rewarmed by use, it is a trip back to Google.
//
// The flow is the loopback redirect, which is what Google's "Desktop app"
// client type is for: the plugin listens on 127.0.0.1, prints the consent
// URL, and Google sends the browser back to that listener with the code. No
// secret is ever pasted through a terminal, and nothing is registered with
// Google beyond the client itself.
package gmailauth

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
)

// Scope is the one scope this plugin asks for: read-only Gmail. It is
// google.golang.org/api/gmail/v1's GmailReadonlyScope, written out so this
// package depends on the OAuth libraries alone. A projection that cannot
// write is a projection that cannot lose the user's mail, and the consent
// screen says so in the user's own words.
const Scope = "https://www.googleapis.com/auth/gmail.readonly"

// Config reads the OAuth client Google issued and points its redirect at
// redirectURL. The file is the one downloaded from the Google Cloud console
// for a client of type "Desktop app"; google.ConfigFromJSON takes both the
// `installed` and `web` shapes it comes in.
func Config(credentialsPath, redirectURL string) (*oauth2.Config, error) {
	raw, err := os.ReadFile(credentialsPath)
	if err != nil {
		return nil, fmt.Errorf("gmail plugin: credentials: %v", err)
	}
	cfg, err := google.ConfigFromJSON(raw, Scope)
	if err != nil {
		return nil, fmt.Errorf("gmail plugin: credentials %s: %v", credentialsPath, err)
	}
	if cfg.ClientID == "" {
		return nil, fmt.Errorf("gmail plugin: credentials %s: no client id", credentialsPath)
	}
	if redirectURL != "" {
		cfg.RedirectURL = redirectURL
	}
	return cfg, nil
}

// LoadToken reads the token the flow wrote.
func LoadToken(path string) (*oauth2.Token, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var tok oauth2.Token
	if err := json.Unmarshal(raw, &tok); err != nil {
		return nil, fmt.Errorf("gmail plugin: token %s: %v", path, err)
	}
	if tok.RefreshToken == "" {
		// An access token alone is an hour of Gmail and then a dead plugin.
		// Saying so now beats saying nothing until it expires.
		return nil, fmt.Errorf("gmail plugin: token %s holds no refresh token; re-run with -auth", path)
	}
	return &tok, nil
}

// SaveToken writes tok to path, 0600, atomically: a temp file beside the
// target, synced, then renamed over it. A reader — this plugin's next boot —
// sees the whole old token or the whole new one, and a crash mid-write costs
// the refresh, not the credential.
func SaveToken(path string, tok *oauth2.Token) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	raw, err := json.Marshal(tok)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp) // a no-op once the rename has moved it away
	// 0600 BEFORE the bytes: a credential must never exist, even for an
	// instant, at a mode another user on the host could read it through.
	if err := f.Chmod(0o600); err != nil {
		f.Close()
		return err
	}
	if _, err := f.Write(raw); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// TokenSource is how the running plugin reads Gmail: oauth2 refreshes the
// access token when it expires, and every refreshed token is written back to
// the same file. Persisting is not an optimization — it is what keeps the
// file the ONE owner of the plugin's credential, so a restart never has to go
// back to Google for something it already had.
//
// A failed write is reported through onErr and nothing else: the refresh
// succeeded, Gmail is readable, and only the next restart pays for the lost
// write.
func TokenSource(ctx context.Context, cfg *oauth2.Config, path string, tok *oauth2.Token, onErr func(error)) oauth2.TokenSource {
	return &savingSource{
		src:   cfg.TokenSource(ctx, tok),
		path:  path,
		last:  tok.AccessToken,
		onErr: onErr,
	}
}

type savingSource struct {
	src   oauth2.TokenSource
	path  string
	onErr func(error)

	mu   sync.Mutex
	last string
}

func (s *savingSource) Token() (*oauth2.Token, error) {
	tok, err := s.src.Token()
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	changed := tok.AccessToken != s.last
	if changed {
		s.last = tok.AccessToken
	}
	s.mu.Unlock()
	if changed {
		if err := SaveToken(s.path, tok); err != nil && s.onErr != nil {
			s.onErr(err)
		}
	}
	return tok, nil
}

// AuthorizeTimeout bounds how long the flow waits for the user to finish at
// Google. A listener left open forever on a machine nobody is sitting at is a
// door with nothing behind it.
const AuthorizeTimeout = 5 * time.Minute

// Authorize runs the one-time flow on ln and answers the token Google issued.
// It prints the consent URL to out; the user opens it, approves, and Google
// redirects the browser back to ln with the code.
//
// The `state` value is minted here and checked on the way back. It is not
// ceremony: without it any page the user later visits could drive this
// listener, and this listener exchanges codes for a token to the user's mail.
//
// A token with no refresh token is refused rather than written. Google issues
// one only with access_type=offline and a prompt the user actually answers;
// without it the plugin works for an hour and then stops, which is a much
// worse thing to debug than a refusal here.
func Authorize(ctx context.Context, cfg *oauth2.Config, ln net.Listener, out io.Writer) (*oauth2.Token, error) {
	state, err := nonce()
	if err != nil {
		return nil, err
	}
	type result struct {
		code string
		err  error
	}
	done := make(chan result, 1)
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		switch {
		case q.Get("state") != state:
			http.Error(w, "this is not the sign-in this plugin started", http.StatusBadRequest)
			return // NOT a result: an unrelated request must not end the flow
		case q.Get("error") != "":
			http.Error(w, "Google refused: "+q.Get("error"), http.StatusBadRequest)
			done <- result{err: fmt.Errorf("gmail plugin: Google refused: %s", q.Get("error"))}
		case q.Get("code") == "":
			http.Error(w, "no code", http.StatusBadRequest)
			return
		default:
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			io.WriteString(w, "Gridwell has your Gmail token. You can close this tab.\n")
			done <- result{code: q.Get("code")}
		}
	})}
	go srv.Serve(ln)
	defer srv.Close()

	// AccessTypeOffline is what asks for a refresh token, and ApprovalForce is
	// what makes Google issue a NEW one even when the user has approved this
	// client before — without it a second run answers an access token alone.
	consent := cfg.AuthCodeURL(state, oauth2.AccessTypeOffline, oauth2.ApprovalForce)
	fmt.Fprintf(out, "Open this in a browser signed in as the account you want Gridwell to read:\n\n%s\n\nWaiting for the redirect...\n", consent)

	ctx, cancel := context.WithTimeout(ctx, AuthorizeTimeout)
	defer cancel()
	var code string
	select {
	case r := <-done:
		if r.err != nil {
			return nil, r.err
		}
		code = r.code
	case <-ctx.Done():
		return nil, fmt.Errorf("gmail plugin: no redirect arrived: %v", ctx.Err())
	}

	tok, err := cfg.Exchange(context.WithoutCancel(ctx), code)
	if err != nil {
		var retrieve *oauth2.RetrieveError
		if errors.As(err, &retrieve) {
			return nil, fmt.Errorf("gmail plugin: Google refused the code: %s", retrieve.ErrorCode)
		}
		return nil, fmt.Errorf("gmail plugin: exchange: %v", err)
	}
	if tok.RefreshToken == "" {
		return nil, errors.New("gmail plugin: Google issued no refresh token; revoke Gridwell's access at https://myaccount.google.com/permissions and run -auth again")
	}
	return tok, nil
}

// RedirectURL is the loopback address the flow listens on. 127.0.0.1 and not
// localhost: the name can resolve to ::1, and Google's own guidance is the
// literal address.
func RedirectURL(ln net.Listener) string {
	return "http://127.0.0.1:" + port(ln) + "/"
}

func port(ln net.Listener) string {
	if a, ok := ln.Addr().(*net.TCPAddr); ok {
		return fmt.Sprintf("%d", a.Port)
	}
	return "0"
}

// nonce mints the flow's state value.
func nonce() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("gmail plugin: %v", err)
	}
	return hex.EncodeToString(b[:]), nil
}

// Run is the whole one-time flow, and the -auth mode's one call: read the
// client, listen on loopback, print the URL, take the redirect, write the
// token. It is here rather than in main so that main is a flag parse and this
// is testable.
func Run(ctx context.Context, credentialsPath, tokenPath string, out io.Writer) error {
	return run(ctx, credentialsPath, tokenPath, out, nil)
}

// run is Run with the one seam a test needs: Google's endpoint. Everything
// else — the listener, the state, the redirect URL, the exchange, the write —
// is the production path, so the test cannot pass over a Run that has drifted
// from it.
func run(ctx context.Context, credentialsPath, tokenPath string, out io.Writer, endpoint *oauth2.Endpoint) error {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return fmt.Errorf("gmail plugin: listen: %v", err)
	}
	defer ln.Close()
	cfg, err := Config(credentialsPath, RedirectURL(ln))
	if err != nil {
		return err
	}
	if endpoint != nil {
		cfg.Endpoint = *endpoint
	}
	tok, err := Authorize(ctx, cfg, ln, out)
	if err != nil {
		return err
	}
	if err := SaveToken(tokenPath, tok); err != nil {
		return fmt.Errorf("gmail plugin: token: %v", err)
	}
	fmt.Fprintf(out, "\nWrote %s (0600). Point the plugin's `token:` at it.\n", tokenPath)
	return nil
}
