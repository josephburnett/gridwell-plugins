// Package plugin is the proc content plugin: a stateless projection of the
// process table. A context is a pid, whose grid lists that process's direct
// children; tile keys are pid strings, plus "info:<pid>" for the @info
// metadata tile. Listings are non-authoritative — a child unreadable this pass
// is not gone — and the node arbitrates absence through Probe. There is no
// database: the process table is the source.
package plugin

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/josephburnett/gridwell-plugins/proc/procsource"
	pluginv1 "github.com/josephburnett/gridwell/api/gen/plugin/v1"
)

// Killer is the signal interface, injected so tests never signal real
// processes. Production uses syscall.Kill, in sysKiller.
type Killer interface {
	Kill(pid int64, sig syscall.Signal) error
}

// infoLabel is the metadata tile's display label.
const infoLabel = "@info"

// infoKeyPrefix namespaces the metadata tiles' keys: "@info" appears in every
// grid, but plugin keys must be unique across the plugin.
const infoKeyPrefix = "info:"

type sysKiller struct{}

func (sysKiller) Kill(pid int64, sig syscall.Signal) error {
	return syscall.Kill(int(pid), sig)
}

// Plugin implements pluginv1.PluginServer for the process table.
type Plugin struct {
	pluginv1.UnimplementedPluginServer
	procRoot string
	rootPID  int64
	killer   Killer
	// served latches the first Info that found the process; see fs's
	// Plugin.served for why the check stops there.
	served atomic.Bool
}

// FromConfig builds the production plugin from the shared config vocabulary.
// It is the one owner of the config-to-plugin derivation, so the subprocess
// main and a bundled binary compose exactly the same plugin. The config key is
// pid, an optional root pid defaulting to 1. A pid that is not a positive
// integer is refused, and the node shows the plugin broken with the reason:
// silently falling back to pid 1 would present the whole process tree as if
// that were what server.yaml said.
func FromConfig(cfg map[string]string) (pluginv1.PluginServer, error) {
	var pid int64
	if raw := strings.TrimSpace(cfg["pid"]); raw != "" {
		n, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || n <= 0 {
			return nil, fmt.Errorf("proc plugin: pid %q is not a positive process id", raw)
		}
		pid = n
	}
	return New("", pid, nil), nil
}

// New builds a plugin. An empty procRoot uses /proc, a rootPID of 0 or less
// uses pid 1, and a nil killer signals real processes.
func New(procRoot string, rootPID int64, killer Killer) *Plugin {
	if procRoot == "" {
		procRoot = procsource.DefaultRoot
	}
	if rootPID <= 0 {
		rootPID = 1
	}
	if killer == nil {
		killer = sysKiller{}
	}
	return &Plugin{procRoot: procRoot, rootPID: rootPID, killer: killer}
}

// Info refuses while the configured process is not there to project: a pid
// that is not running, or a process table that cannot be read. It checks on
// every call until one passes, so a process that starts after launch is
// served without a respawn.
func (p *Plugin) Info(context.Context, *pluginv1.InfoRequest) (*pluginv1.InfoResponse, error) {
	if !p.served.Load() {
		if err := p.servable(); err != nil {
			return nil, status.Error(codes.FailedPrecondition, err.Error())
		}
		p.served.Store(true)
	}
	label := "processes"
	if p.rootPID != 1 {
		label = "pid " + strconv.FormatInt(p.rootPID, 10)
	}
	return &pluginv1.InfoResponse{
		Kind:        "proc",
		DisplayName: label,
		Glyph:       "process",
		// The one collection this plugin serves: the process tree under the
		// configured pid. It declares no label, so the swatch reads as the
		// configured instance.
		MenuEntries: []*pluginv1.MenuEntry{{
			Id:      strconv.FormatInt(p.rootPID, 10),
			Context: strconv.FormatInt(p.rootPID, 10),
		}},
		// The process table is host state, projected: declaring it is what
		// earns these grids the host treatment on the client.
		HostContent: true,
	}, nil
}

// servable's error is the sentence the node shows on the plugin's row.
func (p *Plugin) servable() error {
	if _, err := os.Stat(p.procRoot); err != nil {
		var pe *fs.PathError
		if errors.As(err, &pe) {
			err = pe.Err
		}
		return fmt.Errorf("the process table %s cannot be read: %v", p.procRoot, err)
	}
	up, err := procsource.Exists(p.procRoot, p.rootPID)
	switch {
	case err != nil:
		return fmt.Errorf("pid %d cannot be read: %v", p.rootPID, err)
	case !up:
		return fmt.Errorf("pid %d is not running", p.rootPID)
	}
	return nil
}

