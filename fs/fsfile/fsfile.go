// Package fsfile is the pure core of the fs projection: every derivation from
// a filename or file bytes, with no database and no tile ids.
package fsfile

import (
	"bytes"
	"errors"
	"fmt"
	"image"
	_ "image/gif"
	"image/jpeg"
	_ "image/png"
	"io"
	iofs "io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/josephburnett/gridwell-plugins/fs/fssource"
	gridwellv1 "github.com/josephburnett/gridwell/api/gen/gridwell/v1"
	"github.com/josephburnett/gridwell/api/rpc"
)

// pageMediaTypes is fs's own table, deliberately not mime.TypeByExtension,
// whose answers vary with the host's mime.types and would make serves_page
// differ between machines. A file is a page when a browser presents it
// natively; everything else still serves raw through the door, with a sniffed
// type, but keeps its document descent.
var pageMediaTypes = map[string]string{
	".png":  "image/png",
	".jpg":  "image/jpeg",
	".jpeg": "image/jpeg",
	".gif":  "image/gif",
	".webp": "image/webp",
	".avif": "image/avif",
	".bmp":  "image/bmp",
	".ico":  "image/x-icon",
	".svg":  "image/svg+xml",
	".html": "text/html; charset=utf-8",
	".htm":  "text/html; charset=utf-8",
	".pdf":  "application/pdf",
	".mp4":  "video/mp4",
	".webm": "video/webm",
	".mp3":  "audio/mpeg",
	".ogg":  "audio/ogg",
	".wav":  "audio/wav",
	".m4a":  "audio/mp4",
	".flac": "audio/flac",
	".css":  "text/css; charset=utf-8",
	".js":   "text/javascript; charset=utf-8",
	".json": "application/json",
	".txt":  "text/plain; charset=utf-8",
}

// ServesPage marks which of the served types are worth a page descent, which
// is the same question as which files are url entries: what a browser presents
// natively as a whole document. A subresource type — css, js, json, txt —
// serves through the door for pages that reference it but stays a text entry
// with its own document descent.
func ServesPage(name string) bool {
	mt := PageMediaType(name)
	switch strings.SplitN(mt, "/", 2)[0] {
	case "image", "video", "audio":
		return true
	}
	return strings.HasPrefix(mt, "text/html") || mt == "application/pdf"
}

// PageMediaType returns the declared page media type for a filename, or
// "" for names outside the table.
func PageMediaType(name string) string {
	return pageMediaTypes[strings.ToLower(filepath.Ext(name))]
}

// plainTextExts marks extensions whose bodies present as plain text:
// monospace, with no markdown interpretation, for source, config, logs, and
// data. It is deliberately a list rather than a sniff, because presentation
// stamps onto every tile row at grid load, which must never stat or read the
// files.
var plainTextExts = map[string]bool{
	".txt": true, ".log": true, ".csv": true, ".tsv": true, ".json": true,
	".yaml": true, ".yml": true, ".toml": true, ".ini": true, ".cfg": true,
	".conf": true, ".env": true, ".xml": true, ".sql": true, ".proto": true,
	".go": true, ".py": true, ".js": true, ".ts": true, ".tsx": true,
	".jsx": true, ".c": true, ".h": true, ".cpp": true, ".hpp": true,
	".cc": true, ".rs": true, ".java": true, ".rb": true, ".sh": true,
	".bash": true, ".zsh": true, ".fish": true, ".pl": true, ".lua": true,
	".css": true, ".scss": true, ".dart": true, ".kt": true, ".swift": true,
	".diff": true, ".patch": true, ".lock": true, ".mod": true, ".sum": true,
	".service": true, ".gitignore": true, ".dockerignore": true,
}

// plainTextNames covers the extensionless classics.
var plainTextNames = map[string]bool{
	"makefile": true, "dockerfile": true, "license": true, "readme": true,
	"changelog": true, "authors": true, "notice": true, "todo": true,
	"vagrantfile": true, "gemfile": true, "rakefile": true, "procfile": true,
	".gitignore": true, ".dockerignore": true, ".gitattributes": true,
	".editorconfig": true, ".profile": true, ".bashrc": true, ".zshrc": true,
}

// Renderable reports whether a name marks content the document renderer
// handles. It is this plugin's renderability rule, and it travels: a
// renderable file's descent body is served as text/markdown with the
// TextPresentationBoth declaration, and the client renders what the wire
// declares. The declaration is the pin — a plugin lives in its own module
// and cannot share a package with the client.
func Renderable(name string) bool {
	n := strings.ToLower(strings.TrimSpace(name))
	return strings.HasSuffix(n, ".md") || strings.HasSuffix(n, ".markdown") || strings.HasSuffix(n, ".org")
}

// TextPresentation declares a file tile's text-body presentation, and Body
// answers in the form it names. The plain-text families show verbatim; every
// other name renders: markdown and org as documents, anything else as its
// metadata summary.
func TextPresentation(name string) string {
	if IsPlainText(name) && !Renderable(name) {
		return rpc.TextPresentationPlain
	}
	return rpc.TextPresentationBoth
}

