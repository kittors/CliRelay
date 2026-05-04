package config

import "testing"

func TestNormalizeProxyPoolTrimsDeduplicatesAndValidatesEntries(t *testing.T) {
	t.Parallel()

	input := []ProxyPoolEntry{
		{ID: "  hk-1  ", Name: "  HK 1  ", URL: " socks5://user:pass@127.0.0.1:1080 ", Enabled: true, Description: "  primary  "},
		{ID: "hk-1", Name: "duplicate", URL: "http://127.0.0.1:7890", Enabled: true},
		{ID: "bad", Name: "bad", URL: "ftp://127.0.0.1:21", Enabled: true},
		{ID: "", Name: "auto id", URL: "https://proxy.example.com:8443", Enabled: true},
	}

	got := NormalizeProxyPool(input)

	if len(got) != 2 {
		t.Fatalf("NormalizeProxyPool length = %d, want 2: %#v", len(got), got)
	}
	if got[0].ID != "hk-1" || got[0].Name != "HK 1" || got[0].URL != "socks5://user:pass@127.0.0.1:1080" || got[0].Description != "primary" {
		t.Fatalf("first normalized entry = %#v", got[0])
	}
	if got[1].ID == "" || got[1].URL != "https://proxy.example.com:8443" {
		t.Fatalf("second normalized entry = %#v", got[1])
	}
}

func TestValidateProxyURLAllowsSupportedSchemesOnly(t *testing.T) {
	t.Parallel()

	for _, raw := range []string{
		"http://127.0.0.1:7890",
		"https://proxy.example.com:8443",
		"socks5://user:pass@127.0.0.1:1080",
	} {
		if err := ValidateProxyURL(raw); err != nil {
			t.Fatalf("ValidateProxyURL(%q) returned error: %v", raw, err)
		}
	}

	for _, raw := range []string{"", "127.0.0.1:7890", "ftp://proxy.example.com", "http:///missing-host"} {
		if err := ValidateProxyURL(raw); err == nil {
			t.Fatalf("ValidateProxyURL(%q) returned nil, want error", raw)
		}
	}
}

func TestResolveProxyURLUsesProxyIDBeforeFallback(t *testing.T) {
	t.Parallel()

	cfg := &Config{
		SDKConfig: SDKConfig{ProxyURL: "http://global.example:7890"},
		ProxyPool: []ProxyPoolEntry{
			{ID: "hk", Name: "HK", URL: "socks5://127.0.0.1:1080", Enabled: true},
			{ID: "disabled", Name: "Disabled", URL: "http://disabled.example:7890", Enabled: false},
		},
	}

	tests := []struct {
		name        string
		proxyID     string
		fallbackURL string
		want        string
	}{
		{name: "proxy id wins", proxyID: "hk", fallbackURL: "http://fallback.example:7890", want: "socks5://127.0.0.1:1080"},
		{name: "disabled falls back to entry url", proxyID: "disabled", fallbackURL: "http://fallback.example:7890", want: "http://fallback.example:7890"},
		{name: "missing falls back to entry url", proxyID: "missing", fallbackURL: "http://fallback.example:7890", want: "http://fallback.example:7890"},
		{name: "global fallback", proxyID: "", fallbackURL: "", want: "http://global.example:7890"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := cfg.ResolveProxyURL(tt.proxyID, tt.fallbackURL); got != tt.want {
				t.Fatalf("ResolveProxyURL() = %q, want %q", got, tt.want)
			}
		})
	}
}
