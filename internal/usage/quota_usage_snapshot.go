package usage

import (
	"fmt"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/quota"
)

// QuotaUsageSnapshot 是账号或 Key 的用量快照，定义见 quota.UsageSnapshot。
//
// 窗口与既有美元周期额度完全同源：5h 用该主体的消费锚点，day/week/month 按项目
// 时区的日桶，lifetime 用累计桶；重置基线同样只在原窗口内有效。因此 Total.Cost
// 的四个周期与 QueryPeriodSpendingBy* 返回的值一致。
type QuotaUsageSnapshot = quota.UsageSnapshot

// QueryEndUserQuotaUsageSnapshotsAt 批量读取账号的用量快照。
func QueryEndUserQuotaUsageSnapshotsAt(tenantID string, endUserIDs []string, now time.Time) (map[string]QuotaUsageSnapshot, error) {
	return queryQuotaUsageSnapshots(tenantID, periodSubjectEndUser, endUserIDs, now)
}

// QueryAPIKeyQuotaUsageSnapshotsAt 批量读取 Key 的用量快照。
func QueryAPIKeyQuotaUsageSnapshotsAt(tenantID string, apiKeyIDs []string, now time.Time) (map[string]QuotaUsageSnapshot, error) {
	return queryQuotaUsageSnapshots(tenantID, periodSubjectAPIKey, apiKeyIDs, now)
}

// QueryEndUserQuotaUsageSnapshot 读取单个账号当前的用量快照。
func QueryEndUserQuotaUsageSnapshot(tenantID, endUserID string) (QuotaUsageSnapshot, error) {
	values, err := QueryEndUserQuotaUsageSnapshotsAt(tenantID, []string{endUserID}, time.Now())
	if err != nil {
		return QuotaUsageSnapshot{}, err
	}
	return values[strings.TrimSpace(endUserID)], nil
}

// QueryAPIKeyQuotaUsageSnapshot 读取单个 Key 当前的用量快照。
func QueryAPIKeyQuotaUsageSnapshot(tenantID, apiKeyID string) (QuotaUsageSnapshot, error) {
	values, err := QueryAPIKeyQuotaUsageSnapshotsAt(tenantID, []string{apiKeyID}, time.Now())
	if err != nil {
		return QuotaUsageSnapshot{}, err
	}
	return values[strings.TrimSpace(apiKeyID)], nil
}

func queryQuotaUsageSnapshots(tenantID string, subject periodSubject, ids []string, now time.Time) (map[string]QuotaUsageSnapshot, error) {
	clean := make([]string, 0, len(ids))
	for _, id := range dedupeExactStrings(ids) {
		if id = strings.TrimSpace(id); id != "" {
			clean = append(clean, id)
		}
	}
	out := make(map[string]QuotaUsageSnapshot, len(clean))
	if len(clean) == 0 {
		return out, nil
	}
	if len(clean) > subjectChunkSize {
		for start := 0; start < len(clean); start += subjectChunkSize {
			end := start + subjectChunkSize
			if end > len(clean) {
				end = len(clean)
			}
			part, err := queryQuotaUsageSnapshots(tenantID, subject, clean[start:end], now)
			if err != nil {
				return nil, err
			}
			for id, snapshot := range part {
				out[id] = snapshot
			}
		}
		return out, nil
	}
	windows, err := loadSubjectPeriodWindows(tenantID, subject, clean, now)
	if err != nil {
		return nil, err
	}
	raw, err := queryRawQuotaUsage(tenantID, subject, clean, windows)
	if err != nil {
		return nil, err
	}
	costBaselines, err := listEffectivePeriodBaselines(tenantID, subject, clean, windows)
	if err != nil {
		return nil, fmt.Errorf("%w: period baselines: %v", ErrQuotaUsageUnavailable, err)
	}
	usageBaselines, err := listPeriodUsageBaselines(tenantID, subject, clean, windows)
	if err != nil {
		return nil, fmt.Errorf("%w: usage baselines: %v", ErrQuotaUsageUnavailable, err)
	}
	for _, id := range clean {
		snapshot := raw[id]
		if snapshot.Models == nil {
			snapshot.Models = map[string]quota.Matrix{}
		}
		for period, baseline := range costBaselines[id] {
			snapshot.Total.Set(quota.MetricCost, period, nonNegative(snapshot.Total.Value(quota.MetricCost, period)-baseline))
		}
		for period, baseline := range usageBaselines[id] {
			baseline.applyTo(&snapshot, period)
		}
		if anchor, ok := windows.fiveHourAnchors[id]; ok {
			if windowStart, parsed := parseQuotaMinuteKey(anchor); parsed {
				snapshot.FiveHourWindowStart = windowStart
			}
		}
		out[id] = snapshot
	}
	return out, nil
}

