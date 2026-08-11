package usage

import (
	"database/sql"
	"fmt"
	"strings"
	"sync"
	"time"
)

const aiAccountSubjectDayRetention = 400 * 24 * time.Hour
const aiAccountSubjectWeeklyWindowSeconds = int64(7 * 24 * time.Hour / time.Second)

// Keep every (subject, quota key) cycle so additional weekly windows cannot
// replace the provider's primary card cycle.
var sharedCycleCache = struct {
	sync.RWMutex
	bySubjectQuota map[string]map[string]AIAccountSubjectQuotaCycle
}{bySubjectQuota: make(map[string]map[string]AIAccountSubjectQuotaCycle)}

func resetAIAccountSubjectCycleCache() {
	sharedCycleCache.Lock()
	sharedCycleCache.bySubjectQuota = make(map[string]map[string]AIAccountSubjectQuotaCycle)
	sharedCycleCache.Unlock()
}

func setAIAccountSubjectActiveCycle(cycle AIAccountSubjectQuotaCycle) {
	subjectID := strings.TrimSpace(cycle.AuthSubjectID)
	quotaKey := strings.TrimSpace(cycle.QuotaKey)
	if subjectID == "" || quotaKey == "" || cycle.CycleStartAt.IsZero() || cycle.ResetAt.IsZero() || cycle.WindowSeconds <= 0 {
		return
	}
	sharedCycleCache.Lock()
	byQuota := sharedCycleCache.bySubjectQuota[subjectID]
	if byQuota == nil {
		byQuota = make(map[string]AIAccountSubjectQuotaCycle)
		sharedCycleCache.bySubjectQuota[subjectID] = byQuota
	}
	byQuota[quotaKey] = cycle
	sharedCycleCache.Unlock()
}

func primaryAIAccountSubjectWeeklyQuotaKey(provider string) string {
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case "anthropic", "claude":
		return "seven_day"
	case "codex", "kimi":
		return "code_week"
	case "xai", "grok":
		return "weekly_limit"
	default:
		return ""
	}
}

// PrimaryWeeklyQuotaKeys is the single source of truth for "which quota window
// is this provider's card cycle". The card list and the detail trend must agree
// on it: when they disagree they anchor their totals to different cycles and
// report different numbers for the same account.
func PrimaryWeeklyQuotaKeys(provider string) []string {
	key := primaryAIAccountSubjectWeeklyQuotaKey(provider)
	if key == "" {
		return nil
	}
	return []string{key}
}

// aiAccountSubjectCycleDriftTolerance bounds how far a re-probed cycle start may
// move and still count as the same cycle.
//
// Upstreams report the window as a remaining-seconds countdown, so reset_at (and
// with it cycle_start = reset_at - window) lands apart on every probe. Treating
// those as distinct cycles is what split one weekly period into several usage
// buckets, leaving each reader to see whichever fragment matched the timestamp it
// happened to read.
//
// One percent of the window: production showed drift is not always seconds — a
// single odd probe moved a weekly start by 20 minutes and stranded its own
// fragment — while a genuine rollover moves the start by a full window, a hundred
// times the tolerance. Anything in between does not occur.
func aiAccountSubjectCycleDriftTolerance(windowSeconds int64) time.Duration {
	if windowSeconds <= 0 {
		return time.Minute
	}
	tolerance := time.Duration(windowSeconds) * time.Second / 100
	if tolerance < time.Minute {
		tolerance = time.Minute
	}
	if tolerance > 2*time.Hour {
		tolerance = 2 * time.Hour
	}
	return tolerance
}

func absDuration(value time.Duration) time.Duration {
	if value < 0 {
		return -value
	}
	return value
}

// sameAIAccountSubjectCycle reports whether two cycle anchors describe one cycle.
func sameAIAccountSubjectCycle(a, b time.Time, windowSeconds int64) bool {
	if a.IsZero() || b.IsZero() {
		return false
	}
	return absDuration(a.UTC().Sub(b.UTC())) <= aiAccountSubjectCycleDriftTolerance(windowSeconds)
}

