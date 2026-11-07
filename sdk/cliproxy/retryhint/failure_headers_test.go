package retryhint

import (
	"net/http"
	"testing"
)

func TestFromProxy(t *testing.T) {
	cases := []struct {
		name string
		h    http.Header
		want bool
	}{
		{"squid", headers("X-Squid-Error", "ERR_CONNECT_FAIL 111"), true},
		{"bright data", headers("X-Brd-Error", "target_40010"), true},
		{"luminati", headers("X-Luminati-Error", "Failed to connect"), true},
		{"a rate limit hint is not a proxy marker", headers("Retry-After", "5"), false},
		{"empty marker", headers("X-Squid-Error", ""), false},
		{"no headers", nil, false},
		{"unrelated", headers("Server", "nginx", "Via", "1.1 squid"), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := FromProxy(tc.h); got != tc.want {
				t.Fatalf("FromProxy(%v) = %v, want %v", tc.h, got, tc.want)
			}
		})
	}
}

func TestFailureHeadersKeepsHintsAndProxyMarkersOnly(t *testing.T) {
	src := headers(
		"Retry-After", "30",
		"X-Squid-Error", "ERR_CONNECT_FAIL 111",
		"X-Brd-Error", "target_40010",
		"Set-Cookie", "session=secret",
		"Content-Length", "12",
		"X-Request-Id", "req-1",
		"Via", "1.1 squid",
	)
	got := FailureHeaders(src)
	if len(got) != 3 || got.Get("Retry-After") != "30" || got.Get("X-Squid-Error") == "" || got.Get("X-Brd-Error") == "" {
		t.Fatalf("FailureHeaders() = %v, want Retry-After and the two proxy markers only", got)
	}
	if !FromProxy(got) {
		t.Fatal("the proxy marker must survive the filter, or the scheduler cannot tell")
	}
	if FailureHeaders(headers("Set-Cookie", "x")) != nil || FailureHeaders(nil) != nil {
		t.Fatal("nothing to keep must give nil")
	}
	got["Retry-After"][0] = "changed"
	got["X-Squid-Error"][0] = "changed"
	if src.Get("Retry-After") != "30" || src.Get("X-Squid-Error") != "ERR_CONNECT_FAIL 111" {
		t.Fatal("FailureHeaders must copy values")
	}
}
