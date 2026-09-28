package mailbox

import "errors"

// Delta is what Gmail's history says changed since a history id, reduced to
// what this projection can see: the messages whose watched labels changed or
// that arrived carrying one, the messages deleted, and the history id the
// answer is current to.
type Delta struct {
	Touched   []string
	Deleted   []string
	HistoryID uint64
}

// ErrHistoryExpired is Gmail refusing a history id as too old (its 404 on
// history.list). The only recovery is a full walk, which mints a fresh id.
var ErrHistoryExpired = errors.New("history id expired")

// Labelled is one message as it stands now: its record and every label it
// carries. A history catch-up reads each touched message this way, so where
// it lands is decided by its current labels and never by the order history
// listed its changes in.
type Labelled struct {
	Message
	Labels []string
}

// WatchedLabels are the labels whose changes this projection can see: every
// collection's, and UNREAD. A change to any other label moves no tile and
// changes no face.
func WatchedLabels() []string {
	out := []string{UnreadLabel}
	for _, c := range Collections {
		out = append(out, c.LabelIDs...)
	}
	return out
}
