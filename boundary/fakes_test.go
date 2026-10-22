package boundary

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
)

// The fakes stand in for each plugin's source and count what the plugin asks
// of it. They fake the service, never the plugin: the plugin under test is
// always the shipped binary.

// sourceMode is what a fake answers: the data, an outage (transport-shaped),
// or a refusal of the credential (a verdict).
type sourceMode int32

const (
	answering sourceMode = iota
	down
	refusing
)

// httpFake is the shared shape of the HTTP fakes: a mode, a request count and
// a handler for the answering mode.
type httpFake struct {
	srv    *httptest.Server
	calls  atomic.Int64
	mode   atomic.Int32
	answer func(w http.ResponseWriter, r *http.Request)
	refuse func(w http.ResponseWriter)
}

func newHTTPFake(answer func(http.ResponseWriter, *http.Request), refuse func(http.ResponseWriter)) *httpFake {
	f := &httpFake{answer: answer, refuse: refuse}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	return f
}

func (f *httpFake) serve(w http.ResponseWriter, r *http.Request) {
	f.calls.Add(1)
	switch sourceMode(f.mode.Load()) {
	case down:
		http.Error(w, "503 Service Unavailable", http.StatusServiceUnavailable)
	case refusing:
		f.refuse(w)
	default:
		f.answer(w, r)
	}
}

func (f *httpFake) set(m sourceMode) { f.mode.Store(int32(m)) }
func (f *httpFake) count() int64     { return f.calls.Load() }
func (f *httpFake) close()           { f.srv.Close() }

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// gitlabToken is the one token the GitLab fake takes.
const gitlabToken = "conformance-token"

// newGitLab fakes the three GitLab calls the plugin makes: the token check
// (GET /api/v4/user), the paged todo listing (GET /api/v4/todos) and
// mark-as-done, which the suite never asks for.
func newGitLab() *httpFake {
	todo := map[string]any{
		"id": 1, "state": "pending", "created_at": "2026-10-01T10:00:00Z",
		"action_name": "assigned", "target_type": "MergeRequest",
		"target_url": "https://gitlab.example/merge_requests/7", "body": "Conformance widget",
		"project": map[string]any{"id": 1, "name": "widget", "path_with_namespace": "ada/widget"},
		"author":  map[string]any{"name": "Ada", "username": "ada"},
		"target":  map[string]any{"iid": 7, "title": "Conformance widget", "state": "opened"},
	}
	answer := func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("PRIVATE-TOKEN") != gitlabToken {
			http.Error(w, `{"message":"401 Unauthorized"}`, http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/api/v4/user":
			writeJSON(w, map[string]any{"id": 1, "username": "ada"})
		case "/api/v4/todos":
			w.Header().Set("X-Total-Pages", "1")
			if r.URL.Query().Get("state") == "pending" {
				writeJSON(w, []any{todo})
				return
			}
			writeJSON(w, []any{})
		default:
			http.NotFound(w, r)
		}
	}
	refuse := func(w http.ResponseWriter) {
		http.Error(w, `{"message":"401 Unauthorized"}`, http.StatusUnauthorized)
	}
	return newHTTPFake(answer, refuse)
}

// gmailMessage is one message the Gmail fake holds.
type gmailMessage struct {
	id, subject string
	labels      []string
	ms          int64
}

var gmailMessages = []gmailMessage{
	{"m1", "Conformance hello", []string{"INBOX", "UNREAD"}, 1759312800000},
	{"m2", "Conformance starred", []string{"INBOX", "STARRED"}, 1759316400000},
}

// newGmail fakes the Gmail REST calls the plugin's client makes, matched by
// path suffix so the base path is the client's business: the profile, a
// label's listing, a message's metadata and the history.
func newGmail() *httpFake {
	answer := func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		switch {
		case strings.HasSuffix(p, "/token"):
			writeJSON(w, map[string]any{"access_token": "conformance", "token_type": "Bearer", "expires_in": 3600})
		case strings.HasSuffix(p, "/users/me/profile"):
			writeJSON(w, map[string]any{"emailAddress": "ada@example.com", "historyId": "100"})
		case strings.HasSuffix(p, "/users/me/history"):
			writeJSON(w, map[string]any{"historyId": "100"})
		case strings.HasSuffix(p, "/users/me/messages"):
			want := r.URL.Query()["labelIds"]
			var out []map[string]string
			for _, m := range gmailMessages {
				if hasAll(m.labels, want) {
					out = append(out, map[string]string{"id": m.id, "threadId": m.id})
				}
			}
			writeJSON(w, map[string]any{"messages": out, "resultSizeEstimate": len(out)})
		case strings.Contains(p, "/users/me/messages/"):
			id := p[strings.LastIndex(p, "/")+1:]
			for _, m := range gmailMessages {
				if m.id == id {
					writeJSON(w, map[string]any{
						"id": m.id, "threadId": m.id, "labelIds": m.labels, "snippet": m.subject,
						"internalDate": strconv.FormatInt(m.ms, 10),
						"payload": map[string]any{"mimeType": "text/plain", "headers": []map[string]string{
							{"name": "Subject", "value": m.subject},
							{"name": "From", "value": "Ada <ada@example.com>"},
						}, "body": map[string]any{"data": "aGVsbG8"}},
					})
					return
				}
			}
			gmailError(w, http.StatusNotFound, "Requested entity was not found.")
		default:
			gmailError(w, http.StatusNotFound, "no such call")
		}
	}
	refuse := func(w http.ResponseWriter) {
		gmailError(w, http.StatusUnauthorized, "Request had invalid authentication credentials.")
	}
	return newHTTPFake(answer, refuse)
}

