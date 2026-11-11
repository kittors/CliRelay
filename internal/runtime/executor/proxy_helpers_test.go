package executor

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/util"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

func TestNewProxyAwareHTTPClientUsesProxyIDBeforeProxyURL(t *testing.T) {
	t.Parallel()

	proxyHits := 0
	proxyServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proxyHits++
		if r.URL.String() != "http://target.example/check" {
			t.Fatalf("proxy received URL %q", r.URL.String())
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer proxyServer.Close()

	cfg := &config.Config{
		ProxyPool: []config.ProxyPoolEntry{
			{ID: "pool", Name: "Pool", URL: proxyServer.URL, Enabled: true},
		},
	}
	auth := &cliproxyauth.Auth{
		ProxyID:  "pool",
		ProxyURL: "http://127.0.0.1:1",
	}
	client := newProxyAwareHTTPClient(context.Background(), cfg, auth, 0)

	req, err := http.NewRequest(http.MethodGet, "http://target.example/check", nil)
	if err != nil {
		t.Fatalf("create request: %v", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("client.Do returned error: %v", err)
	}
	_ = resp.Body.Close()

	if proxyHits != 1 {
		t.Fatalf("proxy hits = %d, want 1", proxyHits)
	}
}

func TestNewProxyAwareHTTPClientFallsBackWhenProxyIDMissing(t *testing.T) {
	t.Parallel()

	proxyServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer proxyServer.Close()

	cfg := &config.Config{
		ProxyPool: []config.ProxyPoolEntry{
			{ID: "other", Name: "Other", URL: "http://127.0.0.1:1", Enabled: true},
		},
	}
	auth := &cliproxyauth.Auth{
		ProxyID:  "missing",
		ProxyURL: proxyServer.URL,
	}
	client := newProxyAwareHTTPClient(context.Background(), cfg, auth, 0)

	resp, err := client.Get("http://target.example/check")
	if err != nil {
		t.Fatalf("client.Get returned error: %v", err)
	}
	_ = resp.Body.Close()
}

func TestNewProxyAwareHTTPClientHonorsPreferIPv4ForHTTPProxy(t *testing.T) {
	t.Parallel()

	listener, err := net.Listen("tcp6", "[::1]:0")
	if err != nil {
		t.Skipf("ipv6 loopback unavailable: %v", err)
	}

	proxyHits := 0
	proxyServer := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proxyHits++
		if r.URL.String() != "http://target.example/check" {
			t.Fatalf("proxy received URL %q", r.URL.String())
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	proxyServer.Listener = listener
	proxyServer.Start()
	defer proxyServer.Close()

	cfg := &config.Config{}
	cfg.PreferIPv4 = true
	auth := &cliproxyauth.Auth{
		ProxyURL: fmt.Sprintf("http://%s", listener.Addr().String()),
	}
	client := newProxyAwareHTTPClient(context.Background(), cfg, auth, 0)

	req, err := http.NewRequest(http.MethodGet, "http://target.example/check", nil)
	if err != nil {
		t.Fatalf("create request: %v", err)
	}
	resp, err := client.Do(req)
	if err == nil {
		_ = resp.Body.Close()
		t.Fatalf("client.Do unexpectedly succeeded via IPv6 proxy while preferIPv4 is enabled")
	}
	if proxyHits != 0 {
		t.Fatalf("proxy hits = %d, want 0 when preferIPv4 blocks IPv6-only proxy", proxyHits)
	}
}

// proxyTransportBehindBreaker returns the cached proxy transport of client.
// Every proxied client gets an egress breaker wrapper of its own in front of
// it, so the wrappers always differ; what is shared, or not, is the transport
// behind them.
func proxyTransportBehindBreaker(client *http.Client) http.RoundTripper {
	if wrapped, ok := client.Transport.(interface{ Unwrap() http.RoundTripper }); ok {
		return wrapped.Unwrap()
	}
	return client.Transport
}

func TestNewProxyAwareHTTPClientReusesProxyTransport(t *testing.T) {
	t.Parallel()

	cfg := &config.Config{}
	auth := &cliproxyauth.Auth{ProxyURL: "http://proxy-reuse.example:8080"}

	first := newProxyAwareHTTPClient(context.Background(), cfg, auth, 0)
	second := newProxyAwareHTTPClient(context.Background(), cfg, auth, 0)

	if first.Transport == nil || second.Transport == nil {
		t.Fatalf("expected proxy transports to be configured")
	}
	if proxyTransportBehindBreaker(first) != proxyTransportBehindBreaker(second) {
		t.Fatalf("expected same proxy transport to be reused")
	}
}

func TestNewProxyAwareHTTPClientSeparatesProxyTransportByPreferIPv4(t *testing.T) {
	t.Parallel()

	auth := &cliproxyauth.Auth{ProxyURL: "http://proxy-ip-mode.example:8080"}
	ipv6Allowed := &config.Config{}
	ipv4Only := &config.Config{}
	ipv4Only.PreferIPv4 = true

	first := newProxyAwareHTTPClient(context.Background(), ipv6Allowed, auth, 0)
	second := newProxyAwareHTTPClient(context.Background(), ipv4Only, auth, 0)

	if first.Transport == nil || second.Transport == nil {
		t.Fatalf("expected proxy transports to be configured")
	}
	if proxyTransportBehindBreaker(first) == proxyTransportBehindBreaker(second) {
		t.Fatalf("expected prefer-ipv4 change to use a distinct proxy transport")
	}
}

func TestNewProxyAwareHTTPClientSeparatesProxyTransportByTLSConfig(t *testing.T) {
	t.Parallel()

	auth := &cliproxyauth.Auth{ProxyURL: "http://proxy-tls-mode.example:8080"}
	normalTLS := &config.Config{}
	insecureTLS := &config.Config{}
	insecureTLS.InsecureSkipVerify = true

	first := newProxyAwareHTTPClient(context.Background(), normalTLS, auth, 0)
	second := newProxyAwareHTTPClient(context.Background(), insecureTLS, auth, 0)

	if first.Transport == nil || second.Transport == nil {
		t.Fatalf("expected proxy transports to be configured")
	}
	if proxyTransportBehindBreaker(first) == proxyTransportBehindBreaker(second) {
		t.Fatalf("expected TLS config change to use a distinct proxy transport")
	}
	transport, ok := proxyTransportBehindBreaker(second).(*http.Transport)
	if !ok {
		t.Fatalf("second transport type = %T, want *http.Transport", proxyTransportBehindBreaker(second))
	}
	if transport.TLSClientConfig == nil || !transport.TLSClientConfig.InsecureSkipVerify {
		t.Fatalf("expected insecure TLS setting to be applied to cached proxy transport")
	}
}

func TestNewProxyAwareHTTPClientRecordsProxyUpstreamTiming(t *testing.T) {
	gin.SetMode(gin.TestMode)

	proxyServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.String() != "http://target.example/check" {
			t.Fatalf("proxy received URL %q", r.URL.String())
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer proxyServer.Close()

	ginCtx, _ := gin.CreateTestContext(httptest.NewRecorder())
	inbound := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	ginCtx.Request = inbound
	ctx := context.WithValue(inbound.Context(), util.ContextKeyGin, ginCtx)

	cfg := &config.Config{}
	auth := &cliproxyauth.Auth{ProxyURL: proxyServer.URL}
	client := newProxyAwareHTTPClient(ctx, cfg, auth, 0)

	resp, err := client.Get("http://target.example/check")
	if err != nil {
		t.Fatalf("client.Get returned error: %v", err)
	}
	_ = resp.Body.Close()

	var detail struct {
		Egress map[string]any `json:"egress"`
		Timing map[string]any `json:"upstream_timing"`
	}
	if err := json.Unmarshal([]byte(buildRequestDetailContent(ctx)), &detail); err != nil {
		t.Fatalf("unmarshal request details: %v", err)
	}
	if detail.Egress["route_kind"] != "proxy" {
		t.Fatalf("egress.route_kind = %v, want proxy", detail.Egress["route_kind"])
	}
	if detail.Timing["host"] != "target.example" {
		t.Fatalf("upstream_timing.host = %v, want target.example", detail.Timing["host"])
	}
	if _, ok := detail.Timing["got_conn_reused"]; !ok {
		t.Fatalf("upstream_timing missing got_conn_reused: %#v", detail.Timing)
	}
}

func TestNewProxyAwareHTTPClientReusesDirectTransport(t *testing.T) {
	t.Parallel()

	cfg := &config.Config{}
	first := newProxyAwareHTTPClient(context.Background(), cfg, nil, 0)
	second := newProxyAwareHTTPClient(context.Background(), cfg, nil, 0)

	if first.Transport == nil || second.Transport == nil {
		t.Fatal("expected direct transports")
	}
	if first.Transport != second.Transport {
		t.Fatal("expected direct requests with the same network configuration to reuse one transport")
	}
}

func TestNewProxyAwareHTTPClientSeparatesDirectTransportByNetworkConfig(t *testing.T) {
	t.Parallel()

	normal := &config.Config{}
	ipv4Only := &config.Config{}
	ipv4Only.PreferIPv4 = true
	insecureTLS := &config.Config{}
	insecureTLS.InsecureSkipVerify = true

	normalClient := newProxyAwareHTTPClient(context.Background(), normal, nil, 0)
	ipv4Client := newProxyAwareHTTPClient(context.Background(), ipv4Only, nil, 0)
	insecureClient := newProxyAwareHTTPClient(context.Background(), insecureTLS, nil, 0)

	if normalClient.Transport == ipv4Client.Transport {
		t.Fatal("prefer-ipv4 change must use a distinct direct transport")
	}
	if normalClient.Transport == insecureClient.Transport {
		t.Fatal("TLS configuration change must use a distinct direct transport")
	}
}

func TestNewProxyAwareHTTPClientReusesDirectConnectionAcrossClients(t *testing.T) {
	t.Parallel()

	var newConnections atomic.Int32
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", "2")
		_, _ = w.Write([]byte("ok"))
	}))
	server.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateNew {
			newConnections.Add(1)
		}
	}
	server.Start()
	defer server.Close()

	cfg := &config.Config{}
	for range 2 {
		client := newProxyAwareHTTPClient(context.Background(), cfg, nil, 0)
		resp, err := client.Get(server.URL)
		if err != nil {
			t.Fatalf("client.Get: %v", err)
		}
		if _, err = io.Copy(io.Discard, resp.Body); err != nil {
			t.Fatalf("read response: %v", err)
		}
		if err = resp.Body.Close(); err != nil {
			t.Fatalf("close response: %v", err)
		}
	}
	if got := newConnections.Load(); got != 1 {
		t.Fatalf("new connections = %d, want 1 shared keep-alive connection", got)
	}
}
