package plugin

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/josephburnett/gridwell-plugins/fs/fsfile"
	pluginv1 "github.com/josephburnett/gridwell/api/gen/plugin/v1"
)

// writeStream feeds a WriteContent its messages, then end, which is io.EOF
// for a clean close.
type writeStream struct {
	grpc.ServerStream
	msgs []*pluginv1.WriteContentRequest
	end  error
	resp *pluginv1.WriteContentResponse
}

func (s *writeStream) Recv() (*pluginv1.WriteContentRequest, error) {
	if len(s.msgs) == 0 {
		return nil, s.end
	}
	m := s.msgs[0]
	s.msgs = s.msgs[1:]
	return m, nil
}

func (s *writeStream) SendAndClose(r *pluginv1.WriteContentResponse) error {
	s.resp = r
	return nil
}

func writeKey(p *Plugin, key, stamp string, data []byte) (*pluginv1.WriteContentResponse, error) {
	s := &writeStream{msgs: []*pluginv1.WriteContentRequest{{Key: key, ContentStamp: stamp, Data: data}}, end: io.EOF}
	err := p.WriteContent(s)
	return s.resp, err
}

func readStamp(t *testing.T, p *Plugin, key string) string {
	t.Helper()
	s := &contentStream{}
	if err := p.ReadContent(&pluginv1.ReadContentRequest{Key: key}, s); err != nil {
		t.Fatal(err)
	}
	return s.chunks[0].ContentStamp
}

// moveOn changes a file as another writer would, past any mtime tick.
func moveOn(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	later := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(path, later, later); err != nil {
		t.Fatal(err)
	}
}

func TestInfoDeclaresWritable(t *testing.T) {
	resp, err := New(t.TempDir(), nil).Info(context.Background(), &pluginv1.InfoRequest{})
	if err != nil || !resp.Writable {
		t.Fatalf("Info = %v, %v; want writable declared", resp, err)
	}
}

// A write that claims the file's stamp replaces the whole file, keeps its
// mode, leaves nothing beside it, and answers the stamp the next listing and
// read name.
func TestAWriteClaimingTheStampReplacesTheFile(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "notes.md")
	if err := os.WriteFile(path, []byte("# a longer first version\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	p := New(root, nil)
	resp, err := writeKey(p, "notes.md", readStamp(t, p, "notes.md"), []byte("# two\n"))
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(path); string(got) != "# two\n" {
		t.Errorf("file holds %q", got)
	}
	if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o640 {
		t.Errorf("mode %v, want the file's own 0640", fi.Mode().Perm())
	}
	if resp.ContentStamp == "" || resp.ContentStamp != readStamp(t, p, "notes.md") {
		t.Errorf("answered stamp %q, the read names %q", resp.ContentStamp, readStamp(t, p, "notes.md"))
	}
	names, _ := os.ReadDir(root)
	if len(names) != 1 {
		t.Errorf("the directory holds %v; the temp file stayed", names)
	}
	if _, err := writeKey(p, "notes.md", resp.ContentStamp, []byte("# three\n")); err != nil {
		t.Errorf("a second write claiming the first's answer: %v", err)
	}
}

// Bytes on disk the writer has not seen are never overwritten: a write whose
// stamp is not the file's now is the conflict a stale version is.
func TestAWriteOnAStaleStampIsAConflict(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "notes.md")
	if err := os.WriteFile(path, []byte("# mine\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	p := New(root, nil)
	stamp := readStamp(t, p, "notes.md")
	moveOn(t, path, []byte("# theirs\n"))
	_, err := writeKey(p, "notes.md", stamp, []byte("# my edit\n"))
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("a stale stamp answered %v, want FailedPrecondition", err)
	}
	if got, _ := os.ReadFile(path); string(got) != "# theirs\n" {
		t.Errorf("the file holds %q; the other writer's bytes were overwritten", got)
	}
	if _, err := writeKey(p, "notes.md", "", []byte("# blind\n")); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("a write claiming no stamp answered %v, want FailedPrecondition", err)
	}
}

