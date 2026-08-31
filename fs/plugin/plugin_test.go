package plugin

import (
	"context"
	"testing"

	pluginv1 "github.com/josephburnett/gridwell/api/gen/plugin/v1"
)

// FromConfig is the one config→plugin derivation: root projects, and no
// root is the rootless (listed, not enterable) plugin, not a refusal.
func TestFromConfigOwnsTheRootDerivation(t *testing.T) {
	ctx := context.Background()
	impl, err := FromConfig(map[string]string{"root": " /srv/docs "})
	if err != nil {
		t.Fatal(err)
	}
	if info, err := impl.(*Plugin).Info(ctx, &pluginv1.InfoRequest{}); err != nil || info.RootContext != "." || info.DisplayName != "docs" {
		t.Errorf("rooted → %v, %v", info, err)
	}
	impl, err = FromConfig(map[string]string{})
	if err != nil {
		t.Fatal(err)
	}
	if info, err := impl.(*Plugin).Info(ctx, &pluginv1.InfoRequest{}); err != nil || info.RootContext != "" {
		t.Errorf("rootless → %v, %v; want listed with no root context", info, err)
	}
}

// Same class as convert.relKey: the confinement check must not be a
// hand-built root+"/" prefix — with root "/" that is "//", which no
// path starts with, so every key was refused as an escape.
func TestAbsUnderRootSlash(t *testing.T) {
	p := New("/", nil)
	got, err := p.abs(".nofollow")
	if err != nil {
		t.Fatalf("abs(.nofollow) under root /: %v", err)
	}
	if got != "/.nofollow" {
		t.Fatalf("abs(.nofollow) = %q, want /.nofollow", got)
	}
	if got, err := p.abs("."); err != nil || got != "/" {
		t.Fatalf("abs(.) = %q, %v, want /", got, err)
	}
}

func TestAbsStillRefusesEscapes(t *testing.T) {
	p := New("/home/joe", nil)
	for _, key := range []string{"../etc/passwd", "sub/../../etc"} {
		if got, err := p.abs(key); err != nil {
			t.Fatalf("abs(%q): anchored cleanup should confine, got error %v", key, err)
		} else if got != "/home/joe/etc/passwd" && got != "/home/joe/etc" {
			t.Fatalf("abs(%q) = %q escaped the root", key, got)
		}
	}
}

// The host treatment a directory grid wears — the outside tint on every
// tile, the exit border on a descent — is this DECLARATION, not the node
// recognizing the kind "fs". A rootless fs is still a projection of the
// host, so it declares the same thing.
func TestInfoDeclaresHostContent(t *testing.T) {
	ctx := context.Background()
	for _, cfg := range []map[string]string{{"root": "/srv/docs"}, {}} {
		impl, err := FromConfig(cfg)
		if err != nil {
			t.Fatal(err)
		}
		info, err := impl.(*Plugin).Info(ctx, &pluginv1.InfoRequest{})
		if err != nil {
			t.Fatal(err)
		}
		if !info.GetHostContent() {
			t.Errorf("config %v → host_content false; a directory tree is host state", cfg)
		}
	}
}
