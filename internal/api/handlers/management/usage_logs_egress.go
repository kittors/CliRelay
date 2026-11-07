package management

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/identity"
	proxypoolsettings "github.com/router-for-me/CLIProxyAPI/v6/internal/management/settings/proxypool"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/usage"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/util"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

const usageLogEgressProbeTimeout = 6 * time.Second

var usageLogEgressProbeURLs = []string{
	"https://chatgpt.com/cdn-cgi/trace",
	"https://openai.com/cdn-cgi/trace",
	"https://api.ipify.org",
	"https://ifconfig.me/ip",
	"https://icanhazip.com",
	"https://ipinfo.io/ip",
}

var usageLogEgressProbeFn = probeUsageLogEgressIP

type usageLogEgressMetadata struct {
	RouteKind    string `json:"route_kind,omitempty"`
	ProxySource  string `json:"proxy_source,omitempty"`
	ProxyID      string `json:"proxy_id,omitempty"`
	ProxyName    string `json:"proxy_name,omitempty"`
	ProxyURLHost string `json:"proxy_url_host,omitempty"`
}

type usageLogEgressResponse struct {
	ID              int64  `json:"id"`
	Model           string `json:"model,omitempty"`
	RouteKind       string `json:"route_kind,omitempty"`
	ProxySource     string `json:"proxy_source,omitempty"`
	ProxyID         string `json:"proxy_id,omitempty"`
	ProxyName       string `json:"proxy_name,omitempty"`
	ProxyURLHost    string `json:"proxy_url_host,omitempty"`
	EffectiveIP     string `json:"effective_ip,omitempty"`
	ServerIP        string `json:"server_ip,omitempty"`
	MatchesServerIP *bool  `json:"matches_server_ip,omitempty"`
	UsingProxy      bool   `json:"using_proxy"`
	Error           string `json:"error,omitempty"`
}

