package util

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestTransportsCheckHTTP2Liveness(t *testing.T) {
	cases := []struct {
		name      string
		transport *http.Transport
	}{
		{name: "direct", transport: NewDefaultTransport(false)},
		{name: "direct ipv4", transport: NewDefaultTransport(true)},
		{name: "http proxy", transport: BuildProxyTransport("http://127.0.0.1:8080", false)},
		{name: "https proxy", transport: BuildProxyTransport("https://127.0.0.1:8443", false)},
		{name: "socks5 proxy", transport: BuildProxyTransport("socks5://127.0.0.1:1080", false)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.transport == nil {
				t.Fatal("transport is nil")
			}
			h2 := tc.transport.HTTP2
			if h2 == nil {
				t.Fatal("HTTP2 is nil, want the liveness check configured")
			}
			if h2.SendPingTimeout != DefaultHTTP2ReadIdleTimeout {
				t.Fatalf("SendPingTimeout = %s, want %s", h2.SendPingTimeout, DefaultHTTP2ReadIdleTimeout)
			}
			if h2.PingTimeout != DefaultHTTP2PingTimeout {
				t.Fatalf("PingTimeout = %s, want %s", h2.PingTimeout, DefaultHTTP2PingTimeout)
			}
			// The PINGs keep a connection alive on the wire, so IdleConnTimeout
			// is the only thing that closes one nobody uses any more.
			if tc.transport.IdleConnTimeout <= 0 {
				t.Fatalf("IdleConnTimeout = %s, want it set while PINGs are sent", tc.transport.IdleConnTimeout)
			}
		})
	}

	if first, second := NewDefaultTransport(false), NewDefaultTransport(false); first.HTTP2 == second.HTTP2 {
		t.Fatal("two transports share one HTTP2 config, want one each")
	}
}

// TestTransportLeavesHTTP2ConnectionThatStoppedAnswering reproduces what a
// network interruption leaves behind: an established connection that still
// accepts data but never delivers another byte, while new connections work.
func TestTransportLeavesHTTP2ConnectionThatStoppedAnswering(t *testing.T) {
	cases := []struct {
		name  string
		socks bool
	}{
		{name: "direct"},
		{name: "socks5 proxy", socks: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			upstream := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = io.WriteString(w, "ok")
			}))
			upstream.EnableHTTP2 = true
			upstream.StartTLS()
			t.Cleanup(upstream.Close)

			relay := newSilenceableRelay(t, upstream.Listener.Addr().String(), tc.socks)

			var transport *http.Transport
			target := upstream.URL
			if tc.socks {
				transport = BuildProxyTransport("socks5://"+relay.addr(), false)
			} else {
				transport = NewDefaultTransport(false)
				target = "https://" + relay.addr()
			}
			if transport == nil {
				t.Fatal("transport is nil")
			}
			if transport.HTTP2 == nil {
				t.Fatal("transport has no HTTP/2 liveness check")
			}
			roots := x509.NewCertPool()
			roots.AddCert(upstream.Certificate())
			transport.TLSClientConfig = &tls.Config{RootCAs: roots}
			// Shortened so the test does not wait 45 seconds. The values used
			// in production are asserted by TestTransportsCheckHTTP2Liveness.
			transport.HTTP2.SendPingTimeout = 50 * time.Millisecond
			transport.HTTP2.PingTimeout = 250 * time.Millisecond
			t.Cleanup(transport.CloseIdleConnections)
			client := &http.Client{Transport: transport}

			ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
			defer cancel()

			proto, err := getOnce(ctx, client, target)
			if err != nil {
				t.Fatalf("first request: %v", err)
			}
			if proto != 2 {
				t.Fatalf("first request used HTTP/%d, want HTTP/2", proto)
			}
			silenced := relay.silenceOpenConns()
			if silenced < 1 {
				t.Fatal("relay carried no connection for the first request")
			}

			// The silenced connection is still pooled, so this request is
			// written onto it. Without the liveness check it gets no answer
			// and waits until ctx expires.
			_, err = getOnce(ctx, client, target)
			if ctx.Err() != nil {
				t.Fatalf("request on the silent connection waited for the context to expire: %v", err)
			}
			t.Logf("request on the silent connection ended with: %v", err)

			proto, err = getOnce(ctx, client, target)
			if err != nil {
				t.Fatalf("request after the silent connection was dropped: %v", err)
			}
			if proto != 2 {
				t.Fatalf("request after the silent connection was dropped used HTTP/%d, want HTTP/2", proto)
			}
			if accepted := relay.accepted(); accepted <= silenced {
				t.Fatalf("relay carried %d connections, want a new one beyond the %d silenced", accepted, silenced)
			}
		})
	}
}

func getOnce(ctx context.Context, client *http.Client, target string) (int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return 0, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	if _, err = io.Copy(io.Discard, resp.Body); err != nil {
		return 0, err
	}
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("status %d", resp.StatusCode)
	}
	return resp.ProtoMajor, nil
}

