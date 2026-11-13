package usage

import (
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"time"
)

const aiAccountSubjectCycleRealignMarker = "ai_account_subject_cycle_realign_v1"

// RunAIAccountSubjectCycleRealignAtInit folds every usage bucket that belongs to
// the live period onto that period's anchor.
//
// The earlier merge (see ai_account_subject_cycle_merge.go) clustered buckets by
// drift tolerance, which only reunites fragments minutes apart. It could not
// repair the fragmentation a sliding reset causes: a quota bucket the account has
// not touched is answered as "a full window remaining", so its derived start
// walked forward by hours on every probe and each step opened a bucket of its own.
// Production carried one weekly period split across six buckets — 545 / 13 / 7 /
// 17 / 487 / 15 requests — none of which matched the anchor the readers resolved,
// so a card for an account with 1084 lifetime calls reported 0 for the period.
//
// Anchoring now refuses to roll a period that has not ended, so no new fragments
// appear. This repairs the ones already on disk instead of making operators wait
// out a full window for correct totals.
func RunAIAccountSubjectCycleRealignAtInit() error {
	usageWriteLease.RLock()
	defer usageWriteLease.RUnlock()
	return runAIAccountSubjectCycleRealignDBLeased(getDB())
}

func runAIAccountSubjectCycleRealignDB(db *sql.DB) error {
	usageWriteLease.RLock()
	defer usageWriteLease.RUnlock()
	return runAIAccountSubjectCycleRealignDBLeased(db)
}

