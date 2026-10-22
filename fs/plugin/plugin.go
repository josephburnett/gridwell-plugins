// Package plugin is the fs content plugin: a stateless projection of a
// directory tree. Keys are slash-relative paths under the configured root, and
// a directory's key is its grid, "." the root's. Every derivation and
// byte-level answer comes from fs/fsfile. There is no database, no
// ids, and no layout; the node owns those.
package plugin

import (
	"context"
	"errors"
	"fmt"
	"io"
	iofs "io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/josephburnett/gridwell-plugins/fs/fsfile"
	"github.com/josephburnett/gridwell-plugins/fs/fssource"
	"github.com/josephburnett/gridwell-plugins/fs/trash"
	pluginv1 "github.com/josephburnett/gridwell/api/gen/plugin/v1"
)

// Host is the destructive side-effect surface, injected so tests never touch
// real files.
type Host interface {
	Trash(path string) error
}

type trashHost struct{}

func (trashHost) Trash(p string) error { return trash.Trash(p) }

// Plugin implements pluginv1.PluginServer for one directory root.
type Plugin struct {
	pluginv1.UnimplementedPluginServer
	root  string
	host  Host
	watch *watcher
	// served latches the first Info that found the root readable. The check
	// is a verdict on the config, so it stops there: a root that goes
	// unreadable later is a dark source, which reads answer as weather while
	// the node serves its rows.
	served atomic.Bool
}

// FromConfig builds the production plugin from the shared config vocabulary.
// It is the one owner of the config-to-plugin derivation, so the subprocess
// main and a bundled binary compose exactly the same plugin. The config key is
// root, the projected directory. No root means the plugin declares no
// grid — listed, contributing nothing to the (+) menu — rather than a
// refusal.
func FromConfig(cfg map[string]string) (pluginv1.PluginServer, error) {
	return New(strings.TrimSpace(cfg["root"]), nil), nil
}

// New builds a plugin over root. A nil host trashes, as production does; tests
// inject a recorder.
func New(root string, host Host) *Plugin {
	if host == nil {
		host = trashHost{}
	}
	return &Plugin{root: filepath.Clean(root), host: host, watch: newWatcher()}
}

// errOutside is a key that names nothing in the tree: it escapes the root by
// name, or a directory on its way is a symlink, which this plugin lists as a
// link and never enters (linkEntry). No listing holds such a key, so it
// probes gone, and a call for its bytes is refused with the reason.
var errOutside = errors.New("is not in the tree under the root")

func escapes(clean string) bool {
	return clean == ".." || strings.HasPrefix(clean, "../") || path.IsAbs(clean)
}

// realRoot is the root with its own symlinks resolved, the base every key is
// spelled from. A root that is not there answers its configured path, so a
// listing finds it gone.
func (p *Plugin) realRoot() (string, error) {
	root, err := filepath.EvalSymlinks(p.root)
	if gone(err) {
		return p.root, nil
	}
	return root, err
}

// abs maps a key to its path under the real root, errOutside unless every
// directory on the way is a real one. The last element may be anything, a
// link included, so Probe and Delete act on a link itself.
func (p *Plugin) abs(key string) (string, error) {
	clean := path.Clean(key)
	if escapes(clean) {
		return "", errOutside
	}
	root, err := p.realRoot()
	if err != nil {
		return "", err
	}
	if dir := path.Dir(clean); dir != "." {
		at := root
		for _, part := range strings.Split(dir, "/") {
			at = filepath.Join(at, part)
			fi, err := os.Lstat(at)
			switch {
			case gone(err):
				return filepath.Join(root, filepath.FromSlash(clean)), nil
			case err != nil:
				return "", err
			case fi.Mode()&os.ModeSymlink != 0:
				return "", errOutside
			}
		}
	}
	return filepath.Join(root, filepath.FromSlash(clean)), nil
}

// lstat is abs and the Lstat of what it maps to.
func (p *Plugin) lstat(key string) (string, error) {
	full, err := p.abs(key)
	if err == nil {
		_, err = os.Lstat(full)
	}
	return full, err
}

// dir is abs for a grid, which must not be a link itself either.
func (p *Plugin) dir(gridKey string) (string, error) {
	full, err := p.abs(gridKey)
	if err != nil {
		return "", err
	}
	if fi, err := os.Lstat(full); err == nil && fi.Mode()&os.ModeSymlink != 0 {
		return "", errOutside
	}
	return full, nil
}

