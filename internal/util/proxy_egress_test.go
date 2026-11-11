package util

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/testutil/proxytest"
	"github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/egressbreaker"
)

// These tests pin down which failures of a proxied request are marked as the
// proxy's own (package egressbreaker opens on those and on nothing else), and
// that marking leaves the error text as it was.

func getThroughProxy(t *testing.T, proxyURL, target string) error {
	t.Helper()
	transport := BuildProxyTransport(proxyURL, true)
	if transport == nil {
		t.Fatalf("BuildProxyTransport(%q) returned no transport", proxyURL)
	}
	defer transport.CloseIdleConnections()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	resp, err := transport.RoundTrip(req)
	if err != nil {
		return err
	}
	_ = resp.Body.Close()
	return nil
}

func deadLoopbackAddr(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := listener.Addr().String()
	_ = listener.Close()
	return addr
}

// connectStatusProxy answers every CONNECT with status.
func connectStatusProxy(t *testing.T, status int) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(status)
	}))
	t.Cleanup(server.Close)
	return server
}

func TestBuildProxyTransportMarksRejectedSOCKS5Credentials(t *testing.T) {
	proxy := proxytest.NewSOCKS5(t)
	proxy.RequireAuth(func(string, string) bool { return false })

	err := getThroughProxy(t, proxy.URL("tenant", "expired"), "http://upstream.invalid/")
	if got := egressbreaker.FaultOf(err); got != egressbreaker.FaultAuthRejected {
		t.Fatalf("FaultOf = %v for %v, want the authentication rejection", got, err)
	}
	// The text is still the SOCKS library's own, the one in the logs of
	// 2026-11-11; the mark adds nothing to it.
	want := "socks connect tcp " + proxy.Addr() + "->upstream.invalid:80: username/password authentication failed"
	if err.Error() != want {
		t.Fatalf("error = %q, want %q", err, want)
	}
}

func TestBuildProxyTransportMarksUnreachableSOCKS5Proxy(t *testing.T) {
	err := getThroughProxy(t, "socks5://tenant:valid@"+deadLoopbackAddr(t), "http://upstream.invalid/")
	if got := egressbreaker.FaultOf(err); got != egressbreaker.FaultUnreachable {
		t.Fatalf("FaultOf = %v for %v, want unreachable", got, err)
	}
	if !strings.HasPrefix(err.Error(), "socks connect tcp ") {
		t.Fatalf("error = %q, want the SOCKS dial error unchanged", err)
	}
}

func TestBuildProxyTransportLeavesSOCKS5DestinationFailuresUnmarked(t *testing.T) {
	proxy := proxytest.NewSOCKS5(t)
	proxy.ReplyToConnect(0x04) // host unreachable

	err := getThroughProxy(t, proxy.URL("", ""), "http://upstream.invalid/")
	if err == nil {
		t.Fatal("the request succeeded although the proxy refused the CONNECT")
	}
	if got := egressbreaker.FaultOf(err); got != egressbreaker.FaultNone {
		t.Fatalf("FaultOf = %v for %v, want none", got, err)
	}
}

func TestBuildProxyTransportMarksHTTPProxyThatRejectsCredentials(t *testing.T) {
	proxy := connectStatusProxy(t, http.StatusProxyAuthRequired)

	err := getThroughProxy(t, proxy.URL, "https://upstream.invalid/")
	if got := egressbreaker.FaultOf(err); got != egressbreaker.FaultAuthRejected {
		t.Fatalf("FaultOf = %v for %v, want the authentication rejection", got, err)
	}
	// What net/http reports for a refused CONNECT without the hook.
	if err.Error() != "Proxy Authentication Required" {
		t.Fatalf("error = %q, want the status text net/http uses", err)
	}
}

func TestBuildProxyTransportLeavesOtherConnectStatusesUnmarked(t *testing.T) {
	proxy := connectStatusProxy(t, http.StatusBadGateway)

	err := getThroughProxy(t, proxy.URL, "https://upstream.invalid/")
	if err == nil || err.Error() != "Bad Gateway" {
		t.Fatalf("error = %v, want net/http's own error for a 502 to CONNECT", err)
	}
	if got := egressbreaker.FaultOf(err); got != egressbreaker.FaultNone {
		t.Fatalf("FaultOf = %v for a 502, which may be about the destination; want none", got)
	}
}

func TestBuildProxyTransportMarksUnreachableHTTPProxy(t *testing.T) {
	err := getThroughProxy(t, "http://"+deadLoopbackAddr(t), "https://upstream.invalid/")
	if got := egressbreaker.FaultOf(err); got != egressbreaker.FaultUnreachable {
		t.Fatalf("FaultOf = %v for %v, want unreachable", got, err)
	}
	if !strings.HasPrefix(err.Error(), "proxyconnect tcp: ") {
		t.Fatalf("error = %q, want net/http's proxyconnect error unchanged", err)
	}
}

func TestBuildProxyTransportDoesNotMarkADialTheCallerCancelled(t *testing.T) {
	transport := BuildProxyTransport("http://"+deadLoopbackAddr(t), true)
	defer transport.CloseIdleConnections()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://upstream.invalid/", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	resp, err := transport.RoundTrip(req)
	if err == nil {
		_ = resp.Body.Close()
		t.Fatal("a cancelled request succeeded")
	}
	if got := egressbreaker.FaultOf(err); got != egressbreaker.FaultNone {
		t.Fatalf("FaultOf = %v for a request cancelled before it connected (%v), want none", got, err)
	}
}