// keyPID resolves any key shape to the pid it denotes.
func keyPID(key string) (int64, error) {
	s := strings.TrimPrefix(key, infoKeyPrefix)
	pid, err := strconv.ParseInt(s, 10, 64)
	if err != nil || pid <= 0 {
		return 0, status.Errorf(codes.InvalidArgument, "proc plugin: invalid key %q", key)
	}
	return pid, nil
}

// List enumerates one process's children plus its @info tile, @info first, so
// the ids the node mints stay stable. A process table it cannot read is
// Unavailable: a partial listing would look like the truth.
func (p *Plugin) List(_ context.Context, req *pluginv1.ListRequest) (*pluginv1.ListResponse, error) {
	pid, err := keyPID(req.Context)
	if err != nil {
		return nil, err
	}
	up, err := procsource.Exists(p.procRoot, pid)
	if err != nil {
		return nil, status.Errorf(codes.Unavailable, "proc plugin: pid %d: %v", pid, err)
	}
	resp := &pluginv1.ListResponse{Authoritative: false, SourceLabel: req.Context}
	if up {
		resp.Entries = append(resp.Entries, &pluginv1.Entry{
			Key:  infoKeyPrefix + req.Context,
			Kind: "text", Label: infoLabel, TextPresentation: "both",
		})
	}
	children, err := procsource.Children(p.procRoot, pid)
	if err != nil {
		return nil, status.Errorf(codes.Unavailable, "proc plugin: children of %d: %v", pid, err)
	}
	for _, c := range children {
		key := strconv.FormatInt(c.PID, 10)
		resp.Entries = append(resp.Entries, &pluginv1.Entry{
			Key: key, Kind: "well", Label: key, ChildContext: key,
		})
	}
	return resp, nil
}

func (p *Plugin) ReadContent(req *pluginv1.ReadContentRequest, stream pluginv1.Plugin_ReadContentServer) error {
	if !strings.HasPrefix(req.Key, infoKeyPrefix) {
		// A process well carries no document body.
		return stream.Send(&pluginv1.ContentChunk{})
	}
	pid, err := keyPID(req.Key)
	if err != nil {
		return err
	}
	info, err := procsource.Get(p.procRoot, pid)
	switch {
	case procsource.IsGone(err):
		return status.Errorf(codes.NotFound, "proc plugin: pid %d has exited", pid)
	case err != nil:
		return status.Errorf(codes.Unavailable, "proc plugin: pid %d cannot be read: %v", pid, err)
	}
	return stream.Send(&pluginv1.ContentChunk{
		Data:      []byte(procsource.MetadataMarkdown(info)),
		MediaType: "text/markdown",
	})
}

func (p *Plugin) Probe(_ context.Context, req *pluginv1.ProbeRequest) (*pluginv1.ProbeResponse, error) {
	if strings.HasPrefix(req.Key, infoKeyPrefix) {
		// @info is never swept: it describes the grid's own process, and a
		// grid outliving its process is the wells' problem, not @info's.
		return &pluginv1.ProbeResponse{Presence: pluginv1.ProbeResponse_PRESENCE_PRESENT}, nil
	}
	pid, err := keyPID(req.Key)
	if err != nil {
		return &pluginv1.ProbeResponse{Presence: pluginv1.ProbeResponse_PRESENCE_GONE}, nil
	}
	present, perr := procsource.Exists(p.procRoot, pid)
	switch {
	case perr != nil:
		return &pluginv1.ProbeResponse{Presence: pluginv1.ProbeResponse_PRESENCE_UNSPECIFIED}, nil
	case present:
		return &pluginv1.ProbeResponse{Presence: pluginv1.ProbeResponse_PRESENCE_PRESENT}, nil
	default:
		return &pluginv1.ProbeResponse{Presence: pluginv1.ProbeResponse_PRESENCE_GONE}, nil
	}
}

// Delete sends SIGTERM, best-effort; the tile sweeps once the process is
// definitively gone.
func (p *Plugin) Delete(_ context.Context, req *pluginv1.DeleteRequest) (*pluginv1.DeleteResponse, error) {
	pid, err := keyPID(req.Key)
	if err != nil {
		return nil, err
	}
	if kerr := p.killer.Kill(pid, syscall.SIGTERM); kerr != nil {
		return nil, status.Errorf(codes.Internal, "proc plugin: kill %d: %v", pid, kerr)
	}
	return &pluginv1.DeleteResponse{}, nil
}
