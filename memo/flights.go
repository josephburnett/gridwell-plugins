package memo

import (
	"context"
	"log"
	"maps"
	"slices"
	"sync"
	"time"

	"google.golang.org/grpc/status"
)

// DefaultFirstAnswer bounds how long a cold read waits on a walk before
// answering what memory holds so far.
const DefaultFirstAnswer = time.Second

// FlightOptions configures Flights. Walk is required.
type FlightOptions struct {
	// Name prefixes every log line: the plugin's kind.
	Name string
	// Walk refreshes memory for key from the source. ctx is the plugin's
	// lifetime: a walk is shared by every reader, so no reader's hangup ends
	// it, and the source call's own timeout bounds it.
	Walk func(ctx context.Context, key string) error
	// Window is how long a landed walk keeps its key fresh. Zero means a walk
	// is never fresh: every read past the last one starts another.
	Window time.Duration
	// Fresh replaces the Window rule when set: walkedAt is the key's last
	// landed walk, zero if none. A plugin whose source tells (a live feed)
	// answers from that instead of the clock.
	Fresh func(key string, walkedAt time.Time) bool
	// Covers names the keys whose walk also answers key, broadest last (a
	// root walk covers every week). A read of key joins their flight, is
	// fresh under their stamp, and answers their failure; their landing
	// clears key's failure.
	Covers func(key string) []string
	// FirstAnswer bounds a cold read's wait. Zero means DefaultFirstAnswer.
	FirstAnswer time.Duration
	// Landed runs after a walk's outcome is recorded and before any reader
	// waiting on it is released: where the plugin saves its File and
	// publishes its Changes, so a listing that waited is one a restart repeats.
	Landed func(key string, err error)
	Clock  Clock
	Logf   func(format string, args ...any)
}

// Flights runs one walk per key at a time, shared by every reader and
// detached from all of them, and remembers when each key last landed and why
// it last failed. A read memory can answer never waits and never fails on the
// source: it answers with the failure as its Unreachable reason (rule 7).
type Flights struct {
	life *Life
	o    FlightOptions

	mu       sync.Mutex
	flights  map[string]*flight
	walkedAt map[string]time.Time
	// failed is each key's last refresh failure until a refresh of it, or of
	// a key that covers it, lands. A key's presence here is also its open log
	// episode (rule 14).
	failed map[string]error
	again  map[string]bool
}

type flight struct {
	done chan struct{}
	err  error
}

// NewFlights builds the flights for one plugin's memory, its walks run under
// life.
func NewFlights(life *Life, o FlightOptions) *Flights {
	if o.FirstAnswer <= 0 {
		o.FirstAnswer = DefaultFirstAnswer
	}
	if o.Clock == nil {
		o.Clock = System
	}
	if o.Logf == nil {
		o.Logf = log.Printf
	}
	if o.Covers == nil {
		o.Covers = func(string) []string { return nil }
	}
	return &Flights{
		life:     life,
		o:        o,
		flights:  map[string]*flight{},
		walkedAt: map[string]time.Time{},
		failed:   map[string]error{},
		again:    map[string]bool{},
	}
}

