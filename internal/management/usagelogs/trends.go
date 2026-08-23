package usagelogs

import (
	"net/http"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/usage"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

func (s *Service) AuthExists(authIndex string) bool {
	return s.authByIndex(authIndex) != nil
}

func (s *Service) AuthFileGroupTrend(group string, days int) (AuthFileGroupTrendResponse, error) {
	authIndexes := s.authIndexesForProviderGroup(group)
	points, err := usage.QueryDailyCallsByAuthIndexesForTenant(s.tenantID, authIndexes, days)
	if err != nil {
		return AuthFileGroupTrendResponse{}, err
	}
	if points == nil {
		points = []usage.DailyCountPoint{}
	}
	quotaPoints, err := usage.QueryDailyQuotaByAuthIndexesForTenant(s.tenantID, authIndexes, "code_week", days)
	if err != nil {
		return AuthFileGroupTrendResponse{}, err
	}
	if quotaPoints == nil {
		quotaPoints = []usage.DailyQuotaPoint{}
	}
	return AuthFileGroupTrendResponse{Days: days, Group: group, Points: points, QuotaPoints: quotaPoints}, nil
}

func (s *Service) AuthFileTrend(authIndex string, days int, hours int) (int, any) {
	if strings.TrimSpace(authIndex) == "" {
		return http.StatusBadRequest, map[string]any{"error": "auth_index is required"}
	}
	auth := s.authByIndex(authIndex)
	if auth == nil {
		return http.StatusNotFound, map[string]any{"error": "auth not found"}
	}
	// Always compute tenant-private logs (legacy empty-subject rows + same-tenant traffic).
	status, payload := s.authFileTrendFromTenantLogs(authIndex, auth, days, hours)
	if status != http.StatusOK {
		return status, payload
	}
	tenantTrend, ok := payload.(AuthFileTrendResponse)
	if !ok {
		return status, payload
	}
	identity := usage.ResolveAuthSubjectIdentity(auth)
	if identity == nil || !identity.ShareEligible || identity.ID == "" {
		return status, tenantTrend
	}
	// Overlay shared subject projection so other tenants see the same physical account.
	sharedStatus, sharedPayload := s.authFileTrendFromSharedSubject(authIndex, auth, identity.ID, days, hours)
	if sharedStatus != http.StatusOK {
		return status, tenantTrend
	}
	sharedTrend, ok := sharedPayload.(AuthFileTrendResponse)
	if !ok {
		return status, tenantTrend
	}
	return http.StatusOK, mergeAuthFileTrendShared(tenantTrend, sharedTrend)
}

// mergeAuthFileTrendShared prefers shared subject totals when they exceed tenant-private
// logs (cross-tenant traffic), keeps richer tenant counts for same-tenant legacy aliases,
// and always prefers non-empty shared quota series.
func mergeAuthFileTrendShared(tenant, shared AuthFileTrendResponse) AuthFileTrendResponse {
	out := tenant
	if shared.CycleRequestTotal > tenant.CycleRequestTotal ||
		(shared.CycleRequestTotal == tenant.CycleRequestTotal && shared.CycleCostTotal > tenant.CycleCostTotal) ||
		(shared.CycleRequestTotal == tenant.CycleRequestTotal && shared.CycleCostTotal == tenant.CycleCostTotal && shared.CycleTotalTokens > tenant.CycleTotalTokens) ||
		(tenant.CycleRequestTotal == 0 && shared.RequestTotal > tenant.RequestTotal) {
		out.CycleRequestTotal = shared.CycleRequestTotal
		out.CycleCostTotal = shared.CycleCostTotal
		out.CycleTotalTokens = shared.CycleTotalTokens
		if shared.RequestTotal > tenant.RequestTotal {
			out.RequestTotal = shared.RequestTotal
		}
		if sumDailyRequests(shared.DailyUsage) > sumDailyRequests(tenant.DailyUsage) {
			out.DailyUsage = shared.DailyUsage
		}
	}
	if shared.CycleKnown && shared.CycleStart != "" {
		out.CycleKnown = true
		out.CycleStart = shared.CycleStart
	}
	if len(shared.QuotaSeries) > 0 {
		out.QuotaSeries = shared.QuotaSeries
		if shared.WeeklyQuotaUsed != nil {
			out.WeeklyQuotaUsed = shared.WeeklyQuotaUsed
		}
	}
	// Prefer richer shared hourly (all tenants); keep tenant-only when shared empty.
	if sumHourlyRequests(shared.HourlyUsage) > sumHourlyRequests(tenant.HourlyUsage) {
		out.HourlyUsage = shared.HourlyUsage
	}
	return out
}

func sumDailyRequests(points []usage.DailyUsagePoint) int64 {
	var total int64
	for _, p := range points {
		total += p.Requests
	}
	return total
}

func sumHourlyRequests(points []usage.HourlyUsagePoint) int64 {
	var total int64
	for _, p := range points {
		total += p.Requests
	}
	return total
}

func (s *Service) authFileTrendFromSharedSubject(authIndex string, auth *coreauth.Auth, subjectID string, days, hours int) (int, any) {
	preferredWeeklyQuotaKeys := primaryWeeklyQuotaKeysForProvider(auth.Provider)
	trendStart := time.Now().AddDate(0, 0, -7)
	trendEnd := time.Now().Add(time.Minute)

	dailyRaw, err := usage.QueryAIAccountSubjectDailyUsage(subjectID, days)
	if err != nil {
		return http.StatusInternalServerError, map[string]any{"error": err.Error()}
	}
	daily := fillDailyUsagePoints(dailyRaw, days)
	// Shared day buckets have no hour grain; aggregate recent hours by subject across tenants.
	hourly, err := usage.QueryHourlyUsageByAuthSubjectAcrossTenants(usage.AuthSubjectMatcher{SubjectID: subjectID}, hours)
	if err != nil {
		return http.StatusInternalServerError, map[string]any{"error": err.Error()}
	}
	if hourly == nil {
		hourly = usage.EmptyHourlyUsageBuckets(hours)
	}

	series, err := usage.QueryAIAccountSubjectQuotaSeries(subjectID, trendStart, trendEnd)
	if err != nil {
		return http.StatusInternalServerError, map[string]any{"error": err.Error()}
	}
	if series == nil {
		series = []usage.QuotaSnapshotSeries{}
	}
	weeklyQuotaUsed := latestWeeklyQuotaUsedPercent(series, preferredWeeklyQuotaKeys...)
	projectionQuotaUsed := latestProjectionQuotaUsedPercent(series, auth.Provider)

	cycleStarts, err := usage.QueryLatestAIAccountSubjectWeeklyCyclesBatch([]string{subjectID})
	if err != nil {
		return http.StatusInternalServerError, map[string]any{"error": err.Error()}
	}
	cycleStart := cycleStarts[subjectID]
	if cycleStart.IsZero() {
		if weeklyCycleStart, ok := latestWeeklyQuotaCycleStart(series, preferredWeeklyQuotaKeys...); ok {
			cycleStart = weeklyCycleStart
		}
	}

	summaries, err := usage.QueryAIAccountSubjectUsageSummaries([]string{subjectID}, map[string]time.Time{subjectID: cycleStart})
	if err != nil {
		return http.StatusInternalServerError, map[string]any{"error": err.Error()}
	}
	summary := summaries[subjectID]
	requestTotal := summary.RequestTotal7d
	if days >= 30 {
		requestTotal = summary.RequestTotal30d
	}
	cycleKnown := !cycleStart.IsZero() || summary.CycleKnown
	cycleRequestTotal := summary.CycleRequestTotal
	cycleCostTotal := summary.CycleCostTotal
	cycleTotalTokens := summary.CycleTotalTokens
	cycleStartStr := ""
	if !cycleStart.IsZero() {
		cycleStartStr = cycleStart.UTC().Format(time.RFC3339)
	} else if summary.CycleStart != "" {
		cycleStartStr = summary.CycleStart
	}

	return http.StatusOK, AuthFileTrendResponse{
		AuthIndex:                   authIndex,
		Days:                        days,
		Hours:                       hours,
		RequestTotal:                requestTotal,
		CycleRequestTotal:           cycleRequestTotal,
		CycleCostTotal:              cycleCostTotal,
		CycleTotalTokens:            cycleTotalTokens,
		WeeklyQuotaUsed:             weeklyQuotaUsed,
		ProjectionQuotaUsed:         projectionQuotaUsed,
		ProjectionQuotaAttributable: usage.ProjectionQuotaIsAttributable(auth.Provider),
		CycleKnown:                  cycleKnown,
		CycleStart:                  cycleStartStr,
		DailyUsage:                  daily,
		HourlyUsage:                 hourly,
		QuotaSeries:                 series,
	}
}

func (s *Service) authFileTrendFromTenantLogs(authIndex string, auth *coreauth.Auth, days, hours int) (int, any) {
	matcher := s.authSubjectMatcher(auth)
	preferredWeeklyQuotaKeys := primaryWeeklyQuotaKeysForProvider(auth.Provider)

	dailyRaw, err := usage.QueryDailyUsageByAuthSubjectForTenant(s.tenantID, matcher, days)
	if err != nil {
		return http.StatusInternalServerError, map[string]any{"error": err.Error()}
	}
	daily := fillDailyUsagePoints(dailyRaw, days)

	hourly, err := usage.QueryHourlyUsageByAuthSubjectForTenant(s.tenantID, matcher, hours)
	if err != nil {
		return http.StatusInternalServerError, map[string]any{"error": err.Error()}
	}
	if hourly == nil {
		hourly = []usage.HourlyUsagePoint{}
	}

	cutoff := usage.CutoffStartUTC(days)
	requestTotal, err := usage.QueryRequestCountByAuthSubjectSinceForTenant(s.tenantID, matcher, cutoff)
	if err != nil {
		return http.StatusInternalServerError, map[string]any{"error": err.Error()}
	}

	trendStart := time.Now().AddDate(0, 0, -7)
	trendEnd := time.Now().Add(time.Minute)
	series, err := usage.QueryQuotaSnapshotSeriesByAuthSubjectForTenant(s.tenantID, matcher, trendStart, trendEnd)
	if err != nil {
		return http.StatusInternalServerError, map[string]any{"error": err.Error()}
	}
	if series == nil {
		series = []usage.QuotaSnapshotSeries{}
	}
	weeklyQuotaUsed := latestWeeklyQuotaUsedPercent(series, preferredWeeklyQuotaKeys...)
	projectionQuotaUsed := latestProjectionQuotaUsedPercent(series, auth.Provider)

	var cycleStart time.Time
	if identity := usage.ResolveAuthSubjectIdentity(auth); identity != nil && identity.ID != "" {
		cycle, err := usage.QueryLatestWeeklyQuotaCycleByAuthSubjectForTenant(s.tenantID, identity.ID, preferredWeeklyQuotaKeys...)
		if err != nil {
			return http.StatusInternalServerError, map[string]any{"error": err.Error()}
		}
		if cycle != nil {
			cycleStart = cycle.CycleStartAt.UTC()
		}
	}
	if cycleStart.IsZero() {
		if weeklyCycleStart, ok := latestWeeklyQuotaCycleStart(series, preferredWeeklyQuotaKeys...); ok {
			cycleStart = weeklyCycleStart
		}
	}

	var cycleRequestTotal, cycleTotalTokens int64
	var cycleCostTotal float64
	cycleKnown := !cycleStart.IsZero()
	if cycleKnown {
		cycleRequestTotal, err = usage.QueryRequestCountByAuthSubjectSinceForTenant(s.tenantID, matcher, cycleStart)
		if err != nil {
			return http.StatusInternalServerError, map[string]any{"error": err.Error()}
		}
		cycleCostTotal, err = usage.QueryCostByAuthSubjectSinceForTenant(s.tenantID, matcher, cycleStart)
		if err != nil {
			return http.StatusInternalServerError, map[string]any{"error": err.Error()}
		}
		cycleTotalTokens, err = usage.QueryTotalTokensByAuthSubjectSinceForTenant(s.tenantID, matcher, cycleStart)
		if err != nil {
			return http.StatusInternalServerError, map[string]any{"error": err.Error()}
		}
	}

	cycleStartStr := ""
	if cycleKnown {
		cycleStartStr = cycleStart.UTC().Format(time.RFC3339)
	}
	return http.StatusOK, AuthFileTrendResponse{
		AuthIndex:                   authIndex,
		Days:                        days,
		Hours:                       hours,
		RequestTotal:                requestTotal,
		CycleRequestTotal:           cycleRequestTotal,
		CycleCostTotal:              cycleCostTotal,
		CycleTotalTokens:            cycleTotalTokens,
		WeeklyQuotaUsed:             weeklyQuotaUsed,
		ProjectionQuotaUsed:         projectionQuotaUsed,
		ProjectionQuotaAttributable: usage.ProjectionQuotaIsAttributable(auth.Provider),
		CycleKnown:                  cycleKnown,
		CycleStart:                  cycleStartStr,
		DailyUsage:                  daily,
		HourlyUsage:                 hourly,
		QuotaSeries:                 series,
	}
}

func fillDailyUsagePoints(points []usage.DailyUsagePoint, days int) []usage.DailyUsagePoint {
	if days < 1 {
		days = 7
	}
	byDate := make(map[string]usage.DailyUsagePoint, len(points))
	for _, point := range points {
		existing := byDate[point.Date]
		existing.Date = point.Date
		existing.Requests += point.Requests
		existing.Cost += point.Cost
		byDate[point.Date] = existing
	}
	start := usage.CutoffStartUTC(days)
	result := make([]usage.DailyUsagePoint, 0, days)
	for i := 0; i < days; i++ {
		date := usage.LocalDayKeyAt(start.AddDate(0, 0, i))
		point := byDate[date]
		point.Date = date
		result = append(result, point)
	}
	return result
}

func latestWeeklyQuotaUsedPercent(series []usage.QuotaSnapshotSeries, preferredQuotaKeys ...string) *float64 {
	latest := latestWeeklyQuotaPercentPoint(series, preferredQuotaKeys...)
	return quotaUsedPercentFromPoint(latest)
}

// latestProjectionQuotaUsedPercent picks the divisor for the weekly-budget
// projection: the consumption share produced by traffic this proxy forwards.
//
// Unlike latestWeeklyQuotaUsedPercent this does not fall back to "any weekly
// window". A wrong divisor here does not degrade the projection, it inverts its
// meaning — dividing Grok Build cost by pool-wide consumption reports a smaller
// budget the more the account uses Grok Chat. Returning nil lets the caller fall
// back explicitly, at the pool-wide number it already has.
func latestProjectionQuotaUsedPercent(series []usage.QuotaSnapshotSeries, provider string) *float64 {
	// Summed, not "latest wins": several attributable windows are several slices
	// of one pool, and the projection needs the whole attributable slice. Each
	// window contributes its own most recent reading.
	var total float64
	var found bool
	for i := range series {
		if series[i].WindowSeconds < usage.WeeklyQuotaWindowSeconds {
			continue
		}
		if !usage.MatchesProjectionQuotaKey(provider, series[i].QuotaKey) {
			continue
		}
		var latestPoint *usage.QuotaSnapshotSeriesPoint
		for j := range series[i].Points {
			point := &series[i].Points[j]
			if point.Percent == nil {
				continue
			}
			if latestPoint == nil || point.Timestamp.After(latestPoint.Timestamp) {
				latestPoint = point
			}
		}
		used := quotaUsedPercentFromPoint(latestPoint)
		if used == nil {
			continue
		}
		total += *used
		found = true
	}
	if !found {
		return nil
	}
	if total > 100 {
		total = 100
	}
	return &total
}

func quotaUsedPercentFromPoint(point *usage.QuotaSnapshotSeriesPoint) *float64 {
	if point == nil || point.Percent == nil {
		return nil
	}
	value := 100 - *point.Percent
	if value < 0 {
		value = 0
	}
	if value > 100 {
		value = 100
	}
	return &value
}

func latestWeeklyQuotaPercentPoint(series []usage.QuotaSnapshotSeries, preferredQuotaKeys ...string) *usage.QuotaSnapshotSeriesPoint {
	point := latestWeeklyQuotaPercentPointStrict(series, preferredQuotaKeys...)
	if point != nil {
		return point
	}
	return latestWeeklyQuotaPercentPointStrict(series)
}

func latestWeeklyQuotaPercentPointStrict(series []usage.QuotaSnapshotSeries, preferredQuotaKeys ...string) *usage.QuotaSnapshotSeriesPoint {
	preferred := make(map[string]struct{}, len(preferredQuotaKeys))
	for _, quotaKey := range preferredQuotaKeys {
		if trimmed := strings.TrimSpace(quotaKey); trimmed != "" {
			preferred[trimmed] = struct{}{}
		}
	}
	requiresPreferredKey := len(preferred) > 0
	var latestPoint *usage.QuotaSnapshotSeriesPoint
	for i := range series {
		if series[i].WindowSeconds < 604800 {
			continue
		}
		if requiresPreferredKey {
			if _, ok := preferred[strings.TrimSpace(series[i].QuotaKey)]; !ok {
				continue
			}
		}
		for j := range series[i].Points {
			point := &series[i].Points[j]
			if point.Percent == nil {
				continue
			}
			if latestPoint == nil || point.Timestamp.After(latestPoint.Timestamp) {
				latestPoint = point
			}
		}
	}
	return latestPoint
}

func latestWeeklyQuotaCycleStart(series []usage.QuotaSnapshotSeries, preferredQuotaKeys ...string) (time.Time, bool) {
	preferred := make(map[string]struct{}, len(preferredQuotaKeys))
	for _, quotaKey := range preferredQuotaKeys {
		if trimmed := strings.TrimSpace(quotaKey); trimmed != "" {
			preferred[trimmed] = struct{}{}
		}
	}
	requiresPreferredKey := len(preferred) > 0
	var latestPoint *usage.QuotaSnapshotSeriesPoint
	var latestWindow int64
	for i := range series {
		if series[i].WindowSeconds < 604800 {
			continue
		}
		if requiresPreferredKey {
			if _, ok := preferred[strings.TrimSpace(series[i].QuotaKey)]; !ok {
				continue
			}
		}
		windowSeconds := series[i].WindowSeconds
		for j := range series[i].Points {
			point := &series[i].Points[j]
			if point.ResetAt == nil || point.ResetAt.IsZero() {
				continue
			}
			if latestPoint == nil || point.Timestamp.After(latestPoint.Timestamp) {
				latestPoint = point
				latestWindow = windowSeconds
			}
		}
	}
	if latestPoint == nil || latestWindow <= 0 {
		return time.Time{}, false
	}
	return latestPoint.ResetAt.Add(-time.Duration(latestWindow) * time.Second).UTC(), true
}

// primaryWeeklyQuotaKeysForProvider delegates to the usage package so the detail
// trend anchors to the same cycle the card list and the usage projection use.
func primaryWeeklyQuotaKeysForProvider(provider string) []string {
	return usage.PrimaryWeeklyQuotaKeys(provider)
}

func (s *Service) authIndexesForProviderGroup(group string) []string {
	if s == nil || s.authManager == nil {
		return []string{}
	}
	auths := s.authManager.ListForTenant(s.tenantID)
	indexes := make([]string, 0, len(auths))
	for _, auth := range auths {
		if auth == nil {
			continue
		}
		provider := strings.ToLower(strings.TrimSpace(auth.Provider))
		if group != "all" && provider != group {
			continue
		}
		auth.EnsureIndex()
		if idx := strings.TrimSpace(auth.Index); idx != "" {
			indexes = append(indexes, idx)
		}
	}
	return indexes
}

func (s *Service) authByIndex(authIndex string) *coreauth.Auth {
	if s == nil || s.authManager == nil {
		return nil
	}
	authIndex = strings.TrimSpace(authIndex)
	if authIndex == "" {
		return nil
	}
	for _, auth := range s.authManager.ListForTenant(s.tenantID) {
		if auth == nil {
			continue
		}
		auth.EnsureIndex()
		if strings.TrimSpace(auth.Index) == authIndex {
			return auth
		}
	}
	return nil
}

func (s *Service) authSubjectMatcher(auth *coreauth.Auth) usage.AuthSubjectMatcher {
	if auth == nil {
		return usage.AuthSubjectMatcher{}
	}
	auths := []*coreauth.Auth{}
	if s != nil && s.authManager != nil {
		auths = s.authManager.ListForTenant(s.tenantID)
	}
	return usage.BuildAuthSubjectMatcher(auth, auths)
}