func selectAIAccountSubjectWeeklyCycle(cycles []AIAccountSubjectQuotaCycle) (AIAccountSubjectQuotaCycle, bool) {
	var selected AIAccountSubjectQuotaCycle
	for _, cycle := range cycles {
		if cycle.WindowSeconds < aiAccountSubjectWeeklyWindowSeconds || cycle.CycleStartAt.IsZero() || cycle.ResetAt.IsZero() {
			continue
		}
		primaryKey := primaryAIAccountSubjectWeeklyQuotaKey(cycle.Provider)
		if primaryKey != "" && strings.TrimSpace(cycle.QuotaKey) != primaryKey {
			continue
		}
		if selected.AuthSubjectID == "" || cycle.LastVerifiedAt.After(selected.LastVerifiedAt) {
			selected = cycle
		}
	}
	return selected, selected.AuthSubjectID != ""
}

func cachedAIAccountSubjectWeeklyCycle(subjectID string) (AIAccountSubjectQuotaCycle, bool) {
	subjectID = strings.TrimSpace(subjectID)
	cycles := make([]AIAccountSubjectQuotaCycle, 0)
	sharedCycleCache.RLock()
	for _, cycle := range sharedCycleCache.bySubjectQuota[subjectID] {
		cycles = append(cycles, cycle)
	}
	sharedCycleCache.RUnlock()
	return selectAIAccountSubjectWeeklyCycle(cycles)
}

func loadAIAccountSubjectWeeklyCycleTx(tx *sql.Tx, subjectID string) (AIAccountSubjectQuotaCycle, bool, error) {
	rows, err := tx.Query(`
		SELECT auth_subject_id, provider, quota_key, cycle_start_at, reset_at, window_seconds, last_verified_at
		FROM ai_account_subject_quota_cycles
		WHERE auth_subject_id = ? AND window_seconds >= ?
		ORDER BY last_verified_at DESC, reset_at DESC
	`, subjectID, aiAccountSubjectWeeklyWindowSeconds)
	if err != nil {
		return AIAccountSubjectQuotaCycle{}, false, fmt.Errorf("usage: load shared subject quota cycle: %w", err)
	}
	defer rows.Close()
	cycles := make([]AIAccountSubjectQuotaCycle, 0)
	for rows.Next() {
		var cycle AIAccountSubjectQuotaCycle
		var start, reset, verified storedTime
		if err := rows.Scan(&cycle.AuthSubjectID, &cycle.Provider, &cycle.QuotaKey, &start, &reset, &cycle.WindowSeconds, &verified); err != nil {
			return AIAccountSubjectQuotaCycle{}, false, err
		}
		if start.Valid {
			cycle.CycleStartAt = start.Time
		}
		if reset.Valid {
			cycle.ResetAt = reset.Time
		}
		if verified.Valid {
			cycle.LastVerifiedAt = verified.Time
		}
		setAIAccountSubjectActiveCycle(cycle)
		cycles = append(cycles, cycle)
	}
	if err := rows.Err(); err != nil {
		return AIAccountSubjectQuotaCycle{}, false, err
	}
	cycle, ok := selectAIAccountSubjectWeeklyCycle(cycles)
	return cycle, ok, nil
}

func aiAccountSubjectCycleAt(tx *sql.Tx, subjectID string, at time.Time) (AIAccountSubjectQuotaCycle, bool, error) {
	cycle, ok := cachedAIAccountSubjectWeeklyCycle(subjectID)
	if !ok {
		var err error
		cycle, ok, err = loadAIAccountSubjectWeeklyCycleTx(tx, strings.TrimSpace(subjectID))
		if err != nil {
			return AIAccountSubjectQuotaCycle{}, false, err
		}
	}
	if !ok {
		return AIAccountSubjectQuotaCycle{}, false, nil
	}
	at = at.UTC()
	window := time.Duration(cycle.WindowSeconds) * time.Second
	for !at.Before(cycle.ResetAt) {
		cycle.CycleStartAt = cycle.ResetAt
		cycle.ResetAt = cycle.ResetAt.Add(window)
	}
	return cycle, !at.Before(cycle.CycleStartAt), nil
}

