package management

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	serviceapp "github.com/router-for-me/CLIProxyAPI/v6/internal/app/service"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/identity"
	proxypoolsettings "github.com/router-for-me/CLIProxyAPI/v6/internal/management/settings/proxypool"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/usage"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/util"
	log "github.com/sirupsen/logrus"
)

const (
	defaultProxyCheckFallbackURL = "https://www.gstatic.com/generate_204"
	defaultProxyCheckPublicPath  = "/v0/management/public/ping"
	defaultProxyCheckTimeout     = 8 * time.Second
)

type proxyPoolAPIEntry struct {
	config.ProxyPoolEntry
	MaskedURL string `json:"masked_url"`
}

// GetProxyPool returns reusable proxy entries for the management UI.
func (h *Handler) GetProxyPool(c *gin.Context) {
	tenantID := effectiveTenantID(c)
	var entries []config.ProxyPoolEntry
	if proxypoolsettings.StoreAvailableForTenant(tenantID) {
		entries = proxypoolsettings.ListForTenant(tenantID)
	} else if h != nil && tenantID == identity.SystemTenantID {
		h.mu.Lock()
		if h.cfg != nil {
			entries = append(entries, h.cfg.ProxyPool...)
		}
		h.mu.Unlock()
	}

	items := make([]proxyPoolAPIEntry, 0, len(entries))
	for _, entry := range entries {
		items = append(items, proxyPoolAPIEntry{
			ProxyPoolEntry: entry,
			MaskedURL:      maskProxyPoolURL(entry.URL),
		})
	}
	c.JSON(http.StatusOK, gin.H{"items": items})
}

// PutProxyPool replaces the reusable proxy list after normalization.
func (h *Handler) PutProxyPool(c *gin.Context) {
	tenantID := effectiveTenantID(c)
	var body struct {
		Items []config.ProxyPoolEntry `json:"items"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		var entries []config.ProxyPoolEntry
		if errArray := c.ShouldBindJSON(&entries); errArray != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body"})
			return
		}
		body.Items = entries
	}

	if dups := config.ProxyPoolDuplicateIDs(body.Items); len(dups) > 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("duplicate proxy ids: %s", strings.Join(dups, ", "))})
		return
	}
	normalized := config.NormalizeProxyPool(body.Items)
	if len(body.Items) > 0 && len(normalized) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "no valid proxy entries"})
		return
	}
	if proxypoolsettings.StoreAvailableForTenant(tenantID) {
		if err := proxypoolsettings.ReplaceForTenant(tenantID, normalized); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("failed to save proxy pool: %v", err)})
			return
		}
		if tenantID == identity.SystemTenantID {
			h.mu.Lock()
			if h.cfg == nil {
				h.cfg = &config.Config{}
			}
			h.cfg.ProxyPool = proxypoolsettings.ListForTenant(tenantID)
			cfgRef := h.cfg
			h.mu.Unlock()
			h.notifyConfigMutated(cfgRef)
		} else {
			h.applyTenantProxyPoolConfig(tenantID)
		}
		c.JSON(http.StatusOK, gin.H{"status": "ok"})
		return
	}

	h.mu.Lock()
	if h.cfg == nil {
		h.cfg = &config.Config{}
	}
	h.cfg.ProxyPool = normalized
	h.mu.Unlock()

	h.persist(c)
}

// PatchProxyPoolEntry updates one reusable proxy entry identified by its stable ID.
func (h *Handler) PatchProxyPoolEntry(c *gin.Context) {
	tenantID := effectiveTenantID(c)
	originalID := config.NormalizeProxyID(c.Param("id"))
	if originalID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return
	}

	var body struct {
		Name        *string `json:"name"`
		URL         *string `json:"url"`
		Enabled     *bool   `json:"enabled"`
		Description *string `json:"description"`
	}
	if err := c.ShouldBindJSON(&body); err != nil || body.Name == nil || body.URL == nil || body.Enabled == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body"})
		return
	}

	normalized := config.NormalizeProxyPool([]config.ProxyPoolEntry{{
		ID:          originalID,
		Name:        *body.Name,
		URL:         *body.URL,
		Enabled:     *body.Enabled,
		Description: stringValue(body.Description),
	}})
	if len(normalized) != 1 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "no valid proxy entries"})
		return
	}
	updated := normalized[0]

	if proxypoolsettings.StoreAvailableForTenant(tenantID) {
		if err := proxypoolsettings.UpdateForTenant(tenantID, originalID, updated); err != nil {
			if errors.Is(err, proxypoolsettings.ErrItemNotFound) {
				c.JSON(http.StatusNotFound, gin.H{"error": "item not found"})
				return
			}
			c.JSON(http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("failed to update proxy pool entry: %v", err)})
			return
		}
		if tenantID == identity.SystemTenantID {
			h.mu.Lock()
			if h.cfg == nil {
				h.cfg = &config.Config{}
			}
			h.cfg.ProxyPool = proxypoolsettings.ListForTenant(tenantID)
			cfgRef := h.cfg
			h.mu.Unlock()
			h.notifyConfigMutated(cfgRef)
		} else {
			h.applyTenantProxyPoolConfig(tenantID)
		}
		c.JSON(http.StatusOK, gin.H{"status": "ok"})
		return
	}

	h.mu.Lock()
	if h.cfg == nil {
		h.cfg = &config.Config{}
	}
	found := false
	for i := range h.cfg.ProxyPool {
		if config.NormalizeProxyID(h.cfg.ProxyPool[i].ID) != originalID {
			continue
		}
		h.cfg.ProxyPool[i] = updated
		found = true
		break
	}
	if found {
		h.cfg.ProxyPool = config.NormalizeProxyPool(h.cfg.ProxyPool)
	}
	h.mu.Unlock()

	if !found {
		c.JSON(http.StatusNotFound, gin.H{"error": "item not found"})
		return
	}
	h.persist(c)
}

func (h *Handler) applyTenantProxyPoolConfig(tenantID string) {
	if h == nil || h.authManager == nil || h.cfg == nil {
		return
	}
	// Executors capture a tenant cfg snapshot (including ProxyPool) at bind time.
	// SetConfig alone is not enough: rebind so auth ProxyID resolves to the new URL.
	tenantCfg := usage.BuildTenantRuntimeConfig(h.cfg, tenantID)
	h.authManager.SetConfigForTenant(tenantID, &tenantCfg)
	serviceapp.RebindTenantExecutors(h.cfg, h.authManager, tenantID, nil)
}

// GetPublicPing returns a lightweight public 204 endpoint for proxy latency probes.
func (h *Handler) GetPublicPing(c *gin.Context) {
	c.Status(http.StatusNoContent)
}

// PostProxyPoolCheck checks whether a proxy can reach the deployed server.
func (h *Handler) PostProxyPoolCheck(c *gin.Context) {
	tenantID := effectiveTenantID(c)
	var body struct {
		ID      string `json:"id"`
		URL     string `json:"url"`
		TestURL string `json:"test_url"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body"})
		return
	}

	proxyURL := strings.TrimSpace(body.URL)
	if proxyURL == "" && proxypoolsettings.StoreAvailableForTenant(tenantID) {
		if entry := proxypoolsettings.GetForTenant(tenantID, body.ID); entry != nil {
			proxyURL = strings.TrimSpace(entry.URL)
		}
	}
	if proxyURL == "" && tenantID == identity.SystemTenantID && h != nil && h.cfg != nil {
		if entry := h.cfg.FindProxyPoolEntry(body.ID); entry != nil {
			proxyURL = strings.TrimSpace(entry.URL)
		} else {
			proxyURL = h.cfg.ResolveProxyURL(body.ID, "")
		}
	}
	if err := config.ValidateProxyURL(proxyURL); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	testURL := strings.TrimSpace(body.TestURL)
	if testURL == "" {
		testURL = defaultProxyCheckURL(c.Request)
	}
	if _, err := url.ParseRequestURI(testURL); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid test_url"})
		return
	}

	var sdkCfg *config.SDKConfig
	if h != nil && h.cfg != nil {
		sdkCfg = &h.cfg.SDKConfig
	}
	started := time.Now()
	statusCode, err := checkProxyConnectivity(c.Request.Context(), proxyURL, testURL, sdkCfg)
	latencyMs := time.Since(started).Milliseconds()
	if err != nil {
		c.JSON(http.StatusOK, gin.H{
			"ok":        false,
			"latencyMs": latencyMs,
			"message":   err.Error(),
		})
		return
	}

	ok := statusCode >= 200 && statusCode < 400
	payload := gin.H{
		"ok":         ok,
		"statusCode": statusCode,
		"latencyMs":  latencyMs,
	}
	c.JSON(http.StatusOK, payload)
}

