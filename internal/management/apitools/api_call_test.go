package apitools

import (
	"context"
	"crypto/x509"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"syscall"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

func withMockAPICallLookup(t *testing.T, fn func(ctx context.Context, host string) ([]netip.Addr, error)) {
	t.Helper()
	prev := apiCallLookupNetIP
	apiCallLookupNetIP = fn
	t.Cleanup(func() {
		apiCallLookupNetIP = prev
	})
}

func TestValidateAPICallURLClassifiesBlockReasons(t *testing.T) {
	ctx := context.Background()

	t.Run("unsupported scheme", func(t *testing.T) {
		u, _ := url.Parse("ftp://example.com/file")
		err := validateAPICallURL(ctx, u, false)
		if err == nil || err.Code != "unsupported_url_scheme" || !strings.Contains(err.Message, "ftp") {
			t.Fatalf("got %+v, want unsupported_url_scheme mentioning ftp", err)
		}
	})

	t.Run("missing host", func(t *testing.T) {
		u, _ := url.Parse("https:///only-path")
		err := validateAPICallURL(ctx, u, false)
		if err == nil || err.Code != "missing_url_host" {
			t.Fatalf("got %+v, want missing_url_host", err)
		}
	})

	t.Run("direct domain resolving to Fake-IP 198.18.x.x", func(t *testing.T) {
		withMockAPICallLookup(t, func(context.Context, string) ([]netip.Addr, error) {
			return []netip.Addr{netip.MustParseAddr("198.18.0.42")}, nil
		})
		u, _ := url.Parse("https://chatgpt.com/backend-api/wham/rate-limit-reset-credits")
		err := validateAPICallURL(ctx, u, false)
		if err == nil || err.Code != "blocked_fake_ip_address" {
			t.Fatalf("got %+v, want blocked_fake_ip_address", err)
		}
		if !strings.Contains(err.Message, "198.18.0.0/15") || !strings.Contains(err.Message, "Fake-IP") {
			t.Fatalf("message should explain Fake-IP and remediation, got %q", err.Message)
		}
	})

	t.Run("direct domain resolving to private IP", func(t *testing.T) {
		withMockAPICallLookup(t, func(context.Context, string) ([]netip.Addr, error) {
			return []netip.Addr{netip.MustParseAddr("10.0.0.8")}, nil
		})
		u, _ := url.Parse("https://internal.example.test/api")
		err := validateAPICallURL(ctx, u, false)
		if err == nil || err.Code != "blocked_private_address" || !strings.Contains(err.Message, "10.0.0.8") {
			t.Fatalf("got %+v, want blocked_private_address mentioning 10.0.0.8", err)
		}
	})

	t.Run("direct domain DNS failure", func(t *testing.T) {
		withMockAPICallLookup(t, func(context.Context, string) ([]netip.Addr, error) {
			return nil, &net.DNSError{Err: "no such host", Name: "chatgpt.com"}
		})
		u, _ := url.Parse("https://chatgpt.com/backend-api/wham/usage")
		err := validateAPICallURL(ctx, u, false)
		if err == nil || err.Code != "dns_lookup_failed" || !strings.Contains(err.Message, "chatgpt.com") {
			t.Fatalf("got %+v, want dns_lookup_failed mentioning chatgpt.com", err)
		}
	})

	t.Run("explicit proxy bypasses host local Fake-IP/DNS for domain targets", func(t *testing.T) {
		withMockAPICallLookup(t, func(context.Context, string) ([]netip.Addr, error) {
			return []netip.Addr{netip.MustParseAddr("198.18.0.99")}, nil
		})
		u, _ := url.Parse("https://chatgpt.com/backend-api/wham/rate-limit-reset-credits")
		if err := validateAPICallURL(ctx, u, true); err != nil {
			t.Fatalf("expected domain through explicit proxy to be allowed, got %+v", err)
		}
	})

	t.Run("explicit proxy still blocks literal loopback, private, Fake-IP, and localhost", func(t *testing.T) {
		for _, raw := range []string{
			"http://127.0.0.1:8080/admin",
			"http://10.1.2.3/secret",
			"http://198.18.0.10/fake",
			"http://localhost:9000/status",
			"http://sub.localhost/status",
		} {
			u, _ := url.Parse(raw)
			if err := validateAPICallURL(ctx, u, true); err == nil {
				t.Fatalf("expected %q to stay blocked even with explicit proxy", raw)
			}
		}
	})
}

func TestServiceAPICallExplicitAuthProxyBypassesFakeIPDNS(t *testing.T) {
	// Simulate a host running a Clash/mihomo TUN Fake-IP DNS resolver.
	withMockAPICallLookup(t, func(context.Context, string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("198.18.1.23")}, nil
	})

	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"credits":2}`))
	}))
	t.Cleanup(proxy.Close)

	auth := &coreauth.Auth{
		ID:       "codex-proxied.json",
		Provider: "codex",
		ProxyURL: proxy.URL,
		Metadata: map[string]any{"access_token": "tok-1"},
	}
	auth.EnsureIndex()

	manager := coreauth.NewManager(nil, nil, nil)
	if _, err := manager.Register(context.Background(), auth); err != nil {
		t.Fatalf("register auth: %v", err)
	}

	svc := New(&config.Config{}, manager, Dependencies{})
	idx := auth.EnsureIndex()
	status, payload := svc.APICall(context.Background(), APICallRequest{
		AuthIndexSnake: &idx,
		Method:         http.MethodGet,
		URL:            "http://chatgpt.com/backend-api/wham/rate-limit-reset-credits",
		Header:         map[string]string{"Authorization": "Bearer $TOKEN$"},
	})
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200; payload=%#v", status, payload)
	}
	resp, ok := payload.(APICallResponse)
	if !ok || resp.StatusCode != http.StatusOK || !strings.Contains(resp.Body, `"credits":2`) {
		t.Fatalf("unexpected APICallResponse: %#v", payload)
	}

	// Without a proxy, the exact same domain fails with a clear blocked_fake_ip_address error.
	directSvc := New(&config.Config{}, nil, Dependencies{})
	status, payload = directSvc.APICall(context.Background(), APICallRequest{
		Method: http.MethodGet,
		URL:    "https://chatgpt.com/backend-api/wham/rate-limit-reset-credits",
	})
	if status != http.StatusBadRequest {
		t.Fatalf("direct Fake-IP status = %d, want 400; payload=%#v", status, payload)
	}
	apiErr, ok := payload.(APIErrorPayload)
	if !ok || apiErr.Error.Code != "blocked_fake_ip_address" {
		t.Fatalf("direct Fake-IP payload = %#v, want code blocked_fake_ip_address", payload)
	}
}

func TestDescribeUpstreamNetworkErrorAndSanitize(t *testing.T) {
	cases := []struct {
		name     string
		err      error
		wantCode string
	}{
		{"canceled", context.Canceled, "request_canceled"},
		{"deadline", context.DeadlineExceeded, "upstream_timeout"},
		{"dns", &net.DNSError{Err: "no such host", Name: "api.example.com"}, "dns_lookup_failed"},
		{"proxy connect", errors.New("proxyconnect tcp: dial tcp 127.0.0.1:7890: connect: connection refused"), "proxy_connection_failed"},
		{"proxy timeout", errors.New("proxyconnect tcp: i/o timeout"), "proxy_timeout"},
		{"tls cert", x509.UnknownAuthorityError{}, "upstream_tls_error"},
		{"conn refused", &net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNREFUSED}, "connection_refused"},
		{"conn reset", &net.OpError{Op: "read", Net: "tcp", Err: syscall.ECONNRESET}, "connection_reset"},
		{"generic", errors.New("unexpected upstream stream break"), "upstream_request_failed"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, msg := DescribeUpstreamNetworkError(tc.err)
			if code != tc.wantCode {
				t.Fatalf("code = %q, want %q (msg=%q)", code, tc.wantCode, msg)
			}
			if strings.TrimSpace(msg) == "" {
				t.Fatal("message must not be empty")
			}
		})
	}

	sanitized := SanitizeErrorText("proxy http://alice:supersecret@1.2.3.4:8080 failed with Bearer eyJsecretToken123456 and sk-proj-secretkey999999")
	for _, secret := range []string{"supersecret", "eyJsecretToken123456", "sk-proj-secretkey999999"} {
		if strings.Contains(sanitized, secret) {
			t.Fatalf("SanitizeErrorText leaked %q in %q", secret, sanitized)
		}
	}
}
