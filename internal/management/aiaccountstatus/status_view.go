package aiaccountstatus

import (
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
		ErrorCode: row.ErrorCode, Quotas: quotaWindowViews(row.Quotas), ResetCreditCount: row.ResetCreditCount,
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

// quotaWindowViews copies the stored windows into views. The slice is never
// nil: the API always answers with a list.
func quotaWindowViews(dtos []usage.QuotaWindowDTO) []QuotaWindowView {
	views := make([]QuotaWindowView, len(dtos))
	for i := range dtos {
		views[i].QuotaWindowDTO = dtos[i]
	}
	return views
}

func timePointer(value time.Time) *time.Time {
	if value.IsZero() {
		return nil
	}
	value = value.UTC()
	return &value
}

func weeklyUsedFromQuotas(quotas []usage.QuotaWindowDTO, preferred ...string) *float64 {
	usedFrom := func(q *usage.QuotaWindowDTO) *float64 {
		used := 100 - *q.Percent
		if used < 0 {
			used = 0
		}
		if used > 100 {
			used = 100
		}
		return &used
	}

	if len(preferred) > 0 {
		pref := make(map[string]struct{}, len(preferred))
		for _, k := range preferred {
			pref[k] = struct{}{}
		}
		for i := range quotas {
			q := &quotas[i]
			if q.Percent == nil {
				continue
			}
			if _, ok := pref[q.QuotaKey]; !ok {
				continue
			}
			return usedFrom(q)
		}
		return nil
	}

	// Providers with no fixed weekly key — antigravity names its buckets after
	// whatever the upstream calls them — are matched by window width instead.
	// Falling through to "whichever window came first" reported a 5h window as
	// the weekly figure, so the card showed a number for a cycle it never
	// measured. The narrowest window at or above a week is the weekly one; a
	// monthly window sorts after it rather than displacing it.
	var weekly *usage.QuotaWindowDTO
	for i := range quotas {
		q := &quotas[i]
		if q.Percent == nil || q.WindowSeconds < usage.WeeklyQuotaWindowSeconds {
			continue
		}
		if weekly == nil || q.WindowSeconds < weekly.WindowSeconds {
			weekly = q
		}
	}
	if weekly != nil {
		return usedFrom(weekly)
	}

	// No window is wide enough to be a weekly cycle. Providers that report a
	// single unlabelled window still need a figure, so keep the original
	// first-window behaviour as the last resort.
	for i := range quotas {
		q := &quotas[i]
		if q.Percent == nil {
			continue
		}
		return usedFrom(q)
	}
	return nil
}

// primaryWeeklyKeys delegates to the usage package so the card list, the detail
// trend and the projection that writes the buckets all anchor to one cycle. Three
// private copies of this table is how they drifted apart in the first place.
func primaryWeeklyKeys(provider string) []string {
	return usage.PrimaryWeeklyQuotaKeys(provider)
}
