package plugin

import (
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/josephburnett/gridwell-plugins/hey/mail"
)

// A thread kept because HEY still had it (archived) must still be forgotten
// once HEY deletes it. It is in no box, so no listing reaches it, and prune
// never asks about it again: its record stays in the cache file forever.
func TestAnArchivedThreadHEYLaterDeletesIsForgotten(t *testing.T) {
	f := newFake()
	f.boxes["imbox"] = []mail.Thread{th(2, "archived", "2026-01-02T09:00:00Z"), th(4, "stays", "2026-01-04T09:00:00Z")}
	f.html[2] = "<!doctype html>"
	p := stable(t, f, Options{})
	walk := func() {
		t.Helper()
		p.flights.Rewalk(mail.ImboxGrid)
		idle(t, p)
	}
	walk()
	f.setBox("imbox", th(4, "stays", "2026-01-04T09:00:00Z"))
	walk()
	if _, ok := p.mem.Get(2); !ok {
		t.Fatal("setup: an archived thread HEY still has was forgotten")
	}
	f.mu.Lock()
	delete(f.html, 2)
	f.threadErr[2] = status.Error(codes.NotFound, "thread not found")
	f.mu.Unlock()
	for i := 0; i < 3; i++ {
		walk()
	}
	if _, ok := p.mem.Get(2); ok {
		t.Error("a thread HEY deleted after archiving is remembered forever: nothing asks about it again")
	}
}
