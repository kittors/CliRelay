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

func TestNormalizeProxyPoolEmptyChineseNameUsesURLHash(t *testing.T) {
	t.Parallel()

	a := NormalizeProxyPool([]ProxyPoolEntry{
		{Name: "洛杉矶 ip", URL: "socks5://user:pass@1.2.3.4:1080", Enabled: true},
	})
	b := NormalizeProxyPool([]ProxyPoolEntry{
		{Name: "住宅 ip", URL: "socks5://user:pass@5.6.7.8:1080", Enabled: true},
	})
	if len(a) != 1 || len(b) != 1 {
		t.Fatalf("normalize lengths a=%d b=%d", len(a), len(b))
	}
	if a[0].ID == "ip" || b[0].ID == "ip" {
		t.Fatalf("CJK names must not collapse to id=ip: a=%q b=%q", a[0].ID, b[0].ID)
	}
	if a[0].ID == b[0].ID {
		t.Fatalf("different proxy URLs must get different auto ids: %q", a[0].ID)
	}
}

func TestProxyPoolDuplicateIDsReportsCollisions(t *testing.T) {
	t.Parallel()

	dups := ProxyPoolDuplicateIDs([]ProxyPoolEntry{
		{ID: "ip", Name: "old", URL: "socks5://1.1.1.1:1080", Enabled: true},
		{ID: "IP", Name: "new", URL: "socks5://2.2.2.2:1080", Enabled: true},
		{ID: "ok", Name: "ok", URL: "socks5://3.3.3.3:1080", Enabled: true},
	})
	if len(dups) != 1 || dups[0] != "ip" {
		t.Fatalf("ProxyPoolDuplicateIDs = %#v, want [ip]", dups)
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

func TestFindProxyPoolEntry(t *testing.T) {
	t.Parallel()

	cfg := &Config{
		ProxyPool: []ProxyPoolEntry{
			{ID: "hk", Name: "HK", URL: "socks5://127.0.0.1:1080", Enabled: true},
			{ID: "disabled", Name: "Disabled", URL: "http://disabled.example:7890", Enabled: false},
		},
	}

	if entry := cfg.FindProxyPoolEntry("hk"); entry == nil || entry.URL != "socks5://127.0.0.1:1080" {
		t.Fatalf("FindProxyPoolEntry(hk) = %#v, want enabled entry", entry)
	}
	if entry := cfg.FindProxyPoolEntry("disabled"); entry == nil || entry.URL != "http://disabled.example:7890" {
		t.Fatalf("FindProxyPoolEntry(disabled) = %#v, want disabled entry", entry)
	}
	if entry := cfg.FindProxyPoolEntry("non-existent"); entry != nil {
		t.Fatalf("FindProxyPoolEntry(non-existent) = %#v, want nil", entry)
	}
}
