package plugin

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pluginv1 "github.com/josephburnett/gridwell/api/gen/plugin/v1"
)

// FromConfig is the one config-to-plugin derivation: a missing pid takes the
// documented default of 1, and a pid that is not a positive integer is a
// refusal naming it, never a silent fallback to the whole process tree.
func TestFromConfigOwnsThePidDerivation(t *testing.T) {
	for _, bad := range []string{"abc", "0", "-3", "12x"} {
		if impl, err := FromConfig(map[string]string{"pid": bad}); err == nil || !strings.Contains(err.Error(), bad) {
			t.Errorf("pid %q → %v, %v; want a refusal naming it", bad, impl, err)
		}
	}
	self := strconv.Itoa(os.Getpid())
	for raw, want := range map[string]string{"": "1", "1": "1", " " + self + " ": self} {
		impl, err := FromConfig(map[string]string{"pid": raw})
		if err != nil {
			t.Fatalf("pid %q: %v", raw, err)
		}
		info, err := impl.(*Plugin).Info(context.Background(), &pluginv1.InfoRequest{})
		if err != nil || len(info.MenuEntries) != 1 || info.MenuEntries[0].Context != want {
			t.Errorf("pid %q → collections %v, %v; want the one context %q", raw, info.GetMenuEntries(), err, want)
		}
	}
}