// content is where a key's bytes live: its path with every link followed,
// fsfile.ErrOutside when that leaves the root.
func (p *Plugin) content(key string) (string, error) {
	full, err := p.abs(key)
	if err != nil {
		return "", err
	}
	return fsfile.Resolve(p.root, full)
}

// refusal answers a content verb for a key it cannot read: one outside the
// tree is a verdict with the reason, anything else a source that cannot
// answer right now.
func refusal(key string, err error) error {
	if errors.Is(err, errOutside) || errors.Is(err, fsfile.ErrOutside) {
		return status.Errorf(codes.FailedPrecondition, "fs plugin: %q %v", key, err)
	}
	return status.Errorf(codes.Unavailable, "fs plugin: read %s: %v", key, pathErr(err))
}

func (p *Plugin) Info(context.Context, *pluginv1.InfoRequest) (*pluginv1.InfoResponse, error) {
	resp := &pluginv1.InfoResponse{
		Kind:        "fs",
		DisplayName: "files",
		Glyph:       "folder",
		// A directory tree is host state: its rows summarize files that live
		// outside Gridwell (InfoResponse.host_content).
		HostContent: true,
	}
	// No configured root: no grid (see FromConfig).
	if p.root == "" || p.root == "." {
		return resp, nil
	}
	if !p.served.Load() {
		if err := readableDir(p.root); err != nil {
			return nil, status.Error(codes.FailedPrecondition, err.Error())
		}
		p.served.Store(true)
	}
	// The one grid this plugin serves: the configured tree. It
	// declares no label, so the swatch reads as the configured instance.
	resp.Grids = []*pluginv1.PluginGrid{{Id: ".", Grid: "."}}
	resp.Watch = true
	resp.Writable = true
	resp.Move = true
	if label := filepath.Base(p.root); label != "/" && label != "." {
		resp.DisplayName = label
	}
	return resp, nil
}

// Find is refused: a file tree finds nothing (docs/plugin-standard.md
// rule 20), since the plugin holds no cache to look in and a walk of the
// disk for a query is work done for nobody.
func (p *Plugin) Find(context.Context, *pluginv1.FindRequest) (*pluginv1.FindResponse, error) {
	return nil, status.Error(codes.Unimplemented, "fs plugin: a file tree finds nothing")
}

// readableDir is the check Info makes until it passes, so a root created or
// fixed after launch is served without a respawn. Its error is the sentence
// the node shows on the plugin's row.
func readableDir(root string) error {
	fi, err := os.Stat(root)
	switch {
	case errors.Is(err, iofs.ErrNotExist):
		return fmt.Errorf("root %q does not exist", root)
	case err != nil:
		return fmt.Errorf("root %q cannot be read: %v", root, pathErr(err))
	case !fi.IsDir():
		return fmt.Errorf("root %q is not a directory", root)
	}
	f, err := os.Open(root)
	if err == nil {
		_, err = f.Readdirnames(1)
		_ = f.Close()
		if errors.Is(err, io.EOF) {
			err = nil
		}
	}
	if err != nil {
		return fmt.Errorf("root %q cannot be read: %v", root, pathErr(err))
	}
	return nil
}

// pathErr drops the operation and path a *PathError repeats, since the
// sentence around it already names the root.
func pathErr(err error) error {
	var pe *iofs.PathError
	if errors.As(err, &pe) {
		return pe.Err
	}
	return err
}

// gone reports whether err says definitively that the path is not there: it
// does not exist, or something on the way is a file. Any other error is a
// source that cannot answer right now, which reads as Unavailable.
func gone(err error) bool {
	return errors.Is(err, iofs.ErrNotExist) || errors.Is(err, syscall.ENOTDIR)
}

// List enumerates one directory grid. A directory that is gone, is now a
// file, or is not in the tree (errOutside) is an authoritative empty listing;
// one that cannot be read answers Unavailable, and the node serves its rows
// with the source dark.
func (p *Plugin) List(_ context.Context, req *pluginv1.ListRequest) (*pluginv1.ListResponse, error) {
	dir, err := p.dir(req.Grid)
	if errors.Is(err, errOutside) {
		return &pluginv1.ListResponse{Authoritative: true}, nil
	}
	if err != nil {
		return nil, status.Errorf(codes.Unavailable, "fs plugin: read %s: %v", req.Grid, pathErr(err))
	}
	entries, readErr := fssource.Read(dir)
	if readErr != nil {
		if gone(readErr) {
			return &pluginv1.ListResponse{Authoritative: true, SourceLabel: dir}, nil
		}
		return nil, status.Errorf(codes.Unavailable, "fs plugin: read %s: %v", dir, readErr)
	}
	root, err := p.realRoot()
	if err != nil {
		return nil, status.Errorf(codes.Unavailable, "fs plugin: read %s: %v", dir, pathErr(err))
	}
	resp := &pluginv1.ListResponse{Authoritative: true, SourceLabel: dir}
	for _, e := range entries {
		resp.Entries = append(resp.Entries, entryOf(req.Grid, dir, root, e))
	}
	return resp, nil
}

