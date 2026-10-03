package plugin

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/josephburnett/gridwell-plugins/gmail/gmailapi"
	"github.com/josephburnett/gridwell-plugins/gmail/mailbox"
	pluginv1 "github.com/josephburnett/gridwell/api/gen/plugin/v1"
	"github.com/josephburnett/gridwell/api/rpc"
)

// The seam a fake Source cannot cross: the real gmailapi.Client, speaking the
// real generated Gmail client, against the JSON shapes recorded in
// gmailapi/testdata — feeding the real memory and the real entry derivation.
// A unit test on each side of "what Gmail returns" would not catch a change
// to the shape it returns, and the delta walk, the unread listing and the key
// scheme all cross this line.
func TestOverTheRealGmailContract(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=UTF-8")
		q := r.URL.Query()
		switch {
		case r.URL.Path == "/gmail/v1/users/me/profile":
			w.Write(contract(t, "profile.json"))
		case r.URL.Path == "/gmail/v1/users/me/messages":
			switch strings.Join(q["labelIds"], "+") {
			case "INBOX":
				if q.Get("pageToken") != "" {
					w.Write(contract(t, "messages-list-inbox-page2.json"))
					return
				}
				w.Write(contract(t, "messages-list-inbox.json"))
			case "INBOX+UNREAD":
				w.Write([]byte(`{"messages":[{"id":"18c2a1b3f4d5e6f7"}],"resultSizeEstimate":1}`))
			default: // STARRED, and STARRED+UNREAD
				w.Write(contract(t, "messages-list-empty.json"))
			}
		case strings.HasPrefix(r.URL.Path, "/gmail/v1/users/me/messages/"):
			id := strings.TrimPrefix(r.URL.Path, "/gmail/v1/users/me/messages/")
			if q.Get("format") == "metadata" {
				// The recorded shape, with this message's id: the contract is
				// the SHAPE, and three messages must not collapse into one.
				w.Write(withID(t, "message-metadata.json", id))
				return
			}
			w.Write(withID(t, "message-full-html.json", id))
		default:
			w.WriteHeader(404)
			w.Write([]byte(`{"error":{"code":404,"message":"not found"}}`))
		}
	}))
	defer srv.Close()

	src, err := gmailapi.NewForTest(context.Background(), srv.URL, srv.Client())
	if err != nil {
		t.Fatal(err)
	}
	p := stable(src, Options{StateDir: t.TempDir()})
	ctx := context.Background()

	resp, err := p.List(ctx, &pluginv1.ListRequest{Context: mailbox.InboxContext})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	// Two pages of ids, both walked.
	if len(resp.Entries) != 3 {
		t.Fatalf("entries = %+v", resp.Entries)
	}
	e := resp.Entries[0]
	if !strings.HasPrefix(e.Key, mailbox.KeyPrefix) || !e.ServesPage || e.Kind != rpc.KindURL {
		t.Fatalf("entry = %+v", e)
	}
	if e.Label != "Lunch plans" {
		t.Errorf("label = %q, want the subject", e.Label)
	}
	// The unread listing is what marks it, and only the one id it named.
	marked := 0
	for _, e := range resp.Entries {
		if e.StatusDetail == mailbox.UnreadMark {
			marked++
		}
	}
	if marked != 1 {
		t.Errorf("%d of 3 entries read as unread; the INBOX+UNREAD listing named one", marked)
	}

	s := &server{}
	if err := p.ServeContent(&pluginv1.ServeContentRequest{Key: "msg:18c2a1b3f4d5e6f7"}, s); err != nil {
		t.Fatalf("ServeContent: %v", err)
	}
	if len(s.chunks) != 1 || s.chunks[0].Status != 200 {
		t.Fatalf("chunks = %+v", s.chunks)
	}
	if got := string(s.chunks[0].Data); !strings.Contains(got, "<b>friday</b>") {
		t.Errorf("the email did not arrive: %q", got)
	}
	if got := s.chunks[0].MediaType; !strings.HasPrefix(got, "text/html") {
		t.Errorf("media type = %q", got)
	}

	// Both collections read, so absence is now an answer.
	if _, err := p.List(ctx, &pluginv1.ListRequest{Context: mailbox.StarredContext}); err != nil {
		t.Fatalf("starred: %v", err)
	}
	got, _ := p.Probe(ctx, &pluginv1.ProbeRequest{Key: "msg:ffffffffffffffff"})
	if got.Presence != pluginv1.ProbeResponse_PRESENCE_GONE {
		t.Fatalf("a swept probe = %v", got.Presence)
	}
}

