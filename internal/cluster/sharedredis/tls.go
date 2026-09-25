package sharedredis

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"os"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
)

// BuildTLSConfig turns the cluster Redis TLS settings into a client TLS
// configuration. It returns nil when no TLS setting is present, meaning plain
// TCP. Setting any of them enables TLS: the CA file pins the server's issuer
// (system roots are used without one), the certificate and key are presented
// for mutual TLS, and the server name defaults to the host part of addr.
func BuildTLSConfig(cfg config.ClusterRedisConfig) (*tls.Config, error) {
	caFile := strings.TrimSpace(cfg.TLSCAFile)
	certFile := strings.TrimSpace(cfg.TLSCertFile)
	keyFile := strings.TrimSpace(cfg.TLSKeyFile)
	serverName := strings.TrimSpace(cfg.TLSServerName)
	if caFile == "" && certFile == "" && keyFile == "" && serverName == "" {
		return nil, nil
	}

	tlsCfg := &tls.Config{MinVersion: tls.VersionTLS12}
	if caFile != "" {
		pem, err := os.ReadFile(caFile)
		if err != nil {
			return nil, fmt.Errorf("sharedredis: read tls-ca-file: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("sharedredis: tls-ca-file %s holds no PEM certificate", caFile)
		}
		tlsCfg.RootCAs = pool
	}

	// A lone certificate or key is a configuration mistake, not a request for
	// one-way TLS: failing loudly beats silently dropping the client identity
	// the server is going to demand.
	if (certFile == "") != (keyFile == "") {
		return nil, errors.New("sharedredis: tls-cert-file and tls-key-file must be set together")
	}
	if certFile != "" {
		pair, err := tls.LoadX509KeyPair(certFile, keyFile)
		if err != nil {
			return nil, fmt.Errorf("sharedredis: load client certificate: %w", err)
		}
		tlsCfg.Certificates = []tls.Certificate{pair}
	}

	if serverName == "" {
		serverName = hostOf(cfg.Addr)
	}
	tlsCfg.ServerName = serverName
	return tlsCfg, nil
}

func hostOf(addr string) string {
	addr = strings.TrimSpace(addr)
	if host, _, err := net.SplitHostPort(addr); err == nil {
		return strings.Trim(host, "[]")
	}
	return strings.Trim(addr, "[]")
}
