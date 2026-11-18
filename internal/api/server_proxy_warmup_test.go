package api

import (
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

func TestResolveProxyWarmupURLUsesProxyIDBeforeProxyURL(t *testing.T) {
	t.Parallel()

	cfg := &config.Config{
		ProxyWarmup: config.ProxyWarmConfig{
			ProxyID:  "residential",
			ProxyURL: "http://explicit.example:8080",
		},
		ProxyPool: []config.ProxyPoolEntry{
			{ID: "residential", URL: "socks5://127.0.0.1:1080", Enabled: true},
		},
	}

	if got := resolveProxyWarmupURL(cfg); got != "socks5://127.0.0.1:1080" {
		t.Fatalf("resolveProxyWarmupURL() = %q, want proxy-id target", got)
	}
}

func TestResolveProxyWarmupURLDoesNotFallbackWhenProxyIDMissing(t *testing.T) {
	t.Parallel()

	cfg := &config.Config{
		SDKConfig: config.SDKConfig{ProxyURL: "http://global.example:8080"},
		ProxyWarmup: config.ProxyWarmConfig{
			ProxyID: "missing",
		},
		ProxyPool: []config.ProxyPoolEntry{
			{ID: "residential", URL: "socks5://127.0.0.1:1080", Enabled: true},
		},
	}

	if got := resolveProxyWarmupURL(cfg); got != "" {
		t.Fatalf("resolveProxyWarmupURL() = %q, want empty when explicit proxy-id is missing", got)
	}
}

func TestResolveProxyWarmupURLUsesSingleEnabledProxyPoolEntry(t *testing.T) {
	t.Parallel()

	cfg := &config.Config{
		ProxyPool: []config.ProxyPoolEntry{
			{ID: "disabled", URL: "http://disabled.example:8080", Enabled: false},
			{ID: "residential", URL: "socks5://127.0.0.1:1080", Enabled: true},
		},
	}

	if got := resolveProxyWarmupURL(cfg); got != "socks5://127.0.0.1:1080" {
		t.Fatalf("resolveProxyWarmupURL() = %q, want the only enabled proxy-pool entry", got)
	}
}

func TestResolveProxyWarmupURLRefusesAmbiguousProxyPool(t *testing.T) {
	t.Parallel()

	cfg := &config.Config{
		ProxyPool: []config.ProxyPoolEntry{
			{ID: "one", URL: "socks5://127.0.0.1:1080", Enabled: true},
			{ID: "two", URL: "http://127.0.0.1:8080", Enabled: true},
		},
	}

	if got := resolveProxyWarmupURL(cfg); got != "" {
		t.Fatalf("resolveProxyWarmupURL() = %q, want empty for ambiguous proxy-pool entries", got)
	}
}
