package serviceapp

import (
	"net/http"
	"testing"

	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func TestNewDefaultRoundTripperProvider_HTTPProxyCached(t *testing.T) {
	provider := NewDefaultRoundTripperProvider()
	auth := &coreauth.Auth{ProxyURL: "http://127.0.0.1:8080"}

	first := provider.RoundTripperFor(auth)
	second := provider.RoundTripperFor(auth)
	if first == nil {
		t.Fatal("expected round tripper for http proxy")
	}
	if first != second {
		t.Fatal("expected round tripper cache to reuse the same instance")
	}

	transport, ok := first.(*http.Transport)
	if !ok {
		t.Fatalf("round tripper type = %T, want *http.Transport", first)
	}
	if transport.Proxy == nil {
		t.Fatal("expected proxy func on transport")
	}
}

func TestNewDefaultRoundTripperProvider_UnsupportedProxyReturnsNil(t *testing.T) {
	provider := NewDefaultRoundTripperProvider()
	auth := &coreauth.Auth{ProxyURL: "ftp://127.0.0.1:21"}

	if got := provider.RoundTripperFor(auth); got != nil {
		t.Fatalf("round tripper = %#v, want nil for unsupported proxy scheme", got)
	}
}
