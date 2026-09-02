package management

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/identity"
	managementusagelogs "github.com/router-for-me/CLIProxyAPI/v6/internal/management/usagelogs"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/usage"
	log "github.com/sirupsen/logrus"
)

const authFileGroupTrendCacheTTL = 30 * time.Second

// Short TTL for per-account card trend payloads. Cards re-fetch on page/view
// changes; without this, concurrent auth-file-trend queries can stampede
// request_logs aggregation on every AI Accounts visit.
const authFileTrendCacheTTL = 20 * time.Second

// Entity-stats is requested on every AI Accounts list load (30-day GROUP BY over
// request_logs). A short shared cache absorbs remounts and multi-tab reloads.
const entityUsageStatsCacheTTL = 20 * time.Second

type UsageLogsHandler struct {
	*Handler
}

func (h *Handler) UsageLogs() *UsageLogsHandler {
	if h == nil {
		return nil
	}
	return &UsageLogsHandler{Handler: h}
}

func (h *UsageLogsHandler) service(c *gin.Context) *managementusagelogs.Service {
	tenantID := identity.SystemTenantID
	if principal, ok := principalFromContext(c); ok && strings.TrimSpace(principal.EffectiveTenant.ID) != "" {
		tenantID = principal.EffectiveTenant.ID
	}
	return h.serviceForTenant(tenantID)
}

func (h *UsageLogsHandler) serviceForTenant(tenantID string) *managementusagelogs.Service {
	if h == nil {
		return managementusagelogs.NewForTenant(tenantID, nil, nil)
	}
	return managementusagelogs.NewForTenant(tenantID, h.cfg, h.authManager)
}

// clearTrendCache remains on the root handler as a narrow compatibility bridge
// while quota-related endpoints still invalidate usage trend cache directly.
func (h *Handler) clearTrendCache() {
	if h == nil {
		return
	}
	h.UsageLogs().clearTrendCache()
}

type deleteUsageLogsRequest struct {
	ClearBodyContent    bool `json:"clear_body_content"`
	ClearDetailContent  bool `json:"clear_detail_content"`
	ClearRequestRecords bool `json:"clear_request_records"`
}

