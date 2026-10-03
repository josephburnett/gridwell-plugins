package plugin

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pluginv1 "github.com/josephburnett/gridwell/api/gen/plugin/v1"
)

// FromConfig is the one config→plugin derivation: a configured root is the
// plugin's one collection, and no root means it declares none — listed,
// contributing nothing to the (+) menu, which is not a refusal and not a
// failure.
func TestFromConfigOwnsTheRootDerivation(t *testing.T) {
	ctx := context.Background()
	root := filepath.Join(t.TempDir(), "docs")
	if err := os.Mkdir(root, 0o755); err != nil {
		t.Fatal(err)
	}
	impl, err := FromConfig(map[string]string{"root": " " + root + " "})
	if err != nil {
		t.Fatal(err)
	}
	info, err := impl.(*Plugin).Info(ctx, &pluginv1.InfoRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(info.MenuEntries) != 1 || info.MenuEntries[0].Context != "." || info.DisplayName != "docs" {
		t.Errorf("rooted → %v", info)
	}
	if info.RootContext != "" {
		t.Errorf("root_context is retired; got %q", info.RootContext)
	}
	impl, err = FromConfig(map[string]string{})
	if err != nil {
		t.Fatal(err)
	}
	if info, err := impl.(*Plugin).Info(ctx, &pluginv1.InfoRequest{}); err != nil || len(info.MenuEntries) != 0 {
		t.Errorf("unconfigured → %v, %v; want listed with no collection", info, err)
	}
}

// Same class as convert.relKey: the confinement check must not be a
// hand-built root+"/" prefix — with root "/" that is "//", which no
// path starts with, so every key was refused as an escape.
func TestAbsUnderRootSlash(t *testing.T) {
	p := New("/", nil)
	got, err := p.abs(".nofollow")
	if err != nil {
		t.Fatalf("abs(.nofollow) under root /: %v", err)
	}
	if got != "/.nofollow" {
		t.Fatalf("abs(.nofollow) = %q, want /.nofollow", got)
	}
	if got, err := p.abs("."); err != nil || got != "/" {
		t.Fatalf("abs(.) = %q, %v, want /", got, err)
	}
}

func TestAbsStillRefusesEscapes(t *testing.T) {
	p := New("/home/joe", nil)
	for _, key := range []string{"../etc/passwd", "sub/../../etc"} {
		if got, err := p.abs(key); err != nil {
			t.Fatalf("abs(%q): anchored cleanup should confine, got error %v", key, err)
		} else if got != "/home/joe/etc/passwd" && got != "/home/joe/etc" {
			t.Fatalf("abs(%q) = %q escaped the root", key, got)
		}
	}
}

// The host treatment a directory grid wears — the outside tint on every
// tile, the exit border on a descent — is this DECLARATION, not the node
// recognizing the kind "fs". An fs with no root is still a projection of the
// host, so it declares the same thing.
func TestInfoDeclaresHostContent(t *testing.T) {
	ctx := context.Background()
	for _, cfg := range []map[string]string{{"root": t.TempDir()}, {}} {
		impl, err := FromConfig(cfg)
		if err != nil {
			t.Fatal(err)
		}
		info, err := impl.(*Plugin).Info(ctx, &pluginv1.InfoRequest{})
		if err != nil {
			t.Fatal(err)
		}
		if !info.GetHostContent() {
			t.Errorf("config %v → host_content false; a directory tree is host state", cfg)
		}
	}
}

// A root the plugin cannot serve refuses Info with a sentence naming it, so
// the node shows the plugin broken rather than healthy and empty; an empty
// directory is a root it can serve. The check runs on every Info until it
// passes, so a root created after launch is served without a respawn.
func TestInfoRefusesARootItCannotServe(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	missing := filepath.Join(dir, "missing")
	file := filepath.Join(dir, "file")
	if err := os.WriteFile(file, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	cases := map[string]string{
		missing: `root "` + missing + `" does not exist`,
		file:    `root "` + file + `" is not a directory`,
	}
	if runtime.GOOS != "windows" && os.Getuid() != 0 {
		locked := filepath.Join(dir, "locked")
		if err := os.Mkdir(locked, 0o000); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(locked, 0o755) })
		cases[locked] = `root "` + locked + `" cannot be read: permission denied`
	}
	for root, want := range cases {
		_, err := New(root, nil).Info(ctx, &pluginv1.InfoRequest{})
		if status.Code(err) != codes.FailedPrecondition || status.Convert(err).Message() != want {
			t.Errorf("root %s → Info %v; want FailedPrecondition %q", root, err, want)
		}
	}

	p := New(missing, nil)
	if err := os.Mkdir(missing, 0o755); err != nil {
		t.Fatal(err)
	}
	if info, err := p.Info(ctx, &pluginv1.InfoRequest{}); err != nil || len(info.MenuEntries) != 1 {
		t.Fatalf("root created after launch → Info %v, %v; want its one collection", info, err)
	}
	// Once served, a root that goes away is a dark source, not a refusal.
	if err := os.Remove(missing); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Info(ctx, &pluginv1.InfoRequest{}); err != nil {
		t.Errorf("a served root that went away → Info %v, want the source dark rather than a refusal", err)
	}
	empty := filepath.Join(dir, "empty")
	if err := os.Mkdir(empty, 0o755); err != nil {
		t.Fatal(err)
	}
	if info, err := New(empty, nil).Info(ctx, &pluginv1.InfoRequest{}); err != nil || len(info.MenuEntries) != 1 {
		t.Errorf("empty root → Info %v, %v; want healthy with its one collection", info, err)
	}
}

