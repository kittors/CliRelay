package aiaccountstatus

import (
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/usage"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

func statusViewFromSharedRecord(row usage.AIAccountSubjectStatusRecord, auth *coreauth.Auth, summary usage.AuthSubjectUsageSummary, subject usage.AIAccountSubjectRecord, bindingCount int) AccountStatusView {
	view := AccountStatusView{
		AuthSubjectID: row.AuthSubjectID, Provider: row.Provider, StatusScope: usage.AIAccountStatusScopeShared,
		SubjectScope: subject.SubjectScope, ShareEligible: subject.ShareEligible, SubjectSeedKind: subject.SeedKind,
		CurrentTenantBindingCount: bindingCount, RefreshState: row.LastProbeState, HealthStatus: row.HealthStatus,
		PlanType: row.PlanType, RestrictionSummary: row.RestrictionSummary, ErrorSummary: row.ErrorSummary,
		ErrorCode: row.ErrorCode, Quotas: row.Quotas, ResetCreditCount: row.ResetCreditCount,
		ResetCreditExpirations: row.ResetCreditExpirations, Usage: summary,
		SubscriptionStartedAt: row.SubscriptionStartedAt, SubscriptionExpiresAt: row.SubscriptionExpiresAt,
		SubscriptionSource: row.SubscriptionSource, UpstreamCheckedAt: row.UpstreamCheckedAt,
		QuotaObservedAt: row.QuotaObservedAt,
		ExpiresAt:       row.SubscriptionExpiresAt, Version: row.Version, UpdatedAt: timePointer(row.UpdatedAt),
	}
	if auth != nil {
		view.AuthIndex = auth.Index
		if view.Provider == "" {
			view.Provider = auth.Provider
		}
		if view.HealthStatus == "" {
			view.HealthStatus = string(auth.Status)
		}
	}
	if view.Quotas == nil {
		view.Quotas = []usage.QuotaWindowDTO{}
	}
	if view.Usage.AuthSubjectID == "" {
		view.Usage.AuthSubjectID = row.AuthSubjectID
	}
	if !summary.UpdatedAt.IsZero() {
		view.UsageUpdatedAt = timePointer(summary.UpdatedAt)
	}
	if view.Usage.WeeklyQuotaUsed == nil && auth != nil {
		view.Usage.WeeklyQuotaUsed = weeklyUsedFromQuotas(row.Quotas, primaryWeeklyKeys(auth.Provider)...)
	}
	return view
}

func timePointer(value time.Time) *time.Time {
	if value.IsZero() {
		return nil
	}
	value = value.UTC()
	return &value
}

func weeklyUsedFromQuotas(quotas []usage.QuotaWindowDTO, preferred ...string) *float64 {
	pref := make(map[string]struct{}, len(preferred))
	for _, k := range preferred {
		pref[k] = struct{}{}
	}
	for i := range quotas {
		q := &quotas[i]
		if q.Percent == nil {
			continue
		}
		if len(pref) > 0 {
			if _, ok := pref[q.QuotaKey]; !ok {
				continue
			}
		}
		used := 100 - *q.Percent
		if used < 0 {
			used = 0
		}
		if used > 100 {
			used = 100
		}
		return &used
	}
	return nil
}

func primaryWeeklyKeys(provider string) []string {
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case "anthropic", "claude":
		return []string{"seven_day"}
	case "codex", "kimi":
		return []string{"code_week"}
	case "xai", "grok":
		return []string{"weekly_limit"}
	default:
		return nil
	}
}
