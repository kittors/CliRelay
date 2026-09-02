package usage

import (
	"database/sql"
	"fmt"
	"strings"
	"time"
)

type QuotaSnapshotPoint struct {
	RecordedAt    time.Time  `json:"recorded_at"`
	AuthIndex     string     `json:"auth_index"`
	Provider      string     `json:"provider"`
	QuotaKey      string     `json:"quota_key"`
	QuotaLabel    string     `json:"quota_label"`
	Percent       *float64   `json:"percent"`
	ResetAt       *time.Time `json:"reset_at,omitempty"`
	WindowSeconds int64      `json:"window_seconds"`
}

type QuotaSnapshotSeriesPoint struct {
	Timestamp time.Time  `json:"timestamp"`
	Percent   *float64   `json:"percent"`
	ResetAt   *time.Time `json:"reset_at,omitempty"`
}

type QuotaSnapshotSeries struct {
	QuotaKey      string                     `json:"quota_key"`
	QuotaLabel    string                     `json:"quota_label"`
	WindowSeconds int64                      `json:"window_seconds"`
	Points        []QuotaSnapshotSeriesPoint `json:"points"`
}

func RecordDailyQuotaSnapshot(authIndex, provider string, quotas map[string]*float64) error {
	return RecordDailyQuotaSnapshotIdentityForTenant(systemTenantID, authIndex, "", provider, quotas)
}

func RecordQuotaSnapshotPoints(authIndex, provider string, points []QuotaSnapshotPoint) error {
	return RecordQuotaSnapshotPointsIdentityForTenant(systemTenantID, authIndex, "", provider, points)
}

func QueryDailyQuotaByAuthIndexes(authIndexes []string, quotaKey string, days int) ([]DailyQuotaPoint, error) {
	return QueryDailyQuotaByAuthIndexesForTenant(systemTenantID, authIndexes, quotaKey, days)
}

func QueryDailyQuotaByAuthIndexesForTenant(tenantID string, authIndexes []string, quotaKey string, days int) ([]DailyQuotaPoint, error) {
	tenantID = normalizeTenantID(tenantID)
	db := getDB()
	if db == nil {
		return []DailyQuotaPoint{}, nil
	}
	if days < 1 {
		days = 7
	}
	if len(authIndexes) == 0 {
		return []DailyQuotaPoint{}, nil
	}
	quotaKey = strings.TrimSpace(quotaKey)
	if quotaKey == "" {
		return []DailyQuotaPoint{}, nil
	}

	seen := make(map[string]struct{}, len(authIndexes))
	normalized := make([]string, 0, len(authIndexes))
	for _, idx := range authIndexes {
		idx = strings.TrimSpace(idx)
		if idx == "" {
			continue
		}
		if _, ok := seen[idx]; ok {
			continue
		}
		seen[idx] = struct{}{}
		normalized = append(normalized, idx)
	}
	if len(normalized) == 0 {
		return []DailyQuotaPoint{}, nil
	}

	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(normalized)), ",")
	args := make([]interface{}, 0, len(normalized)+3)
	args = append(args, tenantID, cutoffDayKey(days), quotaKey)
	for _, idx := range normalized {
		args = append(args, idx)
	}

	q := fmt.Sprintf(`
		SELECT date_key, AVG(percent) AS avg_percent, COUNT(percent) AS samples
		FROM auth_file_quota_snapshots
		WHERE tenant_id = ? AND date_key >= ? AND quota_key = ? AND auth_index IN (%s) AND percent IS NOT NULL
		GROUP BY date_key
		ORDER BY date_key ASC
	`, placeholders)

	rows, err := db.Query(q, args...)
	if err != nil {
		return nil, fmt.Errorf("usage: daily quota by auth indexes query: %w", err)
	}
	defer rows.Close()

	result := make([]DailyQuotaPoint, 0, days)
	for rows.Next() {
		var point DailyQuotaPoint
		var percent sql.NullFloat64
		if err := rows.Scan(&point.Date, &percent, &point.Samples); err != nil {
			return nil, fmt.Errorf("usage: daily quota by auth indexes scan: %w", err)
		}
		if percent.Valid {
			v := percent.Float64
			point.Percent = &v
		}
		result = append(result, point)
	}
	return result, rows.Err()
}

