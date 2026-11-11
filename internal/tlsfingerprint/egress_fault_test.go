package tlsfingerprint

import (
	"net"
	"net/http"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/testutil/proxytest"
	"github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/egressbreaker"
)

// The fingerprint transport tunnels through the proxy itself, so it marks the
// failures of the proxy hop itself (package egressbreaker), as
// util.BuildProxyTransport does for the standard transport.

func roundTripVia(t *testing.T, proxyURL, target string) error {
	t.Helper()
	rt, err := New(Options{Profile: ProfileChrome, ProxyURL: proxyURL, InsecureSkipVerify: true})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer rt.CloseIdleConnections()
	req, err := http.NewRequest(http.MethodGet, target, nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	resp, err := rt.RoundTrip(req)
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

func TestHTTPProxyRejectingCredentialsIsMarked(t *testing.T) {
	origin := newTLSEchoServer(t)
	proxy := newConnectProxy(t, "Basic expected-but-not-sent")

	err := roundTripVia(t, "http://"+proxy.addr(), origin.URL)
	if got := egressbreaker.FaultOf(err); got != egressbreaker.FaultAuthRejected {
		t.Fatalf("FaultOf = %v for %v, want the authentication rejection", got, err)
	}
	if !strings.Contains(err.Error(), "407") {
		t.Fatalf("error = %v, want it to keep the proxy status", err)
	}
}

func TestSOCKS5ProxyRejectingCredentialsIsMarked(t *testing.T) {
	origin := newTLSEchoServer(t)
	proxy := proxytest.NewSOCKS5(t)
	proxy.RequireAuth(func(string, string) bool { return false })

	err := roundTripVia(t, proxy.URL("tenant", "expired"), origin.URL)
	if got := egressbreaker.FaultOf(err); got != egressbreaker.FaultAuthRejected {
		t.Fatalf("FaultOf = %v for %v, want the authentication rejection", got, err)
	}
	if !strings.Contains(err.Error(), "username/password authentication failed") {
		t.Fatalf("error = %v, want the SOCKS error text unchanged", err)
	}
}

func TestSOCKS5ProxyThatWorksIsNotMarked(t *testing.T) {
	origin := newTLSEchoServer(t)
	proxy := proxytest.NewSOCKS5(t)
	proxy.RequireAuth(func(username, password string) bool { return username == "tenant" && password == "valid" })

	if err := roundTripVia(t, proxy.URL("tenant", "valid"), origin.URL); err != nil {
		t.Fatalf("RoundTrip through a working SOCKS5 proxy: %v", err)
	}
	if proxy.Connects() != 1 {
		t.Fatalf("the proxy saw %d CONNECT requests, want 1", proxy.Connects())
	}
}

func TestUnreachableProxyIsMarked(t *testing.T) {
	for _, scheme := range []string{"http", "socks5"} {
		err := roundTripVia(t, scheme+"://"+deadLoopbackAddr(t), "https://upstream.invalid/")
		if got := egressbreaker.FaultOf(err); got != egressbreaker.FaultUnreachable {
			t.Fatalf("%s proxy: FaultOf = %v for %v, want unreachable", scheme, got, err)
		}
	}
}

// A proxy that is up and reports that it cannot reach the destination has not
// failed itself.
func TestProxyReportingOnTheDestinationIsNotMarked(t *testing.T) {
	unreachableOrigin := "https://" + deadLoopbackAddr(t) + "/"

	httpProxy := newConnectProxy(t, "")
	err := roundTripVia(t, "http://"+httpProxy.addr(), unreachableOrigin)
	if err == nil || !strings.Contains(err.Error(), "502") {
		t.Fatalf("error = %v, want the proxy's 502", err)
	}
	if got := egressbreaker.FaultOf(err); got != egressbreaker.FaultNone {
		t.Fatalf("HTTP proxy: FaultOf = %v for %v, want none", got, err)
	}

	socksProxy := proxytest.NewSOCKS5(t)
	err = roundTripVia(t, socksProxy.URL("", ""), unreachableOrigin)
	if err == nil {
		t.Fatal("the request succeeded although the destination is down")
	}
	if got := egressbreaker.FaultOf(err); got != egressbreaker.FaultNone {
		t.Fatalf("SOCKS5 proxy: FaultOf = %v for %v, want none", got, err)
	}
}
