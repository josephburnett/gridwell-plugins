package plugin

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pluginv1 "github.com/josephburnett/gridwell/api/gen/plugin/v1"
)

func TestInfoDeclaresMove(t *testing.T) {
	info, err := New(t.TempDir(), nil).Info(context.Background(), &pluginv1.InfoRequest{})
	if err != nil || !info.Move {
		t.Fatalf("Info = %v, %v; want move declared", info, err)
	}
}

func move(p *Plugin, key, grid, to string) (*pluginv1.MoveResponse, error) {
	return p.Move(context.Background(), &pluginv1.MoveRequest{Key: key, Grid: grid, ToGrid: to})
}

// A file or a directory goes into another directory under its name, and its
// key there is the one that directory's listing spells.
func TestMoveRenamesIntoTheDirectoryAndAnswersTheKeyItListsUnder(t *testing.T) {
	root := t.TempDir()
	mkdirs(t, root, "a/b", "d")
	write(t, filepath.Join(root, "a", "f.md"), "body")
	p := New(root, nil)
	for _, c := range []struct{ key, grid, to, want string }{
		{"a/f.md", "a", "a/b", "a/b/f.md"},
		{"a/b/f.md", "a/b", ".", "f.md"},
		{"d", ".", "a", "a/d"},
	} {
		got, err := move(p, c.key, c.grid, c.to)
		if err != nil || got.Key != c.want {
			t.Fatalf("Move(%s, %s -> %s) = %v, %v; want key %s", c.key, c.grid, c.to, got, err, c.want)
		}
		_, entries := listed(t, p, c.to)
		if entries[c.want] == nil {
			t.Fatalf("%s lists %v, not %s", c.to, entries, c.want)
		}
	}
	if b, err := os.ReadFile(filepath.Join(root, "f.md")); err != nil || string(b) != "body" {
		t.Fatalf("moved file reads %q, %v", b, err)
	}
}

// Nothing is replaced and nothing moves on a refusal, and each refusal says
// why.
func TestMoveRefusesWithTheReason(t *testing.T) {
	root := t.TempDir()
	mkdirs(t, root, "a", "b", "d/e")
	write(t, filepath.Join(root, "a", "f.md"), "mine")
	write(t, filepath.Join(root, "b", "f.md"), "theirs")
	write(t, filepath.Join(root, "file"), "")
	p := New(root, nil)
	for _, c := range []struct {
		key, grid, to string
		code          codes.Code
		why           string
	}{
		{"a/f.md", "a", "b", codes.FailedPrecondition, `"f.md" is already there`},
		{"a/gone.md", "a", "b", codes.FailedPrecondition, "it is gone"},
		{"a/f.md", "a", "missing", codes.FailedPrecondition, "that directory is gone"},
		{"a/f.md", "a", "file", codes.InvalidArgument, "not a directory"},
		{"a/f.md", "b", "d", codes.InvalidArgument, `it is not in "b"`},
		{"../x", "..", "a", codes.InvalidArgument, "not in the tree"},
		{"d", ".", "d/e", codes.InvalidArgument, "cannot move into itself"},
	} {
		_, err := move(p, c.key, c.grid, c.to)
		if status.Code(err) != c.code || !strings.Contains(status.Convert(err).Message(), c.why) {
			t.Errorf("Move(%s, %s -> %s) = %v; want %v saying %q", c.key, c.grid, c.to, err, c.code, c.why)
		}
	}
	for path, want := range map[string]string{"a/f.md": "mine", "b/f.md": "theirs"} {
		if b, _ := os.ReadFile(filepath.Join(root, path)); string(b) != want {
			t.Errorf("%s reads %q after the refusals, want %q", path, b, want)
		}
	}
}

// The OS tells a rename in both directories, the one it left and the one it
// came to, so the watcher announces each with no word from Move.
func TestWatchAnnouncesBothDirectoriesOfAMove(t *testing.T) {
	root := t.TempDir()
	mkdirs(t, root, "a", "b")
	write(t, filepath.Join(root, "a", "f.md"), "")
	p := New(root, nil)
	o := openWatch(t, p, "a", "b")
	if _, err := move(p, "a/f.md", "a", "b"); err != nil {
		t.Fatal(err)
	}
	o.all(t, "a", "b")
}

// A directory the OS will not let the plugin read or write is the OS's
// reason, either way across it.
func TestMoveAcrossALockedDirectorySaysTheOSReason(t *testing.T) {
	root := lockedDir(t)
	write(t, filepath.Join(root, "g.md"), "")
	p := New(root, nil)
	for _, c := range [][3]string{{"g.md", ".", "locked"}, {"locked/f.md", "locked", "."}} {
		_, err := move(p, c[0], c[1], c[2])
		if status.Code(err) != codes.FailedPrecondition || !strings.Contains(status.Convert(err).Message(), "permission denied") {
			t.Errorf("Move(%s, %s -> %s) = %v; want FailedPrecondition saying permission denied", c[0], c[1], c[2], err)
		}
	}
}