type weeklyQuotaKey struct {
	QuotaKey      string
	QuotaLabel    string
	WindowSeconds int64
}

// QueryWeeklyQuotaSeriesByAuthIndexesForTenant averages every weekly window
// those credentials recorded, one series per quota_key. The old group-trend
// path only asked for code_week, so Antigravity's gemini_weekly / 3p_weekly
// (and Claude seven_day, extra Codex weeks) never reached the chart.
func QueryWeeklyQuotaSeriesByAuthIndexesForTenant(tenantID string, authIndexes []string, days int) ([]DailyQuotaSeries, error) {
	tenantID = normalizeTenantID(tenantID)
	if days < 1 {
		days = 7
	}
	normalized := uniqueAuthIndexes(authIndexes)
	if len(normalized) == 0 {
		return []DailyQuotaSeries{}, nil
	}
	keys, err := listWeeklyQuotaKeysForAuthIndexes(tenantID, normalized, days)
	if err != nil {
		return nil, err
	}
	if len(keys) == 0 {
		// Hosts that only ever wrote the Codex/Kimi daily snapshot still need a
		// series, otherwise the chart goes blank after this path stopped
		// hardcoding code_week.
		points, err := QueryDailyQuotaByAuthIndexesForTenant(tenantID, normalized, "code_week", days)
		if err != nil {
			return nil, err
		}
		if len(points) == 0 {
			return []DailyQuotaSeries{}, nil
		}
		return []DailyQuotaSeries{{
			QuotaKey:      "code_week",
			WindowSeconds: WeeklyQuotaWindowSeconds,
			Points:        points,
		}}, nil
	}
	out := make([]DailyQuotaSeries, 0, len(keys))
	for _, key := range keys {
		points, err := QueryDailyQuotaByAuthIndexesForTenant(tenantID, normalized, key.QuotaKey, days)
		if err != nil {
			return nil, err
		}
		out = append(out, DailyQuotaSeries{
			QuotaKey:      key.QuotaKey,
			QuotaLabel:    key.QuotaLabel,
			WindowSeconds: key.WindowSeconds,
			Points:        points,
		})
	}
	return out, nil
}

func uniqueAuthIndexes(authIndexes []string) []string {
	seen := make(map[string]struct{}, len(authIndexes))
	out := make([]string, 0, len(authIndexes))
	for _, idx := range authIndexes {
		idx = strings.TrimSpace(idx)
		if idx == "" {
			continue
		}
		if _, ok := seen[idx]; ok {
			continue
		}
		seen[idx] = struct{}{}
		out = append(out, idx)
	}
	return out
}

func listWeeklyQuotaKeysForAuthIndexes(tenantID string, authIndexes []string, days int) ([]weeklyQuotaKey, error) {
	db := getDB()
	if db == nil {
		return nil, nil
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(authIndexes)), ",")
	args := make([]interface{}, 0, len(authIndexes)+3)
	args = append(args, tenantID, CutoffStartUTC(days).UTC().Format(time.RFC3339Nano), WeeklyQuotaWindowSeconds)
	for _, idx := range authIndexes {
		args = append(args, idx)
	}
	q := fmt.Sprintf(`
		SELECT quota_key, MAX(quota_label), MAX(window_seconds)
		FROM auth_file_quota_snapshot_points
		WHERE tenant_id = ? AND recorded_at >= ? AND window_seconds >= ? AND auth_index IN (%s) AND percent IS NOT NULL
		GROUP BY quota_key
		ORDER BY quota_key ASC
	`, placeholders)
	rows, err := db.Query(q, args...)
	if err != nil {
		return nil, fmt.Errorf("usage: list weekly quota keys: %w", err)
	}
	defer rows.Close()
	keys := make([]weeklyQuotaKey, 0)
	for rows.Next() {
		var key weeklyQuotaKey
		var label sql.NullString
		if err := rows.Scan(&key.QuotaKey, &label, &key.WindowSeconds); err != nil {
			return nil, fmt.Errorf("usage: scan weekly quota keys: %w", err)
		}
		if label.Valid {
			key.QuotaLabel = strings.TrimSpace(label.String)
		}
		keys = append(keys, key)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return preferPrimaryWeeklyQuotaKey(keys), nil
}

