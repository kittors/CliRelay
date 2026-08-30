package management

import (
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/identity"
	managementauthfiles "github.com/router-for-me/CLIProxyAPI/v6/internal/management/authfiles"
	apikeysettings "github.com/router-for-me/CLIProxyAPI/v6/internal/management/settings/apikey"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/usage"
	log "github.com/sirupsen/logrus"
)

// dashboardSummaryCacheTTL absorbs multi-tab and 5–20s frontend poll storms so
// request_logs aggregation is not re-run on every refresh. Counts (API keys /
// auth files) stay live; only KPI + trends payloads are cached.
const dashboardSummaryCacheTTL = 15 * time.Second

type dashboardSummaryUsageCache struct {
	kpi             usage.DashboardKPI
	trends          usage.DashboardTrends
	throughputScope string
}

// GetDashboardSummary is a lightweight endpoint that returns only the
// counts and KPIs needed by the frontend dashboard page, avoiding
// the transfer of the full usage / config payloads.
//
// GET /v0/management/dashboard-summary?days=7
//
// Platform super-admins receive throughput_series aggregated across every
// tenant (meta.throughput_scope = "all_tenants"); ordinary tenants stay scoped.
func (h *Handler) GetDashboardSummary(c *gin.Context) {
	cfg := h.cfg
	tenantID := effectiveTenantID(c)
	principal, hasPrincipal := principalFromContext(c)
	allTenantThroughput := hasPrincipal && principal.PlatformAdmin

	// ── Provider key counts ──
	geminiCount := 0
	claudeCount := 0
	codexCount := 0
	vertexCount := 0
	openaiCount := 0
	authFileCount := 0
	apiKeyCount := 0

	if cfg != nil && tenantID == identity.SystemTenantID {
		geminiCount = len(cfg.GeminiKey)
		claudeCount = len(cfg.ClaudeKey)
		codexCount = len(cfg.CodexKey)
		vertexCount = len(cfg.VertexCompatAPIKey)
		openaiCount = len(cfg.OpenAICompatibility)
	}
	apiKeyCount = len(apikeysettings.NewService(nil, apikeysettings.WithTenantID(tenantID)).ListRows())

	if h.authManager != nil {
		authFileCount = len(managementauthfiles.ListEntries(h.authManager.ListForTenant(tenantID), managementauthfiles.EntryOptions{
			OnStatError: func(path string, err error) {
				log.WithError(err).Warnf("failed to stat auth file %s", path)
			},
		}))
	}

	providerTotal := geminiCount + claudeCount + codexCount + vertexCount + openaiCount
	if tenantID != identity.SystemTenantID {
		providerTotal = authFileCount
	}

	// ── Usage KPIs (from SQLite — persists across restarts) ──
	daysStr := c.DefaultQuery("days", "7")
	days := 7
	if v, err := parsePositiveInt(daysStr); err == nil && v > 0 {
		days = v
	}

	scopeKey := "tenant"
	if allTenantThroughput {
		scopeKey = "all_tenants"
	}
	cacheKey := "dashboard-summary:" + tenantID + ":" + strconv.Itoa(days) + ":" + scopeKey

	var (
		kpi             usage.DashboardKPI
		trends          usage.DashboardTrends
		throughputScope string
		fromCache       bool
	)
	if cached, ok := h.getDashboardSummaryCache(cacheKey); ok {
		if payload, ok := cached.(dashboardSummaryUsageCache); ok {
			kpi = payload.kpi
			trends = payload.trends
			throughputScope = payload.throughputScope
			fromCache = true
		}
	}
	if !fromCache {
		kpi, _ = usage.QueryDashboardKPIForTenant(tenantID, days)
		trends, _ = usage.QueryDashboardTrendsForTenant(tenantID, days)
		throughputScope = "tenant"
		if allTenantThroughput {
			if series, err := usage.QueryDashboardThroughputAcrossTenants(); err == nil {
				trends.ThroughputSeries = series
				throughputScope = "all_tenants"
			}
			tenantNames := make(map[string]string)
			if service := h.identity(); service != nil {
				if tenants, tErr := service.ListTenants(c.Request.Context()); tErr == nil {
					for _, t := range tenants {
						name := t.Name
						if name == "" {
							name = t.Slug
						}
						tenantNames[t.ID] = name
					}
				}
			}
			if items, bErr := usage.QueryDashboardTenantBreakdownThroughput(tenantNames); bErr == nil {
				trends.Tenants = items
			}
		}
		h.setDashboardSummaryCache(cacheKey, dashboardSummaryUsageCache{
			kpi:             kpi,
			trends:          trends,
			throughputScope: throughputScope,
		})
	}

	c.JSON(http.StatusOK, gin.H{
		"kpi": gin.H{
			"total_requests":   kpi.TotalRequests,
			"success_requests": kpi.SuccessRequests,
			"failed_requests":  kpi.FailedRequests,
			"success_rate":     kpi.SuccessRate,
			"input_tokens":     kpi.InputTokens,
			"output_tokens":    kpi.OutputTokens,
			"reasoning_tokens": kpi.ReasoningTokens,
			"cached_tokens":    kpi.CachedTokens,
			"total_tokens":     kpi.TotalTokens,
			"total_cost":       kpi.TotalCost,
			"cache_rate":       kpi.CacheRate,
		},
		"counts": gin.H{
			"api_keys":         apiKeyCount,
			"providers_total":  providerTotal,
			"gemini_keys":      geminiCount,
			"claude_keys":      claudeCount,
			"codex_keys":       codexCount,
			"vertex_keys":      vertexCount,
			"openai_providers": openaiCount,
			"auth_files":       authFileCount,
		},
		"trends": trends,
		"meta": gin.H{
			"generated_at":     time.Now().UTC().Format(time.RFC3339),
			"throughput_scope": throughputScope,
		},
		"days": days,
	})
}

func (h *Handler) getDashboardSummaryCache(key string) (any, bool) {
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

func (h *Handler) setDashboardSummaryCache(key string, payload any) {
	if h == nil || payload == nil {
		return
	}
	h.trendCacheMu.Lock()
	defer h.trendCacheMu.Unlock()
	if h.trendCache == nil {
		h.trendCache = make(map[string]trendCacheEntry)
	}
	now := time.Now()
	const maxEntries = 256
	for k, entry := range h.trendCache {
		if now.After(entry.expiresAt) {
			delete(h.trendCache, k)
		}
	}
	if len(h.trendCache) >= maxEntries {
		for k := range h.trendCache {
			delete(h.trendCache, k)
			break
		}
	}
	h.trendCache[key] = trendCacheEntry{expiresAt: now.Add(dashboardSummaryCacheTTL), payload: payload}
}

func parsePositiveInt(s string) (int, error) {
	var v int
	_, err := fmt.Sscanf(s, "%d", &v)
	return v, err
}
