package management

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	apikeysettings "github.com/router-for-me/CLIProxyAPI/v8/internal/management/settings/apikey"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/quota"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/usage"
	log "github.com/sirupsen/logrus"
)

type usageSummaryResponse struct {
	Found       bool             `json:"found"`
	Range       string           `json:"range"`
	Stats       usageStatsBody   `json:"stats"`
	Limits      *usageLimitsBody `json:"limits,omitempty"`
	QuotaScopes []quotaScopeBody `json:"quota-scopes,omitempty"`
}

type usageStatsBody struct {
	TotalCalls int64   `json:"total_calls"`
	QuotaCost  float64 `json:"quota_cost"`
}

type quotaScopeBody struct {
	Scope                string                 `json:"scope"`
	PeriodSpending       []quota.PeriodSpending `json:"period-spending"`
	DailySpendingUsed    float64                `json:"daily-spending-used"`
	LifetimeSpendingUsed float64                `json:"lifetime-spending-used"`
}

// usageLimitsBody exposes only limits that are configured (>0) plus live usage.
type usageLimitsBody struct {
	DailyLimit         *int     `json:"daily-limit,omitempty"`
	DailyUsed          *int64   `json:"daily-used,omitempty"`
	TotalQuota         *int     `json:"total-quota,omitempty"`
	TotalUsed          *int64   `json:"total-used,omitempty"`
	SpendingLimit      *float64 `json:"spending-limit,omitempty"`
	SpendingUsed       *float64 `json:"spending-used,omitempty"`
	DailySpendingLimit *float64 `json:"daily-spending-limit,omitempty"`
	DailySpendingUsed  *float64 `json:"daily-spending-used,omitempty"`
}

// GetPublicUsageSummary returns today's call count and quota cost for an API key.
// This is a lightweight endpoint designed for CC Switch Provider card polling.
// `found` reflects API Key existence (not disabled), not whether it was used today.
// When the key has daily/total/spending limits, `limits` includes only those fields.
func (h *Handler) GetPublicUsageSummary(c *gin.Context) {
	req, status, message := readPublicLookupRequest(c)
	if message != "" {
		c.JSON(status, gin.H{"error": message})
		return
	}
	subject, ok := h.resolvePublicUsageSubject(c, req.APIKey)
	if !ok {
		return
	}

	stats, err := usage.QueryStats(subject.logQueryParams(1))
	if err != nil {
		log.Warnf("management usage logs: public usage summary query failed: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to query usage summary"})
		return
	}

	row := subject.APIKeyRow
	if row == nil && subject.APIKey != "" {
		row = apikeysettings.NewService(nil, apikeysettings.WithTenantID(subject.TenantID)).GetRow(subject.APIKey)
	}
	found := subject.EndUserID != "" && subject.APIKey == ""
	if subject.APIKey != "" {
		found = row != nil && !row.Disabled
	}
	limits := buildPublicUsageLimits(subject.APIKey, row)
	if subject.EndUserID != "" {
		limits = buildEndUserUsageLimits(subject.EndUserID)
	}
	quotaScopes, err := buildPublicQuotaScopes(subject, row)
	if err != nil {
		log.Warnf("management usage summary quota scopes failed: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to query quota usage"})
		return
	}

	resp := usageSummaryResponse{
		Found: found,
		Range: "today",
		Stats: usageStatsBody{
			TotalCalls: stats.Total,
			QuotaCost:  stats.TotalCost,
		},
		Limits:      limits,
		QuotaScopes: quotaScopes,
	}
	c.JSON(http.StatusOK, resp)
}