// Read makes key answerable for a call: fresh memory as it is, otherwise a
// walk, joined if one is in flight. warm says memory already holds an answer
// for key: then Read never waits, and unreachable is the last refresh's
// failure, for ListResponse.unreachable. A cold read waits at most
// FirstAnswer, then answers memory so far; it fails only when its walk failed
// with nothing remembered, or its caller hung up.
func (f *Flights) Read(ctx context.Context, key string, warm bool) (unreachable string, err error) {
	f.mu.Lock()
	last := f.lastLocked(key)
	if f.freshLocked(key) {
		f.mu.Unlock()
		return Reason(last), nil
	}
	fl := f.joinLocked(key)
	if fl == nil {
		fl = f.startLocked(key)
	}
	f.mu.Unlock()
	if warm {
		return Reason(last), nil
	}
	select {
	case <-fl.done:
		return "", fl.err
	case <-f.o.Clock.After(f.o.FirstAnswer):
		return Reason(last), nil
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

// Rewalk walks key whatever its freshness: the source said memory cannot
// know it without a read. A walk already in flight may have begun before
// that, so it is followed by one more.
func (f *Flights) Rewalk(key string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, running := f.flights[key]; running {
		f.again[key] = true
		return
	}
	f.startLocked(key)
}

// Outcome records a refresh of key that was not a walk (a glance, a feed
// ending): its failure is what reads answer as unreachable, its success
// clears that. It stamps no freshness, since it proved nothing about absence.
func (f *Flights) Outcome(key string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err != nil {
		f.failLocked(key, err)
	} else {
		f.clearLocked(key)
	}
}

// Fresh reports whether a read of key would answer without a walk.
func (f *Flights) Fresh(key string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.freshLocked(key)
}

// Busy reports whether a walk that answers key is in flight.
func (f *Flights) Busy(key string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.joinLocked(key) != nil
}

// WalkedAt copies out when each key last landed, for the plugin's File: a
// walk is fresh for its window whichever process ran it.
func (f *Flights) WalkedAt() map[string]time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return maps.Clone(f.walkedAt)
}

// Restore takes back what WalkedAt saved. It is the boot path only.
func (f *Flights) Restore(walkedAt map[string]time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for k, t := range walkedAt {
		if !t.IsZero() {
			f.walkedAt[k] = t
		}
	}
}

// Reason is err as an unreachable reason: its message, without the RPC
// wrapping a user has no use for; "" for nil.
func Reason(err error) string {
	if err == nil {
		return ""
	}
	return status.Convert(err).Message()
}

// Within reports whether t is less than d before now. A stamp in the future
// is not: it can come from a File, and a clock that has since stepped back
// would otherwise freeze memory as fresh.
func Within(now, t time.Time, d time.Duration) bool {
	if t.IsZero() {
		return false
	}
	age := now.Sub(t)
	return age >= 0 && age < d
}

func (f *Flights) keysLocked(key string) []string {
	return append([]string{key}, f.o.Covers(key)...)
}

func (f *Flights) freshLocked(key string) bool {
	for _, k := range f.keysLocked(key) {
		t := f.walkedAt[k]
		if f.o.Fresh != nil {
			if f.o.Fresh(k, t) {
				return true
			}
		} else if Within(f.o.Clock.Now(), t, f.o.Window) {
			return true
		}
	}
	return false
}

func (f *Flights) joinLocked(key string) *flight {
	for _, k := range f.keysLocked(key) {
		if fl, ok := f.flights[k]; ok {
			return fl
		}
	}
	return nil
}

func (f *Flights) lastLocked(key string) error {
	for _, k := range f.keysLocked(key) {
		if err := f.failed[k]; err != nil {
			return err
		}
	}
	return nil
}

func (f *Flights) failLocked(key string, err error) {
	if _, open := f.failed[key]; !open {
		f.o.Logf("%s: refresh %q failed: %v", f.o.Name, key, err)
	}
	f.failed[key] = err
}

func (f *Flights) clearLocked(key string) {
	delete(f.failed, key)
	for k := range f.failed {
		if slices.Contains(f.o.Covers(k), key) {
			delete(f.failed, k)
		}
	}
}

func (f *Flights) startLocked(key string) *flight {
	fl := &flight{done: make(chan struct{})}
	f.flights[key] = fl
	f.life.Go(func(ctx context.Context) { f.run(ctx, key, fl) })
	return fl
}

func (f *Flights) run(ctx context.Context, key string, fl *flight) {
	err := f.o.Walk(ctx, key)
	f.mu.Lock()
	switch {
	case ctx.Err() != nil:
		// The plugin is ending: no outcome to record, and no walk after it.
		delete(f.again, key)
	case err == nil:
		f.walkedAt[key] = f.o.Clock.Now()
		f.clearLocked(key)
	default:
		f.failLocked(key, err)
	}
	delete(f.flights, key)
	if f.again[key] {
		delete(f.again, key)
		f.startLocked(key)
	}
	f.mu.Unlock()
	if f.o.Landed != nil && ctx.Err() == nil {
		f.o.Landed(key, err)
	}
	fl.err = err
	close(fl.done)
}
