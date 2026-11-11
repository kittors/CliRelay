package egressbreaker_test

import (
	"context"
	"errors"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/testutil/proxytest"
	"github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/egressbreaker"
	"golang.org/x/net/proxy"
)

// dialThroughSOCKS5 dials target through the proxy the way the upstream
// transports do: the real golang.org/x/net/proxy client, with the dial to the
// proxy and the handshake result marked.
func dialThroughSOCKS5(t *testing.T, ctx context.Context, proxyAddr string, auth *proxy.Auth, target string) (net.Conn, error, error) {
	t.Helper()
	base := &net.Dialer{Timeout: 2 * time.Second}
	dialer, err := proxy.SOCKS5("tcp4", proxyAddr, auth, egressbreaker.HopDialer{Base: base.DialContext})
	if err != nil {
		t.Fatalf("proxy.SOCKS5: %v", err)
	}
	conn, errDial := dialer.(proxy.ContextDialer).DialContext(ctx, "tcp4", target)
	return conn, errDial, egressbreaker.MarkSOCKS5(errDial)
}

func echoTarget(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		for {
			conn, errAccept := listener.Accept()
			if errAccept != nil {
				return
			}
			_ = conn.Close()
		}
	}()
	return listener.Addr().String()
}

// The rejection is recognised by the text of an error the SOCKS library does
// not export. This runs the real library against a server that rejects, so a
// change of that wording fails here instead of silently disabling the breaker.
func TestSOCKS5AuthRejectionIsMarked(t *testing.T) {
	server := proxytest.NewSOCKS5(t)
	server.RequireAuth(func(string, string) bool { return false })

	conn, raw, marked := dialThroughSOCKS5(t, context.Background(), server.Addr(), &proxy.Auth{User: "alice", Password: "expired"}, echoTarget(t))
	if conn != nil {
		_ = conn.Close()
		t.Fatal("the dial succeeded with rejected credentials")
	}
	if got := egressbreaker.FaultOf(marked); got != egressbreaker.FaultAuthRejected {
		t.Fatalf("FaultOf = %v for %q, want the authentication rejection", got, raw)
	}
	if marked.Error() != raw.Error() {
		t.Fatalf("marking changed the text from %q to %q", raw, marked)
	}
	if !strings.Contains(raw.Error(), "username/password authentication failed") {
		t.Fatalf("the library reports %q, not the wording observed in production", raw)
	}
	var op *net.OpError
	if !errors.As(marked, &op) {
		t.Fatalf("the *net.OpError is no longer reachable through %T", marked)
	}
}

func TestSOCKS5ProxyThatWantsCredentialsRejectsAClientWithout(t *testing.T) {
	server := proxytest.NewSOCKS5(t)
	server.RequireAuth(func(string, string) bool { return true })

	conn, raw, marked := dialThroughSOCKS5(t, context.Background(), server.Addr(), nil, echoTarget(t))
	if conn != nil {
		_ = conn.Close()
		t.Fatal("the dial succeeded without the credentials the proxy demands")
	}
	if got := egressbreaker.FaultOf(marked); got != egressbreaker.FaultAuthRejected {
		t.Fatalf("FaultOf = %v for %q, want the authentication rejection", got, raw)
	}
}

func TestSOCKS5AcceptedCredentialsLeaveNoMark(t *testing.T) {
	server := proxytest.NewSOCKS5(t)
	server.RequireAuth(func(username, password string) bool { return username == "alice" && password == "valid" })

	conn, raw, marked := dialThroughSOCKS5(t, context.Background(), server.Addr(), &proxy.Auth{User: "alice", Password: "valid"}, echoTarget(t))
	if raw != nil || marked != nil {
		t.Fatalf("dial failed: %v", raw)
	}
	_ = conn.Close()
	if server.Connects() != 1 {
		t.Fatalf("the proxy saw %d CONNECT requests, want 1", server.Connects())
	}
}

