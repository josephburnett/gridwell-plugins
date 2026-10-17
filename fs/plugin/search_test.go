package plugin

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pluginv1 "github.com/josephburnett/gridwell/api/gen/plugin/v1"
)

// A file tree answers no search, by rule rather than by omission
// (docs/plugin-standard.md rule 20): Search is Unimplemented with the
// plugin's own reason, so the node remembers it for the process, and no
// query touches the disk, even one naming a file that is there.
func TestSearchIsUnimplementedByRule(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "notes.md"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := New(root, nil).Search(t.Context(), &pluginv1.SearchRequest{Query: "notes"})
	st, _ := status.FromError(err)
	if st.Code() != codes.Unimplemented {
		t.Fatalf("Search = %v, want Unimplemented", err)
	}
	if !strings.HasPrefix(st.Message(), "fs plugin:") {
		t.Fatalf("Search's reason = %q, want the plugin's own, not the generated stub's", st.Message())
	}
}
