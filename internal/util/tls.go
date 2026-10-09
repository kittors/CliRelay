package util

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net/http"
	"os"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v6/sdk/config"
	log "github.com/sirupsen/logrus"
)

// CACertStatFingerprint identifies the current contents of a CA bundle file by
// size and modification time, for use in transport cache keys. Transports read
// the bundle once at construction, so a key that includes this value yields a
// new transport after the file is replaced instead of one pinned to the old
// trust root. An empty path returns "" and an unreadable one "missing".
func CACertStatFingerprint(path string) string {
	if strings.TrimSpace(path) == "" {
		return ""
	}
	info, err := os.Stat(path)
	if err != nil {
		return "missing"
	}
	return fmt.Sprintf("%d:%d", info.Size(), info.ModTime().UnixNano())
}

// ApplyTLSConfig applies TLS settings from SDKConfig to the given HTTP transport.
// It configures InsecureSkipVerify and custom CA certificates when specified.
// This function is safe to call with nil transport or nil sdkCfg.
func ApplyTLSConfig(transport *http.Transport, sdkCfg *config.SDKConfig) {
	if transport == nil {
		return
	}

	tlsConfig := &tls.Config{}
	hasCustom := false

	if sdkCfg != nil && sdkCfg.CACert != "" {
		caCert, err := os.ReadFile(sdkCfg.CACert)
		if err != nil {
			log.Errorf("failed to read CA cert file %s: %v", sdkCfg.CACert, err)
		} else {
			caCertPool := x509.NewCertPool()
			if !caCertPool.AppendCertsFromPEM(caCert) {
				log.Errorf("failed to parse CA cert from %s", sdkCfg.CACert)
			} else {
				tlsConfig.RootCAs = caCertPool
				hasCustom = true
			}
		}
	}

	if sdkCfg != nil && sdkCfg.InsecureSkipVerify {
		tlsConfig.InsecureSkipVerify = true
		hasCustom = true
		log.Warn("TLS certificate verification is disabled for upstream connections. This is insecure and should only be used in testing or trusted internal networks.")
	}

	if hasCustom {
		transport.TLSClientConfig = tlsConfig
	}
}