// A proxy that took the credentials and then reports on the destination has
// not failed itself, so the error must stay unmarked.
func TestSOCKS5ReplyAboutTheDestinationIsNotMarked(t *testing.T) {
	server := proxytest.NewSOCKS5(t)
	server.ReplyToConnect(0x04) // host unreachable

	conn, raw, marked := dialThroughSOCKS5(t, context.Background(), server.Addr(), nil, echoTarget(t))
	if conn != nil {
		_ = conn.Close()
		t.Fatal("the dial succeeded although the proxy refused the CONNECT")
	}
	if raw == nil || !strings.Contains(raw.Error(), "host unreachable") {
		t.Fatalf("error = %v, want the proxy's host-unreachable reply", raw)
	}
	if got := egressbreaker.FaultOf(marked); got != egressbreaker.FaultNone {
		t.Fatalf("FaultOf = %v for %q, want none", got, raw)
	}
}

func TestProxyThatCannotBeReachedIsMarked(t *testing.T) {
	// A port nothing listens on: reserve one, then free it.
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	deadAddr := listener.Addr().String()
	_ = listener.Close()

	conn, raw, marked := dialThroughSOCKS5(t, context.Background(), deadAddr, &proxy.Auth{User: "alice", Password: "valid"}, "192.0.2.1:443")
	if conn != nil {
		_ = conn.Close()
		t.Fatal("the dial succeeded through a proxy nobody runs")
	}
	if got := egressbreaker.FaultOf(marked); got != egressbreaker.FaultUnreachable {
		t.Fatalf("FaultOf = %v for %q, want unreachable", got, raw)
	}
	if marked.Error() != raw.Error() {
		t.Fatalf("marking changed the text from %q to %q", raw, marked)
	}
}

// A dial the caller gave up on says nothing about the proxy.
func TestCancelledDialIsNotMarked(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	dial := egressbreaker.MarkDial(func(ctx context.Context, _, _ string) (net.Conn, error) {
		return nil, ctx.Err()
	})
	if _, err := dial(ctx, "tcp4", "192.0.2.1:443"); egressbreaker.FaultOf(err) != egressbreaker.FaultNone {
		t.Fatalf("a cancelled dial was marked: %v", err)
	}

	failing := egressbreaker.MarkDial(func(context.Context, string, string) (net.Conn, error) {
		return nil, errors.New("connect: connection refused")
	})
	_, err := failing(context.Background(), "tcp4", "192.0.2.1:443")
	if egressbreaker.FaultOf(err) != egressbreaker.FaultUnreachable {
		t.Fatalf("a refused dial was not marked: %v", err)
	}
	// Marking twice keeps the first mark.
	again := egressbreaker.MarkDial(func(context.Context, string, string) (net.Conn, error) {
		return nil, egressbreaker.AuthRejected(errors.New("rejected"))
	})
	if _, err = again(context.Background(), "tcp4", "192.0.2.1:443"); egressbreaker.FaultOf(err) != egressbreaker.FaultAuthRejected {
		t.Fatalf("an already marked error was marked again: %v", err)
	}
}

func TestMarkSOCKS5LeavesOtherErrorsAlone(t *testing.T) {
	if egressbreaker.MarkSOCKS5(nil) != nil {
		t.Fatal("MarkSOCKS5(nil) is not nil")
	}
	plain := errors.New("username/password authentication failed")
	if got := egressbreaker.FaultOf(egressbreaker.MarkSOCKS5(plain)); got != egressbreaker.FaultNone {
		t.Fatalf("an error that is not a SOCKS dial error was marked: %v", got)
	}
	// The same words from another operation (a direct dial, say) do not count.
	other := &net.OpError{Op: "dial", Net: "tcp4", Err: plain}
	if got := egressbreaker.FaultOf(egressbreaker.MarkSOCKS5(other)); got != egressbreaker.FaultNone {
		t.Fatalf("a non-SOCKS operation was marked: %v", got)
	}
}
