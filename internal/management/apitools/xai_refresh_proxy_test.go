package apitools

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

// Every possible destination is a loopback fake, including the token endpoint.
// The proxy fakes answer directly and never forward or contact a provider.
func TestManagementXAIRefreshProxySelection(t *testing.T) {
	cases := []struct {
		name      string
		proxyID   string
		enabled   bool
		legacy    bool
		global    bool
		nilConfig bool
		reject    bool
		want      string
	}{
		{name: "pool before legacy and global", proxyID: "pool", enabled: true, legacy: true, global: true, want: "pool"},
		{name: "pool failure must not fall back", proxyID: "pool", enabled: true, legacy: true, global: true, reject: true, want: "pool"},
		{name: "pool without global", proxyID: "pool", enabled: true, want: "pool"},
		{name: "legacy before global", legacy: true, global: true, want: "legacy"},
		{name: "unbound global", global: true, want: "global"},
		{name: "unknown pool legacy", proxyID: "missing", legacy: true, global: true, want: "legacy"},
		{name: "unknown pool global", proxyID: "missing", global: true, want: "global"},
		{name: "disabled pool legacy", proxyID: "pool", legacy: true, global: true, want: "legacy"},
		{name: "disabled pool global", proxyID: "pool", global: true, want: "global"},
		{name: "no proxy direct", want: "direct"},
		{name: "nil config legacy", nilConfig: true, legacy: true, want: "legacy"},
		{name: "nil config direct", nilConfig: true, want: "direct"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			counts := map[string]*atomic.Int32{}
			servers := map[string]*httptest.Server{}
			for _, name := range []string{"direct", "pool", "legacy", "global"} {
				name := name
				count := &atomic.Int32{}
				counts[name] = count
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					count.Add(1)
					if r.Method != http.MethodPost || r.URL.Path != "/token" {
						t.Errorf("unexpected request: %s %s", r.Method, r.URL)
						http.Error(w, "unexpected request", http.StatusBadRequest)
						return
					}
					if err := r.ParseForm(); err != nil || r.Form.Get("grant_type") != "refresh_token" || r.Form.Get("refresh_token") == "" {
						t.Errorf("invalid refresh form: %v", err)
						http.Error(w, "invalid form", http.StatusBadRequest)
						return
					}
					if tc.reject {
						http.Error(w, "fake proxy rejected", http.StatusBadGateway)
						return
					}
					w.Header().Set("Content-Type", "application/json")
					_, _ = fmt.Fprintf(w, `{"access_token":%q,"refresh_token":"fake-rotated","expires_in":3600,"token_type":"Bearer"}`, "fake-access-"+name)
				}))
				t.Cleanup(server.Close)
				servers[name] = server
			}
			cfg := &config.Config{ProxyPool: []config.ProxyPoolEntry{{ID: "pool", URL: servers["pool"].URL, Enabled: tc.enabled}}}
			if tc.global {
				cfg.ProxyURL = servers["global"].URL
			}
			if tc.nilConfig {
				cfg = nil
			}
			var before *config.Config
			if cfg != nil {
				copied := *cfg
				copied.ProxyPool = append([]config.ProxyPoolEntry(nil), cfg.ProxyPool...)
				before = &copied
			}
			auth := &coreauth.Auth{ID: "test-xai", Provider: "xai", ProxyID: tc.proxyID, Metadata: map[string]any{
				"access_token": "fake-expired", "refresh_token": "fake-refresh-" + servers["direct"].URL,
				"token_endpoint": servers["direct"].URL + "/token", "expired": time.Now().Add(-time.Hour).UTC().Format(time.RFC3339),
			}}
			if tc.legacy {
				auth.ProxyURL = servers["legacy"].URL
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			oldRefresh := auth.Metadata["refresh_token"]
			token, err := New(cfg, nil, Dependencies{}).ResolveTokenForAuth(ctx, auth)
			if tc.reject {
				if err == nil || token != "" {
					t.Fatalf("failed proxy returned token %q, err %v", token, err)
				}
			} else if err != nil || token != "fake-access-"+tc.want {
				t.Fatalf("token = %q, err = %v; want %q", token, err, "fake-access-"+tc.want)
			}
			for name, count := range counts {
				want := int32(0)
				if name == tc.want {
					want = 1
				}
				if got := count.Load(); got != want {
					t.Errorf("%s requests = %d, want %d", name, got, want)
				}
			}
			if tc.reject {
				if auth.Metadata["refresh_token"] != oldRefresh || auth.Metadata["access_token"] != "fake-expired" {
					t.Error("failed refresh changed token metadata")
				}
			} else if auth.Metadata["refresh_token"] != "fake-rotated" || auth.Metadata["access_token"] != token {
				t.Errorf("refreshed token metadata was not updated")
			}
			if !reflect.DeepEqual(cfg, before) {
				t.Error("refresh mutated shared config")
			}
		})
	}
}
