// Package gmailapi is the Gmail half of the gmail plugin: it reads one
// account through Gmail's REST API (google.golang.org/api/gmail/v1) and turns
// what comes back into mailbox.Message records and email HTML. It holds no
// credential of its own — it is handed a token source built from the token
// file the one-time auth flow wrote — and it never writes to the account.
//
// The API surface it is built against, and nothing else:
//
//	GET users/me/messages?labelIds=…&maxResults=…&pageToken=…
//	  the ids one label holds, newest first, plus a nextPageToken
//	GET users/me/messages/<id>?format=metadata&metadataHeaders=Subject,From,Date
//	  one message's headers, snippet and internalDate
//	GET users/me/messages/<id>?format=full
//	  one message's MIME tree, from which the HTML body is taken
//
// testdata/ records the exact JSON each of those answers with, and
// client_test.go serves it from an httptest server the real Gmail client is
// pointed at. That is the contract: if Google changes a shape the plugin
// reads, it shows there.
package gmailapi

import (
	"context"
	"encoding/base64"
	"errors"
	"mime"
	"net/http"
	"net/mail"
	"strings"
	"time"

	"golang.org/x/oauth2"
	"google.golang.org/api/gmail/v1"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/option"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/josephburnett/gridwell-plugins/gmail/mailbox"
)

// User is the authenticated user, which is the only mailbox this plugin ever
// reads. Gmail's own name for "whoever the token belongs to".
const User = "me"

// PageSize is how many ids one messages.list call asks for (Gmail's maximum).
const PageSize = 500

// DefaultTimeout bounds one API call end to end. An http client with no
// timeout and a Gmail response that stalls mid-body would park the walk
// forever — the plugin's shared flight then holds every reader on that one
// hung request, and the grid says "loading" for the life of the process with
// no error to surface. A timeout turns the stall into Unavailable, "not right
// now", and the node serves its remembered listing.
const DefaultTimeout = 60 * time.Second

// MetadataHeaders are the only headers this plugin reads. Asking for three
// rather than all of them is what keeps a metadata read small.
var MetadataHeaders = []string{"Subject", "From", "Date"}

// Client reads one Gmail account.
type Client struct{ svc *gmail.Service }

// New builds a client over a token source. Nothing here refreshes or persists
// the token: the source does both, and it is the one owner of the token file.
// The http client is built here rather than left to the API package's own,
// because that one carries no timeout — see DefaultTimeout.
func New(ctx context.Context, ts oauth2.TokenSource) (*Client, error) {
	hc := oauth2.NewClient(ctx, ts)
	hc.Timeout = DefaultTimeout
	return newService(ctx, option.WithHTTPClient(hc))
}

// newService is the one place a gmail.Service is built: New hands it a
// credentialed http client, and NewForTest an endpoint and a bare one.
func newService(ctx context.Context, opts ...option.ClientOption) (*Client, error) {
	svc, err := gmail.NewService(ctx, opts...)
	if err != nil {
		return nil, status.Errorf(codes.FailedPrecondition, "gmail plugin: %v", err)
	}
	return &Client{svc: svc}, nil
}

// NewForTest builds a client against an alternate endpoint with no
// credentials, which is how the contract tests reach a fake Gmail. It is
// exported so the plugin package's own seam test can use the same door.
func NewForTest(ctx context.Context, endpoint string, hc *http.Client) (*Client, error) {
	// The generated client resolves its path RELATIVE to the base, so an
	// endpoint with no trailing slash would lose its last segment.
	if !strings.HasSuffix(endpoint, "/") {
		endpoint += "/"
	}
	return newService(ctx, option.WithEndpoint(endpoint), option.WithHTTPClient(hc))
}