func checkProxyConnectivity(ctx context.Context, proxyURL string, testURL string, sdkCfg *config.SDKConfig) (int, error) {
	transport := util.BuildProxyTransport(proxyURL, false)
	if transport == nil {
		return 0, fmt.Errorf("failed to build proxy transport")
	}
	util.ApplyTLSConfig(transport, sdkCfg)
	client := &http.Client{Timeout: defaultProxyCheckTimeout, Transport: transport}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, testURL, nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("User-Agent", "CLIProxyAPI proxy checker")
	resp, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	defer func() {
		if errClose := resp.Body.Close(); errClose != nil {
			log.WithError(errClose).Debug("failed to close proxy check response body")
		}
	}()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1024))
	return resp.StatusCode, nil
}

func defaultProxyCheckURL(r *http.Request) string {
	if r == nil {
		return defaultProxyCheckFallbackURL
	}

	host := strings.TrimSpace(r.Header.Get("X-Forwarded-Host"))
	if idx := strings.IndexByte(host, ','); idx >= 0 {
		host = host[:idx]
	}
	if host == "" {
		host = strings.TrimSpace(r.Host)
	}
	if host == "" || isPrivateOrLocalHost(host) {
		return defaultProxyCheckFallbackURL
	}

	scheme := strings.TrimSpace(r.Header.Get("X-Forwarded-Proto"))
	if idx := strings.IndexByte(scheme, ','); idx >= 0 {
		scheme = scheme[:idx]
	}
	if scheme == "" {
		if r.TLS != nil {
			scheme = "https"
		} else {
			scheme = "http"
		}
	}

	return scheme + "://" + host + defaultProxyCheckPublicPath
}

func isPrivateOrLocalHost(rawHost string) bool {
	host := strings.TrimSpace(rawHost)
	if host == "" {
		return true
	}
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	host = strings.TrimPrefix(host, "[")
	host = strings.TrimSuffix(host, "]")
	lower := strings.ToLower(host)

	if lower == "localhost" || strings.HasSuffix(lower, ".localhost") || strings.HasSuffix(lower, ".local") {
		return true
	}

	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}

	return ip.IsLoopback() || ip.IsPrivate() || ip.IsUnspecified() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast()
}

func maskProxyPoolURL(raw string) string {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return ""
	}
	parsed, err := url.Parse(trimmed)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return "***"
	}
	return strings.ToLower(parsed.Scheme) + "://" + parsed.Host
}

func stringValue(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

func (h *Handler) notifyConfigMutated(cfg *config.Config) {
	if h == nil || h.onConfigMutated == nil {
		return
	}
	h.onConfigMutated(cfg)
}
