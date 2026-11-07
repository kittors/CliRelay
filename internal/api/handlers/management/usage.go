package management

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/usage"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

// systemUsageOnly answers 403 unless the caller acts for the system tenant.
// The in-memory statistics behind /usage, /usage/export and /usage/import are
// one process-wide aggregate of every tenant's requests (keys, accounts and
// their sources), so monitor.read in a tenant must not reach them; tenants read
// their own usage from the request log endpoints.
func systemUsageOnly(c *gin.Context) bool {
	if coreauth.NormalizedTenantID(effectiveTenantID(c)) == coreauth.NormalizedTenantID("") {
		return true
	}
	c.JSON(http.StatusForbidden, gin.H{"error": "the process-wide usage statistics are only available to the platform"})
	return false
}

type usageExportPayload struct {
	Version    int                      `json:"version"`
	ExportedAt time.Time                `json:"exported_at"`
	Usage      usage.StatisticsSnapshot `json:"usage"`
}

type usageImportPayload struct {
	Version int                      `json:"version"`
	Usage   usage.StatisticsSnapshot `json:"usage"`
}

// GetUsageStatistics returns the in-memory request statistics snapshot.
func (h *Handler) GetUsageStatistics(c *gin.Context) {
	if !systemUsageOnly(c) {
		return
	}
	var snapshot usage.StatisticsSnapshot
	if h != nil && h.usageStats != nil {
		snapshot = h.usageStats.Snapshot()
	}
	c.JSON(http.StatusOK, gin.H{
		"usage":           snapshot,
		"failed_requests": snapshot.FailureCount,
	})
}

// ExportUsageStatistics returns a complete usage snapshot for backup/migration.
func (h *Handler) ExportUsageStatistics(c *gin.Context) {
	if !systemUsageOnly(c) {
		return
	}
	var snapshot usage.StatisticsSnapshot
	if h != nil && h.usageStats != nil {
		snapshot = h.usageStats.Snapshot()
	}
	c.JSON(http.StatusOK, usageExportPayload{
		Version:    1,
		ExportedAt: time.Now().UTC(),
		Usage:      snapshot,
	})
}

// GetPublicUsageByAPIKey returns usage statistics for a specific API key.
// This endpoint is designed for public access (no management key required).
func (h *Handler) GetPublicUsageByAPIKey(c *gin.Context) {
	req, status, message := readPublicLookupRequest(c)
	if message != "" {
		c.JSON(status, gin.H{"error": message})
		return
	}

	subject, ok := h.resolvePublicUsageSubject(c, req.APIKey)
	if !ok {
		return
	}

	var snapshot usage.StatisticsSnapshot
	if h != nil && h.usageStats != nil {
		snapshot = h.usageStats.Snapshot()
	}

	// The snapshot's buckets are keyed by key id, so the subject is resolved to
	// ids first: the account's keys for a portal session, the presented key
	// otherwise. A secret that resolves to no stored key has no bucket to read.
	var buckets []string
	if subject.EndUserID != "" {
		buckets = usage.ListAPIKeyIDsForEndUserForTenant(subject.TenantID, subject.EndUserID)
	} else if subject.APIKeyRow != nil {
		buckets = []string{strings.TrimSpace(subject.APIKeyRow.ID)}
	}
	apiData, hasUsage := aggregatePublicAPISnapshots(snapshot.APIs, buckets)
	found := hasUsage
	if subject.EndUserID != "" && subject.APIKey == "" {
		found = true
	} else if subject.APIKeyRow != nil {
		// A persisted disabled key must not be revived merely because the in-memory
		// snapshot still contains older usage.
		found = !subject.APIKeyRow.Disabled
	}
	maskedAPIKey := maskKey(subject.APIKey)
	if !found {
		c.JSON(http.StatusOK, gin.H{
			"usage": usage.StatisticsSnapshot{
				APIs: map[string]usage.APISnapshot{},
			},
			"api_key":        maskedAPIKey,
			"api_key_masked": maskedAPIKey,
			"found":          false,
		})
		return
	}
	if !hasUsage {
		apiData.Models = map[string]usage.ModelSnapshot{}
	}

	// Return only the matched API key's data. Use the masked key as the public
	// map key so the response never leaks the full credential.
	filteredSnapshot := usage.StatisticsSnapshot{
		APIs: map[string]usage.APISnapshot{
			maskedAPIKey: apiData,
		},
	}

	// SECURITY: Strip sensitive fields (provider API keys, auth indices)
	// from the public response to prevent credential leakage.
	filteredSnapshot.SanitizeForPublic()

	c.JSON(http.StatusOK, gin.H{
		"usage":          filteredSnapshot,
		"api_key":        maskedAPIKey,
		"api_key_masked": maskedAPIKey,
		"found":          true,
	})
}

func aggregatePublicAPISnapshots(items map[string]usage.APISnapshot, buckets []string) (usage.APISnapshot, bool) {
	result := usage.APISnapshot{Models: make(map[string]usage.ModelSnapshot)}
	found := false
	for _, bucket := range buckets {
		item, ok := items[bucket]
		if !ok {
			continue
		}
		found = true
		result.TotalRequests += item.TotalRequests
		result.TotalTokens += item.TotalTokens
		for model, stats := range item.Models {
			merged := result.Models[model]
			merged.TotalRequests += stats.TotalRequests
			merged.TotalTokens += stats.TotalTokens
			result.Models[model] = merged
		}
	}
	return result, found
}

// ImportUsageStatistics merges a previously exported usage snapshot into memory.
func (h *Handler) ImportUsageStatistics(c *gin.Context) {
	if !systemUsageOnly(c) {
		return
	}
	if h == nil || h.usageStats == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "usage statistics unavailable"})
		return
	}

	data, err := c.GetRawData()
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "failed to read request body"})
		return
	}

	var payload usageImportPayload
	if err := json.Unmarshal(data, &payload); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid json"})
		return
	}
	if payload.Version != 0 && payload.Version != 1 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "unsupported version"})
		return
	}

	result := h.usageStats.MergeSnapshot(payload.Usage)
	snapshot := h.usageStats.Snapshot()
	c.JSON(http.StatusOK, gin.H{
		"added":           result.Added,
		"skipped":         result.Skipped,
		"total_requests":  snapshot.TotalRequests,
		"failed_requests": snapshot.FailureCount,
	})
}