// The process table is host state, and saying so is what earns its grids the
// host treatment on the client. Nothing downstream knows the kind "proc".
func TestInfoDeclaresHostContent(t *testing.T) {
	impl, err := FromConfig(map[string]string{})
	if err != nil {
		t.Fatal(err)
	}
	info, err := impl.(*Plugin).Info(context.Background(), &pluginv1.InfoRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if !info.GetHostContent() {
		t.Error("host_content false; the process table is host state")
	}
}

// A process the plugin cannot project refuses Info with a sentence naming
// it, so the node shows the plugin broken rather than an empty tree; a
// running process with no children is a tree it can serve. The check runs on
// every Info until one passes, so a process that appears later is served
// without a respawn, and one that exits after that is a dark source.
func TestInfoRefusesAProcessItCannotServe(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	cases := map[*Plugin]string{
		New(filepath.Join(root, "absent"), 1, nil): "the process table " + filepath.Join(root, "absent") + " cannot be read",
		New(root, 4242, nil):                       "pid 4242 is not running",
	}
	for p, want := range cases {
		_, err := p.Info(ctx, &pluginv1.InfoRequest{})
		if status.Code(err) != codes.FailedPrecondition || !strings.HasPrefix(status.Convert(err).Message(), want) {
			t.Errorf("procRoot %s pid %d → Info %v; want FailedPrecondition %q", p.procRoot, p.rootPID, err, want)
		}
	}

	p := New(root, 4242, nil)
	if err := os.Mkdir(filepath.Join(root, "4242"), 0o755); err != nil {
		t.Fatal(err)
	}
	if info, err := p.Info(ctx, &pluginv1.InfoRequest{}); err != nil || len(info.MenuEntries) != 1 {
		t.Errorf("a childless process that started after launch → Info %v, %v; want its one collection", info, err)
	}
	if err := os.Remove(filepath.Join(root, "4242")); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Info(ctx, &pluginv1.InfoRequest{}); err != nil {
		t.Errorf("a served process that exited → Info %v, want a dark source rather than a refusal", err)
	}
}

// stubProc writes a fake /proc: each pid maps to its ppid, with a status and
// a stat file shaped as the kernel writes them.
func stubProc(t *testing.T, ppids map[int64]int64) string {
	t.Helper()
	root := t.TempDir()
	for pid, ppid := range ppids {
		dir := filepath.Join(root, strconv.FormatInt(pid, 10))
		if err := os.Mkdir(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		status := fmt.Sprintf("Name:\tp%d\nState:\tS (sleeping)\nPPid:\t%d\nUid:\t1000\t1000\t1000\t1000\n", pid, ppid)
		stat := fmt.Sprintf("%d (p %d) S %d %d %d 0 -1 4194304 0 0 0 0 0 0 0 0 20 0 1 0 %d 0 0\n", pid, pid, ppid, pid, pid, 1000+pid)
		for name, body := range map[string]string{"status": status, "stat": stat} {
			if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	return root
}

// served is a plugin over root that has passed Info.
func served(t *testing.T, root string, pid int64) *Plugin {
	t.Helper()
	p := New(root, pid, nil)
	if _, err := p.Info(context.Background(), &pluginv1.InfoRequest{}); err != nil {
		t.Fatal(err)
	}
	return p
}

// deny makes path unreadable for the rest of the test.
func deny(t *testing.T, path string) {
	t.Helper()
	if err := os.Chmod(path, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(path, 0o755) })
	if _, err := os.ReadDir(path); err == nil {
		t.Skip("running as a user that reads through mode 0 (root)")
	}
}

type chunks struct {
	grpc.ServerStream
	got []*pluginv1.ContentChunk
}

func (c *chunks) Send(m *pluginv1.ContentChunk) error {
	c.got = append(c.got, m)
	return nil
}

func read(p *Plugin, key string) ([]*pluginv1.ContentChunk, error) {
	var c chunks
	err := p.ReadContent(&pluginv1.ReadContentRequest{Key: key}, &c)
	return c.got, err
}

// Once Info has passed, a process table the plugin cannot read is dark:
// Unavailable, never an empty or partial listing that looks true.
func TestListOfAnUnreadableTableIsUnavailable(t *testing.T) {
	root := stubProc(t, map[int64]int64{1: 0, 10: 1})
	p := served(t, root, 1)
	if resp, err := p.List(context.Background(), &pluginv1.ListRequest{Context: "1"}); err != nil || len(resp.Entries) != 2 {
		t.Fatalf("readable table → %v, %v; want @info and pid 10", resp, err)
	}
	deny(t, root)
	if resp, err := p.List(context.Background(), &pluginv1.ListRequest{Context: "1"}); status.Code(err) != codes.Unavailable {
		t.Errorf("unreadable table → List %v, %v; want Unavailable", resp, err)
	}
}

// @info's body is the process's metadata. A process that has exited has no
// body to read (NotFound), and one that cannot be read right now is dark
// (Unavailable); neither is an empty document.
func TestReadingInfoOfAGoneOrUnreadableProcessIsAVerdict(t *testing.T) {
	root := stubProc(t, map[int64]int64{1: 0, 10: 1})
	p := served(t, root, 1)
	if got, err := read(p, "info:10"); err != nil || len(got) != 1 || !strings.Contains(string(got[0].Data), "pid: 10") {
		t.Fatalf("a running process → %v, %v; want its metadata", got, err)
	}
	if got, err := read(p, "info:42"); status.Code(err) != codes.NotFound {
		t.Errorf("a gone process → %v, %v; want NotFound", got, err)
	}
	deny(t, filepath.Join(root, "10"))
	if got, err := read(p, "info:10"); status.Code(err) != codes.Unavailable {
		t.Errorf("an unreadable process → %v, %v; want Unavailable", got, err)
	}
}

// Probe answers for the context it names. A process is under the context of
// its parent now, so a child reparented after its parent died (30, once
// under 10) is gone from the old grid and present in its new parent's; an
// @info tile lives only in its own process's grid, and only while that
// process runs. No context asks about the plugin as a whole.
func TestProbeAnswersForTheContextAsked(t *testing.T) {
	root := stubProc(t, map[int64]int64{1: 0, 10: 1, 20: 10, 30: 1})
	p := served(t, root, 1)
	const (
		present = pluginv1.ProbeResponse_PRESENCE_PRESENT
		gone    = pluginv1.ProbeResponse_PRESENCE_GONE
		unknown = pluginv1.ProbeResponse_PRESENCE_UNSPECIFIED
	)
	cases := []struct {
		key, context string
		want         pluginv1.ProbeResponse_Presence
	}{
		{"20", "10", present},
		{"30", "10", gone},
		{"30", "1", present},
		{"30", "", present},
		{"42", "1", gone},
		{"42", "", gone},
		{"info:10", "10", present},
		{"info:10", "", present},
		{"info:10", "1", gone},
		{"info:42", "42", gone},
		{"info:42", "", gone},
		{"20", "info:10", gone},
		{"nope", "1", gone},
	}
	probe := func(key, ctxKey string) pluginv1.ProbeResponse_Presence {
		resp, err := p.Probe(context.Background(), &pluginv1.ProbeRequest{Key: key, Context: ctxKey})
		if err != nil {
			t.Fatalf("Probe(%q in %q): %v", key, ctxKey, err)
		}
		return resp.Presence
	}
	for _, c := range cases {
		if got := probe(c.key, c.context); got != c.want {
			t.Errorf("Probe(%q in %q) = %v, want %v", c.key, c.context, got, c.want)
		}
	}
	deny(t, filepath.Join(root, "20"))
	if got := probe("20", "10"); got != unknown {
		t.Errorf("Probe of an unreadable process = %v, want UNSPECIFIED", got)
	}
}

// @info's body is markdown, so it declares the document renderer with the
// toggle to its source; every text entry the plugin lists declares one.
func TestEveryTextEntryDeclaresItsPresentation(t *testing.T) {
	p := served(t, stubProc(t, map[int64]int64{1: 0, 10: 1}), 1)
	resp, err := p.List(context.Background(), &pluginv1.ListRequest{Context: "1"})
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range resp.Entries {
		if e.Kind == "text" && e.TextPresentation != "both" {
			t.Errorf("text entry %q declares %q; want both", e.Key, e.TextPresentation)
		}
	}
}

type killer struct {
	answer error
	sent   []int64
}

func (k *killer) Kill(pid int64, _ syscall.Signal) error {
	k.sent = append(k.sent, pid)
	return k.answer
}

// Deleting a well stops its process. One already gone is done, so the node's
// probe retires its row; one the user may not signal is refused with a
// sentence. An @info tile describes a process and is not one: deleting it
// signals nothing.
func TestDeleteStopsAProcessAndNeverAnInfoTile(t *testing.T) {
	ctx := context.Background()
	root := stubProc(t, map[int64]int64{1: 0, 10: 1})
	for answer, want := range map[error]codes.Code{
		nil:            codes.OK,
		syscall.ESRCH:  codes.OK,
		syscall.EPERM:  codes.PermissionDenied,
		syscall.EINVAL: codes.Internal,
	} {
		k := &killer{answer: answer}
		_, err := New(root, 1, k).Delete(ctx, &pluginv1.DeleteRequest{Key: "10"})
		if status.Code(err) != want || len(k.sent) != 1 || k.sent[0] != 10 {
			t.Errorf("kill answers %v → Delete %v, signalled %v; want %v after signalling 10", answer, err, k.sent, want)
		}
		if want == codes.PermissionDenied && !strings.Contains(status.Convert(err).Message(), "not permitted to stop pid 10") {
			t.Errorf("EPERM → %q; want a sentence saying the process may not be stopped", status.Convert(err).Message())
		}
	}

	k := &killer{}
	_, err := New(root, 1, k).Delete(ctx, &pluginv1.DeleteRequest{Key: "info:10"})
	if status.Code(err) != codes.FailedPrecondition || len(k.sent) != 0 {
		t.Errorf("Delete of info:10 → %v, signalled %v; want a refusal that signals nothing", err, k.sent)
	}
}
