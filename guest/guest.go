// Package guest is called by a plugin binary's main() to serve the
// plugin.v1 service over go-plugin's managed subprocess
// transport.
//
// Usage in a plugin binary:
//
//	func main() {
//	    guest.Main(myplugin.FromConfig)
//	}
//
// where FromConfig is the plugin's one config→plugin derivation.
package guest

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"sync"
	"time"

	hclog "github.com/hashicorp/go-hclog"
	"github.com/hashicorp/go-plugin"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	gplug "github.com/josephburnett/gridwell/api/compose"
	pluginv1 "github.com/josephburnett/gridwell/api/gen/plugin/v1"
)

// Config returns the config map the host handed this plugin at spawn,
// decoded from the GRIDWELL_PLUGIN_CONFIG environment variable. An
// unset or empty value yields an empty map. A value that is not a JSON
// object is an error, never an empty map: a plugin that silently ran
// unconfigured (fs with no root, proc at pid 1) would look like a plugin that
// lost its config. A plugin is configured once, at launch.
func Config() (map[string]string, error) {
	raw := os.Getenv(gplug.ConfigEnvVar)
	if raw == "" {
		return map[string]string{}, nil
	}
	out := map[string]string{}
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return nil, fmt.Errorf("%s is not a JSON object of strings: %v", gplug.ConfigEnvVar, err)
	}
	return out, nil
}

// Factory is the plugin's config→plugin derivation: the one owner of how
// server.yaml config becomes a running plugin, and the argument Main takes.
// An error is the verdict "I cannot serve what this config declares", in
// plain words the user reads on the plugin's row.
type Factory func(cfg map[string]string) (pluginv1.PluginServer, error)

// Main decodes the spawn config, builds the plugin, and serves it. A factory
// that refuses is asked again on every call until it builds, so a file the
// config names that is fixed on disk comes back without a respawn; until then
// every call, Info included, answers FailedPrecondition with the reason, which
// the node shows as the plugin broken. Exiting instead would present as a
// handshake failure with the reason lost in the guest's stderr.
func Main(factory Factory) {
	Serve(build(factory))
}

// build is Main without the serving: the decode + factory + refusal
// derivation, testable in-process. A config that will not decode is the
// spawn's, so it is refused for the process's life.
func build(factory Factory) pluginv1.PluginServer {
	cfg, err := Config()
	if err != nil {
		return &rebuilding{factory: func(map[string]string) (pluginv1.PluginServer, error) { return nil, err }}
	}
	impl, err := factory(cfg)
	if err != nil {
		return &rebuilding{factory: factory, cfg: cfg}
	}
	return impl
}

// rebuilding is the plugin whose factory refused: each call runs the factory
// again until it builds, then forwards to what it built. It embeds
// UnsafePluginServer rather than the Unimplemented server, so a verb added to
// plugin.v1 is a compile error here instead of one this wrapper refuses.
type rebuilding struct {
	pluginv1.UnsafePluginServer
	factory Factory
	cfg     map[string]string

	mu   sync.Mutex
	impl pluginv1.PluginServer
}

func (r *rebuilding) built() (pluginv1.PluginServer, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.impl != nil {
		return r.impl, nil
	}
	impl, err := r.factory(r.cfg)
	if err != nil {
		return nil, status.Errorf(codes.FailedPrecondition, "%v", err)
	}
	r.impl = impl
	return impl, nil
}

func (r *rebuilding) Info(ctx context.Context, req *pluginv1.InfoRequest) (*pluginv1.InfoResponse, error) {
	impl, err := r.built()
	if err != nil {
		return nil, err
	}
	return impl.Info(ctx, req)
}

func (r *rebuilding) List(ctx context.Context, req *pluginv1.ListRequest) (*pluginv1.ListResponse, error) {
	impl, err := r.built()
	if err != nil {
		return nil, err
	}
	return impl.List(ctx, req)
}

func (r *rebuilding) ReadContent(req *pluginv1.ReadContentRequest, s grpc.ServerStreamingServer[pluginv1.ContentChunk]) error {
	impl, err := r.built()
	if err != nil {
		return err
	}
	return impl.ReadContent(req, s)
}

func (r *rebuilding) WriteContent(s grpc.ClientStreamingServer[pluginv1.WriteContentRequest, pluginv1.WriteContentResponse]) error {
	impl, err := r.built()
	if err != nil {
		return err
	}
	return impl.WriteContent(s)
}

func (r *rebuilding) ServeContent(req *pluginv1.ServeContentRequest, s grpc.ServerStreamingServer[pluginv1.ServeContentChunk]) error {
	impl, err := r.built()
	if err != nil {
		return err
	}
	return impl.ServeContent(req, s)
}

func (r *rebuilding) GetPreview(ctx context.Context, req *pluginv1.GetPreviewRequest) (*pluginv1.GetPreviewResponse, error) {
	impl, err := r.built()
	if err != nil {
		return nil, err
	}
	return impl.GetPreview(ctx, req)
}

func (r *rebuilding) Probe(ctx context.Context, req *pluginv1.ProbeRequest) (*pluginv1.ProbeResponse, error) {
	impl, err := r.built()
	if err != nil {
		return nil, err
	}
	return impl.Probe(ctx, req)
}

func (r *rebuilding) Delete(ctx context.Context, req *pluginv1.DeleteRequest) (*pluginv1.DeleteResponse, error) {
	impl, err := r.built()
	if err != nil {
		return nil, err
	}
	return impl.Delete(ctx, req)
}

func (r *rebuilding) Search(ctx context.Context, req *pluginv1.SearchRequest) (*pluginv1.SearchResponse, error) {
	impl, err := r.built()
	if err != nil {
		return nil, err
	}
	return impl.Search(ctx, req)
}

func (r *rebuilding) Watch(req *pluginv1.WatchRequest, s grpc.ServerStreamingServer[pluginv1.Change]) error {
	impl, err := r.built()
	if err != nil {
		return err
	}
	return impl.Watch(req, s)
}

// watchHost exits the guest when the spawning host dies. go-plugin gives a
// guest no host-death detection in our configuration: the guest inherits
// the host's stdin (which never closes), and a killed host looks like a
// disconnected gRPC client while the guest keeps listening forever. The
// host hands its pid in the environment — a spawn-time fact, so a
// pre-watchdog race cannot capture a post-death parent — and the guest
// probes it, which is robust against subreaper reparenting where a Getppid
// comparison can lie. How you probe a pid is the one per-platform fact:
// hostAlive lives in hostalive_unix.go and hostalive_other.go. A missing env
// var (a hand-launched guest, a test harness) disables the watchdog rather
// than guessing.
func watchHost() {
	pid, err := strconv.Atoi(os.Getenv(gplug.HostPIDEnvVar))
	if err != nil || pid <= 0 {
		return
	}
	go func() {
		for {
			time.Sleep(2 * time.Second)
			if !hostAlive(pid) {
				os.Exit(0)
			}
		}
	}()
}

// Serve serves impl over the managed subprocess transport. Main is the
// usual door; Serve is for a main that builds its plugin some other way.
func Serve(impl pluginv1.PluginServer) {
	watchHost()
	logger := hclog.New(&hclog.LoggerOptions{
		Level:      hclog.Error,
		Output:     os.Stderr,
		JSONFormat: true,
	})
	plugin.Serve(&plugin.ServeConfig{
		HandshakeConfig: gplug.HandshakeConfig,
		Plugins:         gplug.PluginMap(impl),
		GRPCServer:      plugin.DefaultGRPCServer,
		Logger:          logger,
	})
}
