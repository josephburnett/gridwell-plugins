// Package plugin is the pages plugin, the smallest complete example of a
// plugin that puts web content into Gridwell: the wire half over the pages
// gridwell-plugins/pages/site generates. It serves each page entry's page and
// its assets through the node's content API, and presents the same pages at
// the site addresses its config declares (site.Present).
package plugin

import (
	"context"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/josephburnett/gridwell-plugins/pages/site"
	pluginv1 "github.com/josephburnett/gridwell/api/gen/plugin/v1"
	"github.com/josephburnett/gridwell/api/rpc"
)

// Kind is the plugin's declared kind, and the suffix of its binary name.
const Kind = "pages"

// displayName is the plugin's own name for itself: the fallback when
// server.yaml configures no label.
const displayName = "pages"

// Plugin implements pluginv1.PluginServer over the sample site. The site is
// fixed and the plugin holds no node fact; presents is the one config key.
type Plugin struct {
	pluginv1.UnimplementedPluginServer
	presents []string
}

// FromConfig builds the plugin from the spawn config, and is the one
// config-to-plugin derivation. presents is the site addresses it presents,
// globs separated by spaces, such as "http://example.test/*"; every other
// key (uuid, kind, state_dir) is ignored, since refusing one would refuse
// every launch.
func FromConfig(cfg map[string]string) (pluginv1.PluginServer, error) {
	p := New()
	p.presents = strings.Fields(cfg["presents"])
	return p, nil
}

// New builds the plugin.
func New() *Plugin { return &Plugin{} }

func (p *Plugin) Info(context.Context, *pluginv1.InfoRequest) (*pluginv1.InfoResponse, error) {
	return &pluginv1.InfoResponse{
		Kind:        Kind,
		DisplayName: displayName,
		// No label: the swatch reads as the configured instance.
		Grids: []*pluginv1.PluginGrid{{Id: site.RootGrid, Grid: site.RootGrid}},
		// The pages are the plugin's own content, not a window onto state
		// outside Gridwell as fs and proc are.
		HostContent: false,
		Presents:    p.presents,
	}, nil
}

// List enumerates the site, authoritatively since the site is fixed; any
// grid but the root is empty.
func (p *Plugin) List(_ context.Context, req *pluginv1.ListRequest) (*pluginv1.ListResponse, error) {
	resp := &pluginv1.ListResponse{Authoritative: true, SourceLabel: displayName}
	if req.Grid != site.RootGrid {
		return resp, nil
	}
	for _, d := range site.Docs() {
		e := &pluginv1.Entry{
			Key:           d.Key,
			Label:         d.Label,
			PlacementHint: &pluginv1.PlacementHint{X: d.Col, Y: d.Row, W: 2, H: 2},
		}
		if d.Page != nil {
			// The node derives a served page's address, so none is declared.
			e.Kind = rpc.KindURL
			e.ServesPage = true
			e.PreviewStamp = site.PreviewStamp
		} else {
			e.Kind = rpc.KindText
			e.TextPresentation = rpc.TextPresentationBoth
		}
		resp.Entries = append(resp.Entries, e)
	}
	return resp, nil
}

// ReadContent answers a text doc's markdown note. A page doc has no body and
// an unknown key is an empty chunk, not an error — absence is Probe's answer
// to give.
func (p *Plugin) ReadContent(req *pluginv1.ReadContentRequest, stream pluginv1.Plugin_ReadContentServer) error {
	d := site.Lookup(req.Key)
	if d == nil {
		return stream.Send(&pluginv1.ContentChunk{})
	}
	return stream.Send(&pluginv1.ContentChunk{
		Data:      []byte(d.Note),
		MediaType: "text/markdown",
	})
}

// ServeContent answers an entry's page or asset (site.Serve), or with no key
// an address the plugin presents (site.Present). Anything else is a 404 page,
// never an error: a missing resource is an ordinary web answer.
func (p *Plugin) ServeContent(req *pluginv1.ServeContentRequest, stream pluginv1.Plugin_ServeContentServer) error {
	mediaType, data, ok := site.Present(req.Subpath)
	if req.Key != "" {
		mediaType, data, ok = site.Serve(req.Key, req.Subpath)
	}
	if !ok {
		return notFound(stream)
	}
	return stream.Send(&pluginv1.ServeContentChunk{Status: 200, MediaType: mediaType, Data: data})
}

// notFound is a real 404 status, so the browser sees a miss rather than a
// gateway failure.
func notFound(stream pluginv1.Plugin_ServeContentServer) error {
	return stream.Send(&pluginv1.ServeContentChunk{
		Status:    404,
		MediaType: "text/plain; charset=utf-8",
		Data:      []byte("not found"),
	})
}

// GetPreview answers a page tile's preview. A doc with no page has no thumbnail,
// and nil is that answer — never an error.
func (p *Plugin) GetPreview(_ context.Context, req *pluginv1.GetPreviewRequest) (*pluginv1.GetPreviewResponse, error) {
	d := site.Lookup(req.Key)
	if d == nil {
		return &pluginv1.GetPreviewResponse{}, nil
	}
	return &pluginv1.GetPreviewResponse{Jpeg: d.PreviewJPEG()}, nil
}

// Probe answers definitively, since the site is fixed: a key it does not
// hold, or a grid but the root, is gone. An empty grid asks after the
// plugin as a whole.
func (p *Plugin) Probe(_ context.Context, req *pluginv1.ProbeRequest) (*pluginv1.ProbeResponse, error) {
	if (req.Grid != "" && req.Grid != site.RootGrid) || site.Lookup(req.Key) == nil {
		return &pluginv1.ProbeResponse{Presence: pluginv1.ProbeResponse_PRESENCE_GONE}, nil
	}
	return &pluginv1.ProbeResponse{Presence: pluginv1.ProbeResponse_PRESENCE_PRESENT}, nil
}

// Delete is refused with its reason rather than left to the embedded
// Unimplemented, which would say nothing.
func (p *Plugin) Delete(context.Context, *pluginv1.DeleteRequest) (*pluginv1.DeleteResponse, error) {
	return nil, status.Error(codes.Unimplemented, "pages plugin: the sample site is fixed; nothing to delete")
}