func buildEndUserUsageLimits(endUserID string) *usageLimitsBody {
	quota := usage.GetEndUserQuota(endUserID)
	if quota == nil {
		return nil
	}
	effective := usage.EffectiveEndUserQuota(*quota)
	out := &usageLimitsBody{}
	has := false
	if effective.DailyLimit > 0 {
		has = true
		limit := effective.DailyLimit
		out.DailyLimit = &limit
		if n, err := usage.CountTodayByEndUser(endUserID); err == nil {
			out.DailyUsed = &n
		}
	}
	if effective.TotalQuota > 0 {
		has = true
		limit := effective.TotalQuota
		out.TotalQuota = &limit
		if n, err := usage.CountTotalByEndUser(endUserID); err == nil {
			out.TotalUsed = &n
		}
	}
	if effective.SpendingLimit > 0 {
		has = true
		limit := effective.SpendingLimit
		out.SpendingLimit = &limit
		if n, err := usage.QueryTotalCostByEndUser(endUserID); err == nil {
			out.SpendingUsed = &n
		}
	}
	if effective.DailySpendingLimit > 0 {
		has = true
		limit := effective.DailySpendingLimit
		out.DailySpendingLimit = &limit
		if n, err := usage.QueryTodayCostByEndUser(endUserID); err == nil {
			out.DailySpendingUsed = &n
		}
	}
	if !has {
		return nil
	}
	return out
}

func buildPublicUsageLimits(apiKey string, row *usage.APIKeyRow) *usageLimitsBody {
	if row == nil || row.Disabled {
		return nil
	}
	tenantID := strings.TrimSpace(row.TenantID)
	if tenantID == "" {
		tenantID = usage.ResolveAPIKeyTenant(apiKey)
	}
	effective := usage.EffectiveAPIKeyRowForTenant(tenantID, *row)
	out := &usageLimitsBody{}
	has := false
	if effective.DailyLimit > 0 {
		has = true
		limit := effective.DailyLimit
		out.DailyLimit = &limit
		if n, err := usage.CountTodayByKey(apiKey); err == nil {
			out.DailyUsed = &n
		}
	}
	if effective.TotalQuota > 0 {
		has = true
		limit := effective.TotalQuota
		out.TotalQuota = &limit
		if n, err := usage.CountTotalByKey(apiKey); err == nil {
			out.TotalUsed = &n
		}
	}
	if effective.SpendingLimit > 0 {
		has = true
		limit := effective.SpendingLimit
		out.SpendingLimit = &limit
		if n, err := usage.QueryTotalCostByKey(apiKey); err == nil {
			out.SpendingUsed = &n
		}
	}
	if effective.DailySpendingLimit > 0 {
		has = true
		limit := effective.DailySpendingLimit
		out.DailySpendingLimit = &limit
		if n, err := usage.QueryTodayCostByKey(apiKey); err == nil {
			out.DailySpendingUsed = &n
		}
	}
	if !has {
		return nil
	}
	return out
}

func buildPublicQuotaScopes(subject publicUsageSubject, row *usage.APIKeyRow) ([]quotaScopeBody, error) {
	out := make([]quotaScopeBody, 0, 2)
	if subject.EndUserID != "" {
		accountQuota := usage.GetEndUserQuota(subject.EndUserID)
		if accountQuota != nil {
			effective := usage.EffectiveEndUserQuota(*accountQuota)
			used, err := usage.QueryPeriodSpendingByEndUserForTenant(subject.TenantID, subject.EndUserID)
			if err != nil {
				return nil, err
			}
			out = append(out, quotaScopeBody{Scope: "account", PeriodSpending: quota.BuildPeriodSpending(effective.PeriodSpendingLimits, used), DailySpendingUsed: used.Day, LifetimeSpendingUsed: used.Lifetime})
		}
		// Portal token requests represent the account only. Raw owned keys include both scopes.
		if subject.APIKey == "" {
			return out, nil
		}
	}
	if row != nil && !row.Disabled {
		keyLimits := row.PeriodSpendingLimits
		keyLimits.Day = row.DailySpendingLimit
		used, err := usage.QueryPeriodSpendingByAPIKeyIDForTenant(subject.TenantID, row.ID)
		if err != nil {
			return nil, err
		}
		out = append(out, quotaScopeBody{Scope: "key", PeriodSpending: quota.BuildPeriodSpending(keyLimits, used), DailySpendingUsed: used.Day, LifetimeSpendingUsed: used.Lifetime})
	}
	return out, nil
}
