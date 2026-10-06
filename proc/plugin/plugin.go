// Package plugin is the proc content plugin: a stateless projection of the
// process table. A context is a pid, whose grid lists that process's direct
// children; tile keys are pid strings, plus "info:<pid>" for the @info
// metadata tile. Listings are non-authoritative — a child unreadable this pass
// is not gone — and the node arbitrates absence through Probe. There is no
// database: the process table is the source, and Watch polls it (watch.go).
package plugin

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
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

	"github.com/josephburnett/gridwell-plugins/memo"
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
	// scan is the one read of a process's children, for List and the poll;
	// a test counts it.
	scan    func(ctx context.Context, pid int64) ([]procsource.Stat, error)
	clock   memo.Clock
	life    *memo.Life
	changes *memo.Changes
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
	return newPlugin(procRoot, rootPID, killer, memo.System)
}

func newPlugin(procRoot string, rootPID int64, killer Killer, clock memo.Clock) *Plugin {
	if procRoot == "" {
		procRoot = procsource.DefaultRoot
	}
	if rootPID <= 0 {
		rootPID = 1
	}
	if killer == nil {
		killer = sysKiller{}
	}
	p := &Plugin{procRoot: procRoot, rootPID: rootPID, killer: killer, clock: clock, life: memo.NewLife()}
	p.scan = func(ctx context.Context, pid int64) ([]procsource.Stat, error) {
		return procsource.Children(ctx, p.procRoot, pid)
	}
	p.changes = memo.NewChanges(p.life, memo.ChangeOptions{Clock: clock, Do: p.follow})
	return p
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
		Watch:       true,
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

// parseKey resolves a key to the pid it names, and whether it is that pid's
// @info tile rather than its well.
func parseKey(key string) (pid int64, info bool, err error) {
	s, info := strings.CutPrefix(key, infoKeyPrefix)
	pid, ok := parsePID(s)
	if !ok {
		return 0, false, status.Errorf(codes.InvalidArgument, "proc plugin: invalid key %q", key)
	}
	return pid, info, nil
}

// contextPID resolves a context, which is always a bare pid.
func contextPID(context string) (int64, error) {
	pid, ok := parsePID(context)
	if !ok {
		return 0, status.Errorf(codes.InvalidArgument, "proc plugin: invalid context %q", context)
	}
	return pid, nil
}

func parsePID(s string) (int64, bool) {
	pid, err := strconv.ParseInt(s, 10, 64)
	return pid, err == nil && pid > 0
}

// List enumerates one process's children plus its @info tile, @info first, so
// the ids the node mints stay stable. A process table it cannot read is
// Unavailable: a partial listing would look like the truth.
func (p *Plugin) List(ctx context.Context, req *pluginv1.ListRequest) (*pluginv1.ListResponse, error) {
	pid, err := contextPID(req.Context)
	if err != nil {
		return nil, err
	}
	up, err := procsource.Exists(p.procRoot, pid)
	if err != nil {
		return nil, status.Errorf(codes.Unavailable, "proc plugin: pid %d: %v", pid, err)
	}
	resp := &pluginv1.ListResponse{Authoritative: false, SourceLabel: req.Context}
	if up {
		_, stamp, _ := p.info(pid)
		resp.Entries = append(resp.Entries, infoEntry(req.Context, stamp))
	}
	children, err := p.scan(ctx, pid)
	if ctx.Err() != nil {
		return nil, status.FromContextError(ctx.Err()).Err()
	}
	if err != nil {
		return nil, status.Errorf(codes.Unavailable, "proc plugin: children of %d: %v", pid, err)
	}
	for _, c := range children {
		key := strconv.FormatInt(c.PID, 10)
		resp.Entries = append(resp.Entries, &pluginv1.Entry{
			Key: key, Kind: "well", Label: key, ChildContext: key,
			StatusDetail: stateMark(c.State),
		})
	}
	return resp, nil
}

// infoEntry is context's @info entry, its body named by stamp.
func infoEntry(context, stamp string) *pluginv1.Entry {
	return &pluginv1.Entry{
		Key:  infoKeyPrefix + context,
		Kind: "text", Label: infoLabel, TextPresentation: "both",
		ContentStamp: stamp,
	}
}

// info is pid's @info body and its content stamp: a hash of the body, since
// the process table keeps no version and this plugin no memory, so the stamp
// moves exactly when the bytes do.
func (p *Plugin) info(pid int64) (body []byte, stamp string, err error) {
	info, err := procsource.Get(p.procRoot, pid)
	if err != nil {
		return nil, "", err
	}
	body = []byte(procsource.MetadataMarkdown(info))
	sum := sha256.Sum256(body)
	return body, hex.EncodeToString(sum[:8]), nil
}

// stateMark is a process's status_detail: one emoji for the states worth
// noticing, a zombie (💀) or a process stopped by a signal or a tracer (⏸),
// and nothing for every other state.
func stateMark(state byte) string {
	switch state {
	case 'Z':
		return "💀"
	case 'T', 't':
		return "⏸"
	}
	return ""
}

func (p *Plugin) ReadContent(req *pluginv1.ReadContentRequest, stream pluginv1.Plugin_ReadContentServer) error {
	pid, isInfo, err := parseKey(req.Key)
	if err != nil {
		return err
	}
	if !isInfo {
		// A process well carries no document body.
		return stream.Send(&pluginv1.ContentChunk{})
	}
	body, stamp, err := p.info(pid)
	switch {
	case procsource.IsGone(err):
		return status.Errorf(codes.NotFound, "proc plugin: pid %d has exited", pid)
	case err != nil:
		return status.Errorf(codes.Unavailable, "proc plugin: pid %d cannot be read: %v", pid, err)
	}
	return stream.Send(&pluginv1.ContentChunk{Data: body, MediaType: "text/markdown", ContentStamp: stamp})
}

// Probe answers for the context named, or for the plugin as a whole when none
// is. A process is under its parent's context now, so a child reparented
// after its parent died is gone from the old grid; info:<pid> lives only in
// <pid>'s grid, and only while <pid> runs.
func (p *Plugin) Probe(_ context.Context, req *pluginv1.ProbeRequest) (*pluginv1.ProbeResponse, error) {
	pid, isInfo, err := parseKey(req.Key)
	if err != nil {
		return presence(pluginv1.ProbeResponse_PRESENCE_GONE), nil
	}
	var under int64 // 0: the plugin as a whole
	if req.Context != "" {
		if under, err = contextPID(req.Context); err != nil {
			return presence(pluginv1.ProbeResponse_PRESENCE_GONE), nil // a context this plugin never lists
		}
	}
	if isInfo && under != 0 && under != pid {
		return presence(pluginv1.ProbeResponse_PRESENCE_GONE), nil
	}
	st, err := procsource.ReadStat(p.procRoot, pid)
	switch {
	case procsource.IsGone(err):
		return presence(pluginv1.ProbeResponse_PRESENCE_GONE), nil
	case err != nil:
		return presence(pluginv1.ProbeResponse_PRESENCE_UNSPECIFIED), nil
	case !isInfo && under != 0 && st.PPID != under:
		return presence(pluginv1.ProbeResponse_PRESENCE_GONE), nil
	}
	return presence(pluginv1.ProbeResponse_PRESENCE_PRESENT), nil
}

func presence(p pluginv1.ProbeResponse_Presence) *pluginv1.ProbeResponse {
	return &pluginv1.ProbeResponse{Presence: p}
}

// Delete sends a well's process SIGTERM; the row retires once Probe says the
// process is gone, which for one already gone is at once. An @info tile is
// refused: it describes a process and is not one.
func (p *Plugin) Delete(_ context.Context, req *pluginv1.DeleteRequest) (*pluginv1.DeleteResponse, error) {
	pid, isInfo, err := parseKey(req.Key)
	if err != nil {
		return nil, err
	}
	if isInfo {
		return nil, status.Errorf(codes.FailedPrecondition,
			"proc plugin: @info describes pid %d and is not a process; delete the process's own tile to stop it", pid)
	}
	switch err := p.killer.Kill(pid, syscall.SIGTERM); {
	case err == nil, errors.Is(err, syscall.ESRCH):
		return &pluginv1.DeleteResponse{}, nil
	case errors.Is(err, syscall.EPERM):
		return nil, status.Errorf(codes.PermissionDenied,
			"proc plugin: not permitted to stop pid %d: it belongs to another user", pid)
	default:
		return nil, status.Errorf(codes.Internal, "proc plugin: kill %d: %v", pid, err)
	}
}
