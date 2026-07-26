package util

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestIsLocalOriginRequestAcceptsDirectLoopbackPeers(t *testing.T) {
	for _, remoteAddr := range []string{
		"127.0.0.1:54321",
		"127.0.0.2:54321",
		"[::1]:54321",
		"[::ffff:127.0.0.1]:54321",
	} {
		req := httptest.NewRequest(http.MethodPost, "/v1internal:generateContent", nil)
		req.RemoteAddr = remoteAddr
		if !IsLocalOriginRequest(req) {
			t.Errorf("RemoteAddr %q: IsLocalOriginRequest() = false, want true", remoteAddr)
		}
	}
}

func TestIsLocalOriginRequestRejectsNonLoopbackPeers(t *testing.T) {
	for _, remoteAddr := range []string{
		"203.0.113.10:54321",
		"172.17.0.1:54321",
		"[2001:db8::1]:54321",
		"",
		"not-an-address",
	} {
		req := httptest.NewRequest(http.MethodPost, "/v1internal:generateContent", nil)
		req.RemoteAddr = remoteAddr
		if IsLocalOriginRequest(req) {
			t.Errorf("RemoteAddr %q: IsLocalOriginRequest() = true, want false", remoteAddr)
		}
	}
}

// The reverse-proxy bypass from issue #517: the peer is the local proxy, so RemoteAddr
// looks local even though the original client is external.
func TestIsLocalOriginRequestRejectsRelayedLoopbackPeers(t *testing.T) {
	cases := []struct {
		header string
		value  string
	}{
		{"X-Forwarded-For", "203.0.113.42"},
		{"X-Forwarded-For", "127.0.0.1, 203.0.113.42"},
		{"X-Forwarded-For", "127.0.0.1"},
		{"X-Real-IP", "203.0.113.42"},
		{"X-Forwarded-Proto", "https"},
		{"X-Forwarded-Host", "relay.example.com"},
		{"Forwarded", "for=203.0.113.42;proto=https"},
		{"Via", "1.1 nginx"},
		{"CF-Connecting-IP", "203.0.113.42"},
		{"CF-Ray", "8a1b2c3d4e5f6789-LAX"},
		{"True-Client-IP", "203.0.113.42"},
		{"X-Client-IP", "203.0.113.42"},
		{"X-Cluster-Client-IP", "203.0.113.42"},
		{"X-Original-Forwarded-For", "203.0.113.42"},
		{"Fastly-Client-IP", "203.0.113.42"},
		{"X-Envoy-External-Address", "203.0.113.42"},
		{"X-Azure-ClientIP", "203.0.113.42"},
	}
	for _, tc := range cases {
		req := httptest.NewRequest(http.MethodPost, "/v1internal:generateContent", nil)
		req.RemoteAddr = "127.0.0.1:54321"
		req.Header.Set(tc.header, tc.value)
		if IsLocalOriginRequest(req) {
			t.Errorf("%s: %q: IsLocalOriginRequest() = true, want false", tc.header, tc.value)
		}
		if got := RelayIndicationHeader(req); got != tc.header {
			t.Errorf("%s: RelayIndicationHeader() = %q, want %q", tc.header, got, tc.header)
		}
	}
}

// A header present but empty carries no relay signal; rejecting on it would break local
// clients that send blank headers.
func TestIsLocalOriginRequestIgnoresBlankRelayHeaders(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/v1internal:generateContent", nil)
	req.RemoteAddr = "127.0.0.1:54321"
	req.Header.Set("X-Forwarded-For", "   ")
	if !IsLocalOriginRequest(req) {
		t.Error("blank X-Forwarded-For: IsLocalOriginRequest() = false, want true")
	}
	if got := RelayIndicationHeader(req); got != "" {
		t.Errorf("blank X-Forwarded-For: RelayIndicationHeader() = %q, want empty", got)
	}
}

func TestIsLocalOriginRequestRejectsNilRequest(t *testing.T) {
	if IsLocalOriginRequest(nil) {
		t.Error("IsLocalOriginRequest(nil) = true, want false")
	}
	if got := RelayIndicationHeader(nil); got != "" {
		t.Errorf("RelayIndicationHeader(nil) = %q, want empty", got)
	}
}
