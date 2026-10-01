package guest

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	gplug "github.com/josephburnett/gridwell/api/compose"
	pluginv1 "github.com/josephburnett/gridwell/api/gen/plugin/v1"
)

// TestConfigDecodesEnv: a JSON object in GRIDWELL_PLUGIN_CONFIG decodes to
// the config map the plugin reads at spawn.
func TestConfigDecodesEnv(t *testing.T) {
	t.Setenv(gplug.ConfigEnvVar, `{"db_file":"/x/store.db","uuid":"abc","kind":"home"}`)
	got, err := Config()
	want := map[string]string{"db_file": "/x/store.db", "uuid": "abc", "kind": "home"}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Errorf("Config() = %v, want %v", got, want)
	}
}

// TestConfigEmptyWhenUnset: an unset or empty env yields an empty non-nil
// map, so callers can index it without a nil check.
func TestConfigEmptyWhenUnset(t *testing.T) {
	t.Setenv(gplug.ConfigEnvVar, "")
	got, err := Config()
	if err != nil || got == nil || len(got) != 0 {
		t.Errorf("Config() with unset env = %v, want empty non-nil map", got)
	}
}

// TestConfigMalformedIsAnError: a value that is not a JSON object is an
// error naming the variable — never an empty map, which would run the
// plugin unconfigured (fs with no root, proc at pid 1) as if that were what
// server.yaml said.
func TestConfigMalformedIsAnError(t *testing.T) {
	t.Setenv(gplug.ConfigEnvVar, `{not valid json`)
	got, err := Config()
	if err == nil || !strings.Contains(err.Error(), gplug.ConfigEnvVar) {
		t.Errorf("Config() with malformed env = %v, %v; want an error naming %s", got, err, gplug.ConfigEnvVar)
	}
}

// TestMainRefusesTheHandshake: a config that will not decode, and a factory
// that refuses its config, both become a plugin whose Info answers
// FailedPrecondition with the reason, which the node shows as the plugin
// broken. A factory that builds is served as itself.
func TestMainRefusesTheHandshake(t *testing.T) {
	ctx := context.Background()
	t.Setenv(gplug.ConfigEnvVar, `{not valid json`)
	impl := build(func(map[string]string) (pluginv1.PluginServer, error) {
		t.Fatal("the factory must not run on an undecodable config")
		return nil, nil
	})
	if _, err := impl.Info(ctx, &pluginv1.InfoRequest{}); status.Code(err) != codes.FailedPrecondition || !strings.Contains(err.Error(), gplug.ConfigEnvVar) {
		t.Errorf("undecodable config → Info %v, want FailedPrecondition naming %s", err, gplug.ConfigEnvVar)
	}

	t.Setenv(gplug.ConfigEnvVar, `{"pid":"abc"}`)
	var seen map[string]string
	impl = build(func(cfg map[string]string) (pluginv1.PluginServer, error) {
		seen = cfg
		return nil, errors.New("pid abc is not a process id")
	})
	if seen["pid"] != "abc" {
		t.Errorf("factory saw %v, want the decoded config", seen)
	}
	if _, err := impl.Info(ctx, &pluginv1.InfoRequest{}); status.Code(err) != codes.FailedPrecondition || !strings.Contains(err.Error(), "not a process id") {
		t.Errorf("refusing factory → Info %v, want FailedPrecondition with its reason", err)
	}

	built := &pluginv1.UnimplementedPluginServer{}
	if got := build(func(map[string]string) (pluginv1.PluginServer, error) { return built, nil }); got != built {
		t.Errorf("a factory that builds must be served as itself, got %T", got)
	}
}

type countingInfo struct {
	pluginv1.UnimplementedPluginServer
}

func (countingInfo) Info(context.Context, *pluginv1.InfoRequest) (*pluginv1.InfoResponse, error) {
	return &pluginv1.InfoResponse{DisplayName: "built"}, nil
}

// A refused build is asked again on each call, so a config fixed on disk
// comes back without a respawn, and once built it is asked no more.
func TestARefusedBuildComesBackWhenItsConfigIsFixed(t *testing.T) {
	ctx := context.Background()
	t.Setenv(gplug.ConfigEnvVar, `{"token_file":"/x"}`)
	fixed, builds := false, 0
	impl := build(func(map[string]string) (pluginv1.PluginServer, error) {
		builds++
		if !fixed {
			return nil, errors.New(`token_file "/x" does not exist`)
		}
		return countingInfo{}, nil
	})
	if _, err := impl.Info(ctx, &pluginv1.InfoRequest{}); status.Code(err) != codes.FailedPrecondition || !strings.Contains(err.Error(), "does not exist") {
		t.Fatalf("refused → Info %v, want FailedPrecondition with the reason", err)
	}
	if _, err := impl.List(ctx, &pluginv1.ListRequest{}); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("refused → List %v, want the same refusal on every verb", err)
	}
	fixed = true
	info, err := impl.Info(ctx, &pluginv1.InfoRequest{})
	if err != nil || info.DisplayName != "built" {
		t.Fatalf("fixed → Info %v, %v; want the built plugin's answer", info, err)
	}
	was := builds
	if _, err := impl.Info(ctx, &pluginv1.InfoRequest{}); err != nil {
		t.Fatal(err)
	}
	if builds != was {
		t.Errorf("the factory ran again after it built (%d → %d runs)", was, builds)
	}
}