// Label lists the ids one label holds, newest first — Gmail's own order —
// stopping at limit. whole reports that the read reached the end of the
// label; when it is false the caller has the newest `limit` and knows nothing
// about what is older, which is exactly what mailbox.Memory's watermark
// needs.
//
// labelIDs are ANDed by Gmail, so ["INBOX","UNREAD"] is the unread part of
// the inbox: one cheap call, and how the unread mark stays true for a message
// whose metadata was read months ago.
func (c *Client) Label(ctx context.Context, labelIDs []string, limit int) (ids []string, whole bool, err error) {
	token := ""
	for {
		page := int64(PageSize)
		if left := int64(limit - len(ids)); left < page {
			page = left
		}
		if page <= 0 {
			return ids, false, nil
		}
		call := c.svc.Users.Messages.List(User).LabelIds(labelIDs...).MaxResults(page).Context(ctx)
		if token != "" {
			call = call.PageToken(token)
		}
		resp, err := call.Do()
		if err != nil {
			return nil, false, wrap(ctx, "list "+strings.Join(labelIDs, "+"), err)
		}
		for _, m := range resp.Messages {
			if m.Id != "" {
				ids = append(ids, m.Id)
			}
		}
		token = resp.NextPageToken
		if token == "" {
			return ids, true, nil
		}
		if len(ids) >= limit {
			return ids, false, nil
		}
	}
}

// Headers reads one message's metadata: what a tile is labelled and placed
// by. It asks for three headers rather than the whole message, so a walk that
// fetches a hundred new messages moves kilobytes and not megabytes.
func (c *Client) Headers(ctx context.Context, id string) (mailbox.Message, error) {
	m, err := c.svc.Users.Messages.Get(User, id).
		Format("metadata").MetadataHeaders(MetadataHeaders...).Context(ctx).Do()
	if err != nil {
		return mailbox.Message{}, wrap(ctx, "message "+id, err)
	}
	return record(m), nil
}

// record turns Gmail's message into the plugin's, reading only the facts that
// do not change. internalDate is the one date to trust: Gmail sets it when it
// receives the message, in UTC epoch milliseconds, while a Date header is
// whatever the sender's clock said. The header is the fallback for a message
// with no internalDate, which is better than a tile placed in 1970.
func record(m *gmail.Message) mailbox.Message {
	out := mailbox.Message{ID: m.Id, ThreadID: m.ThreadId, Snippet: m.Snippet}
	var dateHeader string
	if m.Payload != nil {
		for _, h := range m.Payload.Headers {
			switch strings.ToLower(h.Name) {
			case "subject":
				out.Subject = h.Value
			case "from":
				out.FromName, out.FromEmail = mailbox.ParseFrom(h.Value)
			case "date":
				dateHeader = h.Value
			}
		}
	}
	switch {
	case m.InternalDate > 0:
		out.Date = time.UnixMilli(m.InternalDate).UTC()
	case dateHeader != "":
		if t, err := mail.ParseDate(dateHeader); err == nil {
			out.Date = t.UTC()
		}
	}
	return out
}

// HTML reads one message as the HTML the sender wrote: format=full, then the
// first text/html part of the MIME tree. A message with no HTML part — a
// plain-text mail, which many still are — is wrapped rather than refused, so
// descending into it shows the mail instead of an error.
//
// Inline images the sender referenced as `cid:` URLs do NOT resolve: they are
// attachments, and this plugin serves no attachments. The image is broken in
// the page and everything else reads.
func (c *Client) HTML(ctx context.Context, id string) (body []byte, mediaType string, err error) {
	m, err := c.svc.Users.Messages.Get(User, id).Format("full").Context(ctx).Do()
	if err != nil {
		return nil, "", wrap(ctx, "message "+id, err)
	}
	if part := findPart(m.Payload, "text/html"); part != nil {
		data, err := decodeBody(part.Body)
		if err != nil {
			return nil, "", status.Errorf(codes.Internal, "gmail plugin: message %s: html body: %v", id, err)
		}
		if len(data) > 0 {
			return data, htmlMediaType(part), nil
		}
	}
	if part := findPart(m.Payload, "text/plain"); part != nil {
		data, err := decodeBody(part.Body)
		if err != nil {
			return nil, "", status.Errorf(codes.Internal, "gmail plugin: message %s: text body: %v", id, err)
		}
		if len(data) > 0 {
			rec := record(m)
			return mailbox.WrapPlain(rec.Title(), data), "text/html; charset=utf-8", nil
		}
	}
	return nil, "", nil // no body at all; the caller says so on the page
}

