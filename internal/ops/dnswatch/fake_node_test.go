package dnswatch

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"fmt"
	"io"
	"log"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// testPKI is a private CA plus one leaf certificate for the given hosts, so
// probes can verify certificates exactly as they do in production.
type testPKI struct {
	roots *x509.CertPool
	cert  tls.Certificate
}

func newTestPKI(t *testing.T, hosts ...string) *testPKI {
	t.Helper()
	now := time.Now()
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	caTemplate := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "dnswatch test CA"},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(24 * time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	caCert, err := x509.ParseCertificate(caDER)
	if err != nil {
		t.Fatal(err)
	}
	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	leafTemplate := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: hosts[0]},
		DNSNames:     hosts,
		NotBefore:    now.Add(-time.Hour),
		NotAfter:     now.Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	leafDER, err := x509.CreateCertificate(rand.Reader, leafTemplate, caCert, &leafKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(caCert)
	return &testPKI{roots: pool, cert: tls.Certificate{Certificate: [][]byte{leafDER}, PrivateKey: leafKey}}
}

type seenRequest struct {
	host, sni, path string
}

// fakeNode is an application node answering /readyz over TLS.
type fakeNode struct {
	server *httptest.Server
	status atomic.Int32
	delay  atomic.Int64
	hits   atomic.Int32

	// egressStatus answers testEgressPath. legacy drops that route, like a
	// node running a release that predates it.
	egressStatus atomic.Int32
	egressHits   atomic.Int32
	legacy       atomic.Bool

	mu   sync.Mutex
	seen []seenRequest
}

func newFakeNode(t *testing.T, pki *testPKI) *fakeNode {
	t.Helper()
	n := &fakeNode{}
	n.status.Store(http.StatusNoContent)
	n.egressStatus.Store(http.StatusNoContent)
	server := httptest.NewUnstartedServer(http.HandlerFunc(n.serve))
	server.TLS = &tls.Config{Certificates: []tls.Certificate{pki.cert}}
	// Rejected handshakes are expected in some tests; keep them off stderr.
	server.Config.ErrorLog = log.New(io.Discard, "", 0)
	server.StartTLS()
	t.Cleanup(server.Close)
	n.server = server
	return n
}

func (n *fakeNode) serve(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == testEgressPath && !n.legacy.Load() {
		n.egressHits.Add(1)
		n.record(r)
		w.WriteHeader(int(n.egressStatus.Load()))
		return
	}
	n.hits.Add(1)
	n.record(r)

	if delay := time.Duration(n.delay.Load()); delay > 0 {
		select {
		case <-time.After(delay):
		case <-r.Context().Done():
			return
		}
	}
	if r.URL.Path != DefaultProbePath {
		http.NotFound(w, r)
		return
	}
	status := int(n.status.Load())
	if status/100 == 3 {
		w.Header().Set("Location", "/elsewhere")
	}
	w.WriteHeader(status)
}

func (n *fakeNode) record(r *http.Request) {
	seen := seenRequest{host: r.Host, path: r.URL.Path}
	if r.TLS != nil {
		seen.sni = r.TLS.ServerName
	}
	n.mu.Lock()
	n.seen = append(n.seen, seen)
	n.mu.Unlock()
}

func (n *fakeNode) setStatus(status int) { n.status.Store(int32(status)) }

func (n *fakeNode) setEgressStatus(status int) { n.egressStatus.Store(int32(status)) }

func (n *fakeNode) lastRequest() seenRequest {
	n.mu.Lock()
	defer n.mu.Unlock()
	if len(n.seen) == 0 {
		return seenRequest{}
	}
	return n.seen[len(n.seen)-1]
}

// dialRouter maps "<node ip>:443" to the local listener of a fake node, so
// tests keep realistic node addresses in DNS records and probe URLs.
type dialRouter struct {
	mu     sync.Mutex
	routes map[string]string
}

func newDialRouter() *dialRouter {
	return &dialRouter{routes: map[string]string{}}
}

func (d *dialRouter) route(ip string, node *fakeNode) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.routes[net.JoinHostPort(ip, "443")] = node.server.Listener.Addr().String()
}

func (d *dialRouter) unroute(ip string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	delete(d.routes, net.JoinHostPort(ip, "443"))
}

func (d *dialRouter) dial(ctx context.Context, network, address string) (net.Conn, error) {
	d.mu.Lock()
	target, ok := d.routes[address]
	d.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("dial %s: connect: connection refused", address)
	}
	var dialer net.Dialer
	return dialer.DialContext(ctx, network, target)
}
