package util

import (
	"net/http"
	"testing"
)

func TestWebsocketOriginAllowed_AllowsEmptyOrigin(t *testing.T) {
	r, _ := http.NewRequest(http.MethodGet, "http://example.com/v1/ws", nil)
	r.Host = "example.com"
	if !WebsocketOriginAllowed(r) {
		t.Fatal("expected empty Origin to be allowed")
	}
}

func TestWebsocketOriginAllowed_AllowsSameHostOrigin(t *testing.T) {
	r, _ := http.NewRequest(http.MethodGet, "http://example.com/v1/ws", nil)
	r.Host = "example.com"
	r.Header.Set("Origin", "https://example.com")
	if !WebsocketOriginAllowed(r) {
		t.Fatal("expected same-host Origin to be allowed")
	}
}

func TestWebsocketOriginAllowed_DeniesCrossHostOrigin(t *testing.T) {
	r, _ := http.NewRequest(http.MethodGet, "http://example.com/v1/ws", nil)
	r.Host = "example.com"
	r.Header.Set("Origin", "https://evil.example")
	if WebsocketOriginAllowed(r) {
		t.Fatal("expected cross-host Origin to be denied")
	}
}

func TestWebsocketOriginAllowed_DeniesForwardedHostSpoof(t *testing.T) {
	r, _ := http.NewRequest(http.MethodGet, "http://internal.local/v1/ws", nil)
	r.Host = "internal.local"
	r.Header.Set("X-Forwarded-Host", "panel.example.com")
	r.Header.Set("Origin", "https://panel.example.com")
	if WebsocketOriginAllowed(r) {
		t.Fatal("expected spoofed X-Forwarded-Host not to bypass Origin check")
	}
}
