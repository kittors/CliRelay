package usage

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/quota"
)

var ErrQuotaUsageUnavailable = errors.New("quota usage unavailable")

type periodSubject string

const (
	periodSubjectAPIKey  periodSubject = "api_key_id"
	periodSubjectEndUser periodSubject = "end_user_id"
)

// PeriodWindowKeys 只承载与 subject 无关的日历窗口。5h 窗口的起点是
// per-subject 的消费锚点（见 period_window_anchor.go），因此这里只保留上界。
type PeriodWindowKeys struct {
	FiveHourTo string
	Day        string
	WeekFrom   string
	MonthFrom  string
	DayTo      string
}

func PeriodWindowKeysAt(now time.Time, loc *time.Location) PeriodWindowKeys {
	if loc == nil {
		loc = time.Local
	}
	localNow := now.In(loc)
	localMidnight := time.Date(localNow.Year(), localNow.Month(), localNow.Day(), 0, 0, 0, 0, loc)
	weekdayOffset := (int(localMidnight.Weekday()) + 6) % 7
	weekStart := localMidnight.AddDate(0, 0, -weekdayOffset)
	monthStart := time.Date(localNow.Year(), localNow.Month(), 1, 0, 0, 0, 0, loc)
	return PeriodWindowKeys{
		// 上界取下一分钟，保证当前这一分钟的消费桶被计入。
		FiveHourTo: quotaMinuteKey(now.Add(time.Minute)),
		Day:        localDayKeyAtLocation(now, loc),
		WeekFrom:   localDayKeyAtLocation(weekStart, loc),
		MonthFrom:  localDayKeyAtLocation(monthStart, loc),
		DayTo:      localDayKeyAtLocation(localMidnight.AddDate(0, 0, 1), loc),
	}
}

// subjectPeriodWindows 把日历窗口与 per-subject 的 5h 锚点合并成一份窗口视图，
// 让用量查询和重置基线校验共用同一套窗口标识，避免两处各算一次导致漂移。
type subjectPeriodWindows struct {
	keys            PeriodWindowKeys
	fiveHourAnchors map[string]string
}

// windowKey 返回某个 subject 在指定周期下的窗口标识；空串表示当前没有有效窗口
// （5h 从未开窗或已过期），此时该周期的重置基线一律失效。
func (w subjectPeriodWindows) windowKey(subjectID string, period quota.Period) string {
	switch period {
	case quota.PeriodFiveHour:
		return w.fiveHourAnchors[subjectID]
	case quota.PeriodDay:
		return w.keys.Day
	case quota.PeriodWeek:
		return w.keys.WeekFrom
	case quota.PeriodMonth:
		return w.keys.MonthFrom
	case quota.PeriodLifetime:
		// 累计消费没有窗口可滚动，基线在下次授予前一直有效。常量键让通用的
		// 「是否同一窗口」校验对该周期恒成立。
		return lifetimeWindowKey
	default:
		return ""
	}
}

func loadSubjectPeriodWindows(tenantID string, subject periodSubject, ids []string, now time.Time) (subjectPeriodWindows, error) {
	anchors, err := listActiveFiveHourAnchors(tenantID, subject, ids, now)
	if err != nil {
		return subjectPeriodWindows{}, err
	}
	return subjectPeriodWindows{keys: PeriodWindowKeysAt(now, getUsageLocation()), fiveHourAnchors: anchors}, nil
}

func QueryPeriodSpendingByAPIKeyIDForTenant(tenantID, apiKeyID string) (quota.PeriodSpendingUsage, error) {
	values, err := QueryPeriodSpendingByAPIKeyIDsForTenantAt(tenantID, []string{apiKeyID}, time.Now())
	if err != nil {
		return quota.PeriodSpendingUsage{}, err
	}
	return values[strings.TrimSpace(apiKeyID)], nil
}

func QueryPeriodSpendingByEndUserForTenant(tenantID, endUserID string) (quota.PeriodSpendingUsage, error) {
	values, err := QueryPeriodSpendingByEndUsersForTenantAt(tenantID, []string{endUserID}, time.Now())
	if err != nil {
		return quota.PeriodSpendingUsage{}, err
	}
	return values[strings.TrimSpace(endUserID)], nil
}

