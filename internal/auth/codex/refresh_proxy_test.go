package codex

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
)

func newTestConfig() *config.Config {
	cfg := &config.Config{}
	cfg.SDKConfig.ProxyURL = "http://global.example:7890"
	return cfg
}

// TestNewCodexAuthWithProxyPinsEgress checks that the pinned proxy is what the
// refresh client actually dials, rather than the process-wide default. An HTTP
// proxy is used because that scheme lands on Transport.Proxy and is therefore
// directly observable; socks5 goes through the same substitution but is applied
// as a dialer.
func TestNewCodexAuthWithProxyPinsEgress(t *testing.T) {
	cfg := newTestConfig()

	svc := NewCodexAuthWithProxy(cfg, "http://pinned.example:1080")
	transport, ok := svc.httpClient.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("transport = %T, want *http.Transport", svc.httpClient.Transport)
	}
	if transport.Proxy == nil {
		t.Fatal("transport has no proxy resolver, so refresh would go out direct")
	}
	proxyURL, err := transport.Proxy(httptest.NewRequest(http.MethodPost, "https://auth.openai.com/oauth/token", nil))
	if err != nil {
		t.Fatalf("proxy resolver error: %v", err)
	}
	if proxyURL == nil || proxyURL.Host != "pinned.example:1080" {
		t.Fatalf("refresh proxy = %v, want pinned.example:1080", proxyURL)
	}
}

// The substitution must not leak into the shared config: every other caller
// reads the same struct, and one credential's proxy is not theirs.
func TestNewCodexAuthWithProxyLeavesSharedConfigUntouched(t *testing.T) {
	cfg := newTestConfig()

	_ = NewCodexAuthWithProxy(cfg, "http://pinned.example:1080")

	if got := cfg.SDKConfig.ProxyURL; got != "http://global.example:7890" {
		t.Fatalf("shared config proxy = %q, want it unchanged", got)
	}
}

// An empty proxy keeps the configured default, so callers that have no
// credential-scoped proxy behave exactly as before.
func TestNewCodexAuthWithoutProxyKeepsDefault(t *testing.T) {
	cfg := newTestConfig()

	svc := NewCodexAuthWithProxy(cfg, "   ")
	transport, ok := svc.httpClient.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("transport = %T, want *http.Transport", svc.httpClient.Transport)
	}
	proxyURL, err := transport.Proxy(httptest.NewRequest(http.MethodPost, "https://auth.openai.com/oauth/token", nil))
	if err != nil {
		t.Fatalf("proxy resolver error: %v", err)
	}
	if proxyURL == nil || proxyURL.Host != "global.example:7890" {
		t.Fatalf("refresh proxy = %v, want the global default", proxyURL)
	}
}

func TestNewCodexAuthHandlesNilConfig(t *testing.T) {
	if svc := NewCodexAuthWithProxy(nil, "http://pinned.example:1080"); svc == nil || svc.httpClient == nil {
		t.Fatal("nil config must still yield a usable client")
	}
}