func gmailError(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"code": code, "message": msg}})
}

func hasAll(have, want []string) bool {
	for _, w := range want {
		found := false
		for _, h := range have {
			found = found || h == w
		}
		if !found {
			return false
		}
	}
	return true
}

// gmailConfig writes the OAuth client and a token that does not expire, so
// the plugin asks Google's token endpoint nothing; the fake answers one anyway.
func gmailConfig(dir string, f *httpFake) (map[string]string, error) {
	creds := filepath.Join(dir, "credentials.json")
	tok := filepath.Join(dir, "token.json")
	c := fmt.Sprintf(`{"installed":{"client_id":"conformance","client_secret":"s","auth_uri":"%[1]s/auth","token_uri":"%[1]s/token","redirect_uris":["http://localhost"]}}`, f.srv.URL)
	t := `{"access_token":"conformance","token_type":"Bearer","refresh_token":"r","expiry":"2099-01-01T00:00:00Z"}`
	if err := os.WriteFile(creds, []byte(c), 0o600); err != nil {
		return nil, err
	}
	if err := os.WriteFile(tok, []byte(t), 0o600); err != nil {
		return nil, err
	}
	return map[string]string{"credentials": creds, "token": tok, "endpoint": f.srv.URL, "refresh": "100ms"}, nil
}

// heyCLI is a stand-in for the HEY CLI, a shell script that appends each run's
// arguments to a log and answers from the mode file: empty for the boxes, or
// an exit code with its reason on stderr. Its contract is the CLI's:
//
//	hey box view <box> --json --all     the response envelope, data.postings
//	hey thread read <topic-id> --html   a bare HTML document
//	hey watch --events <changes>        one JSON line per change, until killed
const heyCLI = `#!/bin/sh
d=$(dirname "$0")
echo "$*" >> "$d/calls"
code=$(cat "$d/mode" 2>/dev/null)
if [ -n "$code" ]; then
  echo "{\"ok\":false,\"error\":\"conformance: the source says no\"}" >&2
  exit "$code"
fi
case "$1 $2" in
"box view")
  echo '{"ok":true,"data":{"id":1,"kind":"'"$3"'","postings":[{"id":1,"topic_id":101,"kind":"topic","name":"Conformance hello","summary":"hi","seen":false,"created_at":"2026-10-01T10:00:00Z","active_at":"2026-10-01T10:00:00Z","creator":{"name":"Ada","email_address":"ada@example.com"}}]}}' ;;
"thread read")
  echo '<!doctype html><html><body><article>hi</article></body></html>' ;;
"watch --events")
  echo '{"change":"ready"}'
  exec sleep 3600 ;;
*)
  echo "unknown command" >&2; exit 1 ;;
esac
`

// heyFake is one stand-in CLI in its own directory.
type heyFake struct {
	dir string
	mu  sync.Mutex
}

func newHey(dir string) (*heyFake, error) {
	h := &heyFake{dir: dir}
	return h, h.install()
}

func (h *heyFake) binary() string { return filepath.Join(h.dir, "hey") }

func (h *heyFake) install() error {
	return os.WriteFile(h.binary(), []byte(heyCLI), 0o755)
}

// set stages the CLI's answer: the boxes, an outage (exit 6, network failed)
// or a refused sign-in (exit 3, auth required).
func (h *heyFake) set(m sourceMode) {
	h.mu.Lock()
	defer h.mu.Unlock()
	code := map[sourceMode]string{answering: "", down: "6", refusing: "3"}[m]
	_ = os.WriteFile(filepath.Join(h.dir, "mode"), []byte(code), 0o644)
}

func (h *heyFake) count() int64 {
	data, _ := os.ReadFile(filepath.Join(h.dir, "calls"))
	return int64(strings.Count(string(data), "\n"))
}