// recordingHost records what Delete trashed and touches nothing.
type recordingHost struct{ trashed []string }

func (h *recordingHost) Trash(p string) error { h.trashed = append(h.trashed, p); return nil }

type contentStream struct {
	grpc.ServerStream
	chunks []*pluginv1.ContentChunk
}

func (s *contentStream) Send(c *pluginv1.ContentChunk) error {
	s.chunks = append(s.chunks, c)
	return nil
}

// lockedDir answers a root holding locked/f.md, with locked made unsearchable
// so every stat inside it fails with EACCES. It skips where permissions do not
// bind.
func lockedDir(t *testing.T) (root string) {
	t.Helper()
	if runtime.GOOS == "windows" || os.Getuid() == 0 {
		t.Skip("permissions do not bind here")
	}
	root = t.TempDir()
	locked := filepath.Join(root, "locked")
	if err := os.Mkdir(locked, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(locked, "f.md"), []byte("kept"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(locked, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o755) })
	return root
}

// Only a path that is not there is an idempotent delete. A path the plugin
// cannot see is a source it cannot reach right now: Unavailable with the
// reason, and nothing trashed.
func TestDeleteSucceedsOnlyForAGonePath(t *testing.T) {
	ctx := context.Background()
	root := lockedDir(t)
	h := &recordingHost{}
	p := New(root, h)
	if _, err := p.Delete(ctx, &pluginv1.DeleteRequest{Key: "missing.md"}); err != nil {
		t.Errorf("Delete of a gone path = %v, want success", err)
	}
	_, err := p.Delete(ctx, &pluginv1.DeleteRequest{Key: "locked/f.md"})
	if status.Code(err) != codes.Unavailable || !strings.Contains(status.Convert(err).Message(), "permission denied") {
		t.Errorf("Delete inside an unreadable directory = %v, want Unavailable naming the reason", err)
	}
	if len(h.trashed) != 0 {
		t.Errorf("trashed %v, want nothing", h.trashed)
	}
}

// A file and a directory leave the same way: to the trash.
func TestDeleteTrashesFilesAndDirectoriesAlike(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "d"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "f.md"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	h := &recordingHost{}
	p := New(root, h)
	for _, key := range []string{"d", "f.md"} {
		if _, err := p.Delete(context.Background(), &pluginv1.DeleteRequest{Key: key}); err != nil {
			t.Fatalf("Delete(%q): %v", key, err)
		}
	}
	if want := []string{filepath.Join(root, "d"), filepath.Join(root, "f.md")}; strings.Join(h.trashed, ",") != strings.Join(want, ",") {
		t.Errorf("trashed %v, want %v", h.trashed, want)
	}
}

// A file the plugin cannot stat has a body it cannot read right now, which is
// not an empty body; a gone file still answers empty.
func TestReadContentOfAnUnreadableFileIsUnavailable(t *testing.T) {
	root := lockedDir(t)
	p := New(root, nil)
	err := p.ReadContent(&pluginv1.ReadContentRequest{Key: "locked/f.md"}, &contentStream{})
	if status.Code(err) != codes.Unavailable || !strings.Contains(status.Convert(err).Message(), "permission denied") {
		t.Errorf("ReadContent inside an unreadable directory = %v, want Unavailable naming the reason", err)
	}
	gone := &contentStream{}
	if err := p.ReadContent(&pluginv1.ReadContentRequest{Key: "missing.md"}, gone); err != nil || len(gone.chunks) != 1 || len(gone.chunks[0].Data) != 0 {
		t.Errorf("ReadContent of a gone file = %v, %v; want one empty chunk", gone.chunks, err)
	}
}

// A context that is now a file, or lies under one, has no entries, and that is
// definitive: an authoritative empty listing, not a dark source, and a key
// under a file probes gone.
func TestListOfAContextThatIsAFileIsAuthoritativeAndEmpty(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "was-a-dir"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	p := New(root, nil)
	for _, ctxKey := range []string{"was-a-dir", "was-a-dir/sub"} {
		resp, err := p.List(context.Background(), &pluginv1.ListRequest{Context: ctxKey})
		if err != nil || !resp.Authoritative || len(resp.Entries) != 0 {
			t.Errorf("List(%q) = %v, %v; want an authoritative empty listing", ctxKey, resp, err)
		}
	}
	pr, err := p.Probe(context.Background(), &pluginv1.ProbeRequest{Key: "was-a-dir/f.md"})
	if err != nil || pr.Presence != pluginv1.ProbeResponse_PRESENCE_GONE {
		t.Errorf("Probe of a key under a file = %v, %v; want GONE", pr, err)
	}
}
