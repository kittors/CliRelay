package usage

import (
	"database/sql"
	"fmt"
	"strings"
	"time"
)

type AIAccountSubjectQuotaCycle struct {
	AuthSubjectID  string
	Provider       string
	QuotaKey       string
	CycleStartAt   time.Time
	ResetAt        time.Time
	WindowSeconds  int64
	LastVerifiedAt time.Time
}

func RecordAIAccountSubjectQuotaPoints(authSubjectID, provider string, points []QuotaSnapshotPoint) error {
	db := getDB()
	authSubjectID = strings.TrimSpace(authSubjectID)
	provider = strings.TrimSpace(provider)
	if db == nil || authSubjectID == "" || len(points) == 0 {
		return nil
	}
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("usage: shared quota begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	now := time.Now().UTC()
	cycles := make([]AIAccountSubjectQuotaCycle, 0, len(points))
	for _, point := range points {
		key := strings.TrimSpace(point.QuotaKey)
		if key == "" {
			continue
		}
		recordedAt := point.RecordedAt
		if recordedAt.IsZero() {
			recordedAt = now
		}
		recordedAt = recordedAt.UTC()
		pointProvider := strings.TrimSpace(point.Provider)
		if pointProvider == "" {
			pointProvider = provider
		}
		label := strings.TrimSpace(point.QuotaLabel)
		if label == "" {
			label = key
		}
		if point.Percent != nil {
			v := *point.Percent
			if v < 0 {
				v = 0
			}
			if v > 100 {
				v = 100
			}
			point.Percent = &v
		}
		if point.ResetAt != nil && !point.ResetAt.IsZero() && point.WindowSeconds > 0 {
			cycle := AIAccountSubjectQuotaCycle{
				AuthSubjectID: authSubjectID, Provider: pointProvider, QuotaKey: key,
				CycleStartAt: point.ResetAt.UTC().Add(-time.Duration(point.WindowSeconds) * time.Second),
				ResetAt:      point.ResetAt.UTC(), WindowSeconds: point.WindowSeconds, LastVerifiedAt: recordedAt,
			}
			// Cache the anchored cycle, not the freshly derived one: the in-memory
			// cache feeds the usage projection's bucket key, so seeding it with the
			// jittered value would re-split the period the anchor just protected.
			anchored, err := upsertAIAccountSubjectQuotaCycleTx(tx, cycle)
			if err != nil {
				return err
			}
			cycles = append(cycles, anchored)
		}
		var latestAt sql.NullString
		var latestPercent sql.NullFloat64
		var latestReset sql.NullString
		var latestWindow int64
		err := tx.QueryRow(`
			SELECT recorded_at, percent, reset_at, window_seconds
			FROM ai_account_subject_quota_points
			WHERE auth_subject_id = ? AND quota_key = ?
			ORDER BY recorded_at DESC LIMIT 1
		`, authSubjectID, key).Scan(&latestAt, &latestPercent, &latestReset, &latestWindow)
		if err != nil && err != sql.ErrNoRows {
			return err
		}
		if err == nil {
			latest, _ := parseStoredTimeString(latestAt.String)
			samePercent := (!latestPercent.Valid && point.Percent == nil) || (latestPercent.Valid && point.Percent != nil && latestPercent.Float64 == *point.Percent)
			sameReset := nullableStoredTimeEqual(latestReset, point.ResetAt)
			if recordedAt.Sub(latest) < quotaSnapshotHeartbeatInterval && samePercent && sameReset && latestWindow == point.WindowSeconds {
				continue
			}
		}
		if _, err := tx.Exec(`
			INSERT INTO ai_account_subject_quota_points
				(auth_subject_id, provider, quota_key, quota_label, percent, reset_at, window_seconds, recorded_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		`, authSubjectID, pointProvider, key, label, point.Percent, nullableTimeValue(point.ResetAt), point.WindowSeconds, recordedAt.Format(time.RFC3339Nano)); err != nil {
			return fmt.Errorf("usage: insert shared quota point: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	for _, cycle := range cycles {
		setAIAccountSubjectActiveCycle(cycle)
	}
	return nil
}

func nullableStoredTimeEqual(stored sql.NullString, value *time.Time) bool {
	if !stored.Valid || strings.TrimSpace(stored.String) == "" {
		return value == nil || value.IsZero()
	}
	if value == nil || value.IsZero() {
		return false
	}
	parsed, ok := parseStoredTimeString(stored.String)
	return ok && parsed.Equal(value.UTC())
}

// anchorAIAccountSubjectQuotaCycleTx keeps a cycle's identity stable across probes.
//
// The stored cycle_start doubles as the usage bucket key, so letting it follow
// the upstream's per-probe jitter silently opened a new bucket every few seconds
// and split one weekly period into fragments. Within the drift tolerance the
// stored anchor wins and only last_verified_at moves; a real rollover shifts the
// start by a whole window and falls outside the tolerance, so it still rolls.
func anchorAIAccountSubjectQuotaCycleTx(tx *sql.Tx, cycle AIAccountSubjectQuotaCycle) (AIAccountSubjectQuotaCycle, error) {
	var start, reset storedTime
	var window int64
	err := tx.QueryRow(`
		SELECT cycle_start_at, reset_at, window_seconds
		FROM ai_account_subject_quota_cycles
		WHERE auth_subject_id = ? AND quota_key = ?
	`, cycle.AuthSubjectID, cycle.QuotaKey).Scan(&start, &reset, &window)
	if err == sql.ErrNoRows {
		return cycle, nil
	}
	if err != nil {
		return cycle, fmt.Errorf("usage: load shared quota cycle anchor: %w", err)
	}
	if !start.Valid || window != cycle.WindowSeconds {
		return cycle, nil
	}
	if !sameAIAccountSubjectCycle(cycle.CycleStartAt, start.Time, cycle.WindowSeconds) {
		return cycle, nil
	}
	cycle.CycleStartAt = start.Time.UTC()
	if reset.Valid && !reset.Time.IsZero() {
		// Keep reset_at consistent with the anchor so the card countdown stops
		// jittering by a second or two on every probe.
		cycle.ResetAt = reset.Time.UTC()
	}
	return cycle, nil
}

func upsertAIAccountSubjectQuotaCycleTx(tx *sql.Tx, cycle AIAccountSubjectQuotaCycle) (AIAccountSubjectQuotaCycle, error) {
	anchored, err := anchorAIAccountSubjectQuotaCycleTx(tx, cycle)
	if err != nil {
		return cycle, err
	}
	cycle = anchored
	_, err = tx.Exec(`
		INSERT INTO ai_account_subject_quota_cycles
			(auth_subject_id, provider, quota_key, cycle_start_at, reset_at, window_seconds, last_verified_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(auth_subject_id, quota_key) DO UPDATE SET
			provider = excluded.provider,
			cycle_start_at = excluded.cycle_start_at,
			reset_at = excluded.reset_at,
			window_seconds = excluded.window_seconds,
			last_verified_at = excluded.last_verified_at
	`, cycle.AuthSubjectID, cycle.Provider, cycle.QuotaKey,
		cycle.CycleStartAt.UTC().Format(time.RFC3339Nano), cycle.ResetAt.UTC().Format(time.RFC3339Nano),
		cycle.WindowSeconds, cycle.LastVerifiedAt.UTC().Format(time.RFC3339Nano))
	if err != nil {
		return cycle, fmt.Errorf("usage: upsert shared quota cycle: %w", err)
	}
	return cycle, nil
}

// QueryAIAccountSubjectQuotaSeries loads shared quota history for the detail trend chart.
func QueryAIAccountSubjectQuotaSeries(authSubjectID string, start, end time.Time) ([]QuotaSnapshotSeries, error) {
	db := getReadDB()
	authSubjectID = strings.TrimSpace(authSubjectID)
	if db == nil || authSubjectID == "" {
		return []QuotaSnapshotSeries{}, nil
	}
	if start.IsZero() {
		start = time.Now().AddDate(0, 0, -7)
	}
	if end.IsZero() {
		end = time.Now()
	}
	rows, err := db.Query(`
		SELECT recorded_at, provider, quota_key, quota_label, percent, reset_at, window_seconds
		FROM ai_account_subject_quota_points
		WHERE auth_subject_id = ? AND recorded_at >= ? AND recorded_at <= ?
		ORDER BY recorded_at ASC, quota_key ASC
	`, authSubjectID, start.UTC().Format(time.RFC3339Nano), end.UTC().Format(time.RFC3339Nano))
	if err != nil {
		return nil, fmt.Errorf("usage: shared subject quota series: %w", err)
	}
	defer rows.Close()

	series := make([]QuotaSnapshotSeries, 0)
	indexByKey := make(map[string]int)
	for rows.Next() {
		var recorded storedTime
		var provider, quotaKey, quotaLabel string
		var percent sql.NullFloat64
		var reset storedTime
		var windowSeconds int64
		if err := rows.Scan(&recorded, &provider, &quotaKey, &quotaLabel, &percent, &reset, &windowSeconds); err != nil {
			return nil, err
		}
		if !recorded.Valid {
			continue
		}
		seriesKey := fmt.Sprintf("%s\x00%d", quotaKey, windowSeconds)
		idx, ok := indexByKey[seriesKey]
		if !ok {
			idx = len(series)
			series = append(series, QuotaSnapshotSeries{
				QuotaKey:      quotaKey,
				QuotaLabel:    quotaLabel,
				WindowSeconds: windowSeconds,
				Points:        []QuotaSnapshotSeriesPoint{},
			})
			indexByKey[seriesKey] = idx
		}
		point := QuotaSnapshotSeriesPoint{Timestamp: recorded.Time}
		if percent.Valid {
			v := percent.Float64
			point.Percent = &v
		}
		if reset.Valid {
			t := reset.Time
			point.ResetAt = &t
		}
		series[idx].Points = append(series[idx].Points, point)
	}
	return series, rows.Err()
}

func QueryLatestAIAccountSubjectWeeklyCyclesBatch(subjectIDs []string, preferredKeys []string) (map[string]time.Time, error) {
	db := getReadDB()
	ids := dedupeExactStrings(subjectIDs)
	out := make(map[string]time.Time)
	if db == nil || len(ids) == 0 {
		return out, nil
	}
	args := make([]any, 0, len(ids)+len(preferredKeys)+1)
	for _, id := range ids {
		args = append(args, id)
	}
	args = append(args, aiAccountSubjectWeeklyWindowSeconds)
	query := `
		SELECT auth_subject_id, provider, quota_key, cycle_start_at, reset_at, window_seconds, last_verified_at
		FROM ai_account_subject_quota_cycles
		WHERE auth_subject_id IN (` + strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",") + `)
		  AND window_seconds >= ?`
	keys := dedupeExactStrings(preferredKeys)
	if len(keys) > 0 {
		query += ` AND quota_key IN (` + strings.TrimSuffix(strings.Repeat("?,", len(keys)), ",") + `)`
		for _, key := range keys {
			args = append(args, key)
		}
	}
	query += ` ORDER BY last_verified_at DESC, reset_at DESC`
	rows, err := db.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("usage: query shared quota cycles: %w", err)
	}
	defer rows.Close()
	cyclesBySubject := make(map[string][]AIAccountSubjectQuotaCycle)
	for rows.Next() {
		var cycle AIAccountSubjectQuotaCycle
		var start, reset, verified storedTime
		if err := rows.Scan(&cycle.AuthSubjectID, &cycle.Provider, &cycle.QuotaKey, &start, &reset, &cycle.WindowSeconds, &verified); err != nil {
			return nil, err
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
		cyclesBySubject[cycle.AuthSubjectID] = append(cyclesBySubject[cycle.AuthSubjectID], cycle)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for subjectID, cycles := range cyclesBySubject {
		if cycle, ok := selectAIAccountSubjectWeeklyCycle(cycles); ok {
			out[subjectID] = cycle.CycleStartAt
		}
	}
	return out, nil
}

func loadAIAccountSubjectCycleCache(db *sql.DB) error {
	resetAIAccountSubjectCycleCache()
	if db == nil {
		return nil
	}
	rows, err := db.Query(`
		SELECT auth_subject_id, provider, quota_key, cycle_start_at, reset_at, window_seconds, last_verified_at
		FROM ai_account_subject_quota_cycles WHERE window_seconds >= ?
		ORDER BY last_verified_at DESC
	`, aiAccountSubjectWeeklyWindowSeconds)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var cycle AIAccountSubjectQuotaCycle
		var start, reset, verified storedTime
		if err := rows.Scan(&cycle.AuthSubjectID, &cycle.Provider, &cycle.QuotaKey, &start, &reset, &cycle.WindowSeconds, &verified); err != nil {
			return err
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
	}
	return rows.Err()
}

func cleanupExpiredAIAccountSubjectQuotaPoints(db *sql.DB) (int64, error) {
	if db == nil {
		return 0, nil
	}
	cutoff := time.Now().AddDate(0, 0, -8).UTC().Format(time.RFC3339Nano)
	res, err := db.Exec(`DELETE FROM ai_account_subject_quota_points WHERE recorded_at < ?`, cutoff)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