// entryOf is what List answers for e, read from dir, which grid names
// under the real root: the one owner, so a Watch's EntryChanged carries the
// entry a listing would.
func entryOf(gridKey, dir, root string, e fssource.Entry) *pluginv1.Entry {
	key := keyIn(gridKey, e.Name)
	out := &pluginv1.Entry{Key: key, Label: e.Name}
	switch e.Kind {
	case fssource.KindDir:
		out.Kind = "door"
		out.ChildGrid = key
	case fssource.KindLink:
		linkEntry(out, root, e)
	default:
		fileFacts(out, dir, e.Name)
		if out.Kind == "text" {
			out.ContentStamp = fsfile.ContentStamp(e.ModTime, e.Size)
			out.ReadOnly = fsfile.ReadOnly(filepath.Join(dir, e.Name), e.Size)
		}
	}
	return out
}

// keyIn is the key of the thing called name in the directory gridKey names.
func keyIn(gridKey, name string) string {
	if gridKey == "." || gridKey == "" {
		return name
	}
	return gridKey + "/" + name
}

// fileFacts declares a file's content facts from its name. dir "" is a file
// this plugin will not read, which has no picture.
func fileFacts(out *pluginv1.Entry, dir, name string) {
	out.PreviewStamp = fsfile.PreviewStamp(dir, name)
	if fsfile.ServesPage(name) {
		// A file the browser presents whole is a url entry, and the node
		// derives its address at the /content/ API, so there is none to
		// declare. It carries no text body: a url entry has no document
		// preview beside the page.
		out.Kind = "url"
		out.ServesPage = true
		return
	}
	out.Kind = "text"
	out.TextPresentation = fsfile.TextPresentation(name)
}

// linkEntry lists a symlink as a link to where it lands, so a thing reached
// two ways is one key: a directory as a door onto the directory's own
// grid, so a link back up the tree is one grid rather than an endless
// descent, and a file as an entry whose link_target is the file's key. A link
// that lands outside the root, or nowhere, targets the key of where it points,
// which no listing holds, so the node reads it dead.
func linkEntry(out *pluginv1.Entry, root string, e fssource.Entry) {
	target := "../" + e.Name
	if rel, err := filepath.Rel(root, e.Target); err == nil {
		target = filepath.ToSlash(rel)
	}
	reachable := e.TargetKind != "" && !escapes(target)
	if reachable && e.TargetKind == fssource.KindDir {
		out.Kind = "door"
		out.ChildGrid = target
		return
	}
	// The content facts are the target's, for a node that predates
	// link_target and presents the entry as content of its own.
	if reachable {
		fileFacts(out, filepath.Dir(e.Target), filepath.Base(e.Target))
	} else {
		fileFacts(out, "", e.Name)
	}
	out.LinkTarget = &pluginv1.EntryRef{Grid: path.Dir(target), Key: target}
}

func (p *Plugin) ReadContent(req *pluginv1.ReadContentRequest, stream pluginv1.Plugin_ReadContentServer) error {
	real, err := p.content(req.Key)
	var fi os.FileInfo
	if err == nil {
		fi, err = os.Stat(real)
	}
	if err != nil && !gone(err) {
		return refusal(req.Key, err)
	}
	if err != nil || fi.IsDir() {
		// A directory or a vanished file has no document body.
		return stream.Send(&pluginv1.ContentChunk{})
	}
	// The stamp is taken before the read, so bytes written during it read
	// as a stamp they do not match, never as one they do.
	data, mediaType := fsfile.Body(filepath.Dir(real), filepath.Base(real))
	chunk := &pluginv1.ContentChunk{MediaType: mediaType, ContentStamp: fsfile.ContentStamp(fi.ModTime(), fi.Size())}
	for {
		n := min(len(data), fsfile.ChunkBytes)
		chunk.Data, data = data[:n], data[n:]
		if err := stream.Send(chunk); err != nil || len(data) == 0 {
			return err
		}
		chunk = &pluginv1.ContentChunk{}
	}
}

