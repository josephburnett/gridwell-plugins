// Package fssource reads a host directory and projects its contents into the
// abstract entries an fs grid is reconciled against. It is pure Go, with no
// database and no Gridwell types; the fs plugin turns these entries into
// listing entries and the node mints the tile rows.
package fssource

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// EntryKind classifies a directory entry. The string values are internal
// markers; callers compare by constant.
type EntryKind string

const (
	KindDir  EntryKind = "dir"
	KindFile EntryKind = "file"
	// KindLink is a symlink, listed as itself and never followed.
	KindLink EntryKind = "link"
)

// Entry is one item from a directory listing. AbsPath is parent + "/" + Name.
// Size and ModTime are the entry's own, from Lstat.
type Entry struct {
	Name    string
	AbsPath string
	Kind    EntryKind
	Size    int64
	ModTime time.Time
	// Target is where a link lands with every symlink followed. A link that
	// does not land (TargetKind "") names where it points instead; see
	// brokenTarget.
	Target     string
	TargetKind EntryKind
}

// Read lists dir, sorts results alphabetically (so the auto-grid layout
// is deterministic), and returns Entry values. Hidden files (dotfiles)
// are included. Returns the underlying error if dir cannot be read.
func Read(dir string) ([]Entry, error) {
	dir = filepath.Clean(dir)
	f, err := os.Open(dir)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", dir, err)
	}
	defer f.Close()
	dirents, err := f.Readdir(-1)
	if err != nil {
		return nil, fmt.Errorf("readdir %s: %w", dir, err)
	}
	out := make([]Entry, 0, len(dirents))
	for _, info := range dirents {
		e := entryFromFileInfo(dir, info)
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// entryFromFileInfo projects one Lstat answer into an Entry.
func entryFromFileInfo(dir string, info os.FileInfo) Entry {
	name := info.Name()
	e := Entry{
		Name:    name,
		AbsPath: filepath.Join(dir, name),
		Kind:    KindFile,
		Size:    info.Size(),
		ModTime: info.ModTime(),
	}
	switch mode := info.Mode(); {
	case mode&os.ModeSymlink != 0:
		e.Kind = KindLink
		e.Target, e.TargetKind = linkTarget(e.AbsPath)
	case mode.IsDir():
		e.Kind = KindDir
	}
	return e
}

func linkTarget(link string) (string, EntryKind) {
	real, err := filepath.EvalSymlinks(link)
	if err != nil {
		return brokenTarget(link), ""
	}
	fi, err := os.Stat(real)
	switch {
	case err != nil:
		return brokenTarget(link), ""
	case fi.IsDir():
		return real, KindDir
	default:
		return real, KindFile
	}
}

// brokenTarget is the path a link that does not land points at, one hop, with
// its directory resolved: the link goes live when that path appears. When
// that path is itself a link, the chain loops or dangles further on, and
// naming it would make this link a link to a link; the answer is then the
// path through the link itself, which never resolves.
func brokenTarget(link string) string {
	through := filepath.Join(link, filepath.Base(link))
	dest, err := os.Readlink(link)
	if err != nil {
		return through
	}
	if !filepath.IsAbs(dest) {
		dest = filepath.Join(filepath.Dir(link), dest)
	}
	if dir, err := filepath.EvalSymlinks(filepath.Dir(dest)); err == nil {
		dest = filepath.Join(dir, filepath.Base(dest))
	}
	if _, err := os.Lstat(dest); err == nil {
		return through
	}
	return filepath.Clean(dest)
}

// Stat builds an Entry for a single path: the single-item counterpart to Read,
// used to regenerate a file tile's metadata body lazily. A path listed by
// Read and stat'd here project identically.
func Stat(path string) (Entry, error) {
	path = filepath.Clean(path)
	info, err := os.Lstat(path)
	if err != nil {
		return Entry{}, fmt.Errorf("lstat %s: %w", path, err)
	}
	return entryFromFileInfo(filepath.Dir(path), info), nil
}

// MetadataMarkdown returns a small markdown blob describing one Entry: the
// descent body for a file tile whose own bytes are not rendered. The output is
// deterministic, so the blob hash dedupes for unchanged files.
func MetadataMarkdown(e Entry) string {
	var b strings.Builder
	b.WriteString("# ")
	b.WriteString(e.Name)
	b.WriteString("\n\n")
	if e.Kind == KindDir {
		b.WriteString("directory\n\n")
	}
	fmt.Fprintf(&b, "- path: `%s`\n", e.AbsPath)
	fmt.Fprintf(&b, "- size: %d bytes\n", e.Size)
	fmt.Fprintf(&b, "- modified: %s\n", e.ModTime.UTC().Format(time.RFC3339))
	return b.String()
}