func runAIAccountSubjectCycleRealignDBLeased(db *sql.DB) error {
	if db == nil {
		return nil
	}

	usageProjectionMu.Lock()
	defer usageProjectionMu.Unlock()

	ensureUsageProjectionMarkerTable(db)
	if projectionMarkerValue(db, aiAccountSubjectCycleRealignMarker) == rollupMarkerDone {
		return nil
	}

	anchors, err := loadAIAccountSubjectCycleAnchors(db)
	if err != nil {
		return err
	}
	buckets, err := loadAIAccountSubjectCycleBuckets(db)
	if err != nil {
		return err
	}
	bySubject := make(map[string][]aiAccountSubjectCycleBucketRow, len(anchors))
	for _, bucket := range buckets {
		if _, ok := anchors[bucket.subjectID]; !ok {
			continue
		}
		bySubject[bucket.subjectID] = append(bySubject[bucket.subjectID], bucket)
	}

	subjectIDs := make([]string, 0, len(bySubject))
	for subjectID := range bySubject {
		subjectIDs = append(subjectIDs, subjectID)
	}
	sort.Strings(subjectIDs)

	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("usage: begin shared subject cycle realign: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	for _, subjectID := range subjectIDs {
		anchor := anchors[subjectID]
		group := aiAccountSubjectCycleBucketsInPeriod(bySubject[subjectID], anchor)
		if len(group) == 0 {
			continue
		}
		// A lone bucket still needs the pass when it is keyed to a start the
		// readers no longer resolve — one misplaced bucket reads as zero just as
		// surely as six do.
		anchorKey := formatAIAccountSubjectCycleBucketStart(anchor.CycleStartAt)
		if len(group) == 1 && group[0].start == anchorKey {
			continue
		}
		if err := foldAIAccountSubjectCycleBucketsTx(tx, anchor.CycleStartAt, group); err != nil {
			return err
		}
	}

	now := time.Now().UTC()
	if _, err := tx.Exec(`
		INSERT INTO usage_projection_markers (marker_key, marker_value, updated_at)
		VALUES (?, ?, ?)
		ON CONFLICT(marker_key) DO UPDATE SET
			marker_value = excluded.marker_value,
			updated_at = excluded.updated_at
	`, aiAccountSubjectCycleRealignMarker, rollupMarkerDone, now.Format(time.RFC3339Nano)); err != nil {
		return fmt.Errorf("usage: mark shared subject cycle realign: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("usage: commit shared subject cycle realign: %w", err)
	}
	// The projection keys its buckets off this cache; drop it so the next write
	// re-reads the anchors this pass just folded onto.
	resetAIAccountSubjectCycleCache()
	return nil
}

// realignAuthSubjectCycleBucketsTx runs the same repair for one subject inside a
// caller's transaction. Merging two tenants' halves of an account brings together
// buckets anchored independently, which is the fragmented shape all over again.
func realignAuthSubjectCycleBucketsTx(tx *sql.Tx, subjectID string) error {
	subjectID = strings.TrimSpace(subjectID)
	if subjectID == "" {
		return nil
	}
	anchor, ok, err := loadAuthSubjectCycleAnchorTx(tx, subjectID)
	if err != nil || !ok {
		return err
	}
	buckets, err := loadAuthSubjectCycleBucketsTx(tx, subjectID)
	if err != nil {
		return err
	}
	group := aiAccountSubjectCycleBucketsInPeriod(buckets, anchor)
	if len(group) == 0 {
		return nil
	}
	anchorKey := formatAIAccountSubjectCycleBucketStart(anchor.CycleStartAt)
	if len(group) == 1 && group[0].start == anchorKey {
		return nil
	}
	return foldAIAccountSubjectCycleBucketsTx(tx, anchor.CycleStartAt, group)
}

func loadAuthSubjectCycleAnchorTx(tx *sql.Tx, subjectID string) (AIAccountSubjectQuotaCycle, bool, error) {
	rows, err := tx.Query(`
		SELECT auth_subject_id, provider, quota_key, cycle_start_at, reset_at, window_seconds, last_verified_at
		FROM ai_account_subject_quota_cycles
		WHERE auth_subject_id = ? AND window_seconds >= ?
	`, subjectID, aiAccountSubjectWeeklyWindowSeconds)
	if err != nil {
		return AIAccountSubjectQuotaCycle{}, false, fmt.Errorf("usage: query subject quota cycles for realign: %w", err)
	}
	defer rows.Close()
	cycles := make([]AIAccountSubjectQuotaCycle, 0, 4)
	for rows.Next() {
		var cycle AIAccountSubjectQuotaCycle
		var start, reset, verified storedTime
		if err := rows.Scan(&cycle.AuthSubjectID, &cycle.Provider, &cycle.QuotaKey, &start, &reset, &cycle.WindowSeconds, &verified); err != nil {
			return AIAccountSubjectQuotaCycle{}, false, fmt.Errorf("usage: scan subject quota cycle for realign: %w", err)
		}
		if start.Valid {
			cycle.CycleStartAt = start.Time.UTC()
		}
		if reset.Valid {
			cycle.ResetAt = reset.Time.UTC()
		}
		if verified.Valid {
			cycle.LastVerifiedAt = verified.Time.UTC()
		}
		cycles = append(cycles, cycle)
	}
	if err := rows.Err(); err != nil {
		return AIAccountSubjectQuotaCycle{}, false, err
	}
	cycle, ok := selectAIAccountSubjectWeeklyCycle(cycles)
	return cycle, ok, nil
}

func loadAuthSubjectCycleBucketsTx(tx *sql.Tx, subjectID string) ([]aiAccountSubjectCycleBucketRow, error) {
	rows, err := tx.Query(`
		SELECT auth_subject_id, bucket_start, request_count, success_count, failure_count,
			cost_total, total_tokens, first_event_at, updated_at
		FROM ai_account_subject_usage_buckets
		WHERE bucket_kind = 'cycle' AND auth_subject_id = ?
	`, subjectID)
	if err != nil {
		return nil, fmt.Errorf("usage: query subject cycle buckets for realign: %w", err)
	}
	defer rows.Close()
	return scanAIAccountSubjectCycleBuckets(rows)
}

// loadAIAccountSubjectCycleAnchors resolves each subject's live period through the
// same selection the projection and the readers use, so the repair lands on the
// bucket key they will look for.
func loadAIAccountSubjectCycleAnchors(db *sql.DB) (map[string]AIAccountSubjectQuotaCycle, error) {
	bySubject, err := loadAIAccountSubjectWeeklyCyclesBySubject(db)
	if err != nil {
		return nil, err
	}

	out := make(map[string]AIAccountSubjectQuotaCycle, len(bySubject))
	for subjectID, cycles := range bySubject {
		if cycle, ok := selectAIAccountSubjectWeeklyCycle(cycles); ok {
			out[subjectID] = cycle
		}
	}
	return out, nil
}

// loadAIAccountSubjectWeeklyCyclesBySubject returns every stored weekly window,
// grouped by subject. Repairs that re-derive an anchor need all of a subject's
// windows, not just the one currently selected: correcting a single window can
// change which one the selection resolves to.
func loadAIAccountSubjectWeeklyCyclesBySubject(db *sql.DB) (map[string][]AIAccountSubjectQuotaCycle, error) {
	rows, err := db.Query(`
		SELECT auth_subject_id, provider, quota_key, cycle_start_at, reset_at, window_seconds, last_verified_at
		FROM ai_account_subject_quota_cycles
		WHERE window_seconds >= ?
	`, aiAccountSubjectWeeklyWindowSeconds)
	if err != nil {
		return nil, fmt.Errorf("usage: query shared quota cycles: %w", err)
	}
	defer rows.Close()

	bySubject := make(map[string][]AIAccountSubjectQuotaCycle)
	for rows.Next() {
		var cycle AIAccountSubjectQuotaCycle
		var start, reset, verified storedTime
		if err := rows.Scan(&cycle.AuthSubjectID, &cycle.Provider, &cycle.QuotaKey, &start, &reset, &cycle.WindowSeconds, &verified); err != nil {
			return nil, fmt.Errorf("usage: scan shared quota cycle: %w", err)
		}
		cycle.AuthSubjectID = strings.TrimSpace(cycle.AuthSubjectID)
		if cycle.AuthSubjectID == "" {
			continue
		}
		if start.Valid {
			cycle.CycleStartAt = start.Time.UTC()
		}
		if reset.Valid {
			cycle.ResetAt = reset.Time.UTC()
		}
		if verified.Valid {
			cycle.LastVerifiedAt = verified.Time.UTC()
		}
		bySubject[cycle.AuthSubjectID] = append(bySubject[cycle.AuthSubjectID], cycle)
	}
	return bySubject, rows.Err()
}

// aiAccountSubjectCycleBucketsInPeriod returns the buckets whose key falls inside
// the live period, oldest first. A fragment's key is the start the writer believed
// in, and every fragment of one period sits between that period's start and its
// reset; buckets outside it belong to earlier periods and are left alone.
func aiAccountSubjectCycleBucketsInPeriod(
	buckets []aiAccountSubjectCycleBucketRow,
	anchor AIAccountSubjectQuotaCycle,
) []aiAccountSubjectCycleBucketRow {
	if anchor.CycleStartAt.IsZero() || anchor.ResetAt.IsZero() {
		return nil
	}
	// The anchor itself may sit a tolerance away from the fragment that recorded
	// the period's true beginning, so admit that much slack on the lower edge.
	from := anchor.CycleStartAt.Add(-aiAccountSubjectCycleDriftTolerance(anchor.WindowSeconds))
	out := make([]aiAccountSubjectCycleBucketRow, 0, len(buckets))
	for _, bucket := range buckets {
		if bucket.at.Before(from) || !bucket.at.Before(anchor.ResetAt) {
			continue
		}
		out = append(out, bucket)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].at.Before(out[j].at) })
	return out
}