// silenceableRelay forwards TCP connections to a target, optionally after a
// SOCKS5 handshake, and can turn the connections already open into black
// holes: it keeps reading from both ends, so each side still has its data
// acknowledged, but forwards nothing. Connections accepted afterwards are
// forwarded normally.
type silenceableRelay struct {
	listener net.Listener
	target   string
	socks    bool

	mu     sync.Mutex
	closed bool
	open   []net.Conn
	pairs  []*relayedPair
}

type relayedPair struct {
	silent atomic.Bool
}

func newSilenceableRelay(t *testing.T, target string, socks bool) *silenceableRelay {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	relay := &silenceableRelay{listener: listener, target: target, socks: socks}

	var wg sync.WaitGroup
	wg.Go(func() {
		for {
			conn, errAccept := listener.Accept()
			if errAccept != nil {
				return
			}
			wg.Go(func() { relay.serve(conn) })
		}
	})
	t.Cleanup(func() {
		_ = listener.Close()
		relay.mu.Lock()
		relay.closed = true
		for _, conn := range relay.open {
			_ = conn.Close()
		}
		relay.mu.Unlock()
		wg.Wait()
	})
	return relay
}

func (r *silenceableRelay) addr() string {
	return r.listener.Addr().String()
}

// accepted reports how many connections the relay has carried so far.
func (r *silenceableRelay) accepted() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.pairs)
}

// silenceOpenConns stops forwarding on every connection carried so far and
// reports how many there were.
func (r *silenceableRelay) silenceOpenConns() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, pair := range r.pairs {
		pair.silent.Store(true)
	}
	return len(r.pairs)
}

// track registers conn to be closed with the relay. It reports false when the
// relay is already closed, in which case the caller must not keep using conn.
func (r *silenceableRelay) track(conn net.Conn) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return false
	}
	r.open = append(r.open, conn)
	return true
}

func (r *silenceableRelay) serve(client net.Conn) {
	defer func() { _ = client.Close() }()
	if !r.track(client) {
		return
	}

	target := r.target
	if r.socks {
		requested, err := readSOCKS5Connect(client)
		if err != nil {
			return
		}
		target = requested
	}
	upstream, err := net.DialTimeout("tcp", target, 5*time.Second)
	if err != nil {
		return
	}
	defer func() { _ = upstream.Close() }()
	if !r.track(upstream) {
		return
	}
	if r.socks {
		// Success reply with an all-zero IPv4 bind address.
		if _, err = client.Write([]byte{5, 0, 0, 1, 0, 0, 0, 0, 0, 0}); err != nil {
			return
		}
	}

	pair := &relayedPair{}
	r.mu.Lock()
	r.pairs = append(r.pairs, pair)
	r.mu.Unlock()

	done := make(chan struct{}, 2)
	forward := func(dst, src net.Conn) {
		defer func() { done <- struct{}{} }()
		buf := make([]byte, 32*1024)
		for {
			n, errRead := src.Read(buf)
			if n > 0 && !pair.silent.Load() {
				if _, errWrite := dst.Write(buf[:n]); errWrite != nil {
					return
				}
			}
			if errRead != nil {
				return
			}
		}
	}
	go forward(upstream, client)
	go forward(client, upstream)
	<-done
	// One direction ended; closing both ends stops the other one.
	_ = client.Close()
	_ = upstream.Close()
	<-done
}

// readSOCKS5Connect answers a client's greeting the way an unauthenticated
// SOCKS5 server does and returns the address its CONNECT request asks for.
// The caller sends the reply once it has dialed that address.
func readSOCKS5Connect(conn net.Conn) (string, error) {
	greeting := make([]byte, 2)
	if _, err := io.ReadFull(conn, greeting); err != nil {
		return "", err
	}
	if greeting[0] != 5 {
		return "", fmt.Errorf("unexpected SOCKS version %d", greeting[0])
	}
	if _, err := io.CopyN(io.Discard, conn, int64(greeting[1])); err != nil {
		return "", err
	}
	// Version 5, method 0: no authentication.
	if _, err := conn.Write([]byte{5, 0}); err != nil {
		return "", err
	}

	request := make([]byte, 4)
	if _, err := io.ReadFull(conn, request); err != nil {
		return "", err
	}
	if request[0] != 5 || request[1] != 1 {
		return "", fmt.Errorf("unsupported SOCKS request % x", request)
	}
	var host string
	switch request[3] {
	case 1:
		addr := make([]byte, net.IPv4len)
		if _, err := io.ReadFull(conn, addr); err != nil {
			return "", err
		}
		host = net.IP(addr).String()
	case 3:
		length := make([]byte, 1)
		if _, err := io.ReadFull(conn, length); err != nil {
			return "", err
		}
		name := make([]byte, length[0])
		if _, err := io.ReadFull(conn, name); err != nil {
			return "", err
		}
		host = string(name)
	case 4:
		addr := make([]byte, net.IPv6len)
		if _, err := io.ReadFull(conn, addr); err != nil {
			return "", err
		}
		host = net.IP(addr).String()
	default:
		return "", fmt.Errorf("unsupported SOCKS address type %d", request[3])
	}
	port := make([]byte, 2)
	if _, err := io.ReadFull(conn, port); err != nil {
		return "", err
	}
	return net.JoinHostPort(host, strconv.Itoa(int(binary.BigEndian.Uint16(port)))), nil
}