func QueryPeriodSpendingByAPIKeyIDsForTenantAt(tenantID string, apiKeyIDs []string, now time.Time) (map[string]quota.PeriodSpendingUsage, error) {
	return queryPeriodSpendingForSubjects(tenantID, periodSubjectAPIKey, apiKeyIDs, now)
}

func QueryPeriodSpendingByEndUsersForTenantAt(tenantID string, endUserIDs []string, now time.Time) (map[string]quota.PeriodSpendingUsage, error) {
	return queryPeriodSpendingForSubjects(tenantID, periodSubjectEndUser, endUserIDs, now)
}

func QueryPeriodSpendingByAPIKeyIDsForTenant(tenantID string, apiKeyIDs []string) (map[string]quota.PeriodSpendingUsage, error) {
	return QueryPeriodSpendingByAPIKeyIDsForTenantAt(tenantID, apiKeyIDs, time.Now())
}

func QueryPeriodSpendingByEndUsersForTenant(tenantID string, endUserIDs []string) (map[string]quota.PeriodSpendingUsage, error) {
	return QueryPeriodSpendingByEndUsersForTenantAt(tenantID, endUserIDs, time.Now())
}

// subjectChunkSize 限制单条 IN 列表的主体数量，避免撑爆驱动的参数上限。
const subjectChunkSize = 300

func queryRawPeriodSpendingForSubjects(tenantID string, subject periodSubject, ids []string, now time.Time, windows subjectPeriodWindows) (map[string]quota.PeriodSpendingUsage, error) {
	ids = dedupeExactStrings(ids)
	out := make(map[string]quota.PeriodSpendingUsage, len(ids))
	for _, id := range ids {
		if id = strings.TrimSpace(id); id != "" {
			out[id] = quota.PeriodSpendingUsage{}
		}
	}
	if len(out) == 0 {
		return out, nil
	}
	if len(out) > subjectChunkSize {
		cleanIDs := make([]string, 0, len(out))
		for id := range out {
			cleanIDs = append(cleanIDs, id)
		}
		combined := make(map[string]quota.PeriodSpendingUsage, len(cleanIDs))
		for start := 0; start < len(cleanIDs); start += subjectChunkSize {
			end := start + subjectChunkSize
			if end > len(cleanIDs) {
				end = len(cleanIDs)
			}
			part, err := queryRawPeriodSpendingForSubjects(tenantID, subject, cleanIDs[start:end], now, windows)
			if err != nil {
				return nil, err
			}
			for id, used := range part {
				combined[id] = used
			}
		}
		return combined, nil
	}
	ids = ids[:0]
	for id := range out {
		ids = append(ids, id)
	}
	queries := []struct {
		kind string
		from string
		to   string
		set  func(*quota.PeriodSpendingUsage, float64)
	}{
		{rollupBucketDay, windows.keys.Day, windows.keys.DayTo, func(v *quota.PeriodSpendingUsage, n float64) { v.Day = n }},
		{rollupBucketDay, windows.keys.WeekFrom, windows.keys.DayTo, func(v *quota.PeriodSpendingUsage, n float64) { v.Week = n }},
		{rollupBucketDay, windows.keys.MonthFrom, windows.keys.DayTo, func(v *quota.PeriodSpendingUsage, n float64) { v.Month = n }},
		{rollupBucketLifetime, "", "", func(v *quota.PeriodSpendingUsage, n float64) { v.Lifetime = n }},
	}
	for _, query := range queries {
		values, err := queryGroupedPeriodCost(tenantID, subject, ids, query.kind, query.from, query.to)
		if err != nil {
			return nil, err
		}
		for id, value := range values {
			current := out[id]
			query.set(&current, value)
			out[id] = current
		}
	}
	// 5h 每个 subject 的窗口起点不同，无法并进上面的统一区间查询。
	fiveHour, err := queryFiveHourCostByAnchors(tenantID, subject, ids, windows)
	if err != nil {
		return nil, err
	}
	for id, current := range out {
		if anchor, ok := windows.fiveHourAnchors[id]; ok {
			if windowStart, parsed := parseQuotaMinuteKey(anchor); parsed {
				current.FiveHourWindowStart = windowStart
			}
		}
		current.FiveHour = fiveHour[id]
		out[id] = current
	}

	return out, nil
}

