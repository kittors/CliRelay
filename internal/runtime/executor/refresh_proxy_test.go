package executor

import (
	"testing"

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
