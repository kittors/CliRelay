package tlsfingerprint

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"io"
	"math/big"
	"net"
	"net/http"
	"testing"
	"time"

	"golang.org/x/net/http2"
)

// silentH2Server completes the TLS handshake, advertises HTTP/2 through ALPN,
// sends the initial SETTINGS frame so the client considers the connection
// established, and then stops speaking entirely. It never answers a PING.
//
// This is the shape of the production failure: the peer is gone but the socket
// was never closed, so the client has no way to notice without probing.
func silentH2Server(t *testing.T) string {
	t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "localhost"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		DNSNames:     []string{"localhost"},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create certificate: %v", err)
	}

	listener, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{
		Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}},
		NextProtos:   []string{http2.NextProtoTLS},
	})
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })

	go func() {
		conn, errAccept := listener.Accept()
		if errAccept != nil {
			return
		}
		t.Cleanup(func() { _ = conn.Close() })

		// Consume the client preface, then acknowledge the connection with an
		// empty SETTINGS frame. After this the server deliberately goes quiet.
		preface := make([]byte, len(http2.ClientPreface))
		if _, errRead := io.ReadFull(conn, preface); errRead != nil {
			return
		}
		_ = http2.NewFramer(conn, conn).WriteSettings()

		// Block until the listener is closed by t.Cleanup. Reads are not
		// serviced, so PING frames are never answered.
		select {}
	}()

	return listener.Addr().String()
}

// TestRoundTripDetectsDeadHTTP2Peer is the regression test for a relay request
// that hung for 17 minutes against Anthropic. The pooled HTTP/2 connection had
// died silently; CanTakeNewRequest still reported it usable because it only
// inspects local state, so the request sat in kernel retransmission until
// tcp_retries2 was exhausted.
//
// With a health check configured the connection is probed instead, and the
// caller gets an error in bounded time.
func TestRoundTripDetectsDeadHTTP2Peer(t *testing.T) {
	restore := shortenH2HealthCheck(50*time.Millisecond, 50*time.Millisecond)
	defer restore()

	addr := silentH2Server(t)

	rt, err := New(Options{Profile: ProfileChrome, InsecureSkipVerify: true})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer rt.CloseIdleConnections()

	req, err := http.NewRequest(http.MethodGet, "https://"+addr+"/", nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}

	done := make(chan error, 1)
	go func() {
		resp, errRT := rt.RoundTrip(req)
		if resp != nil {
			_ = resp.Body.Close()
		}
		done <- errRT
	}()

	select {
	case errRT := <-done:
		if errRT == nil {
			t.Fatal("a silent peer must not produce a successful response")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("RoundTrip did not return: the dead connection was never detected, " +
			"so the request is waiting on kernel retransmission")
	}

	// The failed connection must not stay in the cache, otherwise the next
	// request repeats the stall.
	rt.mu.Lock()
	_, cached := rt.h2Conns["127.0.0.1"]
	rt.mu.Unlock()
	if cached {
		t.Fatal("the dead connection is still cached for reuse")
	}
}

// TestH2HealthCheckDefaultsAreBounded keeps the shipped values honest: a zero
// ReadIdleTimeout disables probing entirely, which is the defect this package
// had, and an over-long one puts the caller back inside the kernel's
// retransmission window.
func TestH2HealthCheckDefaultsAreBounded(t *testing.T) {
	if h2ReadIdleTimeout <= 0 {
		t.Fatal("ReadIdleTimeout of zero disables the HTTP/2 health check")
	}
	if h2PingTimeout <= 0 {
		t.Fatal("PingTimeout of zero leaves the probe unbounded")
	}
	// Linux gives up on retransmission after roughly fifteen minutes with the
	// default tcp_retries2, and clients commonly time out at five. Detection
	// has to land well inside both.
	if total := h2ReadIdleTimeout + h2PingTimeout; total > time.Minute {
		t.Fatalf("detection budget %s is too slow to beat a client timeout", total)
	}
}

func shortenH2HealthCheck(readIdle, ping time.Duration) func() {
	prevRead, prevPing := h2ReadIdleTimeout, h2PingTimeout
	h2ReadIdleTimeout, h2PingTimeout = readIdle, ping
	return func() { h2ReadIdleTimeout, h2PingTimeout = prevRead, prevPing }
}
