package plugin

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

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
	if !strings.HasPrefix(e.Key, mailbox.KeyPrefix) || !e.ServesPage || e.Kind != rpc.KindText {
		t.Fatalf("entry = %+v", e)
	}
	if !strings.Contains(e.Label, "Alice Example") || !strings.Contains(e.Label, "Lunch plans") {
		t.Errorf("label = %q", e.Label)
	}
	// The unread listing is what marks it, and only the one id it named.
	marked := 0
	for _, e := range resp.Entries {
		if strings.HasPrefix(e.Label, mailbox.UnreadMark) {
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