func nonNegative(value float64) float64 {
	if value < 0 {
		return 0
	}
	return value
}

// queryRawQuotaUsage 以三次查询读出全部窗口的原始用量（未减基线）：
// 日桶一次用条件聚合同时算出 day/week/month，累计桶一次，5h 按各主体的锚点一次。
func queryRawQuotaUsage(tenantID string, subject periodSubject, ids []string, windows subjectPeriodWindows) (map[string]QuotaUsageSnapshot, error) {
	column := string(subject)
	if subject != periodSubjectAPIKey && subject != periodSubjectEndUser {
		return nil, fmt.Errorf("%w: invalid subject", ErrQuotaUsageUnavailable)
	}
	db := getReadDB()
	if db == nil {
		return nil, ErrQuotaUsageUnavailable
	}
	out := make(map[string]QuotaUsageSnapshot, len(ids))
	add := func(id, model string, window quota.Period, cost float64, requests, tokens int64) {
		snapshot := out[id]
		if snapshot.Models == nil {
			snapshot.Models = map[string]quota.Matrix{}
		}
		modelUsage := snapshot.Models[model]
		modelUsage.Set(quota.MetricCost, window, modelUsage.Value(quota.MetricCost, window)+cost)
		modelUsage.Set(quota.MetricRequests, window, modelUsage.Value(quota.MetricRequests, window)+float64(requests))
		modelUsage.Set(quota.MetricTokens, window, modelUsage.Value(quota.MetricTokens, window)+float64(tokens))
		snapshot.Models[model] = modelUsage
		snapshot.Total.Set(quota.MetricCost, window, snapshot.Total.Value(quota.MetricCost, window)+cost)
		snapshot.Total.Set(quota.MetricRequests, window, snapshot.Total.Value(quota.MetricRequests, window)+float64(requests))
		snapshot.Total.Set(quota.MetricTokens, window, snapshot.Total.Value(quota.MetricTokens, window)+float64(tokens))
		out[id] = snapshot
	}

	// 日桶：取 week 与 month 起点中较早者作为下界，一次扫过本月（或跨月的本周）。
	from := windows.keys.MonthFrom
	if windows.keys.WeekFrom < from {
		from = windows.keys.WeekFrom
	}
	dayQuery := `SELECT ` + column + `, model,
		COALESCE(SUM(CASE WHEN bucket_start >= ? THEN cost_total ELSE 0 END), 0),
		COALESCE(SUM(CASE WHEN bucket_start >= ? THEN cost_total ELSE 0 END), 0),
		COALESCE(SUM(CASE WHEN bucket_start >= ? THEN cost_total ELSE 0 END), 0),
		COALESCE(SUM(CASE WHEN bucket_start >= ? THEN request_count ELSE 0 END), 0),
		COALESCE(SUM(CASE WHEN bucket_start >= ? THEN request_count ELSE 0 END), 0),
		COALESCE(SUM(CASE WHEN bucket_start >= ? THEN request_count ELSE 0 END), 0),
		COALESCE(SUM(CASE WHEN bucket_start >= ? THEN total_tokens ELSE 0 END), 0),
		COALESCE(SUM(CASE WHEN bucket_start >= ? THEN total_tokens ELSE 0 END), 0),
		COALESCE(SUM(CASE WHEN bucket_start >= ? THEN total_tokens ELSE 0 END), 0)
		FROM usage_rollup_buckets
		WHERE tenant_id = ? AND bucket_kind = ? AND ` + column + ` IN (` + placeholders(len(ids)) + `)
		  AND bucket_start >= ? AND bucket_start < ?
		GROUP BY ` + column + `, model`
	dayArgs := make([]any, 0, len(ids)+14)
	for range 3 {
		dayArgs = append(dayArgs, windows.keys.Day, windows.keys.WeekFrom, windows.keys.MonthFrom)
	}
	dayArgs = append(dayArgs, normalizeTenantID(tenantID), rollupBucketDay)
	for _, id := range ids {
		dayArgs = append(dayArgs, id)
	}
	dayArgs = append(dayArgs, from, windows.keys.DayTo)
	rows, err := db.Query(dayQuery, dayArgs...)
	if err != nil {
		return nil, fmt.Errorf("%w: query %s day usage: %v", ErrQuotaUsageUnavailable, subject, err)
	}
	for rows.Next() {
		var id, model string
		var dayCost, weekCost, monthCost float64
		var dayReq, weekReq, monthReq, dayTok, weekTok, monthTok int64
		if err := rows.Scan(&id, &model, &dayCost, &weekCost, &monthCost, &dayReq, &weekReq, &monthReq, &dayTok, &weekTok, &monthTok); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("%w: scan %s day usage: %v", ErrQuotaUsageUnavailable, subject, err)
		}
		add(id, model, quota.PeriodDay, dayCost, dayReq, dayTok)
		add(id, model, quota.PeriodWeek, weekCost, weekReq, weekTok)
		add(id, model, quota.PeriodMonth, monthCost, monthReq, monthTok)
	}
	if err := closeRows(rows); err != nil {
		return nil, fmt.Errorf("%w: rows %s day usage: %v", ErrQuotaUsageUnavailable, subject, err)
	}

	lifetimeQuery := `SELECT ` + column + `, model, COALESCE(SUM(cost_total), 0), COALESCE(SUM(request_count), 0), COALESCE(SUM(total_tokens), 0)
		FROM usage_rollup_buckets
		WHERE tenant_id = ? AND bucket_kind = ? AND ` + column + ` IN (` + placeholders(len(ids)) + `)
		GROUP BY ` + column + `, model`
	lifetimeArgs := make([]any, 0, len(ids)+2)
	lifetimeArgs = append(lifetimeArgs, normalizeTenantID(tenantID), rollupBucketLifetime)
	for _, id := range ids {
		lifetimeArgs = append(lifetimeArgs, id)
	}
	lifetimeRows, err := db.Query(lifetimeQuery, lifetimeArgs...)
	if err != nil {
		return nil, fmt.Errorf("%w: query %s lifetime usage: %v", ErrQuotaUsageUnavailable, subject, err)
	}
	if err := scanWindowRows(lifetimeRows, quota.PeriodLifetime, add); err != nil {
		return nil, fmt.Errorf("%w: %s lifetime usage: %v", ErrQuotaUsageUnavailable, subject, err)
	}

	// 5h：每个主体的窗口起点不同，沿用 queryFiveHourCostByAnchors 的 OR 展开写法，
	// 单次往返且仍能用上 (tenant_id, bucket_kind, <subject>, bucket_start) 索引。
	pairs := make([][2]string, 0, len(ids))
	for _, id := range ids {
		if anchor := windows.fiveHourAnchors[id]; anchor != "" {
			pairs = append(pairs, [2]string{id, anchor})
		}
	}
	if len(pairs) > 0 {
		var b strings.Builder
		b.WriteString(`SELECT ` + column + `, model, COALESCE(SUM(cost_total), 0), COALESCE(SUM(request_count), 0), COALESCE(SUM(total_tokens), 0)
			FROM usage_rollup_buckets WHERE tenant_id = ? AND bucket_kind = ? AND bucket_start < ? AND (`)
		args := make([]any, 0, len(pairs)*2+3)
		args = append(args, normalizeTenantID(tenantID), rollupBucketQuotaMinuteUTC, windows.keys.FiveHourTo)
		for i, pair := range pairs {
			if i > 0 {
				b.WriteString(` OR `)
			}
			b.WriteString(`(` + column + ` = ? AND bucket_start >= ?)`)
			args = append(args, pair[0], pair[1])
		}
		b.WriteString(`) GROUP BY ` + column + `, model`)
		rows, err := db.Query(b.String(), args...)
		if err != nil {
			return nil, fmt.Errorf("%w: query %s 5h usage: %v", ErrQuotaUsageUnavailable, subject, err)
		}
		if err := scanWindowRows(rows, quota.PeriodFiveHour, add); err != nil {
			return nil, fmt.Errorf("%w: %s 5h usage: %v", ErrQuotaUsageUnavailable, subject, err)
		}
	}
	return out, nil
}

type rowScanner interface {
	Next() bool
	Scan(dest ...any) error
	Close() error
	Err() error
}

func closeRows(rows rowScanner) error {
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return err
	}
	return rows.Close()
}

func scanWindowRows(rows rowScanner, window quota.Period, add func(id, model string, window quota.Period, cost float64, requests, tokens int64)) error {
	for rows.Next() {
		var id, model string
		var cost float64
		var requests, tokens int64
		if err := rows.Scan(&id, &model, &cost, &requests, &tokens); err != nil {
			_ = rows.Close()
			return err
		}
		add(id, model, window, cost, requests, tokens)
	}
	return closeRows(rows)
}
