package mail

// The words of the CLI's live feed (`hey watch`), one per line. Added,
// updated and deleted are postings entering, changing in and leaving one box;
// resync says a box changed more than the feed could list; ready and
// disconnected are about the feed itself.
const (
	ChangeAdded        = "added"
	ChangeUpdated      = "updated"
	ChangeDeleted      = "deleted"
	ChangeResync       = "resync"
	ChangeReady        = "ready"
	ChangeDisconnected = "disconnected"
)

// Event is one line of the live feed in the memory's terms. A word this
// plugin does not know is carried as it came and ignored downstream, so a
// newer CLI adding one breaks nothing.
type Event struct {
	Change string
	// Box is the kind of the box the line is about; empty on ready and
	// disconnected, which name none.
	Box       string
	PostingID int64
	// Thread is the posting on an added or updated line. TopicID 0 means it
	// opens no thread (a bundle), exactly as in a box listing.
	Thread Thread
}

// Effect is what applying one event or walk did to one collection.
type Effect struct {
	// Changed says the collection's listing differs from before.
	Changed bool
	// Everything says everything's listing differs from before: the union
	// gained or lost a thread, or a thread's record changed.
	Everything bool
	// Rewalk says the memory could not apply the event and only a read of the
	// box can: a deleted posting it cannot map to a thread it shows.
	Rewalk bool
	// Moved names the threads whose page changed in place, by topic id: a
	// known stamp moved to another (Memory.putLocked).
	Moved []int64
}
