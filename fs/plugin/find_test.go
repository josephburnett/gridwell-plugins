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

// A file tree finds nothing, by rule rather than by omission
// (docs/plugin-standard.md rule 20): Find is Unimplemented with the
// plugin's own reason, so the node remembers it for the process, and no
// query touches the disk, even one naming a file that is there.
func TestFindIsUnimplementedByRule(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "notes.md"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := New(root, nil).Find(t.Context(), &pluginv1.FindRequest{Query: "notes"})
	st, _ := status.FromError(err)
	if st.Code() != codes.Unimplemented {
		t.Fatalf("Find = %v, want Unimplemented", err)
	}
	if !strings.HasPrefix(st.Message(), "fs plugin:") {
		t.Fatalf("Find's reason = %q, want the plugin's own, not the generated stub's", st.Message())
	}
}
