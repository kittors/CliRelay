package executor

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

// TestResolveAuthProxyURLMatchesRequestEgress pins the property the outage came
// down to: whatever proxy a credential's requests leave through, its token
// refresh has to leave through the same one. Production had refresh on the
// process default while requests used a proxy-pool entry, so upstream saw the
// account acting from two regions and refused to refresh.
func TestResolveAuthProxyURLMatchesRequestEgress(t *testing.T) {
	t.Parallel()

	cfg := &config.Config{
		SDKConfig: config.SDKConfig{ProxyURL: "http://global.example:7890"},
		ProxyPool: []config.ProxyPoolEntry{
			{ID: "pool-hk", Name: "HK", URL: "socks5://10.0.0.1:1080", Enabled: true},
			{ID: "pool-off", Name: "Off", URL: "socks5://10.0.0.2:1080", Enabled: false},
		},
	}

	tests := []struct {
		name string
		auth *cliproxyauth.Auth
		want string
	}{
		{
			name: "pool entry wins over global",
			auth: &cliproxyauth.Auth{ProxyID: "pool-hk"},
			want: "socks5://10.0.0.1:1080",
		},
		{
			name: "auth url wins over global",
			auth: &cliproxyauth.Auth{ProxyURL: "socks5://10.0.0.9:1080"},
			want: "socks5://10.0.0.9:1080",
		},
		{
			name: "disabled pool entry falls back to the auth url",
			auth: &cliproxyauth.Auth{ProxyID: "pool-off", ProxyURL: "socks5://10.0.0.9:1080"},
			want: "socks5://10.0.0.9:1080",
		},
		{
			name: "no credential proxy keeps the global default",
			auth: &cliproxyauth.Auth{},
			want: "http://global.example:7890",
		},
		{
			name: "nil auth keeps the global default",
			auth: nil,
			want: "http://global.example:7890",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := resolveAuthProxyURL(cfg, tt.auth); got != tt.want {
				t.Fatalf("resolveAuthProxyURL = %q, want %q", got, tt.want)
			}
		})
	}
}

// The refresh path must agree with the request path by construction, so the two
// resolvers are compared directly rather than restated as literals.
func TestResolveAuthProxyURLAgreesWithRequestClientResolution(t *testing.T) {
	t.Parallel()

	cfg := &config.Config{
		SDKConfig: config.SDKConfig{ProxyURL: "http://global.example:7890"},
		ProxyPool: []config.ProxyPoolEntry{
			{ID: "pool-hk", Name: "HK", URL: "socks5://10.0.0.1:1080", Enabled: true},
		},
	}
	auth := &cliproxyauth.Auth{ProxyID: "pool-hk", ProxyURL: "socks5://ignored.example:1080"}

	want := cfg.ResolveProxyURL(auth.ProxyID, auth.ProxyURL)
	if got := resolveAuthProxyURL(cfg, auth); got != want {
		t.Fatalf("refresh proxy %q diverged from request proxy %q", got, want)
	}
}

func TestResolveAuthProxyURLWithoutConfig(t *testing.T) {
	t.Parallel()

	if got := resolveAuthProxyURL(nil, &cliproxyauth.Auth{ProxyURL: "socks5://10.0.0.9:1080"}); got != "socks5://10.0.0.9:1080" {
		t.Fatalf("resolveAuthProxyURL = %q, want the auth proxy", got)
	}
	if got := resolveAuthProxyURL(nil, nil); got != "" {
		t.Fatalf("resolveAuthProxyURL = %q, want empty", got)
	}
}

