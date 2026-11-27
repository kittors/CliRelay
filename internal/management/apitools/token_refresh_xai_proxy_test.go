package apitools

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

func TestRefreshXAIOAuthAccessTokenResolvesProxyID(t *testing.T) {
	for _, tc := range []struct {
		name      string
		proxyID   string
		explicit  bool
		disabled  bool
		nilConfig bool
		want      string
	}{
		{name: "pool proxy", proxyID: "selected", want: "pool"},
		{name: "pool wins over explicit", proxyID: "selected", explicit: true, want: "pool"},
		{name: "explicit fallback", explicit: true, want: "explicit"},
		{name: "unknown ID fallback", proxyID: "missing", explicit: true, want: "explicit"},
		{name: "disabled ID fallback", proxyID: "selected", explicit: true, disabled: true, want: "explicit"},
		{name: "global fallback", want: "global"},
		{name: "nil config explicit", explicit: true, nilConfig: true, want: "explicit"},
		{name: "nil config direct", nilConfig: true, want: "direct"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			newEndpoint := func(name string) *httptest.Server {
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.Method != http.MethodPost || r.URL.Path != "/token" {
						t.Errorf("unexpected token request: %s %s", r.Method, r.URL)
					}
					if err := r.ParseForm(); err != nil {
						t.Errorf("parse form: %v", err)
					}
					if r.Form.Get("grant_type") != "refresh_token" || r.Form.Get("refresh_token") != "test-refresh-token" {
						t.Errorf("unexpected refresh form: %v", r.Form)
					}
					w.Header().Set("Content-Type", "application/json")
					fmt.Fprintf(w, `{"access_token":%q,"refresh_token":"test-rotated-token","expires_in":3600}`, name)
				}))
				t.Cleanup(server.Close)
				return server
			}
			origin := newEndpoint("direct")
			pool := newEndpoint("pool")
			explicit := newEndpoint("explicit")
			global := newEndpoint("global")
			cfg := &config.Config{ProxyPool: []config.ProxyPoolEntry{{ID: "selected", URL: pool.URL, Enabled: !tc.disabled}}}
			cfg.ProxyURL = global.URL
			if tc.nilConfig {
				cfg = nil
			}
			auth := &coreauth.Auth{Provider: "xai", ProxyID: tc.proxyID, Metadata: map[string]any{
				"access_token": "test-expired-token", "refresh_token": "test-refresh-token",
				"expired": "2000-01-01T00:00:00Z", "token_endpoint": origin.URL + "/token",
			}}
			if tc.explicit {
				auth.ProxyURL = explicit.URL
			}
			svc := New(cfg, nil, Dependencies{})
			got, err := svc.refreshXAIOAuthAccessToken(context.Background(), auth)
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Fatalf("refresh used %q route, want %q", got, tc.want)
			}
			if auth.Metadata["access_token"] != tc.want || auth.Metadata["refresh_token"] != "test-rotated-token" {
				t.Fatalf("refreshed tokens not persisted to metadata: %v", auth.Metadata)
			}
		})
	}

	t.Run("disabled or missing proxy_id without explicit fallback fails closed", func(t *testing.T) {
		var leaked atomic.Int32
		sink := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			leaked.Add(1)
			w.WriteHeader(http.StatusOK)
		}))
		defer sink.Close()

		cfg := &config.Config{
			SDKConfig: config.SDKConfig{ProxyURL: sink.URL},
			ProxyPool: []config.ProxyPoolEntry{
				{ID: "selected", URL: "socks5://127.0.0.1:1080", Enabled: false},
			},
		}
		for _, proxyID := range []string{"selected", "missing"} {
			auth := &coreauth.Auth{Provider: "xai", ProxyID: proxyID, Metadata: map[string]any{
				"access_token": "test-expired-token", "refresh_token": "test-refresh-token",
				"expired": "2000-01-01T00:00:00Z", "token_endpoint": sink.URL + "/token",
			}}
			svc := New(cfg, nil, Dependencies{})
			if _, err := svc.refreshXAIOAuthAccessToken(context.Background(), auth); !errors.Is(err, config.ErrBoundProxyUnavailable) {
				t.Fatalf("proxy_id=%q err = %v, want ErrBoundProxyUnavailable", proxyID, err)
			}
		}
		if got := leaked.Load(); got != 0 {
			t.Fatalf("leaked %d refresh calls to global/direct endpoint, want 0", got)
		}
	})
}
