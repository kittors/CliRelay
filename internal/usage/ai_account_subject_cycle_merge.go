package usage

import (
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"time"
)

// v2 re-runs the merge with the widened drift tolerance and, unlike v1, drops the
// in-memory cycle cache afterwards.
const aiAccountSubjectCycleBucketMergeMarker = "ai_account_subject_cycle_bucket_merge_v2"

type aiAccountSubjectCycleBucketRow struct {
	subjectID  string
	start      string
	at         time.Time
	requests   int64
	successes  int64
	failures   int64
	cost       float64
	tokens     int64
	firstEvent time.Time
	updatedAt  time.Time
}

// RunAIAccountSubjectCycleBucketMergeAtInit repairs periods that were split into
// several usage buckets before cycle anchoring landed.
//
// The bucket key was the probed cycle start, and upstreams re-derive that start
// from a remaining-seconds countdown, so a single weekly period accumulated one
// bucket per probe jitter (production carried 404 / 114 / 1 request fragments of
// the same period). Each reader then anchored to whichever fragment matched the
// timestamp it read, which is why the card and the detail dialog disagreed about
// the same account. Anchoring stops new fragments; this merges the existing ones
// so the totals are correct without waiting for the next period to roll over.
func RunAIAccountSubjectCycleBucketMergeAtInit() error {
	return runAIAccountSubjectCycleBucketMergeDB(getDB())
}