// IsPlainText reports the plain-text families by name.
func IsPlainText(name string) bool {
	lower := strings.ToLower(name)
	return plainTextExts[filepath.Ext(lower)] || plainTextNames[lower]
}

// PreviewStamp returns the cheap preview generation for a file: an image's
// mtime to the nanosecond, so two saves within a second are two pictures, and
// 0 for everything else. It travels as Entry.preview_stamp, and the
// node keys the tile's face by it until a screenshot exists (see
// pluginhost.faceKey).
func PreviewStamp(dirPath, name string) int64 {
	if dirPath == "" || !strings.HasPrefix(PageMediaType(name), "image/") {
		return 0
	}
	fi, err := os.Stat(filepath.Join(dirPath, name))
	if err != nil {
		return 0
	}
	return fi.ModTime().UnixNano()
}

// ContentStamp names a file's bytes by its mtime to the nanosecond and its
// size, so a write that lands within one mtime tick still moves it unless it
// keeps the size. It travels as Entry.content_stamp and with the bytes
// ReadContent answers, and a write must claim it.
func ContentStamp(mtime time.Time, size int64) string {
	return strconv.FormatInt(mtime.UnixNano(), 10) + "-" + strconv.FormatInt(size, 10)
}

// renderableBodyCap bounds how much of a file the descent body carries: a
// document view, not a file transfer. A file past the cap shows its
// beginning.
const renderableBodyCap = 4 << 20

// Body returns a file's descent body in the media type its TextPresentation
// names: the bytes, up to the cap, of a renderable or plain file, and the
// metadata summary of any other. The summary is markdown that reads as itself
// verbatim, so it holds under either declaration. A missing or unstattable
// file returns (nil, ""), and the caller decides what absence means.
func Body(dirPath, name string) (data []byte, mediaType string) {
	fullPath := filepath.Join(dirPath, name)
	entry, err := fssource.Stat(fullPath)
	if err != nil {
		return nil, ""
	}
	mediaType = "text/markdown"
	if TextPresentation(name) == rpc.TextPresentationPlain {
		mediaType = "text/plain"
	}
	if Renderable(name) || IsPlainText(name) {
		if body, readErr := readHead(fullPath, renderableBodyCap); readErr == nil {
			return body, mediaType
		}
		// Unreadable despite the stat: the metadata summary still tells the
		// user what is here, instead of a blank pane.
	}
	return []byte(fssource.MetadataMarkdown(entry)), mediaType
}

// Unwritable says why a file's body cannot be saved back to it, nil when it
// can: only a body that is the file's own bytes, whole, writes back. A
// summary is not the file, a file past the body cap shows only its head, and
// a page has no body. It is Body's rule read the other way.
func Unwritable(name string, size int64) error {
	switch {
	case ServesPage(name):
		return errors.New("is a page, which has no body to edit")
	case !Renderable(name) && !IsPlainText(name):
		return errors.New("is shown as a summary of the file, not its bytes")
	case size > renderableBodyCap:
		return fmt.Errorf("is larger than the %d MiB a body shows, so its bytes are not all here to save", renderableBodyCap>>20)
	}
	return nil
}

// Denied says why this process may not replace the file at path, nil when it
// may. A save renames a new file in beside it, so the directory's mode binds
// as well as the file's.
func Denied(path string) error {
	if err := writeAccess(path); err != nil {
		return err
	}
	if err := writeAccess(filepath.Dir(path)); err != nil {
		return fmt.Errorf("its directory takes no new files: %w", err)
	}
	return nil
}

// ReadOnly is the sentence Entry.read_only carries for the text file at path
// of size, "" when a write would take its body: Unwritable's rule, then
// Denied's.
func ReadOnly(path string, size int64) string {
	name := filepath.Base(path)
	if err := Unwritable(name, size); err != nil {
		return name + " " + err.Error()
	}
	if err := Denied(path); err != nil {
		return name + " cannot be written: " + err.Error()
	}
	return ""
}

// MaxWrite is the most a write may carry, the body cap: a body longer than
// it would read back cut short.
const MaxWrite = renderableBodyCap

func readHead(path string, limit int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(io.LimitReader(f, limit))
}

// ChunkBytes keeps each content message, served or read, far under gRPC's 4 MiB
// default limit.
const ChunkBytes = 256 * 1024

// ServeChunkSender is the streaming half the caller provides.
type ServeChunkSender interface {
	Send(*gridwellv1.ServeContentChunk) error
}

