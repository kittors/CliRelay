package executor

import (
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/util"
)

const maxUpstreamTransportCacheEntries = 128

var sharedUpstreamTransportCache = newUpstreamTransportCache()

type upstreamTransportCacheKey struct {
	direct             bool
	proxyURL           string
	preferIPv4         bool
	insecureSkipVerify bool
	caCert             string
	caCertStat         string
}

type upstreamTransportCacheEntry struct {
	transport *http.Transport
	lastUsed  time.Time
}

type upstreamTransportCache struct {
	mu         sync.Mutex
	transports map[upstreamTransportCacheKey]*upstreamTransportCacheEntry
}

func newUpstreamTransportCache() *upstreamTransportCache {
	return &upstreamTransportCache{
		transports: make(map[upstreamTransportCacheKey]*upstreamTransportCacheEntry),
	}
}

func cachedProxyTransport(proxyURL string, sdkCfg *config.SDKConfig) *http.Transport {
	return sharedUpstreamTransportCache.get(proxyURL, sdkCfg, false)
}

// cachedDirectTransport returns a process-wide transport for direct upstream
// requests. http.Transport owns connection pools and must outlive individual
// requests; creating one per request defeats keep-alive and HTTP/2 reuse.
func cachedDirectTransport(sdkCfg *config.SDKConfig) *http.Transport {
	return sharedUpstreamTransportCache.get("", sdkCfg, true)
}

func (c *upstreamTransportCache) get(proxyURL string, sdkCfg *config.SDKConfig, direct bool) *http.Transport {
	if c == nil {
		return nil
	}
	key := newUpstreamTransportCacheKey(proxyURL, sdkCfg)
	key.direct = direct
	if !key.direct && key.proxyURL == "" {
		return nil
	}

	now := time.Now()
	c.mu.Lock()
	defer c.mu.Unlock()

	if entry := c.transports[key]; entry != nil {
		entry.lastUsed = now
		return entry.transport
	}

	var transport *http.Transport
	if key.direct {
		transport = util.NewDefaultTransport(key.preferIPv4)
	} else {
		transport = util.BuildProxyTransport(key.proxyURL, key.preferIPv4)
	}
	if transport == nil {
		return nil
	}
	util.ApplyTLSConfig(transport, sdkCfg)

	if len(c.transports) >= maxUpstreamTransportCacheEntries {
		c.evictOldestLocked()
	}
	c.transports[key] = &upstreamTransportCacheEntry{
		transport: transport,
		lastUsed:  now,
	}
	return transport
}

func (c *upstreamTransportCache) evictOldestLocked() {
	var oldestKey upstreamTransportCacheKey
	var oldestEntry *upstreamTransportCacheEntry
	for key, entry := range c.transports {
		if oldestEntry == nil || entry.lastUsed.Before(oldestEntry.lastUsed) {
			oldestKey = key
			oldestEntry = entry
		}
	}
	if oldestEntry == nil {
		return
	}
	delete(c.transports, oldestKey)
	oldestEntry.transport.CloseIdleConnections()
}

func newUpstreamTransportCacheKey(proxyURL string, sdkCfg *config.SDKConfig) upstreamTransportCacheKey {
	key := upstreamTransportCacheKey{
		proxyURL: strings.TrimSpace(proxyURL),
	}
	if sdkCfg == nil {
		return key
	}
	key.preferIPv4 = sdkCfg.PreferIPv4
	key.insecureSkipVerify = sdkCfg.InsecureSkipVerify
	key.caCert = strings.TrimSpace(sdkCfg.CACert)
	key.caCertStat = util.CACertStatFingerprint(key.caCert)
	return key
}
