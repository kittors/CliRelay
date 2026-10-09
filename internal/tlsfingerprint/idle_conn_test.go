package tlsfingerprint

import (
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// connCountingServer starts an HTTP/2 TLS server that answers every request
// with body and reports how many client connections it currently holds open.
//
// httptest servers never close an idle connection, which is what chatgpt.com
// looks like from the relay while the health-check PINGs keep arriving: in
// production none of the abandoned connections was ever closed by the peer.
func connCountingServer(t *testing.T, body string) (*httptest.Server, *atomic.Int64) {
	t.Helper()
	var open atomic.Int64
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, body)
	}))
	server.EnableHTTP2 = true
	server.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		switch state {
		case http.StateNew:
			open.Add(1)
		case http.StateClosed, http.StateHijacked:
			open.Add(-1)
		}
	}
	server.StartTLS()
	t.Cleanup(server.Close)
	return server, &open
}

func getBody(t *testing.T, rt http.RoundTripper, target string) string {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, target, nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	resp, err := rt.RoundTrip(req)
	if err != nil {
		t.Fatalf("RoundTrip %s: %v", target, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.ProtoMajor != 2 {
		t.Fatalf("negotiated %s, want HTTP/2", resp.Proto)
	}
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return string(raw)
}

func waitForOpen(open *atomic.Int64, want int64, within time.Duration) int64 {
	deadline := time.Now().Add(within)
	for open.Load() != want && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	return open.Load()
}

// TestIdleConnectionClosesDespitePings is the regression test for connections
// that outlived every user. The management quota probe built a RoundTripper per
// call and dropped it; with the health check sending a PING every
// ReadIdleTimeout, neither the peer nor any NAT ever saw the connection go idle,
// so each one stayed open for good. A relay probing four Codex accounts every
// fifteen minutes held about 380 of them to chatgpt.com after a day.
func TestIdleConnectionClosesDespitePings(t *testing.T) {
	// PING far more often than the idle timeout, so the connections are
	// demonstrably kept busy on the wire while no request uses them.
	restore := shortenH2HealthCheck(20*time.Millisecond, time.Second)
	defer restore()
	restoreIdle := shortenH2IdleConnTimeout(300 * time.Millisecond)
	defer restoreIdle()

	server, open := connCountingServer(t, "ok")

	const transports = 3
	for i := 0; i < transports; i++ {
		rt, err := New(Options{Profile: ProfileChrome, InsecureSkipVerify: true})
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		_ = getBody(t, rt, server.URL)
		// Dropped without CloseIdleConnections, as the probe path did.
	}
	if got := open.Load(); got != transports {
		t.Fatalf("server holds %d connections after %d requests, want %d", got, transports, transports)
	}

	// Several PING intervals pass inside the idle window, yet every connection
	// has to be gone shortly after it.
	if got := waitForOpen(open, 0, 5*time.Second); got != 0 {
		t.Fatalf("%d idle connections are still open: the PINGs keep them alive and nothing closes them", got)
	}
}

// TestCachedConnectionRedialsAfterIdleClose checks the other side of the idle
// timeout: a RoundTripper whose cached connection was closed for idleness must
// dial a fresh one instead of failing the next request.
func TestCachedConnectionRedialsAfterIdleClose(t *testing.T) {
	restoreIdle := shortenH2IdleConnTimeout(100 * time.Millisecond)
	defer restoreIdle()

	server, open := connCountingServer(t, "ok")
	rt, err := New(Options{Profile: ProfileChrome, InsecureSkipVerify: true})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer rt.CloseIdleConnections()

	// Back-to-back requests share the cached connection.
	_ = getBody(t, rt, server.URL)
	_ = getBody(t, rt, server.URL)
	if got := open.Load(); got != 1 {
		t.Fatalf("two back-to-back requests used %d connections, want 1", got)
	}

	if got := waitForOpen(open, 0, 5*time.Second); got != 0 {
		t.Fatalf("idle connection still open after the idle timeout: %d", got)
	}
	if body := getBody(t, rt, server.URL); body != "ok" {
		t.Fatalf("request after the idle close returned %q", body)
	}
}

// TestConnectionCacheSeparatesPorts guards the cache key. Callers share one
// RoundTripper across arbitrary target URLs (the management api-call tool takes
// any URL for a Codex credential), so a connection to one port must never carry
// a request meant for another port on the same host.
func TestConnectionCacheSeparatesPorts(t *testing.T) {
	first, _ := connCountingServer(t, "first")
	second, _ := connCountingServer(t, "second")

	rt, err := New(Options{Profile: ProfileChrome, InsecureSkipVerify: true})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer rt.CloseIdleConnections()

	if body := getBody(t, rt, first.URL); body != "first" {
		t.Fatalf("first server answered %q", body)
	}
	if body := getBody(t, rt, second.URL); body != "second" {
		t.Fatalf("request for the second port reached %q: the connection to the first port was reused", body)
	}
}

func shortenH2IdleConnTimeout(d time.Duration) func() {
	prev := h2IdleConnTimeout
	h2IdleConnTimeout = d
	return func() { h2IdleConnTimeout = prev }
}
