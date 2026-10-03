package fsfile

import (
	"errors"
	"path/filepath"
	"strings"
)

// UnderRoot reports whether path lies within root's subtree, root itself
// included. It is the one confinement predicate for the fs plugin's path
// checks, and it uses filepath.Rel rather than a hand-built root+"/" prefix:
// with root "/" that prefix is "//", which no path starts with, so the check
// would refuse everything under a whole-machine root.
func UnderRoot(root, path string) bool {
	rel, err := filepath.Rel(filepath.Clean(root), filepath.Clean(path))
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// ErrOutside is a path that lands, symlinks followed, outside the tree it is
// confined to.
var ErrOutside = errors.New("links outside the root")

// Resolve answers where path lands with every symlink followed, root's own
// included, and ErrOutside when that is not under root. It is the one
// confinement check that sees through links: a name under the root can link
// out of it. A path that does not resolve answers EvalSymlinks' error.
func Resolve(root, path string) (string, error) {
	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", err
	}
	real, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", err
	}
	if !UnderRoot(realRoot, real) {
		return "", ErrOutside
	}
	return real, nil
}