// WriteContent replaces a text file's bytes, the whole file, by a temp file
// in its directory renamed over it, so a reader sees the old bytes or the
// new and a broken stream writes nothing. The write claims the stamp its
// bytes were read under, and a file whose stamp has moved is refused as a
// conflict, so bytes the writer has not seen are never overwritten. Every
// refusal is a verdict with its reason; an I/O failure is Unavailable.
func (p *Plugin) WriteContent(stream pluginv1.Plugin_WriteContentServer) error {
	first, err := stream.Recv()
	if err != nil {
		return status.Error(codes.InvalidArgument, "fs plugin: write: empty stream")
	}
	data := append([]byte(nil), first.Data...)
	for len(data) <= fsfile.MaxWrite {
		msg, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		data = append(data, msg.Data...)
	}
	if len(data) > fsfile.MaxWrite {
		return status.Errorf(codes.InvalidArgument, "fs plugin: %q: a body is at most %d MiB", first.Key, fsfile.MaxWrite>>20)
	}
	stamp, err := p.write(first.Key, first.ContentStamp, data)
	if err != nil {
		return err
	}
	return stream.SendAndClose(&pluginv1.WriteContentResponse{ContentStamp: stamp})
}

// write puts data in place of key's file if claimed is its stamp, and
// answers the stamp of the bytes written.
func (p *Plugin) write(key, claimed string, data []byte) (string, error) {
	real, err := p.content(key)
	var fi os.FileInfo
	if err == nil {
		fi, err = os.Stat(real)
	}
	switch {
	case errors.Is(err, errOutside) || errors.Is(err, fsfile.ErrOutside):
		return "", status.Errorf(codes.PermissionDenied, "fs plugin: %q %v, and nothing outside it is written", key, err)
	case gone(err):
		return "", status.Errorf(codes.NotFound, "fs plugin: %q is gone", key)
	case err != nil:
		return "", status.Errorf(codes.Unavailable, "fs plugin: write %s: %v", key, pathErr(err))
	case fi.IsDir():
		return "", status.Errorf(codes.InvalidArgument, "fs plugin: %q is a directory", key)
	}
	if err := fsfile.Unwritable(filepath.Base(real), fi.Size()); err != nil {
		return "", status.Errorf(codes.InvalidArgument, "fs plugin: %q %v", key, err)
	}
	if now := fsfile.ContentStamp(fi.ModTime(), fi.Size()); claimed != now {
		return "", status.Errorf(codes.FailedPrecondition, "fs plugin: %q changed on disk since it was read", key)
	}
	// The rename would replace a file its mode refuses writes to.
	if err := fsfile.Denied(real); err != nil {
		return "", writeRefusal(key, err)
	}
	return replace(real, fi.Mode().Perm(), data, key)
}

// replace writes data beside real and renames it over real, answering the
// stamp of what was written.
func replace(real string, perm os.FileMode, data []byte, key string) (string, error) {
	tmp, err := os.CreateTemp(filepath.Dir(real), "."+filepath.Base(real)+".gridwell-*")
	if err != nil {
		return "", writeRefusal(key, err)
	}
	keep := false
	defer func() {
		if !keep {
			_ = os.Remove(tmp.Name())
		}
	}()
	_, err = tmp.Write(data)
	if err == nil {
		err = tmp.Sync()
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Chmod(tmp.Name(), perm)
	}
	var fi os.FileInfo
	if err == nil {
		fi, err = os.Stat(tmp.Name())
	}
	if err == nil {
		err = os.Rename(tmp.Name(), real)
	}
	if err != nil {
		return "", writeRefusal(key, err)
	}
	keep = true
	return fsfile.ContentStamp(fi.ModTime(), fi.Size()), nil
}

// writeRefusal answers a failed write: a permission the file or its
// directory refuses is a verdict with that reason, anything else a source
// that cannot answer right now.
func writeRefusal(key string, err error) error {
	if errors.Is(err, iofs.ErrPermission) || errors.Is(err, syscall.EROFS) {
		return status.Errorf(codes.PermissionDenied, "fs plugin: %q cannot be written: %v", key, pathErr(err))
	}
	return status.Errorf(codes.Unavailable, "fs plugin: write %s: %v", key, pathErr(err))
}

func (p *Plugin) ServeContent(req *pluginv1.ServeContentRequest, stream pluginv1.Plugin_ServeContentServer) error {
	full, err := p.abs(req.Key)
	if err != nil {
		return refusal(req.Key, err)
	}
	if fi, statErr := os.Lstat(full); statErr == nil && fi.IsDir() {
		return status.Error(codes.NotFound, "fs plugin: directories serve no page")
	}
	return fsfile.ServeFile(stream, p.root, filepath.Dir(full), filepath.Base(full), req.Subpath)
}