// contract reads one of the recorded Gmail responses. They live beside the
// client that parses them, so there is one copy of the contract.
func contract(t *testing.T, name string) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "gmailapi", "testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// withID is a recorded response re-addressed to one message.
func withID(t *testing.T, name, id string) []byte {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(contract(t, name), &m); err != nil {
		t.Fatal(err)
	}
	m["id"] = id
	m["threadId"] = id
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// A catch-up over the real contract: the recorded history, read by the real
// client, applied to the real memory. The recorded pages name an arrival in
// the inbox, the first inbox message archived and read, a label change and
// an arrival this projection cannot see, and the third inbox message deleted.
func TestACatchUpOverTheRealGmailContract(t *testing.T) {
	// labels is what each message's metadata answers it carries now.
	labels := map[string][]string{
		"18c2a1b3f4d5e7a1": {"UNREAD", "INBOX"},
		"18c2a1b3f4d5e6f7": {"IMPORTANT"},
	}
	var historyAsked string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=UTF-8")
		q := r.URL.Query()
		switch {
		case r.URL.Path == "/gmail/v1/users/me/profile":
			w.Write(contract(t, "profile.json"))
		case r.URL.Path == "/gmail/v1/users/me/history":
			if q.Get("pageToken") != "" {
				w.Write(contract(t, "history-list-page2.json"))
				return
			}
			historyAsked = q.Get("startHistoryId")
			w.Write(contract(t, "history-list.json"))
		case r.URL.Path == "/gmail/v1/users/me/messages":
			switch strings.Join(q["labelIds"], "+") {
			case "INBOX":
				if q.Get("pageToken") != "" {
					w.Write(contract(t, "messages-list-inbox-page2.json"))
					return
				}
				w.Write(contract(t, "messages-list-inbox.json"))
			default:
				w.Write(contract(t, "messages-list-empty.json"))
			}
		case strings.HasPrefix(r.URL.Path, "/gmail/v1/users/me/messages/"):
			id := strings.TrimPrefix(r.URL.Path, "/gmail/v1/users/me/messages/")
			var m map[string]any
			if err := json.Unmarshal(withID(t, "message-metadata.json", id), &m); err != nil {
				t.Error(err)
			}
			if l, ok := labels[id]; ok {
				m["labelIds"] = l
			}
			raw, _ := json.Marshal(m)
			w.Write(raw)
		default:
			w.WriteHeader(404)
			w.Write([]byte(`{"error":{"code":404,"message":"not found"}}`))
		}
	}))
	defer srv.Close()

	src, err := gmailapi.NewForTest(context.Background(), srv.URL, srv.Client())
	if err != nil {
		t.Fatal(err)
	}
	clock := at("2026-01-06T12:00:00Z")
	p := stable(src, Options{Refresh: time.Minute, Now: func() time.Time { return clock }})
	listAll(t, p)
	if got := keys(t, p, mailbox.InboxContext); len(strings.Split(got, ",")) != 3 {
		t.Fatalf("inbox after the walk = %s", got)
	}

	refreshed(t, p, &clock, 2*time.Minute)
	if historyAsked != "9912345" {
		t.Errorf("history asked from %q, want the profile's id", historyAsked)
	}
	got := strings.Split(keys(t, p, mailbox.InboxContext), ",")
	slices.Sort(got)
	if want := []string{"msg:18c2a1b3f4d5e6f8", "msg:18c2a1b3f4d5e7a1"}; !slices.Equal(got, want) {
		t.Errorf("inbox after the catch-up = %v, want %v", got, want)
	}
	if id := p.mem.HistoryID(); id != 9912431 {
		t.Errorf("history id = %d, want the last page's", id)
	}
}