// findPart walks the MIME tree depth-first for the first part of this type. A
// multipart/alternative puts the richest form last, but the tree is searched
// in order and the caller asks for text/html first, so the answer is the same
// either way and a nested multipart/related (the shape a mail with inline
// images has) is reached.
func findPart(p *gmail.MessagePart, mimeType string) *gmail.MessagePart {
	if p == nil {
		return nil
	}
	if strings.EqualFold(baseType(p.MimeType), mimeType) && p.Body != nil && p.Body.Data != "" {
		return p
	}
	for _, sub := range p.Parts {
		if found := findPart(sub, mimeType); found != nil {
			return found
		}
	}
	return nil
}

func baseType(mimeType string) string {
	base, _, _ := strings.Cut(mimeType, ";")
	return strings.TrimSpace(base)
}

// htmlMediaType keeps the charset the part declared. The part's `mimeType`
// field is the bare type — Gmail strips the parameters off it — so the
// charset is read from the part's own Content-Type header, where the sender
// wrote it. Transcoding is not this plugin's job: a part that says it is
// ISO-8859-1 is served as such and the browser decodes it. Anything with no
// charset to read falls back to UTF-8, which is what all but the oldest mail
// is.
func htmlMediaType(p *gmail.MessagePart) string {
	for _, h := range p.Headers {
		if !strings.EqualFold(h.Name, "Content-Type") {
			continue
		}
		if _, params, err := mime.ParseMediaType(h.Value); err == nil {
			if cs := strings.TrimSpace(params["charset"]); cs != "" {
				return "text/html; charset=" + cs
			}
		}
	}
	return "text/html; charset=utf-8"
}

// decodeBody decodes a message part's body. Gmail encodes it base64url, and
// pads it or not depending on the part, so the padding is stripped and the
// unpadded decoder used for both.
func decodeBody(b *gmail.MessagePartBody) ([]byte, error) {
	if b == nil || b.Data == "" {
		return nil, nil
	}
	return base64.RawURLEncoding.DecodeString(strings.TrimRight(b.Data, "="))
}

// wrap maps a Gmail failure onto the node's error vocabulary. Transport-shaped
// codes mean "not right now", and the node serves what it has, stamped stale.
// The rest are verdicts and surface: a plugin that answered "not right now" to
// "this token has been revoked" would leave the user staring at an empty grid
// with nothing said.
func wrap(ctx context.Context, what string, err error) error {
	if ctx.Err() != nil {
		return status.Errorf(codes.Unavailable, "gmail plugin: %s: %v", what, err)
	}
	// A refresh that Google refuses is a verdict about the token — revoked,
	// expired, or for the wrong client — and the fix is running the auth flow
	// again, so it must reach the user rather than degrade to a stale grid.
	var retrieve *oauth2.RetrieveError
	if errors.As(err, &retrieve) {
		return status.Errorf(codes.PermissionDenied,
			"gmail plugin: %s: the stored token was refused (%s); re-run gridwell-plugin-gmail -auth", what, retrieve.ErrorCode)
	}
	var api *googleapi.Error
	if errors.As(err, &api) {
		switch {
		case api.Code == http.StatusUnauthorized || api.Code == http.StatusForbidden:
			return status.Errorf(codes.PermissionDenied,
				"gmail plugin: %s: %s (the token needs the gmail.readonly scope)", what, api.Message)
		case api.Code == http.StatusNotFound:
			return status.Errorf(codes.NotFound, "gmail plugin: %s: %s", what, api.Message)
		case api.Code == http.StatusTooManyRequests || api.Code >= 500:
			return status.Errorf(codes.Unavailable, "gmail plugin: %s: %s", what, api.Message)
		default:
			return status.Errorf(codes.Internal, "gmail plugin: %s: %d %s", what, api.Code, api.Message)
		}
	}
	// Anything else reached no verdict: a refused connection, a DNS failure, a
	// body that stopped mid-read.
	return status.Errorf(codes.Unavailable, "gmail plugin: %s: %v", what, err)
}
