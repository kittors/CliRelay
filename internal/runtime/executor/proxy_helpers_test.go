package executor

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

func TestNewProxyAwareHTTPClientUsesProxyIDBeforeProxyURL(t *testing.T) {
	t.Parallel()

	proxyHits := 0
	proxyServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proxyHits++
		if r.URL.String() != "http://target.example/check" {
			t.Fatalf("proxy received URL %q", r.URL.String())
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer proxyServer.Close()

	cfg := &config.Config{
		ProxyPool: []config.ProxyPoolEntry{
			{ID: "pool", Name: "Pool", URL: proxyServer.URL, Enabled: true},
		},
	}
	auth := &cliproxyauth.Auth{
		ProxyID:  "pool",
		ProxyURL: "http://127.0.0.1:1",
	}
	client := newProxyAwareHTTPClient(context.Background(), cfg, auth, 0)

	req, err := http.NewRequest(http.MethodGet, "http://target.example/check", nil)
	if err != nil {
		t.Fatalf("create request: %v", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("client.Do returned error: %v", err)
	}
	_ = resp.Body.Close()

	if proxyHits != 1 {
		t.Fatalf("proxy hits = %d, want 1", proxyHits)
	}
}

func TestNewProxyAwareHTTPClientFallsBackWhenProxyIDMissing(t *testing.T) {
	t.Parallel()

	proxyServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer proxyServer.Close()

	cfg := &config.Config{
		ProxyPool: []config.ProxyPoolEntry{
			{ID: "other", Name: "Other", URL: "http://127.0.0.1:1", Enabled: true},
		},
	}
	auth := &cliproxyauth.Auth{
		ProxyID:  "missing",
		ProxyURL: proxyServer.URL,
	}
	client := newProxyAwareHTTPClient(context.Background(), cfg, auth, 0)

	resp, err := client.Get("http://target.example/check")
	if err != nil {
		t.Fatalf("client.Get returned error: %v", err)
	}
	_ = resp.Body.Close()
}