// ServeFile streams a file's raw bytes as web content. subpath "" is the named
// file itself, confined to root; a non-empty subpath is a page-relative
// resource resolved against the file's directory and confined to that
// directory's subtree, which is the plugin-side guarantee, independent of the
// door's URL grammar. Both confinements see through symlinks (Resolve). A
// named file that links out of the root is refused with that reason, a
// resource that does is a 404 page like any other it cannot have. Absence
// answers a 404 page; a file that is there and cannot be read is an error
// that says why.
func ServeFile(stream ServeChunkSender, root, dirPath, name, subpath string) error {
	target := filepath.Join(dirPath, name)
	served, confine := name, root
	if subpath != "" {
		target = filepath.Join(dirPath, filepath.FromSlash(subpath))
		served, confine = subpath, dirPath
		if !UnderRoot(dirPath, target) {
			return notFoundPage(stream)
		}
	}
	real, err := Resolve(confine, target)
	switch {
	case errors.Is(err, ErrOutside) && subpath == "":
		return status.Errorf(codes.FailedPrecondition, "fs plugin: %s %v", served, err)
	case errors.Is(err, ErrOutside), errors.Is(err, iofs.ErrNotExist), errors.Is(err, syscall.ENOTDIR):
		return notFoundPage(stream)
	case err != nil:
		return unreadable(served, err)
	}
	f, err := os.Open(real)
	if errors.Is(err, iofs.ErrNotExist) || errors.Is(err, syscall.ENOTDIR) {
		return notFoundPage(stream)
	}
	if err != nil {
		return unreadable(served, err)
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return unreadable(served, err)
	}
	if fi.IsDir() {
		return notFoundPage(stream)
	}

	mediaType := PageMediaType(served)
	buf := make([]byte, ChunkBytes)
	n, readErr := io.ReadFull(f, buf)
	if readErr != nil && readErr != io.EOF && readErr != io.ErrUnexpectedEOF {
		return readErr
	}
	if mediaType == "" {
		// The door sets X-Content-Type-Options: nosniff, so this server-side
		// sniff is the only one that happens.
		mediaType = http.DetectContentType(buf[:n])
	}
	if err := stream.Send(&gridwellv1.ServeContentChunk{Status: 200, MediaType: mediaType, Data: buf[:n]}); err != nil {
		return err
	}
	for readErr == nil {
		n, readErr = io.ReadFull(f, buf)
		if n > 0 {
			if err := stream.Send(&gridwellv1.ServeContentChunk{Data: buf[:n]}); err != nil {
				return err
			}
		}
	}
	if readErr == io.EOF || readErr == io.ErrUnexpectedEOF {
		return nil
	}
	return readErr
}

func unreadable(served string, err error) error {
	code := codes.Internal
	if errors.Is(err, iofs.ErrPermission) {
		code = codes.PermissionDenied
	}
	return status.Errorf(code, "fs plugin: cannot read %s: %v", served, err)
}

func notFoundPage(stream ServeChunkSender) error {
	return stream.Send(&gridwellv1.ServeContentChunk{
		Status:    404,
		MediaType: "text/plain; charset=utf-8",
		Data:      []byte("not found"),
	})
}

const (
	// previewMaxEdge bounds the thumbnail: previews are small tiles, and the
	// full image is always one descent away through the /content/ door.
	previewMaxEdge = 512
	// previewFileCap skips very large files rather than decode them on every
	// grid paint. Past it the tile falls back to its label, like a url tile
	// with no capture yet.
	previewFileCap = 64 << 20
)

// PreviewJPEG returns a bounded JPEG thumbnail for an image file, or nil for a
// non-image, oversized, or undecodable file. The caller treats nil as "no
// preview", never an error.
func PreviewJPEG(dirPath, name string) []byte {
	if !strings.HasPrefix(PageMediaType(name), "image/") {
		return nil
	}
	full := filepath.Join(dirPath, name)
	if fi, err := os.Stat(full); err != nil || fi.Size() > previewFileCap {
		return nil
	}
	data, err := os.ReadFile(full)
	if err != nil {
		return nil
	}
	return thumbnailJPEG(data)
}

// thumbnailJPEG decodes any stdlib-supported image — png, jpeg, gif — and
// re-encodes a bounded JPEG. Undecodable input returns nil.
func thumbnailJPEG(data []byte) []byte {
	img, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil
	}
	img = downscale(img, previewMaxEdge)
	var out bytes.Buffer
	if err := jpeg.Encode(&out, img, &jpeg.Options{Quality: 80}); err != nil {
		return nil
	}
	return out.Bytes()
}

// downscale bounds the longest edge to maxEdge with nearest-neighbor
// sampling: a preview, not an archival resize, and stdlib-only on purpose.
func downscale(img image.Image, maxEdge int) image.Image {
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	if w <= maxEdge && h <= maxEdge {
		return img
	}
	scale := float64(maxEdge) / float64(w)
	if h > w {
		scale = float64(maxEdge) / float64(h)
	}
	nw, nh := int(float64(w)*scale), int(float64(h)*scale)
	if nw < 1 {
		nw = 1
	}
	if nh < 1 {
		nh = 1
	}
	dst := image.NewRGBA(image.Rect(0, 0, nw, nh))
	for y := 0; y < nh; y++ {
		sy := b.Min.Y + y*h/nh
		for x := 0; x < nw; x++ {
			sx := b.Min.X + x*w/nw
			dst.Set(x, y, img.At(sx, sy))
		}
	}
	return dst
}
