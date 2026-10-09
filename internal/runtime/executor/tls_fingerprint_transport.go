package executor

import (
	"net/http"
	"strings"
	"sync"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/tlsfingerprint"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/util"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

// TLS fingerprint transports are cached process-wide for the same reason plain
// transports are: they own connection pools, so building one per request would
// force a fresh TLS handshake on every call.

type tlsFingerprintTransportKey struct {
	profile            string
	proxyURL           string
	preferIPv4         bool
	insecureSkipVerify bool
	caCert             string
	// caCertStat changes when the bundle on disk is replaced, so a rotated
	// certificate produces a new transport instead of reusing one pinned to the
	// old trust root.
	caCertStat string
}

var tlsFingerprintTransports = struct {
	sync.Mutex
	entries map[tlsFingerprintTransportKey]http.RoundTripper
}{entries: map[tlsFingerprintTransportKey]http.RoundTripper{}}

// codexTLSFingerprintConfig returns the TLS fingerprint settings that apply to
// this auth, and whether fingerprinting is active for it.
//
// The settings live under the Codex identity fingerprint block and only apply
// to Codex traffic: a ClientHello has to match the client the rest of the
// request claims to be, so it cannot be configured independently of the
// identity headers.
func codexTLSFingerprintConfig(cfg *config.Config, auth *cliproxyauth.Auth) (config.TLSFingerprintConfig, bool) {
	if cfg == nil || auth == nil {
		return config.TLSFingerprintConfig{}, false
	}
	if !strings.EqualFold(strings.TrimSpace(auth.Provider), "codex") {
		return config.TLSFingerprintConfig{}, false
	}
	tlsCfg := cfg.IdentityFingerprint.Codex.TLSFingerprint
	if !tlsCfg.Enabled {
		return config.TLSFingerprintConfig{}, false
	}
	return tlsCfg, true
}

// tlsFingerprintRoundTripper returns a cached fingerprinting round tripper for
// this auth, or nil when fingerprinting does not apply.
//
// A construction failure is logged and returns nil so the caller falls back to
// the standard transport: a misconfigured profile must not take upstream
// traffic down, it only means the ClientHello stays the Go default.
func tlsFingerprintRoundTripper(cfg *config.Config, auth *cliproxyauth.Auth, proxyURL string) http.RoundTripper {
	tlsCfg, ok := codexTLSFingerprintConfig(cfg, auth)
	if !ok {
		return nil
	}

	profile := tlsfingerprint.NormalizeProfileName(tlsCfg.Profile)
	if profile == "" {
		profile = tlsfingerprint.DefaultProfile
	}
	key := tlsFingerprintTransportKey{
		profile:  profile,
		proxyURL: strings.TrimSpace(proxyURL),
	}
	// The fingerprint transport does its own TLS, so it has to honour the same
	// trust settings the standard transport applies via util.ApplyTLSConfig;
	// otherwise enabling fingerprinting would silently drop a configured CA
	// bundle or an operator's decision to skip verification.
	if sdkCfg := cfgToSDKCfg(cfg); sdkCfg != nil {
		key.preferIPv4 = sdkCfg.PreferIPv4
		key.insecureSkipVerify = sdkCfg.InsecureSkipVerify
		key.caCert = strings.TrimSpace(sdkCfg.CACert)
		key.caCertStat = util.CACertStatFingerprint(key.caCert)
	}

	tlsFingerprintTransports.Lock()
	defer tlsFingerprintTransports.Unlock()
	if cached, exists := tlsFingerprintTransports.entries[key]; exists {
		return cached
	}

	rt, err := tlsfingerprint.New(tlsfingerprint.Options{
		Profile:            profile,
		ProxyURL:           key.proxyURL,
		PreferIPv4:         key.preferIPv4,
		InsecureSkipVerify: key.insecureSkipVerify,
		CACertPath:         key.caCert,
	})
	if err != nil {
		log.WithError(err).Warn("tls fingerprint: falling back to the default transport")
		// Cached as nil so a broken profile is not re-reported on every request.
		tlsFingerprintTransports.entries[key] = nil
		return nil
	}
	tlsFingerprintTransports.entries[key] = rt
	return rt
}
