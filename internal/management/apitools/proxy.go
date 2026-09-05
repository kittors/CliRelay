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
}

type managementTransportEntry struct {
	transport *http.Transport
	lastUsed  time.Time
}

var (
	managementTransportMu    sync.Mutex
	managementTransportCache = map[managementTransportKey]*managementTransportEntry{}
)

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
		if targetURL != nil && isCodexTargetURL(auth, targetURL) {
			if fpTransport := cachedManagementTLSFingerprintTransport(proxyStr, sdkCfg); fpTransport != nil {
				return fpTransport
			}
		}
		if transport := cachedManagementProxyTransport(proxyStr, sdkCfg); transport != nil {
			return transport
		}
	}
	if targetURL != nil && isCodexTargetURL(auth, targetURL) {
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
	key := managementTransportKey{proxyURL: strings.TrimSpace(proxyStr)}
	if sdkCfg != nil {
		key.preferIPv4 = sdkCfg.PreferIPv4
		key.insecureSkipVerify = sdkCfg.InsecureSkipVerify
		key.caCert = strings.TrimSpace(sdkCfg.CACert)
	}
	opts := tlsfingerprint.Options{
		Profile:            tlsfingerprint.DefaultProfile,
		ProxyURL:           key.proxyURL,
		PreferIPv4:         key.preferIPv4,
		InsecureSkipVerify: key.insecureSkipVerify,
		CACertPath:         key.caCert,
		DialTimeout:        30 * time.Second,
	}
	rt, err := tlsfingerprint.New(opts)
	if err != nil {
		return nil
	}
	return rt
}

func cachedManagementProxyTransport(proxyStr string, sdkCfg *config.SDKConfig) *http.Transport {
	proxyStr = strings.TrimSpace(proxyStr)
	if proxyStr == "" {
		return nil
	}
	key := managementTransportKey{proxyURL: proxyStr}
	if sdkCfg != nil {
		key.preferIPv4 = sdkCfg.PreferIPv4
		key.insecureSkipVerify = sdkCfg.InsecureSkipVerify
		key.caCert = strings.TrimSpace(sdkCfg.CACert)
	}

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
