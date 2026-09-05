package apitools

import (
	"net/http"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"net/url"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

func TestCachedManagementProxyTransportReusesSameKey(t *testing.T) {
	// Reset cache for isolation.
	managementTransportMu.Lock()
	managementTransportCache = map[managementTransportKey]*managementTransportEntry{}
	managementTransportMu.Unlock()

	sdk := &config.SDKConfig{PreferIPv4: true}
	a := cachedManagementProxyTransport("http://127.0.0.1:18080", sdk)
	b := cachedManagementProxyTransport("http://127.0.0.1:18080", sdk)
	if a == nil || b == nil {
		t.Fatal("expected transports")
	}
	if a != b {
		t.Fatal("same key should reuse transport")
	}
	c := cachedManagementProxyTransport("http://127.0.0.1:18081", sdk)
	if c == nil || c == a {
		t.Fatal("different proxy key must isolate")
	}
	d := cachedManagementProxyTransport("http://127.0.0.1:18080", &config.SDKConfig{PreferIPv4: false})
	if d == nil || d == a {
		t.Fatal("different TLS/IPv4 key must isolate")
	}
}

func TestCachedManagementProxyTransportEvictsOldest(t *testing.T) {
	managementTransportMu.Lock()
	managementTransportCache = map[managementTransportKey]*managementTransportEntry{}
	// Fill beyond max with dummy entries.
	for i := 0; i < maxManagementTransportEntries; i++ {
		key := managementTransportKey{proxyURL: "http://127.0.0.1:" + itoa(20000+i)}
		managementTransportCache[key] = &managementTransportEntry{transport: &http.Transport{}}
	}
	managementTransportMu.Unlock()

	tr := cachedManagementProxyTransport("http://127.0.0.1:29999", &config.SDKConfig{})
	if tr == nil {
		t.Fatal("expected transport after eviction")
	}
	managementTransportMu.Lock()
	n := len(managementTransportCache)
	managementTransportMu.Unlock()
	if n > maxManagementTransportEntries {
		t.Fatalf("cache size %d > max", n)
	}
}
func TestAPICallTransportForURL_SelectsTLSFingerprintForCodexUpstream(t *testing.T) {
	svc := NewForTenant("test-tenant", &config.Config{}, nil, Dependencies{})
	targetURL, _ := url.Parse("https://chatgpt.com/backend-api/wham/usage")
	auth := &coreauth.Auth{Provider: "codex"}
	tr := svc.APICallTransportForURL(auth, targetURL)
	if tr == nil {
		t.Fatal("expected non-nil transport for Codex URL")
	}
}


func itoa(v int) string {
	if v == 0 {
		return "0"
	}
	var b [12]byte
	i := len(b)
	for v > 0 {
		i--
		b[i] = byte('0' + v%10)
		v /= 10
	}
	return string(b[i:])
}
