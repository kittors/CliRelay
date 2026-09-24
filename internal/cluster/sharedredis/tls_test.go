package sharedredis

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
)

// testPKI is a throwaway CA with one server and one client certificate, the
// shape of the production mutual-TLS setup.
type testPKI struct {
	dir                   string
	caFile                string
	serverCert            tls.Certificate
	clientCert, clientKey string
	caPool                *x509.CertPool
}

func newTestPKI(t *testing.T, serverDNS string) *testPKI {
	t.Helper()
	dir := t.TempDir()
	caKey := mustKey(t)
	caTmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "clirelay test ca"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	caCert, _ := x509.ParseCertificate(caDER)
	pool := x509.NewCertPool()
	pool.AddCert(caCert)

	issue := func(serial int64, cn string, usage x509.ExtKeyUsage, dns []string, ips []net.IP) ([]byte, *ecdsa.PrivateKey) {
		key := mustKey(t)
		tmpl := &x509.Certificate{
			SerialNumber: big.NewInt(serial),
			Subject:      pkix.Name{CommonName: cn},
			NotBefore:    time.Now().Add(-time.Hour),
			NotAfter:     time.Now().Add(time.Hour),
			KeyUsage:     x509.KeyUsageDigitalSignature,
			ExtKeyUsage:  []x509.ExtKeyUsage{usage},
			DNSNames:     dns,
			IPAddresses:  ips,
		}
		der, err := x509.CreateCertificate(rand.Reader, tmpl, caCert, &key.PublicKey, caKey)
		if err != nil {
			t.Fatal(err)
		}
		return der, key
	}

	serverDER, serverKey := issue(2, serverDNS, x509.ExtKeyUsageServerAuth, []string{serverDNS}, []net.IP{net.ParseIP("127.0.0.1")})
	clientDER, clientKey := issue(3, "clirelay-node", x509.ExtKeyUsageClientAuth, nil, nil)

	p := &testPKI{dir: dir, caPool: pool}
	p.caFile = writePEM(t, dir, "ca.pem", "CERTIFICATE", caDER)
	p.clientCert = writePEM(t, dir, "client.pem", "CERTIFICATE", clientDER)
	p.clientKey = writePEM(t, dir, "client-key.pem", "EC PRIVATE KEY", mustMarshalKey(t, clientKey))
	p.serverCert = tls.Certificate{Certificate: [][]byte{serverDER}, PrivateKey: serverKey}
	return p
}

func (p *testPKI) serverTLS() *tls.Config {
	return &tls.Config{
		Certificates: []tls.Certificate{p.serverCert},
		ClientCAs:    p.caPool,
		ClientAuth:   tls.RequireAndVerifyClientCert,
		MinVersion:   tls.VersionTLS12,
	}
}

func mustKey(t *testing.T) *ecdsa.PrivateKey {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return key
}

func mustMarshalKey(t *testing.T, key *ecdsa.PrivateKey) []byte {
	t.Helper()
	der, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return der
}

func writePEM(t *testing.T, dir, name, blockType string, der []byte) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: blockType, Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestMutualTLSWithServerNameOverride(t *testing.T) {
	pki := newTestPKI(t, "redis.cluster.internal")
	mr, err := miniredis.RunTLS(pki.serverTLS())
	if err != nil {
		t.Fatalf("start TLS miniredis: %v", err)
	}
	t.Cleanup(mr.Close)

	cfg := config.ClusterRedisConfig{
		Addr:          mr.Addr(),
		TLSCAFile:     pki.caFile,
		TLSCertFile:   pki.clientCert,
		TLSKeyFile:    pki.clientKey,
		TLSServerName: "redis.cluster.internal",
	}
	c, err := New(cfg, fastOptions())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	c.Start()
	t.Cleanup(c.Close)
	if !c.Available() {
		t.Fatalf("mutual TLS client must connect: %+v", c.Status())
	}
	if _, err := c.Eval(context.Background(), incrScript, []string{KeyPrefix + "tls"}); err != nil {
		t.Fatalf("Eval over TLS: %v", err)
	}
}

func TestTLSServerNameDefaultsToAddrHost(t *testing.T) {
	pki := newTestPKI(t, "redis.cluster.internal")
	mr, err := miniredis.RunTLS(pki.serverTLS())
	if err != nil {
		t.Fatalf("start TLS miniredis: %v", err)
	}
	t.Cleanup(mr.Close)

	// The server certificate carries 127.0.0.1 as an IP SAN, so leaving the
	// server name empty must verify against the host of addr.
	cfg := config.ClusterRedisConfig{
		Addr:        mr.Addr(),
		TLSCAFile:   pki.caFile,
		TLSCertFile: pki.clientCert,
		TLSKeyFile:  pki.clientKey,
	}
	c, err := New(cfg, fastOptions())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	c.Start()
	t.Cleanup(c.Close)
	if !c.Available() {
		t.Fatalf("client must verify the IP SAN from addr: %+v", c.Status())
	}
}

func TestTLSRejectsWrongServerNameAndMissingClientCert(t *testing.T) {
	pki := newTestPKI(t, "redis.cluster.internal")
	mr, err := miniredis.RunTLS(pki.serverTLS())
	if err != nil {
		t.Fatalf("start TLS miniredis: %v", err)
	}
	t.Cleanup(mr.Close)

	wrongName, err := New(config.ClusterRedisConfig{
		Addr: mr.Addr(), TLSCAFile: pki.caFile, TLSCertFile: pki.clientCert, TLSKeyFile: pki.clientKey,
		TLSServerName: "someone-else.example",
	}, fastOptions())
	if err != nil {
		t.Fatal(err)
	}
	wrongName.Start()
	t.Cleanup(wrongName.Close)
	if wrongName.Available() {
		t.Fatal("a certificate for another name must be rejected")
	}

	noClientCert, err := New(config.ClusterRedisConfig{
		Addr: mr.Addr(), TLSCAFile: pki.caFile, TLSServerName: "redis.cluster.internal",
	}, fastOptions())
	if err != nil {
		t.Fatal(err)
	}
	noClientCert.Start()
	t.Cleanup(noClientCert.Close)
	if noClientCert.Available() {
		t.Fatal("the server demands a client certificate; connecting without one must fail")
	}
}

func TestBuildTLSConfigValidation(t *testing.T) {
	if cfg, err := BuildTLSConfig(config.ClusterRedisConfig{Addr: "10.0.0.1:6379"}); err != nil || cfg != nil {
		t.Fatalf("no TLS settings must mean plain TCP, got %v %v", cfg, err)
	}
	pki := newTestPKI(t, "redis.cluster.internal")
	if _, err := BuildTLSConfig(config.ClusterRedisConfig{Addr: "x:1", TLSCertFile: pki.clientCert}); err == nil ||
		!strings.Contains(err.Error(), "together") {
		t.Fatalf("certificate without key must be rejected, got %v", err)
	}
	bogus := filepath.Join(pki.dir, "bogus.pem")
	_ = os.WriteFile(bogus, []byte("not a certificate"), 0o600)
	if _, err := BuildTLSConfig(config.ClusterRedisConfig{Addr: "x:1", TLSCAFile: bogus}); err == nil {
		t.Fatal("a CA file without certificates must be rejected")
	}
	cfg, err := BuildTLSConfig(config.ClusterRedisConfig{Addr: "[::1]:6380", TLSCAFile: pki.caFile})
	if err != nil || cfg.ServerName != "::1" || cfg.MinVersion != tls.VersionTLS12 {
		t.Fatalf("server name must default to the addr host, got %+v %v", cfg, err)
	}
}