// foldAIAccountSubjectCycleBucketsTx rewrites a period's fragments as one bucket
// keyed to the anchor the readers resolve.
func foldAIAccountSubjectCycleBucketsTx(
	tx *sql.Tx,
	anchorAt time.Time,
	group []aiAccountSubjectCycleBucketRow,
) error {
	subjectID := group[0].subjectID
	anchorKey := formatAIAccountSubjectCycleBucketStart(anchorAt)

	merged := aiAccountSubjectCycleBucketRow{subjectID: subjectID, start: anchorKey, at: anchorAt}
	for _, bucket := range group {
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
		merged.firstEvent = group[0].at
	}
	if merged.updatedAt.IsZero() {
		merged.updatedAt = group[len(group)-1].at
	}

	for _, bucket := range group {
		if _, err := tx.Exec(`
			DELETE FROM ai_account_subject_usage_buckets
			WHERE auth_subject_id = ? AND bucket_kind = 'cycle' AND bucket_start = ?
		`, bucket.subjectID, bucket.start); err != nil {
			return fmt.Errorf("usage: drop realigned cycle bucket: %w", err)
		}
	}
	// Deleting first and inserting once keeps this correct whether or not the
	// anchor's own key was among the fragments.
	if _, err := tx.Exec(`
		INSERT INTO ai_account_subject_usage_buckets (
			auth_subject_id, bucket_kind, bucket_start, request_count,
			success_count, failure_count, cost_total, total_tokens, first_event_at, updated_at
		) VALUES (?, 'cycle', ?, ?, ?, ?, ?, ?, ?, ?)
	`, subjectID, anchorKey, merged.requests, merged.successes, merged.failures,
		merged.cost, merged.tokens,
		merged.firstEvent.UTC().Format(time.RFC3339Nano),
		merged.updatedAt.UTC().Format(time.RFC3339Nano)); err != nil {
		return fmt.Errorf("usage: write realigned cycle bucket: %w", err)
	}
	return nil
}
