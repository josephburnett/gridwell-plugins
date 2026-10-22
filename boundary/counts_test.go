package boundary

import (
	"bytes"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The production tree is measured as gridwell's test/boundary measures its
// own (isProduction, productionLines, productionPackages there): the
// definition is copied, not the code, since a test package of another
// repository does not import. Every .go and .ts file, pruning .git,
// node_modules, vendor and any directory that is its own checkout, less test
// files and the paths notProduction names.

func repoRoot(t *testing.T) string {
	t.Helper()
	root, err := pluginRoot()
	if err != nil {
		t.Fatal(err)
	}
	return root
}

func isTestFile(rel string) bool {
	return strings.HasSuffix(rel, "_test.go") || strings.HasSuffix(rel, ".test.ts") || strings.HasSuffix(rel, ".spec.ts")
}

// notProduction is generated code, build output, harnesses and test
// scaffolding.
var notProduction = []string{
	"/gen/", "/dist/", "/out/", "/e2e/", "e2e-web", "playwright",
	"/harness/", "test/",
}

func isProduction(rel string) bool {
	slashed := "/" + filepath.ToSlash(rel)
	for _, frag := range notProduction {
		if strings.Contains(slashed, frag) {
			return false
		}
	}
	return !strings.HasSuffix(rel, ".d.ts")
}

// pruned is a directory that is not this tree: version control, vendored
// code, or a checkout of its own (a worktree's .git is a file).
func pruned(root, path string, d fs.DirEntry) bool {
	switch d.Name() {
	case ".git", "node_modules", "vendor":
		return true
	}
	if path == root {
		return false
	}
	_, err := os.Lstat(filepath.Join(path, ".git"))
	return err == nil
}

func walkRepo(t *testing.T, root string, visit func(rel, path string, data []byte)) {
	t.Helper()
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if pruned(root, path, d) {
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") && !strings.HasSuffix(path, ".ts") {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		visit(rel, path, data)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// productionPackages are the directories holding a non-test Go file of the
// production tree.
func productionPackages(t *testing.T, root string) []string {
	dirs := map[string]bool{}
	walkRepo(t, root, func(rel, _ string, _ []byte) {
		if strings.HasSuffix(rel, ".go") && !isTestFile(rel) && isProduction(rel) {
			dirs[filepath.Dir(rel)] = true
		}
	})
	var out []string
	for d := range dirs {
		out = append(out, d)
	}
	return out
}

// productionLines counts the production tree's lines, a last line with no
// newline included.
func productionLines(t *testing.T, root string) int {
	n := 0
	walkRepo(t, root, func(rel, _ string, data []byte) {
		if isTestFile(rel) || !isProduction(rel) {
			return
		}
		n += bytes.Count(data, []byte("\n"))
		if len(data) > 0 && data[len(data)-1] != '\n' {
			n++
		}
	})
	return n
}

// A directory that is its own checkout is not this tree, wherever it sits.
func TestWalkersPruneNestedCheckouts(t *testing.T) {
	root := t.TempDir()
	for _, rel := range []string{".git", "fs/plugin", ".worktrees/x/fs/plugin"} {
		if err := os.MkdirAll(filepath.Join(root, rel), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, ".worktrees/x/.git"), []byte("gitdir: elsewhere\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, rel := range []string{"fs/plugin/a.go", ".worktrees/x/fs/plugin/a.go"} {
		if err := os.WriteFile(filepath.Join(root, rel), []byte("package plugin\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if got := productionLines(t, root); got != 1 {
		t.Errorf("productionLines = %d, want 1: the nested checkout is not this tree", got)
	}
}

// allowLines are an allow file's entries, comments and blanks dropped.
func allowLines(t *testing.T, path string) []string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, l := range strings.Split(string(data), "\n") {
		if l = strings.TrimSpace(l); l != "" && !strings.HasPrefix(l, "#") {
			out = append(out, l)
		}
	}
	return out
}