func runAIAccountSubjectCycleBucketMergeDB(db *sql.DB) error {
	if db == nil {
		return nil
	}

	usageProjectionMu.Lock()
	defer usageProjectionMu.Unlock()

	ensureUsageProjectionMarkerTable(db)
	if projectionMarkerValue(db, aiAccountSubjectCycleBucketMergeMarker) == rollupMarkerDone {
		return nil
	}

	rows, err := loadAIAccountSubjectCycleBuckets(db)
	if err != nil {
		return err
	}
	groups := groupAIAccountSubjectCycleBuckets(rows)

	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("usage: begin shared subject cycle merge: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	now := time.Now().UTC()
	for _, group := range groups {
		if len(group) < 2 {
			continue
		}
		if err := mergeAIAccountSubjectCycleGroupTx(tx, group); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(`
		INSERT INTO usage_projection_markers (marker_key, marker_value, updated_at)
		VALUES (?, ?, ?)
		ON CONFLICT(marker_key) DO UPDATE SET
			marker_value = excluded.marker_value,
			updated_at = excluded.updated_at
	`, aiAccountSubjectCycleBucketMergeMarker, rollupMarkerDone, now.Format(time.RFC3339Nano)); err != nil {
		return fmt.Errorf("usage: mark shared subject cycle merge: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("usage: commit shared subject cycle merge: %w", err)
	}
	// The projection keys its buckets off this cache, and a probe that ran before
	// the merge left it holding a start that no longer has a bucket. Production
	// showed exactly that: minutes after the merge a fresh fragment appeared at the
	// pre-merge start. Dropping the cache forces the next write to re-read the
	// realigned anchor.
	resetAIAccountSubjectCycleCache()
	return nil
}

func loadAIAccountSubjectCycleBuckets(db *sql.DB) ([]aiAccountSubjectCycleBucketRow, error) {
	rows, err := db.Query(`
		SELECT auth_subject_id, bucket_start, request_count, success_count, failure_count,
			cost_total, total_tokens, first_event_at, updated_at
		FROM ai_account_subject_usage_buckets
		WHERE bucket_kind = 'cycle'
	`)
	if err != nil {
		return nil, fmt.Errorf("usage: query shared subject cycle buckets: %w", err)
	}
	defer rows.Close()

	out := make([]aiAccountSubjectCycleBucketRow, 0)
	for rows.Next() {
		var row aiAccountSubjectCycleBucketRow
		var first, updated storedTime
		if err := rows.Scan(&row.subjectID, &row.start, &row.requests, &row.successes, &row.failures,
			&row.cost, &row.tokens, &first, &updated); err != nil {
			return nil, fmt.Errorf("usage: scan shared subject cycle bucket: %w", err)
		}
		row.subjectID = strings.TrimSpace(row.subjectID)
		parsed, ok := parseStoredTimeString(row.start)
		if row.subjectID == "" || !ok {
			continue
		}
		row.at = parsed.UTC()
		if first.Valid {
			row.firstEvent = first.Time.UTC()
		}
		if updated.Valid {
			row.updatedAt = updated.Time.UTC()
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

// groupAIAccountSubjectCycleBuckets clusters one subject's buckets into periods.
//
// Membership is tested against the group's anchor rather than the previous
// bucket, so a long chain of small drifts can never merge two genuinely
// different periods into one.
func groupAIAccountSubjectCycleBuckets(rows []aiAccountSubjectCycleBucketRow) [][]aiAccountSubjectCycleBucketRow {
	bySubject := make(map[string][]aiAccountSubjectCycleBucketRow)
	for _, row := range rows {
		bySubject[row.subjectID] = append(bySubject[row.subjectID], row)
	}
	subjectIDs := make([]string, 0, len(bySubject))
	for subjectID := range bySubject {
		subjectIDs = append(subjectIDs, subjectID)
	}
	sort.Strings(subjectIDs)

	groups := make([][]aiAccountSubjectCycleBucketRow, 0)
	for _, subjectID := range subjectIDs {
		buckets := bySubject[subjectID]
		sort.Slice(buckets, func(i, j int) bool { return buckets[i].at.Before(buckets[j].at) })
		var current []aiAccountSubjectCycleBucketRow
		for _, bucket := range buckets {
			if len(current) > 0 &&
				!sameAIAccountSubjectCycle(current[0].at, bucket.at, aiAccountSubjectWeeklyWindowSeconds) {
				groups = append(groups, current)
				current = nil
			}
			current = append(current, bucket)
		}
		if len(current) > 0 {
			groups = append(groups, current)
		}
	}
	return groups
}

// mergeAIAccountSubjectCycleGroupTx folds a fragmented period onto its earliest
// bucket, which is the closest surviving record of when the period actually began.
func mergeAIAccountSubjectCycleGroupTx(tx *sql.Tx, group []aiAccountSubjectCycleBucketRow) error {
	anchor := group[0]
	merged := anchor
	for _, bucket := range group[1:] {
		merged.requests += bucket.requests
		merged.successes += bucket.successes
		merged.failures += bucket.failures
		merged.cost += bucket.cost
		merged.tokens += bucket.tokens
		if !bucket.firstEvent.IsZero() && (merged.firstEvent.IsZero() || bucket.firstEvent.Before(merged.firstEvent)) {
			merged.firstEvent = bucket.firstEvent
		}
		if bucket.updatedAt.After(merged.updatedAt) {
			merged.updatedAt = bucket.updatedAt
		}
	}
	if merged.firstEvent.IsZero() {
		merged.firstEvent = anchor.at
	}
	if merged.updatedAt.IsZero() {
		merged.updatedAt = anchor.at
	}

	for _, bucket := range group[1:] {
		if _, err := tx.Exec(`
			DELETE FROM ai_account_subject_usage_buckets
			WHERE auth_subject_id = ? AND bucket_kind = 'cycle' AND bucket_start = ?
		`, bucket.subjectID, bucket.start); err != nil {
			return fmt.Errorf("usage: drop fragmented cycle bucket: %w", err)
		}
	}
	if _, err := tx.Exec(`
		UPDATE ai_account_subject_usage_buckets
		SET request_count = ?, success_count = ?, failure_count = ?, cost_total = ?,
			total_tokens = ?, first_event_at = ?, updated_at = ?
		WHERE auth_subject_id = ? AND bucket_kind = 'cycle' AND bucket_start = ?
	`, merged.requests, merged.successes, merged.failures, merged.cost, merged.tokens,
		merged.firstEvent.Format(time.RFC3339Nano), merged.updatedAt.Format(time.RFC3339Nano),
		anchor.subjectID, anchor.start); err != nil {
		return fmt.Errorf("usage: write merged cycle bucket: %w", err)
	}

	return realignAIAccountSubjectQuotaCycleTx(tx, anchor.subjectID, anchor.at)
}

// realignAIAccountSubjectQuotaCycleTx repoints live cycle rows at the merged
// anchor. A row still carrying a dropped fragment's start would send the next
// read looking for a bucket that no longer exists, which reads as zero usage.
func realignAIAccountSubjectQuotaCycleTx(tx *sql.Tx, subjectID string, anchorAt time.Time) error {
	rows, err := tx.Query(`
		SELECT quota_key, cycle_start_at, window_seconds
		FROM ai_account_subject_quota_cycles
		WHERE auth_subject_id = ? AND window_seconds >= ?
	`, subjectID, aiAccountSubjectWeeklyWindowSeconds)
	if err != nil {
		return fmt.Errorf("usage: load shared quota cycles for realign: %w", err)
	}
	type realignTarget struct {
		quotaKey string
		window   int64
	}
	targets := make([]realignTarget, 0)
	for rows.Next() {
		var quotaKey string
		var start storedTime
		var window int64
		if err := rows.Scan(&quotaKey, &start, &window); err != nil {
			rows.Close()
			return fmt.Errorf("usage: scan shared quota cycle for realign: %w", err)
		}
		if !start.Valid || start.Time.UTC().Equal(anchorAt) {
			continue
		}
		if !sameAIAccountSubjectCycle(start.Time, anchorAt, window) {
			continue
		}
		targets = append(targets, realignTarget{quotaKey: quotaKey, window: window})
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}

	for _, target := range targets {
		resetAt := anchorAt.Add(time.Duration(target.window) * time.Second)
		if _, err := tx.Exec(`
			UPDATE ai_account_subject_quota_cycles
			SET cycle_start_at = ?, reset_at = ?
			WHERE auth_subject_id = ? AND quota_key = ?
		`, anchorAt.Format(time.RFC3339Nano), resetAt.Format(time.RFC3339Nano),
			subjectID, target.quotaKey); err != nil {
			return fmt.Errorf("usage: realign shared quota cycle: %w", err)
		}
	}
	return nil
}
