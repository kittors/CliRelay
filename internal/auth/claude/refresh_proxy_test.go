package claude

import (
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
)

// Pinning a credential's proxy must not leak into the shared config, and the
// Firefox TLS fingerprint transport must survive it: this provider relies on
// that fingerprint to get past Cloudflare, so losing it would trade one outage
// for another.
func TestNewClaudeAuthWithProxyKeepsFingerprintAndConfig(t *testing.T) {
	cfg := &config.Config{}
	cfg.SDKConfig.ProxyURL = "http://global.example:7890"

	svc := NewClaudeAuthWithProxy(cfg, "socks5://pinned.example:1080")

	if got := cfg.SDKConfig.ProxyURL; got != "http://global.example:7890" {
		t.Fatalf("shared config proxy = %q, want it unchanged", got)
	}
	if svc == nil || svc.httpClient == nil {
		t.Fatal("no client was built")
	}
	if _, ok := svc.httpClient.Transport.(*utlsRoundTripper); !ok {
		t.Fatalf("transport = %T, want the utls fingerprint transport", svc.httpClient.Transport)
	}
}

func TestNewClaudeAuthWithProxyHandlesNilConfig(t *testing.T) {
	if svc := NewClaudeAuthWithProxy(nil, "socks5://pinned.example:1080"); svc == nil || svc.httpClient == nil {
		t.Fatal("nil config must still yield a usable client")
	}
}
