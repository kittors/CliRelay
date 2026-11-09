package executor

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

// recordingProxy is an HTTP proxy that notes which host each request asked it
// to reach and turns every one away, so a refresh fails right after its first
// hop has been seen. The token endpoints are https, so what it sees is the
// CONNECT for host:443.
type recordingProxy struct {
	mu    sync.Mutex
	hosts []string
	url   string
}

func newRecordingProxy(t *testing.T) *recordingProxy {
	t.Helper()
	p := &recordingProxy{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p.mu.Lock()
		p.hosts = append(p.hosts, r.Host)
		p.mu.Unlock()
		http.Error(w, "refused by test proxy", http.StatusForbidden)
	}))
	t.Cleanup(srv.Close)
	p.url = srv.URL
	return p
}

func (p *recordingProxy) seen() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.hosts...)
}

type refresher interface {
	Refresh(context.Context, *cliproxyauth.Auth) (*cliproxyauth.Auth, error)
}

// TestOAuthRefreshLeavesThroughTheCredentialsPoolProxy pins, for each provider
// whose refresh builds its own OAuth client, that a credential bound to a
// proxy-pool entry refreshes through that entry and never through the global
// proxy-url. xAI, Kimi and iFlow used to fall back to the global proxy; on
// 2026-11-09 the global proxy died, and Grok accounts on a healthy pool proxy
// stopped refreshing and went 401 when their access tokens ran out.
func TestOAuthRefreshLeavesThroughTheCredentialsPoolProxy(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name     string
		newExec  func(*config.Config) refresher
		provider string
		metadata map[string]any
		wantHost string
	}{
		{
			name:     "xai",
			newExec:  func(cfg *config.Config) refresher { return NewXAIExecutor(cfg) },
			provider: "xai",
			metadata: map[string]any{
				"refresh_token":  "egress-test-xai-refresh-token",
				"token_endpoint": "https://auth.x.ai/oauth2/token",
			},
			wantHost: "auth.x.ai:443",
		},
		{
			name:     "kimi",
			newExec:  func(cfg *config.Config) refresher { return NewKimiExecutor(cfg) },
			provider: "kimi",
			metadata: map[string]any{"refresh_token": "egress-test-kimi-refresh-token"},
			wantHost: "auth.kimi.com:443",
		},
		{
			name:     "iflow oauth",
			newExec:  func(cfg *config.Config) refresher { return NewIFlowExecutor(cfg) },
			provider: "iflow",
			metadata: map[string]any{"refresh_token": "egress-test-iflow-refresh-token"},
			wantHost: "iflow.cn:443",
		},
		{
			// An empty expiry can't be checked, so the key is refreshed anyway.
			name:     "iflow cookie",
			newExec:  func(cfg *config.Config) refresher { return NewIFlowExecutor(cfg) },
			provider: "iflow",
			metadata: map[string]any{"cookie": "BXAuth=egress-test", "email": "egress-test@example.com"},
			wantHost: "iflow.cn:443",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			global := newRecordingProxy(t)
			pool := newRecordingProxy(t)
			cfg := &config.Config{
				SDKConfig: config.SDKConfig{ProxyURL: global.url},
				ProxyPool: []config.ProxyPoolEntry{{ID: "pool-sg", Name: "SG", URL: pool.url, Enabled: true}},
			}
			auth := &cliproxyauth.Auth{
				ID:       tc.provider + "-egress-test.json",
				Provider: tc.provider,
				ProxyID:  "pool-sg",
				Metadata: tc.metadata,
			}

			if _, err := tc.newExec(cfg).Refresh(context.Background(), auth); err == nil {
				t.Fatalf("refresh succeeded through a proxy that refuses every request")
			}

			hosts := pool.seen()
			if len(hosts) == 0 {
				t.Fatalf("refresh never reached the credential's pool proxy (global proxy saw %v)", global.seen())
			}
			for _, host := range hosts {
				if !strings.HasSuffix(host, tc.wantHost) {
					t.Fatalf("pool proxy was asked for %q, want %q", host, tc.wantHost)
				}
			}
			if got := global.seen(); len(got) != 0 {
				t.Fatalf("refresh went through the global proxy-url as well: %v", got)
			}
		})
	}
}

// TestOAuthRefreshWithoutAPoolBindingKeepsTheGlobalProxy keeps the other half:
// a credential with no proxy of its own still refreshes through the global
// proxy-url, as its requests do.
func TestOAuthRefreshWithoutAPoolBindingKeepsTheGlobalProxy(t *testing.T) {
	t.Parallel()

	global := newRecordingProxy(t)
	cfg := &config.Config{SDKConfig: config.SDKConfig{ProxyURL: global.url}}
	auth := &cliproxyauth.Auth{
		ID:       "xai-unbound-egress-test.json",
		Provider: "xai",
		Metadata: map[string]any{
			"refresh_token":  "egress-test-xai-unbound-refresh-token",
			"token_endpoint": "https://auth.x.ai/oauth2/token",
		},
	}

	if _, err := NewXAIExecutor(cfg).Refresh(context.Background(), auth); err == nil {
		t.Fatalf("refresh succeeded through a proxy that refuses every request")
	}
	hosts := global.seen()
	if len(hosts) == 0 || !strings.HasSuffix(hosts[0], "auth.x.ai:443") {
		t.Fatalf("global proxy saw %v, want the refresh's CONNECT to auth.x.ai:443", hosts)
	}
}
