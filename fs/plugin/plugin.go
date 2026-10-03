// Package plugin is the fs content plugin: a stateless projection of a
// directory tree. Keys are slash-relative paths under the configured root, and
// a directory's key is its context, "." the root's. Every derivation and
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
	gridwellv1 "github.com/josephburnett/gridwell/api/gen/gridwell/v1"
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
// collection — listed, contributing nothing to the (+) menu — rather than a
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

// dir is abs for a context, which must not be a link itself either.
func (p *Plugin) dir(context string) (string, error) {
	full, err := p.abs(context)
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
		// A directory tree is host state this plugin projects: its rows are
		// summaries of files that live outside Gridwell. Declaring it is what
		// earns the grids their host treatment on the client — the node has
		// no list of host-backed kinds to consult.
		HostContent: true,
	}
	// No configured root means there is no context to serve, so the plugin
	// declares no collection and contributes nothing to the (+) menu.
	if p.root == "" || p.root == "." {
		return resp, nil
	}
	if !p.served.Load() {
		if err := readableDir(p.root); err != nil {
			return nil, status.Error(codes.FailedPrecondition, err.Error())
		}
		p.served.Store(true)
	}
	// The one collection this plugin serves: the configured tree. It
	// declares no label, so the swatch reads as the configured instance.
	resp.MenuEntries = []*pluginv1.MenuEntry{{Id: ".", Context: "."}}
	resp.Watch = true
	if label := filepath.Base(p.root); label != "/" && label != "." {
		resp.DisplayName = label
	}
	return resp, nil
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

// List enumerates one directory context. A directory that is gone, is now a
// file, or is not in the tree (errOutside) is an authoritative empty listing;
// one that cannot be read answers Unavailable, and the node serves its rows
// with the source dark.
func (p *Plugin) List(_ context.Context, req *pluginv1.ListRequest) (*pluginv1.ListResponse, error) {
	dir, err := p.dir(req.Context)
	if errors.Is(err, errOutside) {
		return &pluginv1.ListResponse{Authoritative: true}, nil
	}
	if err != nil {
		return nil, status.Errorf(codes.Unavailable, "fs plugin: read %s: %v", req.Context, pathErr(err))
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
		key := e.Name
		if req.Context != "." && req.Context != "" {
			key = req.Context + "/" + e.Name
		}
		out := &pluginv1.Entry{Key: key, Label: e.Name}
		switch e.Kind {
		case fssource.KindDir:
			out.Kind = "well"
			out.ChildContext = key
		case fssource.KindLink:
			linkEntry(out, root, e)
		default:
			fileFacts(out, dir, e.Name)
		}
		resp.Entries = append(resp.Entries, out)
	}
	return resp, nil
}

// fileFacts declares a file's content facts from its name. dir "" is a file
// this plugin will not read, which has no picture.
func fileFacts(out *pluginv1.Entry, dir, name string) {
	out.PreviewStamp = fsfile.PreviewStamp(dir, name)
	if fsfile.ServesPage(name) {
		// A file the browser presents whole is a url entry, and the node
		// derives its address at the /content/ door, so there is none to
		// declare. It carries no text body: a url entry has no document
		// face beside the page.
		out.Kind = "url"
		out.ServesPage = true
		return
	}
	out.Kind = "text"
	out.TextPresentation = fsfile.TextPresentation(name)
}

// linkEntry lists a symlink as a link to where it lands, so a thing reached
// two ways is one key: a directory as a well onto the directory's own
// context, so a link back up the tree is one grid rather than an endless
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
		out.Kind = "well"
		out.ChildContext = target
		return
	}
	// The content facts are the target's, for a node that predates
	// link_target and presents the entry as content of its own.
	if reachable {
		fileFacts(out, filepath.Dir(e.Target), filepath.Base(e.Target))
	} else {
		fileFacts(out, "", e.Name)
	}
	out.LinkTarget = &pluginv1.EntryRef{Context: path.Dir(target), Key: target}
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
	data, mediaType := fsfile.Body(filepath.Dir(real), filepath.Base(real))
	return stream.Send(&pluginv1.ContentChunk{Data: data, MediaType: mediaType})
}

// serveStream adapts the plugin chunk stream to fsfile's sender; the two chunk
// shapes match field for field.
type serveStream struct {
	s pluginv1.Plugin_ServeContentServer
}

func (w serveStream) Send(c *gridwellv1.ServeContentChunk) error {
	return w.s.Send(&pluginv1.ServeContentChunk{Status: c.Status, MediaType: c.MediaType, Data: c.Data})
}

func (p *Plugin) ServeContent(req *pluginv1.ServeContentRequest, stream pluginv1.Plugin_ServeContentServer) error {
	full, err := p.abs(req.Key)
	if err != nil {
		return refusal(req.Key, err)
	}
	if fi, statErr := os.Lstat(full); statErr == nil && fi.IsDir() {
		return status.Error(codes.NotFound, "fs plugin: directories serve no page")
	}
	return fsfile.ServeFile(serveStream{stream}, p.root, filepath.Dir(full), filepath.Base(full), req.Subpath)
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
	full, err := p.abs(req.Key)
	if errors.Is(err, errOutside) {
		return &pluginv1.ProbeResponse{Presence: pluginv1.ProbeResponse_PRESENCE_GONE}, nil
	}
	if err == nil {
		_, err = os.Lstat(full)
	}
	switch {
	case err == nil:
		return &pluginv1.ProbeResponse{Presence: pluginv1.ProbeResponse_PRESENCE_PRESENT}, nil
	case gone(err):
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
	full, err := p.abs(req.Key)
	if errors.Is(err, errOutside) {
		return nil, status.Errorf(codes.InvalidArgument, "fs plugin: key %q %v", req.Key, err)
	}
	if err == nil {
		_, err = os.Lstat(full)
	}
	switch {
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
