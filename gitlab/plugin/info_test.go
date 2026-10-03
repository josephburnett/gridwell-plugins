package plugin

import (
	"context"
	"sync"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pluginv1 "github.com/josephburnett/gridwell/api/gen/plugin/v1"
)

// token answers CheckToken with err and counts the asks.
type token struct {
	mu   sync.Mutex
	err  error
	asks int
}

func (k *token) CheckToken(context.Context) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.asks++
	return k.err
}

func (k *token) set(err error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.err = err
}

func (k *token) count() int {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.asks
}

// A token GitLab refuses is a config the plugin cannot serve: Info refuses
// with GitLab's sentence and asks again on every call, so a fixed token heals
// without a restart. Once Info has passed it never asks again, so a token
// revoked later is weather memory answers through, not a refusal.
func TestInfoRefusesARefusedTokenThenLatches(t *testing.T) {
	const sentence = "GitLab refused the token in /etc/gitlab-token (401 Unauthorized): write a personal access token with the read_api scope there"
	k := &token{err: status.Error(codes.PermissionDenied, sentence)}
	p := New(&oneShot{}, Options{Token: k})
	ctx := context.Background()
	for range 2 {
		_, err := p.Info(ctx, &pluginv1.InfoRequest{})
		if status.Code(err) != codes.FailedPrecondition || status.Convert(err).Message() != sentence {
			t.Fatalf("Info with a refused token = %v, want FailedPrecondition with GitLab's sentence", err)
		}
	}
	if k.count() != 2 {
		t.Errorf("two refused Infos asked GitLab %d times, want each to ask", k.count())
	}

	k.set(nil)
	if info, err := p.Info(ctx, &pluginv1.InfoRequest{}); err != nil || len(info.MenuEntries) != 1 {
		t.Fatalf("Info once the token is fixed = (%v, %v)", info, err)
	}
	k.set(status.Error(codes.PermissionDenied, sentence))
	if _, err := p.Info(ctx, &pluginv1.InfoRequest{}); err != nil {
		t.Errorf("Info after a pass = %v; it latched", err)
	}
	if k.count() != 3 {
		t.Errorf("Info asked GitLab %d times, want none after the pass", k.count()-3)
	}
}

// GitLab not answering at the first Info is not a refusal: the plugin passes,
// its entries present, and the source is dark until GitLab answers.
func TestInfoPassesWhenGitLabDoesNotAnswer(t *testing.T) {
	k := &token{err: status.Error(codes.Unavailable, "gitlab: dial tcp: connection refused")}
	p := New(&oneShot{}, Options{Token: k})
	if info, err := p.Info(context.Background(), &pluginv1.InfoRequest{}); err != nil || len(info.MenuEntries) != 1 {
		t.Fatalf("Info with GitLab unreachable = (%v, %v), want a pass", info, err)
	}
	k.set(status.Error(codes.PermissionDenied, "refused"))
	if _, err := p.Info(context.Background(), &pluginv1.InfoRequest{}); err != nil {
		t.Errorf("Info after an unreachable pass = %v; it latched", err)
	}
}