func queryPeriodSpendingForSubjects(tenantID string, subject periodSubject, ids []string, now time.Time) (map[string]quota.PeriodSpendingUsage, error) {
	windows, err := loadSubjectPeriodWindows(tenantID, subject, dedupeExactStrings(ids), now)
	if err != nil {
		return nil, err
	}
	out, err := queryRawPeriodSpendingForSubjects(tenantID, subject, ids, now, windows)
	if err != nil || len(out) == 0 {
		return out, err
	}
	cleanIDs := make([]string, 0, len(out))
	for id := range out {
		cleanIDs = append(cleanIDs, id)
	}
	baselines, err := listEffectivePeriodBaselines(tenantID, subject, cleanIDs, windows)
	if err != nil {
		return nil, fmt.Errorf("%w: period baselines: %v", ErrQuotaUsageUnavailable, err)
	}
	for id, periodBaselines := range baselines {
		current := out[id]
		for period, baseline := range periodBaselines {
			applyPeriodBaseline(&current, period, baseline)
		}
		out[id] = current
	}
	return out, nil
}

func listEffectivePeriodBaselines(tenantID string, subject periodSubject, ids []string, windows subjectPeriodWindows) (map[string]map[quota.Period]float64, error) {
	const baselineChunkSize = 300
	if len(ids) > baselineChunkSize {
		combined := make(map[string]map[quota.Period]float64)
		for start := 0; start < len(ids); start += baselineChunkSize {
			end := start + baselineChunkSize
			if end > len(ids) {
				end = len(ids)
			}
			part, err := listEffectivePeriodBaselines(tenantID, subject, ids[start:end], windows)
			if err != nil {
				return nil, err
			}
			for id, values := range part {
				combined[id] = values
			}
		}
		return combined, nil
	}
	out, err := listPeriodSpendingResetBaselines(tenantID, subject, ids, windows)
	if err != nil {
		return nil, err
	}
	var legacy map[string]float64
	if subject == periodSubjectAPIKey {
		legacy, err = listDailySpendingResetBaselinesAt(tenantID, ids, windows.keys.Day)
	} else {
		legacy, err = listEndUserDailySpendingResetBaselines(tenantID, ids, windows.keys.Day)
	}
	if err != nil {
		return nil, err
	}
	for id, baseline := range legacy {
		if out[id] == nil {
			out[id] = make(map[quota.Period]float64)
		}
		if _, exists := out[id][quota.PeriodDay]; !exists {
			out[id][quota.PeriodDay] = baseline
		}
	}
	return out, nil
}

func queryGroupedPeriodCost(tenantID string, subject periodSubject, ids []string, kind, from, to string) (map[string]float64, error) {
	db := getReadDB()
	if db == nil {
		return nil, ErrQuotaUsageUnavailable
	}
	column := string(subject)
	if subject != periodSubjectAPIKey && subject != periodSubjectEndUser {
		return nil, fmt.Errorf("%w: invalid subject", ErrQuotaUsageUnavailable)
	}
	var b strings.Builder
	b.WriteString(`SELECT ` + column + `, COALESCE(SUM(cost_total), 0) FROM usage_rollup_buckets WHERE tenant_id = ? AND bucket_kind = ? AND ` + column + ` IN (` + placeholders(len(ids)) + `)`)
	args := make([]any, 0, len(ids)+4)
	args = append(args, normalizeTenantID(tenantID), kind)
	for _, id := range ids {
		args = append(args, id)
	}
	if from != "" {
		b.WriteString(` AND bucket_start >= ?`)
		args = append(args, from)
	}
	if to != "" {
		b.WriteString(` AND bucket_start < ?`)
		args = append(args, to)
	}
	b.WriteString(` GROUP BY ` + column)
	rows, err := db.Query(b.String(), args...)
	if err != nil {
		return nil, fmt.Errorf("%w: query %s %s: %v", ErrQuotaUsageUnavailable, subject, kind, err)
	}
	defer rows.Close()
	out := make(map[string]float64, len(ids))
	for rows.Next() {
		var id string
		var value float64
		if err := rows.Scan(&id, &value); err != nil {
			return nil, fmt.Errorf("%w: scan %s %s: %v", ErrQuotaUsageUnavailable, subject, kind, err)
		}
		out[id] = value
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("%w: rows %s %s: %v", ErrQuotaUsageUnavailable, subject, kind, err)
	}
	return out, nil
}