// GetUsageLogEgress probes the effective egress IP for a stored request log.
func (h *Handler) GetUsageLogEgress(c *gin.Context) {
	idStr := c.Param("id")
	id, err := strconv.ParseInt(strings.TrimSpace(idStr), 10, 64)
	if err != nil || id < 1 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid log id"})
		return
	}

	tenantID := effectiveTenantID(c)
	logRow, err := usage.QueryLogRowByIDForTenant(tenantID, id)
	if err != nil {
		if strings.Contains(err.Error(), "no rows") {
			c.JSON(http.StatusNotFound, gin.H{"error": "log entry not found"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	detailsPart, err := usage.QueryLogContentPartForTenant(tenantID, id, "details")
	if err != nil && !strings.Contains(err.Error(), "no rows") {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	meta := parseUsageLogEgressMetadata(detailsPart.Content)
	resp := usageLogEgressResponse{
		ID:           logRow.ID,
		Model:        firstNonEmpty(strings.TrimSpace(logRow.Model), strings.TrimSpace(detailsPart.Model)),
		RouteKind:    meta.RouteKind,
		ProxySource:  meta.ProxySource,
		ProxyID:      meta.ProxyID,
		ProxyName:    meta.ProxyName,
		ProxyURLHost: meta.ProxyURLHost,
		UsingProxy:   strings.EqualFold(meta.RouteKind, "proxy"),
	}

	if resp.RouteKind == "" {
		resp.Error = "no stored egress metadata for this request"
		c.JSON(http.StatusOK, resp)
		return
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), usageLogEgressProbeTimeout)
	defer cancel()

	serverIP, serverErr := usageLogEgressProbeFn(ctx, "", h.sdkConfig())
	if serverErr == nil {
		resp.ServerIP = strings.TrimSpace(serverIP)
	}

	effectiveIP := resp.ServerIP
	effectiveErr := serverErr
	if resp.UsingProxy {
		proxyURL := h.resolveUsageLogEgressProxyURL(tenantID, logRow.AuthIndex, meta)
		if proxyURL == "" {
			resp.Error = "proxy configuration is no longer available for recheck"
			if serverErr != nil {
				resp.Error += "; server egress probe failed: " + serverErr.Error()
			}
			c.JSON(http.StatusOK, resp)
			return
		}
		effectiveIP, effectiveErr = usageLogEgressProbeFn(ctx, proxyURL, h.sdkConfig())
	}

	if effectiveErr == nil {
		resp.EffectiveIP = strings.TrimSpace(effectiveIP)
	}
	if resp.EffectiveIP != "" && resp.ServerIP != "" {
		matches := strings.EqualFold(resp.EffectiveIP, resp.ServerIP)
		resp.MatchesServerIP = &matches
	}

	if effectiveErr != nil {
		resp.Error = effectiveErr.Error()
	} else if serverErr != nil {
		resp.Error = "server egress probe failed: " + serverErr.Error()
	}

	c.JSON(http.StatusOK, resp)
}

func parseUsageLogEgressMetadata(raw string) usageLogEgressMetadata {
	if strings.TrimSpace(raw) == "" {
		return usageLogEgressMetadata{}
	}
	var payload struct {
		Egress usageLogEgressMetadata `json:"egress"`
	}
	if err := json.Unmarshal([]byte(raw), &payload); err != nil {
		return usageLogEgressMetadata{}
	}
	payload.Egress.RouteKind = strings.TrimSpace(payload.Egress.RouteKind)
	payload.Egress.ProxySource = strings.TrimSpace(payload.Egress.ProxySource)
	payload.Egress.ProxyID = strings.TrimSpace(payload.Egress.ProxyID)
	payload.Egress.ProxyName = strings.TrimSpace(payload.Egress.ProxyName)
	payload.Egress.ProxyURLHost = strings.TrimSpace(payload.Egress.ProxyURLHost)
	return payload.Egress
}

func (h *Handler) resolveUsageLogEgressProxyURL(tenantID, authIndex string, meta usageLogEgressMetadata) string {
	auth := h.findAuthByIndexForTenant(tenantID, authIndex)
	// Recheck through the proxy the request left by: credentials resolve
	// proxies where they run, so a platform credential shared with this tenant
	// resolves in the system tenant's pool, and the tenant's own credentials
	// in its effective pool, which includes the proxies shared with it.
	if auth != nil {
		tenantID = coreauth.NormalizedTenantID(auth.TenantID)
	}
	var cfg *config.Config
	if tenantID == identity.SystemTenantID {
		cfg = h.cfg
	} else if h.cfg != nil {
		tenantCfg := usage.BuildTenantRuntimeConfig(h.cfg, tenantID)
		cfg = &tenantCfg
	}
	proxyID := strings.TrimSpace(meta.ProxyID)
	if proxyID == "" && auth != nil {
		proxyID = strings.TrimSpace(auth.ProxyID)
	}
	authProxyURL := ""
	if auth != nil {
		authProxyURL = strings.TrimSpace(auth.ProxyURL)
	}

	switch strings.ToLower(meta.ProxySource) {
	case "proxy_id":
		if proxyID != "" {
			if resolved := resolveUsageLogProxyPoolURL(tenantID, cfg, proxyID, authProxyURL); resolved != "" {
				return resolved
			}
		}
	case "auth_proxy_url":
		if authProxyURL != "" && usageLogProxyURLMatchesMaskedHost(authProxyURL, meta.ProxyURLHost) {
			return authProxyURL
		}
	case "global_proxy_url":
		if cfg != nil {
			if proxyURL := strings.TrimSpace(cfg.ProxyURL); proxyURL != "" && usageLogProxyURLMatchesMaskedHost(proxyURL, meta.ProxyURLHost) {
				return proxyURL
			}
		}
	case "proxy_url":
		if authProxyURL != "" && usageLogProxyURLMatchesMaskedHost(authProxyURL, meta.ProxyURLHost) {
			return authProxyURL
		}
		if cfg != nil {
			if proxyURL := strings.TrimSpace(cfg.ProxyURL); proxyURL != "" && usageLogProxyURLMatchesMaskedHost(proxyURL, meta.ProxyURLHost) {
				return proxyURL
			}
		}
	}

	if proxyID != "" {
		if resolved := resolveUsageLogProxyPoolURL(tenantID, cfg, proxyID, authProxyURL); resolved != "" {
			return resolved
		}
	}
	if authProxyURL != "" && (meta.ProxyURLHost == "" || usageLogProxyURLMatchesMaskedHost(authProxyURL, meta.ProxyURLHost)) {
		return authProxyURL
	}
	if cfg != nil {
		if proxyURL := strings.TrimSpace(cfg.ProxyURL); proxyURL != "" && (meta.ProxyURLHost == "" || usageLogProxyURLMatchesMaskedHost(proxyURL, meta.ProxyURLHost)) {
			return proxyURL
		}
	}
	return ""
}

func resolveUsageLogProxyPoolURL(tenantID string, cfg *config.Config, proxyID string, fallbackURL string) string {
	if entry := proxypoolsettings.GetForTenant(tenantID, proxyID); entry != nil && entry.Enabled {
		if proxyURL := strings.TrimSpace(entry.URL); proxyURL != "" {
			return proxyURL
		}
	}
	if cfg == nil {
		return ""
	}
	return strings.TrimSpace(cfg.ResolveProxyURL(proxyID, fallbackURL))
}

func (h *Handler) findAuthByIndexForTenant(tenantID, authIndex string) *coreauth.Auth {
	authIndex = strings.TrimSpace(authIndex)
	if authIndex == "" || h == nil || h.authManager == nil {
		return nil
	}
	// A request of this tenant may have been served by a platform credential
	// shared with it.
	for _, auth := range h.authManager.ListUsableForTenant(tenantID) {
		if auth == nil {
			continue
		}
		if strings.EqualFold(strings.TrimSpace(auth.EnsureIndex()), authIndex) {
			return auth
		}
	}
	return nil
}

func (h *Handler) sdkConfig() *config.SDKConfig {
	if h == nil || h.cfg == nil {
		return nil
	}
	return &h.cfg.SDKConfig
}

func usageLogProxyURLMatchesMaskedHost(proxyURL string, maskedHost string) bool {
	maskedHost = strings.TrimSpace(strings.ToLower(maskedHost))
	if maskedHost == "" {
		return true
	}
	parsed, err := url.Parse(strings.TrimSpace(proxyURL))
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return false
	}
	return maskedHost == strings.ToLower(parsed.Scheme)+"://"+parsed.Host
}

func probeUsageLogEgressIP(ctx context.Context, proxyURL string, sdkCfg *config.SDKConfig) (string, error) {
	preferIPv4 := sdkCfg != nil && sdkCfg.PreferIPv4
	transport := util.NewDefaultTransport(preferIPv4)
	if strings.TrimSpace(proxyURL) != "" {
		transport = util.BuildProxyTransport(proxyURL, preferIPv4)
		if transport == nil {
			return "", fmt.Errorf("failed to build proxy transport")
		}
	}
	util.ApplyTLSConfig(transport, sdkCfg)
	client := &http.Client{Timeout: usageLogEgressProbeTimeout, Transport: transport}

	var lastErr error
	for _, probeURL := range usageLogEgressProbeURLs {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, probeURL, nil)
		if err != nil {
			lastErr = err
			continue
		}
		req.Header.Set("User-Agent", "CLIProxyAPI egress probe")
		resp, err := client.Do(req)
		if err != nil {
			lastErr = err
			continue
		}
		body, readErr := io.ReadAll(io.LimitReader(resp.Body, 256))
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			lastErr = fmt.Errorf("probe %s returned status %d", probeURL, resp.StatusCode)
			continue
		}
		if readErr != nil {
			lastErr = readErr
			continue
		}
		if ip := extractUsageLogProbeIP(body); ip != "" {
			return ip, nil
		}
		lastErr = fmt.Errorf("probe %s returned unusable body", probeURL)
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("all egress probe services failed")
	}
	return "", lastErr
}

func extractUsageLogProbeIP(body []byte) string {
	text := strings.TrimSpace(string(body))
	if text == "" {
		return ""
	}

	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(strings.ToLower(line), "ip=") {
			continue
		}
		value := strings.TrimSpace(line[len("ip="):])
		if parsed := net.ParseIP(strings.Trim(value, "[]")); parsed != nil {
			return parsed.String()
		}
		return ""
	}

	if parsed := net.ParseIP(strings.Trim(text, "[]")); parsed != nil {
		return parsed.String()
	}
	return ""
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return trimmed
		}
	}
	return ""
}
