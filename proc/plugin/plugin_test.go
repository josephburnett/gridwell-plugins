package plugin

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pluginv1 "github.com/josephburnett/gridwell/api/gen/plugin/v1"
)

// FromConfig is the one config-to-plugin derivation: a missing pid takes the
// documented default of 1, and a pid that is not a positive integer is a
// refusal naming it, never a silent fallback to the whole process tree.
func TestFromConfigOwnsThePidDerivation(t *testing.T) {
	for _, bad := range []string{"abc", "0", "-3", "12x"} {
		if impl, err := FromConfig(map[string]string{"pid": bad}); err == nil || !strings.Contains(err.Error(), bad) {
			t.Errorf("pid %q → %v, %v; want a refusal naming it", bad, impl, err)
		}
	}
	self := strconv.Itoa(os.Getpid())
	for raw, want := range map[string]string{"": "1", "1": "1", " " + self + " ": self} {
		impl, err := FromConfig(map[string]string{"pid": raw})
		if err != nil {
			t.Fatalf("pid %q: %v", raw, err)
		}
		info, err := impl.(*Plugin).Info(context.Background(), &pluginv1.InfoRequest{})
		if err != nil || len(info.MenuEntries) != 1 || info.MenuEntries[0].Context != want {
			t.Errorf("pid %q → collections %v, %v; want the one context %q", raw, info.GetMenuEntries(), err, want)
		}
	}
}

// The process table is host state, and saying so is what earns its grids the
// host treatment on the client. Nothing downstream knows the kind "proc".
func TestInfoDeclaresHostContent(t *testing.T) {
	impl, err := FromConfig(map[string]string{})
	if err != nil {
		t.Fatal(err)
	}
	info, err := impl.(*Plugin).Info(context.Background(), &pluginv1.InfoRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if !info.GetHostContent() {
		t.Error("host_content false; the process table is host state")
	}
}

// A process the plugin cannot project refuses Info with a sentence naming
// it, so the node shows the plugin broken rather than an empty tree; a
// running process with no children is a tree it can serve. The check runs on
// every Info, so a process that appears later is served without a respawn.
func TestInfoRefusesAProcessItCannotServe(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	cases := map[*Plugin]string{
		New(filepath.Join(root, "absent"), 1, nil): "the process table " + filepath.Join(root, "absent") + " cannot be read",
		New(root, 4242, nil):                       "pid 4242 is not running",
	}
	for p, want := range cases {
		_, err := p.Info(ctx, &pluginv1.InfoRequest{})
		if status.Code(err) != codes.FailedPrecondition || !strings.HasPrefix(status.Convert(err).Message(), want) {
			t.Errorf("procRoot %s pid %d → Info %v; want FailedPrecondition %q", p.procRoot, p.rootPID, err, want)
		}
	}

	p := New(root, 4242, nil)
	if err := os.Mkdir(filepath.Join(root, "4242"), 0o755); err != nil {
		t.Fatal(err)
	}
	if info, err := p.Info(ctx, &pluginv1.InfoRequest{}); err != nil || len(info.MenuEntries) != 1 {
		t.Errorf("a childless process that started after launch → Info %v, %v; want its one collection", info, err)
	}
}
