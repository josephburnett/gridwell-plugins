package gmailapi

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"google.golang.org/api/gmail/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// fakeGmail is the Gmail contract, served: the exact JSON the API answers
// with, recorded under testdata/ and handed back by path and query. The real
// generated client is pointed at it, so the URLs, the query parameters, the
// status codes and every JSON shape this plugin reads are exercised for real
// — a unit test on each side of "what Gmail returns" would not catch a change
// to what it returns.
type fakeGmail struct {
	t    *testing.T
	reqs []*http.Request
	srv  *httptest.Server
	// status, when non-zero, is answered for every request instead of data.
	status int
	body   string
}

func newFake(t *testing.T) *fakeGmail {
	t.Helper()
	f := &fakeGmail{t: t}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeGmail) serve(w http.ResponseWriter, r *http.Request) {
	f.reqs = append(f.reqs, r)
	w.Header().Set("Content-Type", "application/json; charset=UTF-8")
	if f.status != 0 {
		w.WriteHeader(f.status)
		switch {
		case f.body != "":
			w.Write([]byte(f.body))
		case f.status == 401:
			// The recorded shape, verbatim: Gmail's error envelope carries the
			// code in the BODY, which is what googleapi reads.
			w.Write(golden(f.t, "error-401.json"))
		default:
			fmt.Fprintf(w, `{"error":{"code":%d,"message":"Request had invalid authentication credentials.","status":"ERROR"}}`, f.status)
		}
		return
	}
	q := r.URL.Query()
	switch {
	case r.URL.Path == "/gmail/v1/users/me/messages":
		switch {
		case strings.Join(q["labelIds"], "+") == "INBOX+UNREAD":
			// The unread part of the inbox: Gmail ANDs the label ids.
			w.Write([]byte(`{"messages":[{"id":"18c2a1b3f4d5e6f7"}],"resultSizeEstimate":1}`))
		case q.Get("pageToken") != "":
			w.Write(golden(f.t, "messages-list-inbox-page2.json"))
		case strings.Join(q["labelIds"], "+") == "STARRED":
			w.Write(golden(f.t, "messages-list-empty.json"))
		default:
			w.Write(golden(f.t, "messages-list-inbox.json"))
		}
	case strings.HasPrefix(r.URL.Path, "/gmail/v1/users/me/messages/"):
		id := strings.TrimPrefix(r.URL.Path, "/gmail/v1/users/me/messages/")
		if q.Get("format") == "metadata" {
			w.Write(golden(f.t, "message-metadata.json"))
			return
		}
		switch id {
		case "18c2a1b3f4d5e6f8":
			w.Write(golden(f.t, "message-full-plain.json"))
		case "18c2a1b3f4d5e6f9":
			w.Write(golden(f.t, "message-full-nobody.json"))
		default:
			w.Write(golden(f.t, "message-full-html.json"))
		}
	default:
		w.WriteHeader(404)
		w.Write([]byte(`{"error":{"code":404,"message":"not found"}}`))
	}
}

func (f *fakeGmail) client() *Client {
	f.t.Helper()
	c, err := NewForTest(context.Background(), f.srv.URL, f.srv.Client())
	if err != nil {
		f.t.Fatal(err)
	}
	return c
}

func (f *fakeGmail) lastQuery() url.Values {
	f.t.Helper()
	if len(f.reqs) == 0 {
		f.t.Fatal("no request was made")
	}
	return f.reqs[len(f.reqs)-1].URL.Query()
}