func preferPrimaryWeeklyQuotaKey(keys []weeklyQuotaKey) []weeklyQuotaKey {
	if len(keys) < 2 {
		return keys
	}
	primary := -1
	for i, key := range keys {
		if key.QuotaKey == "code_week" {
			primary = i
			break
		}
	}
	if primary <= 0 {
		return keys
	}
	out := make([]weeklyQuotaKey, 0, len(keys))
	out = append(out, keys[primary])
	out = append(out, keys[:primary]...)
	out = append(out, keys[primary+1:]...)
	return out
}

func QueryQuotaSnapshotPoints(authIndex string, start, end time.Time) ([]QuotaSnapshotPoint, error) {
	return QueryQuotaSnapshotPointsForTenant(systemTenantID, authIndex, start, end)
}

func QueryQuotaSnapshotPointsForTenant(tenantID, authIndex string, start, end time.Time) ([]QuotaSnapshotPoint, error) {
	tenantID = normalizeTenantID(tenantID)
	db := getDB()
	if db == nil {
		return []QuotaSnapshotPoint{}, nil
	}
	authIndex = strings.TrimSpace(authIndex)
	if authIndex == "" {
		return []QuotaSnapshotPoint{}, nil
	}
	if start.IsZero() {
		start = time.Now().AddDate(0, 0, -7)
	}
	if end.IsZero() {
		end = time.Now()
	}

	rows, err := db.Query(`
		SELECT recorded_at, auth_index, provider, quota_key, quota_label, percent, reset_at, window_seconds
		FROM auth_file_quota_snapshot_points
		WHERE tenant_id = ? AND auth_index = ? AND recorded_at >= ? AND recorded_at <= ?
		ORDER BY recorded_at ASC, quota_key ASC
	`, tenantID, authIndex, start.UTC().Format(time.RFC3339Nano), end.UTC().Format(time.RFC3339Nano))
	if err != nil {
		return nil, fmt.Errorf("usage: quota snapshot points query: %w", err)
	}
	defer rows.Close()

	result := make([]QuotaSnapshotPoint, 0)
	for rows.Next() {
		var point QuotaSnapshotPoint
		var recordedAt storedTime
		var resetAt storedTime
		var percent sql.NullFloat64
		if err := rows.Scan(
			&recordedAt,
			&point.AuthIndex,
			&point.Provider,
			&point.QuotaKey,
			&point.QuotaLabel,
			&percent,
			&resetAt,
			&point.WindowSeconds,
		); err != nil {
			return nil, fmt.Errorf("usage: quota snapshot points scan: %w", err)
		}
		if recordedAt.Valid {
			point.RecordedAt = recordedAt.Time
		}
		if percent.Valid {
			v := percent.Float64
			point.Percent = &v
		}
		if resetAt.Valid {
			parsed := resetAt.Time
			point.ResetAt = &parsed
		}
		result = append(result, point)
	}
	return result, rows.Err()
}

func QueryQuotaSnapshotSeries(authIndex string, start, end time.Time) ([]QuotaSnapshotSeries, error) {
	return QueryQuotaSnapshotSeriesForTenant(systemTenantID, authIndex, start, end)
}

func QueryQuotaSnapshotSeriesForTenant(tenantID, authIndex string, start, end time.Time) ([]QuotaSnapshotSeries, error) {
	points, err := QueryQuotaSnapshotPointsForTenant(tenantID, authIndex, start, end)
	if err != nil {
		return nil, err
	}
	series := make([]QuotaSnapshotSeries, 0)
	indexByKey := make(map[string]int)
	for _, point := range points {
		seriesKey := fmt.Sprintf("%s\x00%d", point.QuotaKey, point.WindowSeconds)
		idx, ok := indexByKey[seriesKey]
		if !ok {
			idx = len(series)
			indexByKey[seriesKey] = idx
			series = append(series, QuotaSnapshotSeries{
				QuotaKey:      point.QuotaKey,
				QuotaLabel:    point.QuotaLabel,
				WindowSeconds: point.WindowSeconds,
				Points:        []QuotaSnapshotSeriesPoint{},
			})
		}
		series[idx].Points = append(series[idx].Points, QuotaSnapshotSeriesPoint{
			Timestamp: point.RecordedAt,
			Percent:   point.Percent,
			ResetAt:   point.ResetAt,
		})
	}
	return series, nil
}
