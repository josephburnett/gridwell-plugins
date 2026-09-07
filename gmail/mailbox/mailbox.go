// Package mailbox is the pure half of the gmail plugin: the message record,
// the two Gmail labels it projects, the keys, labels and placement hints
// derived from them, the memory of every message seen, and the disposable
// cache file that memory rewarms itself from. There is no HTTP and no gRPC
// here, so everything is unit-tested against fakes, and the plugin package
// only wires it to the wire.
package mailbox

import (
	"net/mail"
	"strings"
	"time"
)

// Message is one email as Gmail's metadata describes it: the facts that do
// not change once the message exists. What CAN change — read/unread, starred,
// which label holds it — is not here: it belongs to Memory, which every walk
// refreshes, so one fact has one owner and a tile's body does not flip with
// the order the walks happened to run in.
//
// The HTML body is not here either. It is fetched on descent and never
// remembered, because a mailbox listing is small and a mailbox's bodies are
// not.
type Message struct {
	// ID is Gmail's message id: the one fact the plugin's key is built from,
	// and what messages.get takes.
	ID string `json:"id"`
	// ThreadID is the conversation the message belongs to. It is remembered
	// for diagnosis and never keyed on: this plugin projects messages, and a
	// thread key would put one tile on the grid for many emails.
	ThreadID  string    `json:"threadId,omitempty"`
	Subject   string    `json:"subject"`
	Snippet   string    `json:"snippet"`
	FromName  string    `json:"fromName"`
	FromEmail string    `json:"fromEmail"`
	Date      time.Time `json:"date"`
}

// View is a message as one grid shows it: the record, plus the state Memory
// owns. Nothing derives a label, a card or an entry from a bare Message, so
// the mutable half can never be read from a stale copy.
type View struct {
	Message
	Unread  bool
	Starred bool
}

// Collection is one of the two Gmail labels this plugin projects. Key is the
// plugin's context key, stable forever. LabelIDs are the label ids Gmail's
// messages.list takes. Label is the face of the (+) menu row that opens it.
type Collection struct {
	Key      string
	LabelIDs []string
	Label    string
}

// The context keys. They embed Gmail's own label id rather than a display
// name, because a display name is Gmail's to translate and a key is forever.
const (
	InboxContext   = "label:INBOX"
	StarredContext = "label:STARRED"
)

// UnreadLabel is Gmail's label id for an unread message. It is not a
// collection — nobody wants an "unread" grid — but every walk asks for its
// intersection with the collection it is walking, which is how the unread
// mark stays true for a message whose metadata was fetched long ago.
const UnreadLabel = "UNREAD"

// StarredLabel is Gmail's label id for a starred message.
const StarredLabel = "STARRED"

// Collections is the projection, in the order the (+) menu offers it.
// InboxContext is first because it is the collection to read first.
var Collections = []Collection{
	{Key: InboxContext, LabelIDs: []string{"INBOX"}, Label: "inbox"},
	{Key: StarredContext, LabelIDs: []string{StarredLabel}, Label: "starred"},
}

// LookupCollection resolves a context key to its collection.
func LookupCollection(key string) (Collection, bool) {
	for _, c := range Collections {
		if c.Key == key {
			return c, true
		}
	}
	return Collection{}, false
}

// KeyPrefix namespaces message keys. A key names the same email for the life
// of the plugin, whichever label holds it: a message starred out of the inbox
// keeps its key, so the node keeps its id and every link to it still
// resolves.
const KeyPrefix = "msg:"

// Key is the plugin key of the message with this Gmail id.
func Key(id string) string { return KeyPrefix + id }

// Key is the message's plugin key.
func (m *Message) Key() string { return Key(m.ID) }

// ParseKey resolves a message key to its Gmail id. Gmail ids are opaque
// lowercase hex, but the check is a shape and not a claim about the id: it
// rejects anything that could not be one — an empty id, or one carrying a
// path separator, which would arrive at ServeContent looking like a subpath.
func ParseKey(key string) (string, bool) {
	id, ok := strings.CutPrefix(key, KeyPrefix)
	if !ok || id == "" {
		return "", false
	}
	for _, r := range id {
		switch {
		case r >= '0' && r <= '9', r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r == '-', r == '_':
		default:
			return "", false
		}
	}
	return id, true
}

// NoSubject stands in for a message Gmail named nothing: a subject is what a
// tile is read by, and a blank banner is worse than saying it is blank.
const NoSubject = "(no subject)"

// Title is the message's headline: its subject, else the first line of the
// snippet, else NoSubject.
func (m *Message) Title() string {
	if s := strings.TrimSpace(m.Subject); s != "" {
		return s
	}
	if line, _, _ := strings.Cut(strings.TrimSpace(m.Snippet), "\n"); line != "" {
		return line
	}
	return NoSubject
}

// From is who the message is from: the sender's name, else their address.
func (m *Message) From() string {
	if n := strings.TrimSpace(m.FromName); n != "" {
		return n
	}
	return strings.TrimSpace(m.FromEmail)
}

// UnreadMark is the unread message's banner glyph, and StarMark the starred
// one. They lead the label, so state reads from a zoomed-out grid where the
// text does not.
const (
	UnreadMark = "●"
	StarMark   = "★"
)

// Label is the tile's banner: the state marks, the sender, and the subject.
// It is the same string wherever the message appears, so the inbox and the
// starred grid never disagree about one email.
func (v View) Label() string {
	var b strings.Builder
	if v.Unread {
		b.WriteString(UnreadMark)
	}
	if v.Starred {
		b.WriteString(StarMark)
	}
	if b.Len() > 0 {
		b.WriteString(" ")
	}
	if from := v.From(); from != "" {
		b.WriteString(from)
		b.WriteString(": ")
	}
	b.WriteString(v.Title())
	return b.String()
}

// StatusDetail is the one word the tile carries about its state.
func (v View) StatusDetail() string {
	if v.Unread {
		return "unread"
	}
	return "read"
}

// ParseFrom splits a From header into a display name and an address. A header
// no parser will take is not dropped: the whole of it becomes the address, so
// a tile still says who the mail came from.
func ParseFrom(header string) (name, addr string) {
	header = strings.TrimSpace(header)
	if header == "" {
		return "", ""
	}
	a, err := mail.ParseAddress(header)
	if err != nil {
		return "", header
	}
	return strings.TrimSpace(a.Name), strings.TrimSpace(a.Address)
}

// ── placement ──────────────────────────────────────────────────────────

// HintEpoch anchors the calendar a collection is hinted as: the day
// containing it is row y=0, later days climb into negative y, and earlier
// days descend. It is a fixed date, so a message's hint is the same on every
// host and every restart and two nodes never disagree about where a message
// first lands.
var HintEpoch = time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)

// MessageTileW is a message tile's hinted width: two cells, so the sender and
// the subject read together on one banner.
const MessageTileW = 2

// Day is the number of whole days from HintEpoch to t, in UTC because Gmail's
// internalDate is UTC and a hint must never shift with the host's zone.
func Day(t time.Time) int64 {
	u := t.UTC()
	d := time.Date(u.Year(), u.Month(), u.Day(), 0, 0, 0, 0, time.UTC)
	return int64(d.Sub(HintEpoch).Hours() / 24)
}

// Cell is the hint for the index'th message of its day: one row per day,
// newest at the top, the day's messages left to right in arrival order.
func Cell(date time.Time, index int) (x, y int64) {
	return int64(index) * MessageTileW, -Day(date)
}