func (p *Plugin) GetPreview(_ context.Context, req *pluginv1.GetPreviewRequest) (*pluginv1.GetPreviewResponse, error) {
	real, err := p.content(req.Key)
	switch {
	case gone(err):
		return &pluginv1.GetPreviewResponse{}, nil
	case err != nil:
		return nil, refusal(req.Key, err)
	}
	return &pluginv1.GetPreviewResponse{Jpeg: fsfile.PreviewJPEG(filepath.Dir(real), filepath.Base(real))}, nil
}

func (p *Plugin) Probe(_ context.Context, req *pluginv1.ProbeRequest) (*pluginv1.ProbeResponse, error) {
	_, err := p.lstat(req.Key)
	switch {
	case err == nil:
		return &pluginv1.ProbeResponse{Presence: pluginv1.ProbeResponse_PRESENCE_PRESENT}, nil
	case gone(err) || errors.Is(err, errOutside):
		return &pluginv1.ProbeResponse{Presence: pluginv1.ProbeResponse_PRESENCE_GONE}, nil
	default:
		return &pluginv1.ProbeResponse{Presence: pluginv1.ProbeResponse_PRESENCE_UNSPECIFIED}, nil
	}
}

// Delete moves the source path to the trash, through Host; a link goes, never
// what it points at. An already-gone path succeeds: the delete gesture is
// idempotent. A key not in the tree is never listed, so asking to delete one
// is a bug or an attack, refused loudly.
func (p *Plugin) Delete(_ context.Context, req *pluginv1.DeleteRequest) (*pluginv1.DeleteResponse, error) {
	full, err := p.lstat(req.Key)
	switch {
	case errors.Is(err, errOutside):
		return nil, status.Errorf(codes.InvalidArgument, "fs plugin: key %q %v", req.Key, err)
	case gone(err):
		return &pluginv1.DeleteResponse{}, nil
	case err != nil:
		return nil, status.Errorf(codes.Unavailable, "fs plugin: delete %s: %v", req.Key, pathErr(err))
	}
	if err := p.host.Trash(full); err != nil {
		return nil, status.Errorf(codes.Internal, "fs plugin: remove %s: %v", full, err)
	}
	return &pluginv1.DeleteResponse{}, nil
}

// Move renames key's file, directory or link into to_grid's directory under
// its name and answers its key there. It replaces nothing: a taken name, a
// gone source and an OS refusal are FailedPrecondition; a key outside its
// grid or a target that is not a directory of the tree is InvalidArgument.
func (p *Plugin) Move(_ context.Context, req *pluginv1.MoveRequest) (*pluginv1.MoveResponse, error) {
	refuse := func(c codes.Code, why string, a ...any) error {
		return status.Errorf(c, "fs plugin: %q cannot move to %q: "+why, append([]any{req.Key, req.ToGrid}, a...)...)
	}
	clean := path.Clean(req.Key)
	if clean == "." || path.Dir(clean) != path.Clean(req.Grid) {
		return nil, refuse(codes.InvalidArgument, "it is not in %q", req.Grid)
	}
	from, err := p.lstat(clean)
	who, dir, fi := "it", "", os.FileInfo(nil)
	if err == nil {
		who = "that directory"
		if dir, err = p.dir(req.ToGrid); err == nil {
			fi, err = os.Stat(dir)
		}
	}
	switch {
	case gone(err):
		return nil, refuse(codes.FailedPrecondition, who+" is gone")
	case errors.Is(err, errOutside):
		return nil, refuse(codes.InvalidArgument, who+" %v", err)
	case err != nil:
		return nil, refuse(codes.FailedPrecondition, "%v", pathErr(err))
	case !fi.IsDir():
		return nil, refuse(codes.InvalidArgument, "that is not a directory")
	case strings.HasPrefix(path.Clean(req.ToGrid)+"/", clean+"/"):
		return nil, refuse(codes.InvalidArgument, "a directory cannot move into itself")
	}
	to := filepath.Join(dir, filepath.Base(from))
	_, err = os.Lstat(to)
	switch {
	case err == nil:
		return nil, refuse(codes.FailedPrecondition, "%q is already there", path.Base(clean))
	case gone(err):
		err = os.Rename(from, to)
	}
	if err != nil {
		return nil, refuse(codes.FailedPrecondition, "%v", errors.Unwrap(err))
	}
	return &pluginv1.MoveResponse{Key: keyIn(req.ToGrid, path.Base(clean))}, nil
}