func formatAIAccountSubjectCycleBucketStart(value time.Time) string {
	return value.UTC().Format(time.RFC3339Nano)
}

// projectAIAccountSubjectUsageTx is the request-hot B-layer projection. It only
// uses the server-computed subject already captured by usageReporter; it never
// reads or upserts the low-frequency tenant binding table.
func projectAIAccountSubjectUsageTx(tx *sql.Tx, authSubjectID string, failed bool, cost float64, totalTokens int64, at time.Time) error {
	if tx == nil {
		return nil
	}
	authSubjectID = strings.TrimSpace(authSubjectID)
	if authSubjectID == "" {
		return nil
	}
	if at.IsZero() {
		at = time.Now()
	}
	loc := usageLoc
	if loc == nil {
		loc = time.Local
	}
	successInc, failureInc := int64(1), int64(0)
	if failed {
		successInc, failureInc = 0, 1
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	first := at.UTC().Format(time.RFC3339Nano)
	buckets := []struct{ kind, start string }{
		{kind: "day", start: at.In(loc).Format("2006-01-02")},
		{kind: "lifetime", start: rollupLifetimeStart},
	}
	cycle, cycleKnown, err := aiAccountSubjectCycleAt(tx, authSubjectID, at)
	if err != nil {
		return err
	}
	if cycleKnown {
		buckets = append(buckets, struct{ kind, start string }{kind: "cycle", start: formatAIAccountSubjectCycleBucketStart(cycle.CycleStartAt)})
	}
	const upsert = `
		INSERT INTO ai_account_subject_usage_buckets (
			auth_subject_id, bucket_kind, bucket_start, request_count,
			success_count, failure_count, cost_total, total_tokens, first_event_at, updated_at
		) VALUES (?, ?, ?, 1, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(auth_subject_id, bucket_kind, bucket_start) DO UPDATE SET
			request_count = ai_account_subject_usage_buckets.request_count + 1,
			success_count = ai_account_subject_usage_buckets.success_count + excluded.success_count,
			failure_count = ai_account_subject_usage_buckets.failure_count + excluded.failure_count,
			cost_total = ai_account_subject_usage_buckets.cost_total + excluded.cost_total,
			total_tokens = ai_account_subject_usage_buckets.total_tokens + excluded.total_tokens,
			updated_at = excluded.updated_at
	`
	// The fixed day -> lifetime -> cycle order is shared by every writer.
	for _, bucket := range buckets {
		if _, err := tx.Exec(upsert, authSubjectID, bucket.kind, bucket.start, successInc, failureInc, cost, totalTokens, first, now); err != nil {
			return fmt.Errorf("usage: project shared subject %s: %w", bucket.kind, err)
		}
	}
	return nil
}

// QueryAIAccountSubjectDailyUsage returns day buckets for one shared subject (no tenant filter).
func QueryAIAccountSubjectDailyUsage(authSubjectID string, days int) ([]DailyUsagePoint, error) {
	db := getReadDB()
	authSubjectID = strings.TrimSpace(authSubjectID)
	if db == nil || authSubjectID == "" {
		return []DailyUsagePoint{}, nil
	}
	if days < 1 {
		days = 7
	}
	loc := getUsageLocation()
	start := time.Now().In(loc).AddDate(0, 0, -(days - 1)).Format("2006-01-02")
	rows, err := db.Query(`
		SELECT bucket_start, request_count, cost_total
		FROM ai_account_subject_usage_buckets
		WHERE auth_subject_id = ? AND bucket_kind = 'day' AND bucket_start >= ?
		ORDER BY bucket_start ASC
	`, authSubjectID, start)
	if err != nil {
		return nil, fmt.Errorf("usage: shared subject daily usage: %w", err)
	}
	defer rows.Close()
	out := make([]DailyUsagePoint, 0, days)
	for rows.Next() {
		var point DailyUsagePoint
		if err := rows.Scan(&point.Date, &point.Requests, &point.Cost); err != nil {
			return nil, err
		}
		point.Date = strings.TrimSpace(point.Date)
		if point.Date == "" {
			continue
		}
		out = append(out, point)
	}
	return out, rows.Err()
}

// EmptyHourlyUsageBuckets returns a zero-filled hourly window in the usage timezone.
// Shared subject tables have day/cycle/lifetime only; detail charts use zeros for hours.
func EmptyHourlyUsageBuckets(hours int) []HourlyUsagePoint {
	if hours < 1 {
		hours = 5
	}
	if hours > 24 {
		hours = 24
	}
	loc := getUsageLocation()
	now := time.Now().In(loc).Truncate(time.Hour)
	start := now.Add(-time.Duration(hours-1) * time.Hour)
	out := make([]HourlyUsagePoint, 0, hours)
	for i := 0; i < hours; i++ {
		out = append(out, HourlyUsagePoint{
			Hour: start.Add(time.Duration(i) * time.Hour).Format("2006-01-02 15:00"),
		})
	}
	return out
}

func QueryAIAccountSubjectUsageSummaries(subjectIDs []string, cycleStartBySubject map[string]time.Time) (map[string]AuthSubjectUsageSummary, error) {
	db := getReadDB()
	ids := dedupeExactStrings(subjectIDs)
	out := make(map[string]AuthSubjectUsageSummary, len(ids))
	for _, id := range ids {
		out[id] = AuthSubjectUsageSummary{AuthSubjectID: id}
	}
	if db == nil || len(ids) == 0 {
		return out, nil
	}

	loc := getUsageLocation()
	now := time.Now().In(loc)
	start7 := now.AddDate(0, 0, -6).Format("2006-01-02")
	start30 := now.AddDate(0, 0, -29).Format("2006-01-02")
	args := make([]any, 0, len(ids)+1)
	args = append(args, start30)
	for _, id := range ids {
		args = append(args, id)
	}
	rows, err := db.Query(`
		SELECT auth_subject_id,
			SUM(CASE WHEN bucket_start >= ? THEN request_count ELSE 0 END),
			SUM(CASE WHEN bucket_start >= ? THEN cost_total ELSE 0 END),
			SUM(request_count), SUM(success_count), SUM(failure_count), MAX(updated_at)
		FROM ai_account_subject_usage_buckets
		WHERE bucket_kind = 'day' AND bucket_start >= ?
		  AND auth_subject_id IN (`+strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",")+`)
		GROUP BY auth_subject_id
	`, append([]any{start7, start7}, args...)...)
	if err != nil {
		return nil, fmt.Errorf("usage: query shared subject day summaries: %w", err)
	}
	for rows.Next() {
		var id string
		var r7 int64
		var c7 float64
		var r30, s30, f30 int64
		var updated sql.NullString
		if err := rows.Scan(&id, &r7, &c7, &r30, &s30, &f30, &updated); err != nil {
			rows.Close()
			return nil, err
		}
		s := out[id]
		s.RequestTotal7d, s.CostTotal7d = r7, c7
		s.RequestTotal30d, s.SuccessTotal30d, s.FailureTotal30d = r30, s30, f30
		if t, ok := parseStoredTimeString(updated.String); ok {
			s.UpdatedAt = t
		}
		out[id] = s
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}

	lifeArgs := make([]any, 0, len(ids))
	for _, id := range ids {
		lifeArgs = append(lifeArgs, id)
	}
	lifeRows, err := db.Query(`
		SELECT u.auth_subject_id, u.request_count, u.success_count, u.failure_count, u.cost_total,
			u.first_event_at, u.updated_at, s.usage_projected_since, s.usage_history_complete
		FROM ai_account_subject_usage_buckets u
		LEFT JOIN ai_account_subjects s ON s.auth_subject_id = u.auth_subject_id
		WHERE u.bucket_kind = 'lifetime' AND u.bucket_start = '1970-01-01'
		  AND u.auth_subject_id IN (`+strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",")+`)
	`, lifeArgs...)
	if err != nil {
		return nil, err
	}
	for lifeRows.Next() {
		var id string
		var requestTotal, successTotal, failureTotal int64
		var costTotal float64
		var first, updated, projected sql.NullString
		var complete sql.NullBool
		if err := lifeRows.Scan(&id, &requestTotal, &successTotal, &failureTotal, &costTotal, &first, &updated, &projected, &complete); err != nil {
			lifeRows.Close()
			return nil, err
		}
		s := out[id]
		s.AuthSubjectID = id
		s.RequestTotal = requestTotal
		s.SuccessTotal = successTotal
		s.FailureTotal = failureTotal
		s.CostTotal = costTotal
		denom := s.SuccessTotal + s.FailureTotal
		if denom > 0 {
			rate := float64(s.SuccessTotal) / float64(denom)
			s.SuccessRate = &rate
		}
		s.ProjectedSince = parseNullableTime(projected)
		if s.ProjectedSince == nil {
			s.ProjectedSince = parseNullableTime(first)
		}
		s.HistoryComplete = complete.Valid && complete.Bool
		if t, ok := parseStoredTimeString(updated.String); ok && t.After(s.UpdatedAt) {
			s.UpdatedAt = t
		}
		out[id] = s
	}
	if err := lifeRows.Close(); err != nil {
		return nil, err
	}

	cycleIDs := make([]string, 0, len(cycleStartBySubject))
	for id, start := range cycleStartBySubject {
		if _, ok := out[id]; !ok || start.IsZero() {
			continue
		}
		s := out[id]
		s.CycleKnown = true
		s.CycleStart = start.UTC().Format(time.RFC3339)
		out[id] = s
		cycleIDs = append(cycleIDs, id)
	}
	if len(cycleIDs) == 0 {
		return out, nil
	}

	cycleArgs := make([]any, 0, len(cycleIDs))
	for _, id := range cycleIDs {
		cycleArgs = append(cycleArgs, id)
	}
	// Match buckets by tolerance rather than by exact key. Buckets written before
	// cycle anchoring landed are still keyed to jittered starts, and an exact
	// match would report only the fragment that happens to carry the current
	// timestamp — the same period read as 19 requests here and 436 there.
	cycleRows, err := db.Query(`
		SELECT auth_subject_id, bucket_start, request_count, cost_total, total_tokens, updated_at
		FROM ai_account_subject_usage_buckets
		WHERE bucket_kind = 'cycle'
		  AND auth_subject_id IN (`+strings.TrimSuffix(strings.Repeat("?,", len(cycleIDs)), ",")+`)
	`, cycleArgs...)
	if err != nil {
		return nil, err
	}
	for cycleRows.Next() {
		var id, bucketStart string
		var req, totalTokens int64
		var cost float64
		var updated sql.NullString
		if err := cycleRows.Scan(&id, &bucketStart, &req, &cost, &totalTokens, &updated); err != nil {
			cycleRows.Close()
			return nil, err
		}
		expected, ok := cycleStartBySubject[id]
		if !ok {
			continue
		}
		bucketAt, parsed := parseStoredTimeString(bucketStart)
		if !parsed || !sameAIAccountSubjectCycle(bucketAt, expected, aiAccountSubjectWeeklyWindowSeconds) {
			continue
		}
		s := out[id]
		s.CycleRequestTotal += req
		s.CycleCostTotal += cost
		s.CycleTotalTokens += totalTokens
		if t, ok := parseStoredTimeString(updated.String); ok && t.After(s.UpdatedAt) {
			s.UpdatedAt = t
		}
		out[id] = s
	}
	if err := cycleRows.Close(); err != nil {
		return nil, err
	}
	return out, nil
}

func cleanupExpiredAIAccountSubjectUsageBuckets(db *sql.DB) (int64, error) {
	if db == nil {
		return 0, nil
	}
	cutoff := time.Now().Add(-aiAccountSubjectDayRetention).In(getUsageLocation()).Format("2006-01-02")
	res, err := db.Exec(`DELETE FROM ai_account_subject_usage_buckets WHERE bucket_kind = 'day' AND bucket_start < ?`, cutoff)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
