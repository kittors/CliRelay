package util

import (
	"crypto/tls"
	"crypto/x509"
	"net/http"
	"os"

	"github.com/router-for-me/CLIProxyAPI/v6/sdk/config"
	log "github.com/sirupsen/logrus"
)

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