func golden(t *testing.T, name string) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// The listing is what a walk's membership is built from: the ids, in Gmail's
// order, and whether the read reached the end.
func TestLabelPagesAndPinsTheRequest(t *testing.T) {
	f := newFake(t)
	ids, whole, err := f.client().Label(context.Background(), []string{"INBOX"}, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 3 || ids[0] != "18c2a1b3f4d5e6f7" || ids[2] != "18c2a1b3f4d5e6f9" {
		t.Fatalf("ids = %v", ids)
	}
	if !whole {
		t.Error("a listing that ran out of pages did not read as whole")
	}
	if len(f.reqs) != 2 {
		t.Fatalf("%d requests for two pages", len(f.reqs))
	}
	if got := f.reqs[0].URL.Path; got != "/gmail/v1/users/me/messages" {
		t.Errorf("path = %q", got)
	}
	if got := f.reqs[0].URL.Query(); got.Get("labelIds") != "INBOX" || got.Get("maxResults") == "" {
		t.Errorf("query = %v", got)
	}
	if got := f.reqs[1].URL.Query().Get("pageToken"); got != "07123456789012345678" {
		t.Errorf("page 2 token = %q", got)
	}
}

// A read that hits the caller's limit is NOT whole: what came back is the
// newest N and nothing is known about what is older. Reading that as a
// complete membership would retire every older message's tile.
func TestLabelStopsAtTheLimitAndSaysItIsNotWhole(t *testing.T) {
	f := newFake(t)
	ids, whole, err := f.client().Label(context.Background(), []string{"INBOX"}, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 2 {
		t.Fatalf("ids = %v", ids)
	}
	if whole {
		t.Fatal("a capped read said it reached the end of the label")
	}
	if len(f.reqs) != 1 {
		t.Errorf("a capped read made %d requests", len(f.reqs))
	}
	if got := f.lastQuery().Get("maxResults"); got != "2" {
		t.Errorf("maxResults = %q; the limit must bound the page, not just the loop", got)
	}
}

// The unread mark comes from one extra cheap listing: Gmail ANDs the label
// ids, so INBOX+UNREAD is the unread part of the inbox and no message needs
// its metadata re-read to stay honest.
func TestLabelIntersectsLabelIDs(t *testing.T) {
	f := newFake(t)
	ids, whole, err := f.client().Label(context.Background(), []string{"INBOX", "UNREAD"}, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 1 || ids[0] != "18c2a1b3f4d5e6f7" || !whole {
		t.Fatalf("ids = %v whole = %v", ids, whole)
	}
	if got := f.lastQuery()["labelIds"]; len(got) != 2 || got[0] != "INBOX" || got[1] != "UNREAD" {
		t.Errorf("labelIds = %v; both must ride the query", got)
	}
}

// An empty label is an answer, not a failure.
func TestAnEmptyLabelIsWhole(t *testing.T) {
	f := newFake(t)
	ids, whole, err := f.client().Label(context.Background(), []string{"STARRED"}, 100)
	if err != nil || len(ids) != 0 || !whole {
		t.Fatalf("empty label = %v %v %v", ids, whole, err)
	}
}

// Metadata is what a tile is labelled and placed by. internalDate is the date
// to trust — Gmail's own receipt time — and only three headers are asked for,
// which is what keeps a hundred-message delta small.
func TestHeadersReadTheRecordAndPinTheRequest(t *testing.T) {
	f := newFake(t)
	m, err := f.client().Headers(context.Background(), "18c2a1b3f4d5e6f7")
	if err != nil {
		t.Fatal(err)
	}
	if m.ID != "18c2a1b3f4d5e6f7" || m.ThreadID != "18c2a1b3f4d5e6f7" {
		t.Errorf("ids = %q/%q", m.ID, m.ThreadID)
	}
	if m.Subject != "Lunch plans" || m.FromName != "Alice Example" || m.FromEmail != "alice@example.com" {
		t.Errorf("record = %+v", m)
	}
	if got := m.Date.UTC().Format(time.RFC3339); got != "2026-01-05T14:03:00Z" {
		t.Errorf("date = %s; internalDate is epoch MILLISECONDS", got)
	}
	if !strings.Contains(m.Snippet, "free friday") {
		t.Errorf("snippet = %q", m.Snippet)
	}
	q := f.lastQuery()
	if q.Get("format") != "metadata" {
		t.Errorf("format = %q", q.Get("format"))
	}
	if got := q["metadataHeaders"]; len(got) != 3 {
		t.Errorf("metadataHeaders = %v; asking for all of them is what makes a delta expensive", got)
	}
}

// The page is the email: the sender's own HTML, out of the MIME tree, with
// the charset the part declared.
func TestHTMLTakesTheHTMLPart(t *testing.T) {
	f := newFake(t)
	body, media, err := f.client().HTML(context.Background(), "18c2a1b3f4d5e6f7")
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != `<div dir="ltr">Are you free <b>friday</b>?</div>` {
		t.Fatalf("body = %q", body)
	}
	if media != "text/html; charset=UTF-8" {
		t.Errorf("media type = %q", media)
	}
	if got := f.lastQuery().Get("format"); got != "full" {
		t.Errorf("format = %q", got)
	}
}

// Many mails are still plain text. They are wrapped, not refused — and
// escaped, so text that happens to contain markup is read as the text it is.
func TestHTMLWrapsAPlainTextMessage(t *testing.T) {
	f := newFake(t)
	body, media, err := f.client().HTML(context.Background(), "18c2a1b3f4d5e6f8")
	if err != nil {
		t.Fatal(err)
	}
	got := string(body)
	if !strings.HasPrefix(got, "<!doctype html>") || media != "text/html; charset=utf-8" {
		t.Fatalf("wrapped = %q (%s)", got, media)
	}
	if strings.Contains(got, "<b>no</b>") || !strings.Contains(got, "&lt;b&gt;no&lt;/b&gt;") {
		t.Errorf("plain text reached the page as markup:\n%s", got)
	}
	if !strings.Contains(got, "Build &lt;failed&gt;") {
		t.Errorf("the subject did not title the page:\n%s", got)
	}
}

// A message whose only parts are attachments has no body. That is not an
// error and not an empty page: the caller says so.
func TestHTMLAnswersNothingForAMessageWithNoTextPart(t *testing.T) {
	f := newFake(t)
	body, media, err := f.client().HTML(context.Background(), "18c2a1b3f4d5e6f9")
	if err != nil {
		t.Fatal(err)
	}
	if len(body) != 0 || media != "" {
		t.Fatalf("body = %q (%s)", body, media)
	}
}

// The failure vocabulary: weather degrades to the remembered grid, a verdict
// surfaces. Getting this backwards either hides "this token was revoked" or
// throws the grid away over a dropped packet.
func TestFailuresMapToTheNodesVocabulary(t *testing.T) {
	cases := []struct {
		status int
		want   codes.Code
	}{
		{401, codes.PermissionDenied},
		{403, codes.PermissionDenied},
		{404, codes.NotFound},
		{429, codes.Unavailable},
		{500, codes.Unavailable},
		{503, codes.Unavailable},
		{400, codes.Internal},
	}
	for _, c := range cases {
		f := newFake(t)
		f.status = c.status
		_, _, err := f.client().Label(context.Background(), []string{"INBOX"}, 10)
		if got := status.Code(err); got != c.want {
			t.Errorf("HTTP %d = %v, want %v (%v)", c.status, got, c.want, err)
		}
	}
	// The reason travels: a plugin's error is read by a person looking at a
	// grid that will not fill.
	f := newFake(t)
	f.status = 401
	_, _, err := f.client().Label(context.Background(), []string{"INBOX"}, 10)
	if !strings.Contains(err.Error(), "invalid authentication credentials") || !strings.Contains(err.Error(), "gmail.readonly") {
		t.Errorf("the reason was lost: %v", err)
	}
}

// A Gmail that is not there at all is weather, not a verdict.
func TestAnUnreachableGmailIsUnavailable(t *testing.T) {
	f := newFake(t)
	c := f.client()
	f.srv.Close()
	if _, _, err := c.Label(context.Background(), []string{"INBOX"}, 10); status.Code(err) != codes.Unavailable {
		t.Fatalf("a refused connection = %v, want Unavailable", err)
	}
}

// A response that stalls mid-body must not wedge the walk: without a timeout
// the plugin's shared flight holds every reader on that one hung request and
// the grid says "loading" for the life of the process.
func TestAStalledResponseIsUnavailable(t *testing.T) {
	stall := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(2 * time.Second)
	}))
	defer stall.Close()
	hc := stall.Client()
	hc.Timeout = 50 * time.Millisecond
	c, err := NewForTest(context.Background(), stall.URL, hc)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := c.Label(context.Background(), []string{"INBOX"}, 10); status.Code(err) != codes.Unavailable {
		t.Fatalf("a stalled read = %v, want Unavailable", err)
	}
}

