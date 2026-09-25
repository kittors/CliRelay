package egresshealth

import (
	"slices"
	"strings"
	"testing"
)

func TestEndpointParsesSupportedProxyURLs(t *testing.T) {
	cases := []struct {
		raw  string
		want string
	}{
		{"socks5://user:secret@203.0.113.9:1080", "203.0.113.9:1080"},
		{"socks5://203.0.113.9", "203.0.113.9:1080"},
		{"socks5h://gate.Example.TEST", "gate.example.test:1080"},
		{"http://proxy.example.test", "proxy.example.test:80"},
		{"https://user:secret@proxy.example.test", "proxy.example.test:443"},
		{"HTTP://Proxy.Example.test:8080/ignored?x=1", "proxy.example.test:8080"},
		{"http://[2001:db8::7]:3128", "[2001:db8::7]:3128"},
		{"  socks5://203.0.113.9:7000  ", "203.0.113.9:7000"},
	}
	for _, tc := range cases {
		got, ok := Endpoint(tc.raw)
		if !ok || got != tc.want {
			t.Errorf("Endpoint(%q) = %q, %v; want %q", tc.raw, got, ok, tc.want)
		}
		if strings.Contains(got, "secret") || strings.Contains(got, "user") {
			t.Errorf("Endpoint(%q) leaks credentials: %q", tc.raw, got)
		}
	}
}

func TestEndpointRejectsWhatIsNotAProxy(t *testing.T) {
	for _, raw := range []string{
		"",
		"direct",
		"203.0.113.9:1080",
		"socks4://203.0.113.9:1080",
		"ftp://203.0.113.9",
		"http://:8080",
		"socks5://user:secret@",
		"socks5://203.0.113.9:0",
		"socks5://203.0.113.9:70000",
		"http://proxy.example.test:port",
		"socks5:203.0.113.9:1080",
	} {
		if got, ok := Endpoint(raw); ok {
			t.Errorf("Endpoint(%q) = %q, want it rejected", raw, got)
		}
	}
}

func TestEndpointsDeduplicatesByHostAndPort(t *testing.T) {
	endpoints, skipped := Endpoints([]string{
		"socks5://alice:one@gate.example.test:7000",
		"socks5h://bob:two@GATE.example.test:7000",
		"",
		"  ",
		"http://203.0.113.9",
		"socks5://203.0.113.5",
		"http://203.0.113.9:80",
		"not a proxy",
		"socks4://203.0.113.9",
	})
	want := []string{"203.0.113.5:1080", "203.0.113.9:80", "gate.example.test:7000"}
	if !slices.Equal(endpoints, want) {
		t.Fatalf("endpoints = %v, want %v", endpoints, want)
	}
	// Blank entries mean "no proxy"; only entries that are set but unusable
	// are reported as skipped.
	if skipped != 2 {
		t.Fatalf("skipped = %d, want 2", skipped)
	}
	if endpoints, skipped := Endpoints(nil); len(endpoints) != 0 || skipped != 0 {
		t.Fatalf("Endpoints(nil) = %v, %d", endpoints, skipped)
	}
}
