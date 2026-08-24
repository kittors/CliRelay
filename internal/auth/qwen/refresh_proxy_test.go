package qwen

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
)

// Token refresh must dial the credential's own proxy, and pinning it must not
// leak into the shared config that every other caller reads.
func TestNewQwenAuthWithProxyPinsEgressWithoutMutatingConfig(t *testing.T) {
	cfg := &config.Config{}
	cfg.SDKConfig.ProxyURL = "http://global.example:7890"

	svc := NewQwenAuthWithProxy(cfg, "http://pinned.example:1080")

	if got := cfg.SDKConfig.ProxyURL; got != "http://global.example:7890" {
		t.Fatalf("shared config proxy = %q, want it unchanged", got)
	}
	transport, ok := svc.httpClient.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("transport = %T, want *http.Transport", svc.httpClient.Transport)
	}
	if transport.Proxy == nil {
		t.Fatal("transport has no proxy resolver, so refresh would go out direct")
	}
	proxyURL, err := transport.Proxy(httptest.NewRequest(http.MethodPost, "https://example.com/token", nil))
	if err != nil {
		t.Fatalf("proxy resolver error: %v", err)
	}
	if proxyURL == nil || proxyURL.Host != "pinned.example:1080" {
		t.Fatalf("refresh proxy = %v, want pinned.example:1080", proxyURL)
	}
}

// The OAuth user agent is part of this provider's identity and must survive the
// proxy substitution.
func TestNewQwenAuthWithProxyKeepsUserAgent(t *testing.T) {
	cfg := &config.Config{OAuthUserAgent: "custom-agent/1.0"}

	if got := NewQwenAuthWithProxy(cfg, "http://pinned.example:1080").userAgent; got != "custom-agent/1.0" {
		t.Fatalf("userAgent = %q, want the configured value", got)
	}
	if got := NewQwenAuthWithProxy(&config.Config{}, "").userAgent; got != defaultOAuthUserAgent {
		t.Fatalf("userAgent = %q, want the default", got)
	}
}

func TestNewQwenAuthWithProxyHandlesNilConfig(t *testing.T) {
	if svc := NewQwenAuthWithProxy(nil, "http://pinned.example:1080"); svc == nil || svc.httpClient == nil {
		t.Fatal("nil config must still yield a usable client")
	}
}
