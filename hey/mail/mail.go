// Package mail is the pure half of the hey plugin: the thread record, the
// three HEY collections it projects, the keys, labels and placement hints
// derived from them, the memory of every thread seen, and the disposable
// cache file that memory rewarms itself from. There is no process and no
// gRPC here, so everything is unit-tested against fakes, and the plugin
// package only wires it to the wire.
package mail

import (
	"strconv"
	"strings"
	"time"
)

// Thread is one email thread as HEY's CLI lists it: the subset of a posting
// the plugin shows. The HTML body is not here — it is fetched on descent and
// never remembered, because a mailbox listing is small and a mailbox's bodies
// are not.
type Thread struct {
	// TopicID is HEY's thread id: what `hey thread read` takes, and the one
	// fact the plugin's key is built from.
	TopicID int64 `json:"topicId"`
	// PostingID is the box item id — the row in one box. It changes when the
	// thread moves between boxes, so it is remembered for diagnosis and never
	// keyed on.
	PostingID int64 `json:"postingId"`
	// Collection is the context key of the collection the thread was last
	// seen in.
	Collection string    `json:"collection"`
	Subject    string    `json:"subject"`
	Summary    string    `json:"summary"`
	FromName   string    `json:"fromName"`
	FromEmail  string    `json:"fromEmail"`
	CreatedAt  time.Time `json:"createdAt"`
	Seen       bool      `json:"seen"`
}

// Collection is one of the three HEY stacks this plugin projects. Key is the
// plugin's context key, stable forever. Box is the selector `hey box view`
// takes — the hey-cli's own named getter, so no listing call is needed to
// resolve it. Label is the face of the (+) menu row that opens it.
type Collection struct {
	Key   string
	Box   string
	Label string
}

// The three context keys. They embed the CLI's box selector rather than a
// display name, because a display name is HEY's to change and a key is
// forever.
const (
	ImboxContext      = "box:imbox"
	ReplyLaterContext = "box:laterbox"
	SetAsideContext   = "box:asidebox"
)

// Collections is the projection, in the order the (+) menu offers it.
// ImboxContext is first because it is also the plugin's root context.
var Collections = []Collection{
	{Key: ImboxContext, Box: "imbox", Label: "imbox"},
	{Key: ReplyLaterContext, Box: "laterbox", Label: "reply later"},
	{Key: SetAsideContext, Box: "asidebox", Label: "set aside"},
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

// KeyPrefix namespaces thread keys. A key names the same email thread for the
// life of the plugin, whichever collection holds it: a thread moved from the
// Imbox to Reply Later keeps its key, so the node keeps its id and every link
// to it still resolves.
const KeyPrefix = "thread:"

// Key is the plugin key of the thread with this topic id.
func Key(topicID int64) string { return KeyPrefix + strconv.FormatInt(topicID, 10) }

// Key is the thread's plugin key.
func (t *Thread) Key() string { return Key(t.TopicID) }

// ParseKey resolves a thread key to its topic id.
func ParseKey(key string) (int64, bool) {
	s, ok := strings.CutPrefix(key, KeyPrefix)
	if !ok {
		return 0, false
	}
	id, err := strconv.ParseInt(s, 10, 64)
	return id, err == nil && id > 0
}

// NoSubject stands in for a thread HEY named nothing: a subject is what a
// tile is read by, and a blank banner is worse than saying it is blank.
const NoSubject = "(no subject)"

// Title is the thread's headline: HEY's subject, else the first line of the
// preview, else NoSubject.
func (t *Thread) Title() string {
	if s := strings.TrimSpace(t.Subject); s != "" {
		return s
	}
	if line, _, _ := strings.Cut(strings.TrimSpace(t.Summary), "\n"); line != "" {
		return line
	}
	return NoSubject
}

// UnseenMark is the unread thread's banner glyph. It leads the label, so
// unread reads from a zoomed-out grid where the text does not.
const UnseenMark = "●"

// From is who the thread is from: the sender's name, else their address.
func (t *Thread) From() string {
	if n := strings.TrimSpace(t.FromName); n != "" {
		return n
	}
	return strings.TrimSpace(t.FromEmail)
}

// Label is the tile's banner: the unread mark, the sender, and the subject.
func (t *Thread) Label() string {
	var b strings.Builder
	if !t.Seen {
		b.WriteString(UnseenMark + " ")
	}
	if from := t.From(); from != "" {
		b.WriteString(from)
		b.WriteString(": ")
	}
	b.WriteString(t.Title())
	return b.String()
}

// StatusDetail is the one word the tile carries about its state.
func (t *Thread) StatusDetail() string {
	if t.Seen {
		return "seen"
	}
	return "unseen"
}

// ── placement ──────────────────────────────────────────────────────────

// HintEpoch anchors the calendar a collection is hinted as: the day
// containing it is row y=0, later days climb into negative y, and earlier
// days descend. It is a fixed date, so a thread's hint is the same on every
// host and every restart and two nodes never disagree about where a thread
// first lands.
var HintEpoch = time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)

// ThreadTileW is a thread tile's hinted width: two cells, so the sender and
// the subject read together on one banner.
const ThreadTileW = 2

// Day is the number of whole days from HintEpoch to t, in UTC because HEY's
// timestamps are UTC and a hint must never shift with the host's zone.
func Day(t time.Time) int64 {
	u := t.UTC()
	d := time.Date(u.Year(), u.Month(), u.Day(), 0, 0, 0, 0, time.UTC)
	return int64(d.Sub(HintEpoch).Hours() / 24)
}

// Cell is the hint for the index'th thread of its day: one row per day,
// newest at the top, the day's threads left to right in arrival order.
func Cell(created time.Time, index int) (x, y int64) {
	return int64(index) * ThreadTileW, -Day(created)
}