// A body that is not the file's own bytes, whole, does not write back, and
// nothing outside the tree is written; each refusal is a verdict with its
// reason, and the file is untouched.
func TestAWriteRefusesWhatItCannotWrite(t *testing.T) {
	root := t.TempDir()
	files := map[string][]byte{
		"blob.bin":  {0, 1, 2},
		"page.html": []byte("<p>hi</p>"),
		"big.log":   bytes.Repeat([]byte("x"), fsfile.MaxWrite+1),
	}
	for name, data := range files {
		if err := os.WriteFile(filepath.Join(root, name), data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	p := New(root, nil)
	for key, want := range map[string]codes.Code{
		"blob.bin":  codes.InvalidArgument,
		"page.html": codes.InvalidArgument,
		"big.log":   codes.InvalidArgument,
		"../out.md": codes.PermissionDenied,
		"gone.md":   codes.NotFound,
	} {
		stamp := ""
		if _, ok := files[key]; ok {
			stamp = readStamp(t, p, key)
		}
		_, err := writeKey(p, key, stamp, []byte("typed"))
		if status.Code(err) != want || status.Convert(err).Message() == "" {
			t.Errorf("%s: %v, want %v with a reason", key, err, want)
		}
		if data, ok := files[key]; ok {
			if got, _ := os.ReadFile(filepath.Join(root, key)); !bytes.Equal(got, data) {
				t.Errorf("%s was changed by a refused write", key)
			}
		}
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(root), "out.md")); err == nil {
		t.Error("a write outside the root created a file there")
	}
	if err := os.WriteFile(filepath.Join(root, "small.md"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := writeKey(p, "small.md", readStamp(t, p, "small.md"), bytes.Repeat([]byte("x"), fsfile.MaxWrite+1))
	if status.Code(err) != codes.InvalidArgument {
		t.Errorf("a body past the cap answered %v, want InvalidArgument", err)
	}
}

// A file or directory its mode will not let this user write is refused with
// the reason, never replaced by a rename around the mode.
func TestAWriteTheModeForbidsIsPermissionDenied(t *testing.T) {
	if runtime.GOOS == "windows" || os.Getuid() == 0 {
		t.Skip("permissions do not bind here")
	}
	root := t.TempDir()
	path := filepath.Join(root, "ro.md")
	if err := os.WriteFile(path, []byte("# locked\n"), 0o444); err != nil {
		t.Fatal(err)
	}
	p := New(root, nil)
	_, err := writeKey(p, "ro.md", readStamp(t, p, "ro.md"), []byte("typed"))
	if status.Code(err) != codes.PermissionDenied || !strings.Contains(status.Convert(err).Message(), "permission denied") {
		t.Errorf("a read-only file answered %v, want PermissionDenied naming the reason", err)
	}
	if got, _ := os.ReadFile(path); string(got) != "# locked\n" {
		t.Errorf("a read-only file now holds %q", got)
	}

	dir := filepath.Join(root, "sealed")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "f.md"), []byte("# f\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	stamp := readStamp(t, p, "sealed/f.md")
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })
	if _, err := writeKey(p, "sealed/f.md", stamp, []byte("typed")); status.Code(err) != codes.PermissionDenied {
		t.Errorf("a file in a directory that takes no new names answered %v, want PermissionDenied", err)
	}
}

// A stream that breaks before its clean close writes nothing.
func TestABrokenWriteWritesNothing(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "notes.md")
	if err := os.WriteFile(path, []byte("# kept\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	p := New(root, nil)
	s := &writeStream{
		msgs: []*pluginv1.WriteContentRequest{{Key: "notes.md", ContentStamp: readStamp(t, p, "notes.md"), Data: []byte("# half")}},
		end:  errors.New("the caller went away"),
	}
	if err := p.WriteContent(s); err == nil {
		t.Fatal("a broken stream answered no error")
	}
	if got, _ := os.ReadFile(path); string(got) != "# kept\n" {
		t.Errorf("a broken write left %q", got)
	}
}