// A cancelled read is weather, whatever it failed with.
func TestACancelledReadIsWeather(t *testing.T) {
	f := newFake(t)
	f.status = 401 // a verdict, which the cancellation must outrank
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := f.client().Label(ctx, []string{"INBOX"}, 10); status.Code(err) != codes.Unavailable {
		t.Fatalf("a cancelled read = %v, want Unavailable", err)
	}
}

// Gmail pads its base64url bodies or does not, depending on the part.
func TestDecodeBodyTakesPaddedAndUnpadded(t *testing.T) {
	for _, in := range []string{"QXJlIHlvdSBmcmVlIGZyaWRheT8", "QXJlIHlvdSBmcmVlIGZyaWRheT8="} {
		got, err := decodeBody(&gmail.MessagePartBody{Data: in})
		if err != nil || string(got) != "Are you free friday?" {
			t.Errorf("decode(%q) = %q, %v", in, got, err)
		}
	}
	if got, err := decodeBody(nil); got != nil || err != nil {
		t.Errorf("decode(nil) = %q, %v", got, err)
	}
}

// The production client carries a timeout of its own: the API package's
// default http client has none.
func TestTheProductionClientHasATimeout(t *testing.T) {
	if DefaultTimeout <= 0 {
		t.Fatal("a Gmail read with no deadline wedges the walk forever")
	}
}