// queryFiveHourCostByAnchors 按每个 subject 各自的窗口起点求和。起点不同导致无法
// 共用一个区间条件，因此把 (subject, anchor) 展开成 OR 组合塞进同一条查询：既保持
// 单次往返，也让 (tenant_id, bucket_kind, <subject>, bucket_start) 索引仍然可用。
// 没有活跃窗口的 subject 不参与查询，其用量按 0 处理。
func queryFiveHourCostByAnchors(tenantID string, subject periodSubject, ids []string, windows subjectPeriodWindows) (map[string]float64, error) {
	out := make(map[string]float64, len(ids))
	pairs := make([][2]string, 0, len(ids))
	for _, id := range ids {
		if anchor := windows.fiveHourAnchors[id]; anchor != "" {
			pairs = append(pairs, [2]string{id, anchor})
		}
	}
	if len(pairs) == 0 {
		return out, nil
	}
	column := string(subject)
	if subject != periodSubjectAPIKey && subject != periodSubjectEndUser {
		return nil, fmt.Errorf("%w: invalid subject", ErrQuotaUsageUnavailable)
	}
	db := getReadDB()
	if db == nil {
		return nil, ErrQuotaUsageUnavailable
	}
	var b strings.Builder
	b.WriteString(`SELECT ` + column + `, COALESCE(SUM(cost_total), 0) FROM usage_rollup_buckets WHERE tenant_id = ? AND bucket_kind = ? AND bucket_start < ? AND (`)
	args := make([]any, 0, len(pairs)*2+3)
	args = append(args, normalizeTenantID(tenantID), rollupBucketQuotaMinuteUTC, windows.keys.FiveHourTo)
	for i, pair := range pairs {
		if i > 0 {
			b.WriteString(` OR `)
		}
		b.WriteString(`(` + column + ` = ? AND bucket_start >= ?)`)
		args = append(args, pair[0], pair[1])
	}
	b.WriteString(`) GROUP BY ` + column)
	rows, err := db.Query(b.String(), args...)
	if err != nil {
		return nil, fmt.Errorf("%w: query %s 5h: %v", ErrQuotaUsageUnavailable, subject, err)
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var value float64
		if err := rows.Scan(&id, &value); err != nil {
			return nil, fmt.Errorf("%w: scan %s 5h: %v", ErrQuotaUsageUnavailable, subject, err)
		}
		out[id] = value
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("%w: rows %s 5h: %v", ErrQuotaUsageUnavailable, subject, err)
	}
	return out, nil
}

func listEndUserDailySpendingResetBaselines(tenantID string, ids []string, dayKey string) (map[string]float64, error) {
	db := getReadDB()
	if db == nil {
		return nil, ErrQuotaUsageUnavailable
	}
	query := `SELECT end_user_id, cost_baseline FROM end_user_daily_spending_resets WHERE tenant_id = ? AND day_key = ? AND end_user_id IN (` + placeholders(len(ids)) + `)`
	args := make([]any, 0, len(ids)+2)
	args = append(args, normalizeTenantID(tenantID), dayKey)
	for _, id := range ids {
		args = append(args, id)
	}
	rows, err := db.Query(query, args...)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return map[string]float64{}, nil
		}
		return nil, err
	}
	defer rows.Close()
	out := make(map[string]float64, len(ids))
	for rows.Next() {
		var id string
		var baseline float64
		if err := rows.Scan(&id, &baseline); err != nil {
			return nil, err
		}
		out[id] = baseline
	}
	return out, rows.Err()
}
