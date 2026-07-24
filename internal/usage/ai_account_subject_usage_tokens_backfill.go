package usage

import (
	"database/sql"
	"fmt"
	"strings"
	"time"
)

const aiAccountSubjectUsageTokensBackfillMarker = "ai_account_subject_usage_tokens_backfill_v1"

type aiAccountSubjectTokenBucket struct {
	subjectID  string
	kind       string
	start      string
	total      int64
	firstEvent time.Time
}

// RunAIAccountSubjectUsageTokensBackfillAtInit fills the new token projection
// from durable rollups and exact surviving cycle logs once per marker version.
func RunAIAccountSubjectUsageTokensBackfillAtInit() error {
	return runAIAccountSubjectUsageTokensBackfillAtInitDB(getDB())
}

func runAIAccountSubjectUsageTokensBackfillAtInitDB(db *sql.DB) error {
	if db == nil {
		return nil
	}

	usageProjectionMu.Lock()
	defer usageProjectionMu.Unlock()

	ensureUsageProjectionMarkerTable(db)
	if projectionMarkerValue(db, aiAccountSubjectUsageTokensBackfillMarker) == rollupMarkerDone {
		return nil
	}
	if err := setProjectionMarker(db, aiAccountSubjectUsageTokensBackfillMarker, rollupMarkerPending); err != nil {
		return fmt.Errorf("usage: mark shared subject token backfill pending: %w", err)
	}

	buckets, err := loadAIAccountSubjectDayAndLifetimeTokenBuckets(db)
	if err != nil {
		return err
	}
	cycleBuckets, err := loadAIAccountSubjectCycleTokenBuckets(db)
	if err != nil {
		return err
	}
	buckets = append(buckets, cycleBuckets...)

	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("usage: begin shared subject token backfill: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	now := time.Now().UTC()
	updatedAt := now.Format(time.RFC3339Nano)
	const upsert = `
		INSERT INTO ai_account_subject_usage_buckets (
			auth_subject_id, bucket_kind, bucket_start, request_count,
			success_count, failure_count, cost_total, total_tokens, first_event_at, updated_at
		) VALUES (?, ?, ?, 0, 0, 0, 0, ?, ?, ?)
		ON CONFLICT(auth_subject_id, bucket_kind, bucket_start) DO UPDATE SET
			total_tokens = excluded.total_tokens,
			updated_at = excluded.updated_at
	`
	for _, bucket := range buckets {
		firstEvent := bucket.firstEvent
		if firstEvent.IsZero() {
			firstEvent = now
		}
		if _, err := tx.Exec(upsert, bucket.subjectID, bucket.kind, bucket.start, bucket.total, firstEvent.UTC().Format(time.RFC3339Nano), updatedAt); err != nil {
			return fmt.Errorf("usage: backfill shared subject %s tokens: %w", bucket.kind, err)
		}
	}
	if _, err := tx.Exec(`
		INSERT INTO usage_projection_markers (marker_key, marker_value, updated_at)
		VALUES (?, ?, ?)
		ON CONFLICT(marker_key) DO UPDATE SET
			marker_value = excluded.marker_value,
			updated_at = excluded.updated_at
	`, aiAccountSubjectUsageTokensBackfillMarker, rollupMarkerDone, updatedAt); err != nil {
		return fmt.Errorf("usage: mark shared subject token backfill: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("usage: commit shared subject token backfill: %w", err)
	}
	return nil
}

func loadAIAccountSubjectDayAndLifetimeTokenBuckets(db *sql.DB) ([]aiAccountSubjectTokenBucket, error) {
	rows, err := db.Query(`
		SELECT auth_subject_id, bucket_kind, bucket_start,
			COALESCE(SUM(total_tokens), 0), MIN(updated_at)
		FROM usage_rollup_buckets
		WHERE bucket_kind IN ('day', 'lifetime')
		  AND trim(coalesce(auth_subject_id, '')) <> ''
		GROUP BY auth_subject_id, bucket_kind, bucket_start
	`)
	if err != nil {
		return nil, fmt.Errorf("usage: query shared subject token rollups: %w", err)
	}
	defer rows.Close()

	buckets := make([]aiAccountSubjectTokenBucket, 0)
	for rows.Next() {
		var bucket aiAccountSubjectTokenBucket
		var first storedTime
		if err := rows.Scan(&bucket.subjectID, &bucket.kind, &bucket.start, &bucket.total, &first); err != nil {
			return nil, fmt.Errorf("usage: scan shared subject token rollup: %w", err)
		}
		bucket.subjectID = strings.TrimSpace(bucket.subjectID)
		if bucket.subjectID == "" {
			continue
		}
		if first.Valid {
			bucket.firstEvent = first.Time
		}
		buckets = append(buckets, bucket)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return buckets, nil
}

func loadAIAccountSubjectCycleTokenBuckets(db *sql.DB) ([]aiAccountSubjectTokenBucket, error) {
	rows, err := db.Query(`
		SELECT auth_subject_id, provider, quota_key, cycle_start_at, reset_at, window_seconds, last_verified_at
		FROM ai_account_subject_quota_cycles
		WHERE window_seconds >= ?
		ORDER BY auth_subject_id, last_verified_at DESC, reset_at DESC
	`, aiAccountSubjectWeeklyWindowSeconds)
	if err != nil {
		return nil, fmt.Errorf("usage: query shared subject token cycles: %w", err)
	}

	bySubject := make(map[string][]AIAccountSubjectQuotaCycle)
	for rows.Next() {
		var cycle AIAccountSubjectQuotaCycle
		var start, reset, verified storedTime
		if err := rows.Scan(&cycle.AuthSubjectID, &cycle.Provider, &cycle.QuotaKey, &start, &reset, &cycle.WindowSeconds, &verified); err != nil {
			rows.Close()
			return nil, fmt.Errorf("usage: scan shared subject token cycle: %w", err)
		}
		cycle.AuthSubjectID = strings.TrimSpace(cycle.AuthSubjectID)
		if cycle.AuthSubjectID == "" || !start.Valid || !reset.Valid {
			continue
		}
		cycle.CycleStartAt = start.Time
		cycle.ResetAt = reset.Time
		if verified.Valid {
			cycle.LastVerifiedAt = verified.Time
		}
		bySubject[cycle.AuthSubjectID] = append(bySubject[cycle.AuthSubjectID], cycle)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}

	selected := make(map[string]AIAccountSubjectQuotaCycle, len(bySubject))
	var earliest time.Time
	for subjectID, cycles := range bySubject {
		cycle, ok := selectAIAccountSubjectWeeklyCycle(cycles)
		if !ok {
			continue
		}
		selected[subjectID] = cycle
		if earliest.IsZero() || cycle.CycleStartAt.Before(earliest) {
			earliest = cycle.CycleStartAt
		}
	}
	if len(selected) == 0 {
		return []aiAccountSubjectTokenBucket{}, nil
	}

	totals := make(map[string]int64, len(selected))
	firstEvents := make(map[string]time.Time, len(selected))
	logRows, err := db.Query(`
		SELECT auth_subject_id, timestamp, total_tokens
		FROM request_logs
		WHERE timestamp >= ?
		  AND trim(coalesce(auth_subject_id, '')) <> ''
	`, earliest.UTC().Format(time.RFC3339Nano))
	if err != nil {
		return nil, fmt.Errorf("usage: query shared subject cycle tokens: %w", err)
	}
	defer logRows.Close()
	for logRows.Next() {
		var subjectID string
		var at storedTime
		var totalTokens int64
		if err := logRows.Scan(&subjectID, &at, &totalTokens); err != nil {
			return nil, fmt.Errorf("usage: scan shared subject cycle tokens: %w", err)
		}
		cycle, ok := selected[strings.TrimSpace(subjectID)]
		if !ok || !at.Valid || at.Time.Before(cycle.CycleStartAt) {
			continue
		}
		totals[cycle.AuthSubjectID] += totalTokens
		if first := firstEvents[cycle.AuthSubjectID]; first.IsZero() || at.Time.Before(first) {
			firstEvents[cycle.AuthSubjectID] = at.Time
		}
	}
	if err := logRows.Close(); err != nil {
		return nil, err
	}

	buckets := make([]aiAccountSubjectTokenBucket, 0, len(selected))
	for subjectID, cycle := range selected {
		firstEvent := firstEvents[subjectID]
		if firstEvent.IsZero() {
			firstEvent = cycle.CycleStartAt
		}
		buckets = append(buckets, aiAccountSubjectTokenBucket{
			subjectID:  subjectID,
			kind:       "cycle",
			start:      formatAIAccountSubjectCycleBucketStart(cycle.CycleStartAt),
			total:      totals[subjectID],
			firstEvent: firstEvent,
		})
	}
	return buckets, nil
}
