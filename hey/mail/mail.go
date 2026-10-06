// Package mail is the pure half of the hey plugin: the thread record, the
// six HEY boxes it projects and everything (their union), the keys, labels
// and placement hints
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
	// ActiveAt is HEY's active_at, when the thread last had an entry: a reply
	// landing moves it, so it names the page (Stamp). Zero when HEY did not
	// say, or the record is from a cache that predates it.
	ActiveAt time.Time `json:"activeAt"`
}

// Stamp is the thread's content stamp, naming the page ServeContent answers:
// its ActiveAt, HEY's own word for when the thread last changed. Empty when
// unknown.
func (t *Thread) Stamp() string {
	if t.ActiveAt.IsZero() {
		return ""
	}
	return t.ActiveAt.UTC().Format(time.RFC3339Nano)
}

// Collection is one of the six HEY boxes this plugin projects. Key is the
// plugin's context key, stable forever. Box is the selector `hey box view`
// takes — the hey-cli's own named getter, so no listing call is needed to
// resolve it. Label is the face of the (+) menu row that opens it. Doorway
// says whether the (+) menu offers it: every box is walked into everything,
// but a box nobody opens on purpose is clutter in the one menu row.
type Collection struct {
	Key     string
	Box     string
	Label   string
	Doorway bool
}

// The box context keys. They embed the CLI's box selector rather than a
// display name, because a display name is HEY's to change and a key is
// forever.
const (
	ImboxContext      = "box:imbox"
	ReplyLaterContext = "box:laterbox"
	SetAsideContext   = "box:asidebox"
	FeedContext       = "box:feedbox"
	PaperTrailContext = "box:trailbox"
	BubbleUpContext   = "box:bubblebox"
)

// Collections is the projection, in the order the (+) menu offers its
// doorways. ImboxContext is first because it is the collection to read first.
var Collections = []Collection{
	{Key: ImboxContext, Box: "imbox", Label: "imbox", Doorway: true},
	{Key: ReplyLaterContext, Box: "laterbox", Label: "reply later", Doorway: true},
	{Key: SetAsideContext, Box: "asidebox", Label: "set aside", Doorway: true},
	{Key: FeedContext, Box: "feedbox", Label: "the feed"},
	{Key: PaperTrailContext, Box: "trailbox", Label: "paper trail"},
	{Key: BubbleUpContext, Box: "bubblebox", Label: "bubble up"},
}

// EverythingContext is the union of every box: each thread once, its one
// home. A box lists links to it (BoxEntries), so a thread that moves between
// boxes keeps its tile and every reference to it. It is no box of HEY's, so
// nothing walks it; it is derived from the boxes' memory.
const (
	EverythingContext = "everything"
	EverythingLabel   = "everything"
)

// LookupCollection resolves a context key to its collection.
func LookupCollection(key string) (Collection, bool) {
	for _, c := range Collections {
		if c.Key == key {
			return c, true
		}
	}
	return Collection{}, false
}

// LookupBox resolves a box kind, as the CLI's live feed names a box, to the
// collection that projects it. The selectors `box view` takes are HEY's box
// kinds, so one table answers both.
func LookupBox(kind string) (Collection, bool) {
	for _, c := range Collections {
		if c.Box == kind {
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

// UnseenMark is the status a box tile carries while its thread is unseen
// there. The label never carries it: the name stays as it was.
const UnseenMark = "●"

// From is who the thread is from: the sender's name, else their address.
func (t *Thread) From() string {
	if n := strings.TrimSpace(t.FromName); n != "" {
		return n
	}
	return strings.TrimSpace(t.FromEmail)
}

// Label is the tile's banner: the sender and the subject. It reads the same
// seen or unseen; StatusDetail carries the difference.
func (t *Thread) Label() string {
	var b strings.Builder
	if from := t.From(); from != "" {
		b.WriteString(from)
		b.WriteString(": ")
	}
	b.WriteString(t.Title())
	return b.String()
}

// StatusDetail is UnseenMark while the thread is unseen in the box it was
// listed from, and nothing once it is seen.
func (t *Thread) StatusDetail() string {
	if t.Seen {
		return ""
	}
	return UnseenMark
}

// ThreadTileW is a thread tile's hinted width: two cells, so the sender and
// the subject read together on one banner.
const ThreadTileW = 2
