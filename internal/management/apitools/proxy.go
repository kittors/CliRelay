package apitools

import (
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/tlsfingerprint"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/util"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

// Management probes must reuse transports by proxy/TLS config. Creating a new
// proxy transport per /api-call burns keep-alive pools and multiplies fds.
const maxManagementTransportEntries = 64

type managementTransportKey struct {
	proxyURL           string
	preferIPv4         bool
	insecureSkipVerify bool
	caCert             string
	// caCertStat changes when the bundle on disk is replaced. Both transport
	// kinds read it once at construction, so without this a rotated bundle
	// would keep being served by a transport pinned to the old trust root.
	caCertStat string
}

type managementTransportEntry struct {
	transport *http.Transport
	lastUsed  time.Time
}

var (
	managementTransportMu    sync.Mutex
	managementTransportCache = map[managementTransportKey]*managementTransportEntry{}
	// managementFingerprintTransports holds the utls transports used for Codex
	// targets. Building one per call leaked a connection per call: every quota
	// probe dialed its own HTTP/2 connection to chatgpt.com and abandoned it,
	// and the transport's health-check PINGs kept each one open indefinitely.
	//
	// Entries are never evicted, as in the executor's fingerprint cache: a
	// transport caches one connection per host and every connection closes
	// itself once idle, so an unused entry costs only its struct, and the keys
	// are bounded by the configured egress settings. Do not reuse the LRU's
	// CloseIdleConnections here: on these transports it also closes
	// connections that still carry requests.
	managementFingerprintTransports = map[managementTransportKey]http.RoundTripper{}
)

func newManagementTransportKey(proxyStr string, sdkCfg *config.SDKConfig) managementTransportKey {
	key := managementTransportKey{proxyURL: strings.TrimSpace(proxyStr)}
	if sdkCfg != nil {
		key.preferIPv4 = sdkCfg.PreferIPv4
		key.insecureSkipVerify = sdkCfg.InsecureSkipVerify
		key.caCert = strings.TrimSpace(sdkCfg.CACert)
		key.caCertStat = util.CACertStatFingerprint(key.caCert)
	}
	return key
}

func (s *Service) AuthByIndex(authIndex string) *coreauth.Auth {
	authIndex = strings.TrimSpace(authIndex)
	if authIndex == "" || s == nil || s.authManager == nil {
		return nil
	}
	auths := s.authManager.ListForTenant(s.tenantID)
	for _, auth := range auths {
		if auth == nil {
			continue
		}
		auth.EnsureIndex()
		if auth.Index == authIndex {
			return auth
		}
	}
	return nil
}

func (s *Service) APICallTransport(auth *coreauth.Auth) http.RoundTripper {
	return s.APICallTransportForURL(auth, nil)
}

func (s *Service) APICallTransportForURL(auth *coreauth.Auth, targetURL *url.URL) http.RoundTripper {
	var proxyCandidates []string
	if s != nil && s.cfg != nil {
		proxyID := ""
		fallbackURL := ""
		if auth != nil {
			proxyID = auth.ProxyID
			fallbackURL = auth.ProxyURL
		}
		if proxyStr := strings.TrimSpace(s.cfg.ResolveProxyURL(proxyID, fallbackURL)); proxyStr != "" {
			proxyCandidates = append(proxyCandidates, proxyStr)
		}
	} else if auth != nil {
		if proxyStr := strings.TrimSpace(auth.ProxyURL); proxyStr != "" {
			proxyCandidates = append(proxyCandidates, proxyStr)
		}
	}

	var sdkCfg *config.SDKConfig
	if s != nil && s.cfg != nil {
		sdkCfg = &s.cfg.SDKConfig
	}
	for _, proxyStr := range proxyCandidates {
		if targetURL != nil && strings.EqualFold(targetURL.Scheme, "https") && isCodexTargetURL(auth, targetURL) {
			if fpTransport := cachedManagementTLSFingerprintTransport(proxyStr, sdkCfg); fpTransport != nil {
				return fpTransport
			}
		}
		if transport := cachedManagementProxyTransport(proxyStr, sdkCfg); transport != nil {
			return transport
		}
	}
	if targetURL != nil && strings.EqualFold(targetURL.Scheme, "https") && isCodexTargetURL(auth, targetURL) {
		if fpTransport := cachedManagementTLSFingerprintTransport("", sdkCfg); fpTransport != nil {
			return fpTransport
		}
	}
	return nil
}

func isCodexTargetURL(auth *coreauth.Auth, u *url.URL) bool {
	if u == nil {
		return false
	}
	host := strings.ToLower(strings.TrimSpace(u.Hostname()))
	if host == "chatgpt.com" || strings.HasSuffix(host, ".chatgpt.com") ||
		host == "openai.com" || strings.HasSuffix(host, ".openai.com") {
		return true
	}
	if auth != nil && strings.EqualFold(strings.TrimSpace(auth.Provider), "codex") {
		return true
	}
	return false
}

func cachedManagementTLSFingerprintTransport(proxyStr string, sdkCfg *config.SDKConfig) http.RoundTripper {
	key := newManagementTransportKey(proxyStr, sdkCfg)

	managementTransportMu.Lock()
	defer managementTransportMu.Unlock()
	if rt, ok := managementFingerprintTransports[key]; ok {
		return rt
	}
	rt, err := tlsfingerprint.New(tlsfingerprint.Options{
		Profile:            tlsfingerprint.DefaultProfile,
		ProxyURL:           key.proxyURL,
		PreferIPv4:         key.preferIPv4,
		InsecureSkipVerify: key.insecureSkipVerify,
		CACertPath:         key.caCert,
		DialTimeout:        30 * time.Second,
	})
	if err != nil {
		// Not cached, like a failed standard transport: the caller falls back
		// to the standard path for this call.
		return nil
	}
	managementFingerprintTransports[key] = rt
	return rt
}

func cachedManagementProxyTransport(proxyStr string, sdkCfg *config.SDKConfig) *http.Transport {
	proxyStr = strings.TrimSpace(proxyStr)
	if proxyStr == "" {
		return nil
	}
	key := newManagementTransportKey(proxyStr, sdkCfg)

	now := time.Now()
	managementTransportMu.Lock()
	defer managementTransportMu.Unlock()
	if entry := managementTransportCache[key]; entry != nil {
		entry.lastUsed = now
		return entry.transport
	}

	transport := util.BuildProxyTransport(proxyStr, key.preferIPv4)
	if transport == nil {
		return nil
	}
	util.ApplyTLSConfig(transport, sdkCfg)
	if len(managementTransportCache) >= maxManagementTransportEntries {
		var oldestKey managementTransportKey
		var oldest *managementTransportEntry
		for k, e := range managementTransportCache {
			if oldest == nil || e.lastUsed.Before(oldest.lastUsed) {
				oldestKey = k
				oldest = e
			}
		}
		if oldest != nil {
			delete(managementTransportCache, oldestKey)
			oldest.transport.CloseIdleConnections()
		}
	}
	managementTransportCache[key] = &managementTransportEntry{transport: transport, lastUsed: now}
	return transport
}