func TestBoundProxyUnavailableFailsClosedAcrossHTTPWebsocketAndRefresh(t *testing.T) {
	t.Parallel()

	var directHits atomic.Int32
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		directHits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer origin.Close()

	cfg := &config.Config{
		SDKConfig: config.SDKConfig{ProxyURL: origin.URL},
		ProxyPool: []config.ProxyPoolEntry{
			{ID: "pool-off", Name: "Off", URL: "socks5://10.0.0.2:1080", Enabled: false},
		},
	}

	cases := []struct {
		name string
		auth *cliproxyauth.Auth
	}{
		{
			name: "disabled bound proxy_id",
			auth: &cliproxyauth.Auth{
				ID:       "codex-disabled-proxy",
				Provider: "codex",
				ProxyID:  "pool-off",
				Metadata: map[string]any{"refresh_token": "rt-test"},
			},
		},
		{
			name: "missing bound proxy_id",
			auth: &cliproxyauth.Auth{
				ID:       "codex-missing-proxy",
				Provider: "codex",
				ProxyID:  "does-not-exist",
				Metadata: map[string]any{"refresh_token": "rt-test"},
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := resolveAuthProxyURLStrict(cfg, tc.auth); !errors.Is(err, config.ErrBoundProxyUnavailable) {
				t.Fatalf("resolveAuthProxyURLStrict err = %v, want ErrBoundProxyUnavailable", err)
			}

			httpClient := newProxyAwareHTTPClient(context.Background(), cfg, tc.auth, 2*time.Second)
			resp, err := httpClient.Get(origin.URL)
			if resp != nil {
				_ = resp.Body.Close()
			}
			if !errors.Is(err, config.ErrBoundProxyUnavailable) {
				t.Fatalf("HTTP client err = %v, want ErrBoundProxyUnavailable", err)
			}

			wsDialer := newProxyAwareWebsocketDialer(context.Background(), cfg, tc.auth)
			wsURL := "ws" + strings.TrimPrefix(origin.URL, "http")
			conn, wsResp, wsErr := wsDialer.DialContext(context.Background(), wsURL, nil)
			if conn != nil {
				_ = conn.Close()
			}
			if wsResp != nil && wsResp.Body != nil {
				_ = wsResp.Body.Close()
			}
			if !errors.Is(wsErr, config.ErrBoundProxyUnavailable) {
				t.Fatalf("websocket dial err = %v, want ErrBoundProxyUnavailable", wsErr)
			}

			codexExec := NewCodexExecutor(cfg)
			if _, refreshErr := codexExec.Refresh(context.Background(), tc.auth.Clone()); !errors.Is(refreshErr, config.ErrBoundProxyUnavailable) {
				t.Fatalf("CodexExecutor.Refresh err = %v, want ErrBoundProxyUnavailable", refreshErr)
			}

			claudeExec := NewClaudeExecutor(cfg)
			if _, refreshErr := claudeExec.Refresh(context.Background(), tc.auth.Clone()); !errors.Is(refreshErr, config.ErrBoundProxyUnavailable) {
				t.Fatalf("ClaudeExecutor.Refresh err = %v, want ErrBoundProxyUnavailable", refreshErr)
			}
		})
	}

	t.Run("invalid proxy url fails closed instead of dialing direct", func(t *testing.T) {
		badAuth := &cliproxyauth.Auth{
			ID:       "codex-bad-proxy-url",
			Provider: "codex",
			ProxyURL: "ftp://127.0.0.1:21",
		}
		httpClient := newProxyAwareHTTPClient(context.Background(), &config.Config{}, badAuth, 2*time.Second)
		resp, err := httpClient.Get(origin.URL)
		if resp != nil {
			_ = resp.Body.Close()
		}
		if err == nil {
			t.Fatal("HTTP client with invalid proxy URL succeeded, want fail-closed error")
		}

		wsDialer := newProxyAwareWebsocketDialer(context.Background(), &config.Config{}, badAuth)
		wsURL := "ws" + strings.TrimPrefix(origin.URL, "http")
		conn, wsResp, wsErr := wsDialer.DialContext(context.Background(), wsURL, nil)
		if conn != nil {
			_ = conn.Close()
		}
		if wsResp != nil && wsResp.Body != nil {
			_ = wsResp.Body.Close()
		}
		if wsErr == nil {
			t.Fatal("websocket dialer with invalid proxy URL succeeded, want fail-closed error")
		}
	})

	if hits := directHits.Load(); hits != 0 {
		t.Fatalf("origin/global proxy received %d leaked requests, want 0", hits)
	}
}

func TestCodexAuthClientIdentityUsesConfiguredOrDefaultFingerprint(t *testing.T) {
	t.Parallel()

	ua, orig := codexAuthClientIdentity(&config.Config{}, &cliproxyauth.Auth{Provider: "codex"}, context.Background())
	if ua != config.DefaultCodexFingerprintUserAgent || orig != config.DefaultCodexFingerprintOriginator {
		t.Fatalf("default identity = (%q, %q), want (%q, %q)", ua, orig, config.DefaultCodexFingerprintUserAgent, config.DefaultCodexFingerprintOriginator)
	}

	customCfg := &config.Config{
		IdentityFingerprint: config.IdentityFingerprintConfig{
			Codex: config.CodexIdentityFingerprintConfig{
				Enabled:    true,
				UserAgent:  "codex_cli_rs/0.161.0 (Mac OS 26.5; arm64)",
				Originator: "codex_cli_rs",
			},
		},
	}
	ua, orig = codexAuthClientIdentity(customCfg, &cliproxyauth.Auth{Provider: "codex"}, context.Background())
	if ua != "codex_cli_rs/0.161.0 (Mac OS 26.5; arm64)" || orig != "codex_cli_rs" {
		t.Fatalf("custom identity = (%q, %q)", ua, orig)
	}
}