// GetUsageLogs returns paginated, filterable request log entries.
func (h *UsageLogsHandler) GetUsageLogs(c *gin.Context) {
	channelValues := queryStringListMulti(c, "channel", "channels")
	if raw := strings.TrimSpace(c.Query("channel_name")); raw != "" {
		channelValues = append(channelValues, raw)
	}
	if raw := strings.TrimSpace(c.Query("channel-name")); raw != "" {
		channelValues = append(channelValues, raw)
	}
	chanSeen := make(map[string]struct{}, len(channelValues))
	deduped := make([]string, 0, len(channelValues))
	for _, v := range channelValues {
		lower := strings.ToLower(strings.TrimSpace(v))
		if lower == "" {
			continue
		}
		if _, ok := chanSeen[lower]; ok {
			continue
		}
		chanSeen[lower] = struct{}{}
		deduped = append(deduped, v)
	}

	payload, err := h.service(c).ManagementLogs(managementusagelogs.ManagementLogQueryInput{
		Page:            intQueryDefault(c, "page", 1),
		Size:            intQueryDefault(c, "size", 50),
		Days:            intQueryDefault(c, "days", 7),
		APIKeys:         queryStringListMulti(c, "api_key", "api_keys"),
		Models:          queryStringListMulti(c, "model", "models"),
		Statuses:        queryStringListMulti(c, "status", "statuses"),
		Channels:        deduped,
		MatchNoAPIKeys:  queryBool(c, "api_keys_empty"),
		MatchNoModels:   queryBool(c, "models_empty"),
		MatchNoStatuses: queryBool(c, "statuses_empty"),
		MatchNoChannels: queryBool(c, "channels_empty"),
	})
	if err != nil {
		log.Warnf("management usage logs: get usage logs failed: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, payload)
}

func (h *UsageLogsHandler) DeleteUsageLogs(c *gin.Context) {
	if c.Request.ContentLength == 0 {
		result, err := h.service(c).ClearAllRequestLogs()
		if err != nil {
			log.Warnf("management usage logs: clear all request logs failed: %v", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, result)
		return
	}

	var req deleteUsageLogsRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	status, payload, err := h.service(c).ClearRequestLogs(usage.ClearRequestLogsOptions{
		ClearBodyContent:    req.ClearBodyContent,
		ClearDetailContent:  req.ClearDetailContent,
		ClearRequestRecords: req.ClearRequestRecords,
	})
	if err != nil {
		c.JSON(status, payload)
		return
	}
	c.JSON(status, payload)
}

// GetLogContent returns the stored request/response content for a single log entry.
func (h *UsageLogsHandler) GetLogContent(c *gin.Context) {
	id, ok := parseLogID(c)
	if !ok {
		return
	}
	renderLogContentResponse(c, h.service(c).LogContent(
		id,
		managementusagelogs.NormalizeLogContentPartValue(c.Query("part")),
		managementusagelogs.NormalizeLogContentFormatValue(c.Query("format")),
	))
}

// GetPublicUsageLogs returns paginated request log entries for a specific API key.
func (h *UsageLogsHandler) GetPublicUsageLogs(c *gin.Context) {
	req, status, message := readPublicLookupRequest(c)
	if message != "" {
		c.JSON(status, gin.H{"error": message})
		return
	}
	subject, ok := h.resolvePublicUsageSubject(c, req.APIKey)
	if !ok {
		return
	}
	payload, err := h.serviceForTenant(subject.TenantID).PublicUsageLogs(managementusagelogs.PublicLogQueryInput{
		APIKey:           subject.APIKey,
		EndUserID:        subject.EndUserID,
		APIKeyIDs:        req.APIKeyIDs,
		Models:           req.Models,
		Channels:         req.Channels,
		Statuses:         req.Statuses,
		MatchNoAPIKeyIDs: req.APIKeyIDsEmpty,
		MatchNoModels:    req.ModelsEmpty,
		MatchNoChannels:  req.ChannelsEmpty,
		MatchNoStatuses:  req.StatusesEmpty,
		Page:             req.Page,
		Size:             req.Size,
		Days:             req.Days,
	})
	if err != nil {
		log.Warnf("management usage logs: public usage logs failed: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, payload)
}

// GetPublicUsageChartData returns pre-aggregated chart data for a specific API key.
func (h *UsageLogsHandler) GetPublicUsageChartData(c *gin.Context) {
	req, status, message := readPublicLookupRequest(c)
	if message != "" {
		c.JSON(status, gin.H{"error": message})
		return
	}
	subject, ok := h.resolvePublicUsageSubject(c, req.APIKey)
	if !ok {
		return
	}
	service := h.serviceForTenant(subject.TenantID)
	var payload map[string]any
	var err error
	// Portal session has EndUserID without a presented secret → account label.
	// Secret lookup (even when the key is owned) must keep key-own-name labeling.
	if subject.EndUserID != "" && strings.TrimSpace(subject.APIKey) == "" {
		payload, err = service.PublicChartDataForEndUser(subject.EndUserID, req.Days)
	} else {
		payload, err = service.PublicChartData(subject.APIKey, req.Days)
	}
	if err != nil {
		log.Warnf("management usage logs: public chart data failed: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, payload)
}

// GetPublicLogContent returns the stored request/response content for a single log entry,
// but only if it belongs to the specified API key. This is a public endpoint.
func (h *UsageLogsHandler) GetPublicLogContent(c *gin.Context) {
	req, status, message := readPublicLookupRequest(c)
	if message != "" {
		c.JSON(status, gin.H{"error": message})
		return
	}
	subject, ok := h.resolvePublicUsageSubject(c, req.APIKey)
	if !ok {
		return
	}
	id, ok := parseLogID(c)
	if !ok {
		return
	}
	service := h.serviceForTenant(subject.TenantID)
	if subject.EndUserID != "" {
		renderLogContentResponse(c, service.PublicLogContentForEndUser(id, subject.EndUserID, req.Part, req.Format))
		return
	}
	renderLogContentResponse(c, service.PublicLogContent(id, subject.APIKey, req.Part, req.Format))
}

// GetUsageChartData returns pre-aggregated chart data for the management portal.
func (h *UsageLogsHandler) GetUsageChartData(c *gin.Context) {
	payload, err := h.service(c).UsageChartData(strings.TrimSpace(c.Query("api_key")), intQueryDefault(c, "days", 7))
	if err != nil {
		log.Warnf("management usage logs: usage chart data failed: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, payload)
}

// GetEntityUsageStats returns aggregated statistics grouped by source or auth_index
func (h *UsageLogsHandler) GetEntityUsageStats(c *gin.Context) {
	apiKey := strings.TrimSpace(c.Query("api_key"))
	days := intQueryDefault(c, "days", 7)
	authIndexes := queryStringList(c, "auth_index")
	sources := queryStringList(c, "source")

	tenantID := identity.SystemTenantID
	if principal, ok := principalFromContext(c); ok && strings.TrimSpace(principal.EffectiveTenant.ID) != "" {
		tenantID = principal.EffectiveTenant.ID
	}
	cacheKey := "entity-stats:" + tenantID + ":" + apiKey + ":" + strconv.Itoa(days) + ":" +
		strings.Join(authIndexes, ",") + ":" + strings.Join(sources, ",")
	if cached, ok := h.getAuthFileTrendCache(cacheKey); ok {
		c.JSON(http.StatusOK, cached)
		return
	}

	payload, err := h.service(c).EntityUsageStats(apiKey, days, authIndexes, sources)
	if err != nil {
		log.Warnf("management usage logs: entity usage stats failed: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	h.setTrendCacheWithTTL(cacheKey, payload, entityUsageStatsCacheTTL)
	c.JSON(http.StatusOK, payload)
}

func queryStringList(c *gin.Context, key string) []string {
	rawValues := c.QueryArray(key)
	if len(rawValues) == 0 {
		rawValues = []string{c.Query(key)}
	}
	seen := map[string]struct{}{}
	values := make([]string, 0, len(rawValues))
	for _, raw := range rawValues {
		for _, part := range strings.Split(raw, ",") {
			trimmed := strings.TrimSpace(part)
			if trimmed == "" {
				continue
			}
			if _, ok := seen[trimmed]; ok {
				continue
			}
			seen[trimmed] = struct{}{}
			values = append(values, trimmed)
		}
	}
	return values
}

func queryStringListMulti(c *gin.Context, singular, plural string) []string {
	values := make([]string, 0)
	values = append(values, c.QueryArray(singular)...)
	if raw := strings.TrimSpace(c.Query(plural)); raw != "" {
		values = append(values, strings.Split(raw, ",")...)
	}
	if raw := strings.TrimSpace(c.Query(singular)); raw != "" {
		values = append(values, raw)
	}
	seen := make(map[string]struct{}, len(values))
	result := make([]string, 0, len(values))
	for _, raw := range values {
		trimmed := strings.TrimSpace(raw)
		if trimmed == "" {
			continue
		}
		lower := strings.ToLower(trimmed)
		if _, ok := seen[lower]; ok {
			continue
		}
		seen[lower] = struct{}{}
		result = append(result, trimmed)
	}
	return result
}

func (h *UsageLogsHandler) GetAuthFileGroupTrend(c *gin.Context) {
	group := strings.ToLower(strings.TrimSpace(c.Query("group")))
	if group == "" {
		group = "all"
	}
	days := intQueryDefault(c, "days", 7)
	if days > 7 {
		days = 7
	}

	tenantID := identity.SystemTenantID
	if principal, ok := principalFromContext(c); ok && strings.TrimSpace(principal.EffectiveTenant.ID) != "" {
		tenantID = principal.EffectiveTenant.ID
	}
	cacheKey := tenantID + ":" + group + ":" + strconv.Itoa(days)
	if cached, ok := h.getTrendCache(cacheKey); ok {
		c.JSON(http.StatusOK, cached)
		return
	}

	payload, err := h.service(c).AuthFileGroupTrend(group, days)
	if err != nil {
		log.Warnf("management usage logs: auth file group trend failed: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	h.setTrendCache(cacheKey, payload)
	c.JSON(http.StatusOK, payload)
}

func (h *UsageLogsHandler) GetAuthFileTrend(c *gin.Context) {
	authIndex := strings.TrimSpace(c.Query("auth_index"))
	if authIndex == "" {
		authIndex = strings.TrimSpace(c.Query("authIndex"))
	}
	days := intQueryDefault(c, "days", 7)
	if days > 7 {
		days = 7
	}
	hours := intQueryDefault(c, "hours", 5)
	if hours > 24 {
		hours = 24
	}

	tenantID := identity.SystemTenantID
	if principal, ok := principalFromContext(c); ok && strings.TrimSpace(principal.EffectiveTenant.ID) != "" {
		tenantID = principal.EffectiveTenant.ID
	}
	if authIndex != "" {
		cacheKey := "auth-file-trend:" + tenantID + ":" + authIndex + ":" + strconv.Itoa(days) + ":" + strconv.Itoa(hours)
		if cached, ok := h.getAuthFileTrendCache(cacheKey); ok {
			c.JSON(http.StatusOK, cached)
			return
		}
		status, payload := h.service(c).AuthFileTrend(authIndex, days, hours)
		if status == http.StatusOK {
			h.setAuthFileTrendCache(cacheKey, payload)
		}
		c.JSON(status, payload)
		return
	}

	status, payload := h.service(c).AuthFileTrend(authIndex, days, hours)
	c.JSON(status, payload)
}

func intQueryDefault(c *gin.Context, key string, def int) int {
	return managementusagelogs.IntQueryDefault(c.Query(key), def)
}

func queryBool(c *gin.Context, key string) bool {
	switch strings.ToLower(strings.TrimSpace(c.Query(key))) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

func normalizeLogContentFormatValue(format string) string {
	return managementusagelogs.NormalizeLogContentFormatValue(format)
}

func normalizeLogContentPartValue(part string) string {
	return managementusagelogs.NormalizeLogContentPartValue(part)
}

func parseLogID(c *gin.Context) (int64, bool) {
	idStr := c.Param("id")
	id, err := strconv.ParseInt(strings.TrimSpace(idStr), 10, 64)
	if err != nil || id < 1 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid log id"})
		return 0, false
	}
	return id, true
}

func renderLogContentResponse(c *gin.Context, response managementusagelogs.LogContentResponse) {
	if response.ContentType != "" {
		c.Header("Content-Type", response.ContentType)
		for key, value := range response.Headers {
			c.Header(key, value)
		}
		c.String(response.Status, response.Text)
		return
	}
	c.JSON(response.Status, response.Payload)
}

func (h *UsageLogsHandler) getTrendCache(key string) (managementusagelogs.AuthFileGroupTrendResponse, bool) {
	if h == nil {
		return managementusagelogs.AuthFileGroupTrendResponse{}, false
	}
	h.trendCacheMu.Lock()
	defer h.trendCacheMu.Unlock()
	entry, ok := h.trendCache[key]
	if !ok || time.Now().After(entry.expiresAt) {
		if ok {
			delete(h.trendCache, key)
		}
		return managementusagelogs.AuthFileGroupTrendResponse{}, false
	}
	payload, ok := entry.payload.(managementusagelogs.AuthFileGroupTrendResponse)
	return payload, ok
}

func (h *UsageLogsHandler) setTrendCache(key string, payload managementusagelogs.AuthFileGroupTrendResponse) {
	if h == nil {
		return
	}
	h.trendCacheMu.Lock()
	defer h.trendCacheMu.Unlock()
	if h.trendCache == nil {
		h.trendCache = make(map[string]trendCacheEntry)
	}
	now := time.Now()
	for k, entry := range h.trendCache {
		if now.After(entry.expiresAt) {
			delete(h.trendCache, k)
		}
	}
	h.trendCache[key] = trendCacheEntry{expiresAt: now.Add(authFileGroupTrendCacheTTL), payload: payload}
}

func (h *UsageLogsHandler) getAuthFileTrendCache(key string) (any, bool) {
	if h == nil {
		return nil, false
	}
	h.trendCacheMu.Lock()
	defer h.trendCacheMu.Unlock()
	entry, ok := h.trendCache[key]
	if !ok || time.Now().After(entry.expiresAt) {
		if ok {
			delete(h.trendCache, key)
		}
		return nil, false
	}
	return entry.payload, true
}

func (h *UsageLogsHandler) setAuthFileTrendCache(key string, payload any) {
	h.setTrendCacheWithTTL(key, payload, authFileTrendCacheTTL)
}

func (h *UsageLogsHandler) setTrendCacheWithTTL(key string, payload any, ttl time.Duration) {
	if h == nil || payload == nil {
		return
	}
	if ttl <= 0 {
		ttl = authFileTrendCacheTTL
	}
	h.trendCacheMu.Lock()
	defer h.trendCacheMu.Unlock()
	if h.trendCache == nil {
		h.trendCache = make(map[string]trendCacheEntry)
	}
	now := time.Now()
	// Bound memory: drop expired entries and cap total cached keys.
	const maxTrendCacheEntries = 256
	for k, entry := range h.trendCache {
		if now.After(entry.expiresAt) {
			delete(h.trendCache, k)
		}
	}
	if len(h.trendCache) >= maxTrendCacheEntries {
		// Best-effort: drop an arbitrary entry when full (map iteration order).
		for k := range h.trendCache {
			delete(h.trendCache, k)
			break
		}
	}
	h.trendCache[key] = trendCacheEntry{expiresAt: now.Add(ttl), payload: payload}
}

func (h *UsageLogsHandler) clearTrendCache() {
	if h == nil {
		return
	}
	h.trendCacheMu.Lock()
	defer h.trendCacheMu.Unlock()
	h.trendCache = make(map[string]trendCacheEntry)
}
