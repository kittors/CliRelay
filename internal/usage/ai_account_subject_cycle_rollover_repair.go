package usage

import (
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"time"

	log "github.com/sirupsen/logrus"
)

const (
	aiAccountSubjectCycleRolloverRepairMarker = "ai_account_subject_cycle_rollover_repair_v1"
	aiAccountSubjectCycleRefillRepairMarker   = "ai_account_subject_cycle_refill_repair_v1"
)

type meteredWeeklyQuotaProbe struct {
	resetAt       time.Time
	windowSeconds int64
	recordedAt    time.Time
}

type aiAccountSubjectCycleTotals struct {
	requests   int64
	successes  int64
	failures   int64
	cost       float64
	tokens     int64
	firstEvent time.Time
	lastEvent  time.Time
}

// RunAIAccountSubjectCycleRolloverRepairAtInit re-derives anchors that were held
// on a period the upstream had already ended, and rebuilds the usage bucket of
// the period they should have rolled into.
//
// Anchoring refused to leave a stored period before its own reset, to stop an
// idle quota bucket's "a full window remaining" countdown from re-keying the
// usage bucket on every probe. That also blocked the legitimate case: an upstream
// may end a period early. Production had a Codex account whose code_week window
// reported 41% spent against a reset on the 27th, then a full allowance against a
// reset on the 31st — a real new period, four days before the stored one was due.
// The anchor stayed on the old period, so the card kept showing the previous
// week's start while the new week's requests were added to the previous week's
// bucket: 12735 requests and $702 against a window the upstream said was 1% used.
//
// Anchoring now rolls on a metering probe, so no new anchor freezes. This repairs
// the ones already on disk rather than leaving operators to wait out a window
// whose totals stay wrong until it ends.
func RunAIAccountSubjectCycleRolloverRepairAtInit() error {
	return runAIAccountSubjectCycleRolloverRepairDB(getDB())
}

// RunAIAccountSubjectCycleRefillRepairAtInit repairs the anchors the metering rule
// could not: those held on a period the upstream refilled rather than spent out.
//
// A refilled period sits at a full allowance until the account draws on it, so the
// pass above — which only trusts metered probes — has no probe to roll them with,
// and the anchor stays frozen with the new period's usage landing in the old
// period's bucket for as long as the account leaves the fresh allowance alone.
func RunAIAccountSubjectCycleRefillRepairAtInit() error {
	return runAIAccountSubjectCycleRefillRepairDB(getDB())
}

func runAIAccountSubjectCycleRolloverRepairDB(db *sql.DB) error {
	return runAIAccountSubjectCycleAnchorRepairDB(db, aiAccountSubjectCycleRolloverRepairMarker,
		loadLatestMeteredWeeklyQuotaProbes, aiAccountSubjectQuotaObservation{metering: true})
}

func runAIAccountSubjectCycleRefillRepairDB(db *sql.DB) error {
	return runAIAccountSubjectCycleAnchorRepairDB(db, aiAccountSubjectCycleRefillRepairMarker,
		loadLatestWeeklyQuotaRefillProbes, aiAccountSubjectQuotaObservation{refilled: true})
}

// runAIAccountSubjectCycleAnchorRepairDB rolls stored anchors forward onto the
// period a probe shows is live, and rebuilds the usage buckets on both sides of
// the move. The probe loader and the observation it stands for are the only
// difference between the passes, so both judge with the same rule the running
// server uses and cannot disagree with it about which period an account is in.
func runAIAccountSubjectCycleAnchorRepairDB(
	db *sql.DB,
	marker string,
	loadProbes func(*sql.DB) (map[string]meteredWeeklyQuotaProbe, error),
	observed aiAccountSubjectQuotaObservation,
) error {
	if db == nil {
		return nil
	}

	usageProjectionMu.Lock()
	defer usageProjectionMu.Unlock()

	ensureUsageProjectionMarkerTable(db)
	if projectionMarkerValue(db, marker) == rollupMarkerDone {
		return nil
	}

	cyclesBySubject, err := loadAIAccountSubjectWeeklyCyclesBySubject(db)
	if err != nil {
		return err
	}
	probes, err := loadProbes(db)
	if err != nil {
		return err
	}

	subjectIDs := make([]string, 0, len(cyclesBySubject))
	for subjectID := range cyclesBySubject {
		subjectIDs = append(subjectIDs, subjectID)
	}
	sort.Strings(subjectIDs)

	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("usage: begin shared subject cycle rollover repair: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	now := time.Now().UTC()
	repaired := 0
	anchorsRolled := 0
	for _, subjectID := range subjectIDs {
		cycles := cyclesBySubject[subjectID]
		before, hadBefore := selectAIAccountSubjectWeeklyCycle(cycles)

		if !hadBefore {
			continue
		}
		rolled := false
		for i, cycle := range cycles {
			probe, ok := probes[aiAccountSubjectQuotaProbeKey(subjectID, cycle.QuotaKey)]
			if !ok {
				continue
			}
			next, ok := aiAccountSubjectCycleRolloverFromProbe(cycle, probe, observed)
			if !ok {
				continue
			}
			if err := writeAIAccountSubjectQuotaCycleAnchorTx(tx, next); err != nil {
				return err
			}
			cycles[i] = next
			rolled = true
		}
		if rolled {
			anchorsRolled++
		}
		after, hadAfter := selectAIAccountSubjectWeeklyCycle(cycles)
		if !hadAfter {
			continue
		}
		previousStart := before.CycleStartAt
		if !after.CycleStartAt.After(before.CycleStartAt) {
			// The anchor already names the period the probe describes, so there is
			// nothing to roll — but a frozen anchor rolls on its own the moment the
			// account spends into the fresh allowance, and everything it served while
			// frozen stays filed under the period before it. The bucket boundary is
			// then the only thing left wrong, and the period that kept those requests
			// is the one whose bucket precedes this period's.
			probe, ok := probes[aiAccountSubjectQuotaProbeKey(subjectID, after.QuotaKey)]
			if !ok || !aiAccountSubjectCycleMatchesProbe(after, probe) {
				continue
			}
			earlier, found, err := previousAIAccountSubjectCycleBucketStartTx(tx, subjectID, after.CycleStartAt)
			if err != nil {
				return err
			}
			if !found {
				continue
			}
			previousStart = earlier
		}
		moved, err := repairAIAccountSubjectCycleBucketsTx(tx, subjectID, previousStart, after.CycleStartAt, now)
		if err != nil {
			return err
		}
		if moved == 0 && !rolled {
			continue
		}
		repaired++
		log.WithFields(log.Fields{
			"auth_subject_id": subjectID,
			"marker":          marker,
			"from":            previousStart.Format(time.RFC3339),
			"to":              after.CycleStartAt.Format(time.RFC3339),
			"moved_requests":  moved,
		}).Info("usage: repaired a cycle anchor held on an ended period")
	}

	if _, err := tx.Exec(`
		INSERT INTO usage_projection_markers (marker_key, marker_value, updated_at)
		VALUES (?, ?, ?)
		ON CONFLICT(marker_key) DO UPDATE SET
			marker_value = excluded.marker_value,
			updated_at = excluded.updated_at
	`, marker, rollupMarkerDone, now.Format(time.RFC3339Nano)); err != nil {
		return fmt.Errorf("usage: mark shared subject cycle rollover repair: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("usage: commit shared subject cycle rollover repair: %w", err)
	}
	if repaired > 0 || anchorsRolled > 0 {
		// The projection keys its buckets off this cache; drop it so the next write
		// re-reads the anchors this pass rolled.
		resetAIAccountSubjectCycleCache()
	}
	return nil
}

// aiAccountSubjectCycleMatchesProbe reports whether a probe describes the period
// the stored anchor already names, which is what separates "the anchor rolled on
// its own and left usage behind" from "this probe has nothing to do with the
// stored period".
func aiAccountSubjectCycleMatchesProbe(cycle AIAccountSubjectQuotaCycle, probe meteredWeeklyQuotaProbe) bool {
	if probe.windowSeconds != cycle.WindowSeconds || probe.resetAt.IsZero() {
		return false
	}
	start := probe.resetAt.Add(-time.Duration(probe.windowSeconds) * time.Second)
	return sameAIAccountSubjectCycle(start, cycle.CycleStartAt, cycle.WindowSeconds)
}

// previousAIAccountSubjectCycleBucketStartTx finds the bucket a period's requests
// were being added to before the anchor named that period. Bucket starts are
// stored as UTC RFC3339, so ordering them as text orders them in time.
func previousAIAccountSubjectCycleBucketStartTx(
	tx *sql.Tx,
	subjectID string,
	before time.Time,
) (time.Time, bool, error) {
	var start storedTime
	err := tx.QueryRow(`
		SELECT bucket_start
		FROM ai_account_subject_usage_buckets
		WHERE auth_subject_id = ? AND bucket_kind = 'cycle' AND bucket_start < ?
		ORDER BY bucket_start DESC LIMIT 1
	`, subjectID, formatAIAccountSubjectCycleBucketStart(before)).Scan(&start)
	if err == sql.ErrNoRows {
		return time.Time{}, false, nil
	}
	if err != nil {
		return time.Time{}, false, fmt.Errorf("usage: load preceding cycle bucket: %w", err)
	}
	if !start.Valid {
		return time.Time{}, false, nil
	}
	return start.Time.UTC(), true, nil
}

func aiAccountSubjectQuotaProbeKey(subjectID, quotaKey string) string {
	return subjectID + "\x00" + quotaKey
}

// loadLatestMeteredWeeklyQuotaProbes keeps only probes that reported consumption.
// A window at a full allowance carries no evidence about where its period began,
// which is exactly the countdown the anchor must not follow.
func loadLatestMeteredWeeklyQuotaProbes(db *sql.DB) (map[string]meteredWeeklyQuotaProbe, error) {
	rows, err := db.Query(`
		SELECT auth_subject_id, quota_key, reset_at, window_seconds, recorded_at
		FROM ai_account_subject_quota_points
		WHERE window_seconds >= ? AND percent IS NOT NULL AND percent < 100
		ORDER BY recorded_at ASC
	`, aiAccountSubjectWeeklyWindowSeconds)
	if err != nil {
		return nil, fmt.Errorf("usage: query metered weekly quota probes: %w", err)
	}
	defer rows.Close()

	out := make(map[string]meteredWeeklyQuotaProbe)
	for rows.Next() {
		var subjectID, quotaKey string
		var reset, recorded storedTime
		var windowSeconds int64
		if err := rows.Scan(&subjectID, &quotaKey, &reset, &windowSeconds, &recorded); err != nil {
			return nil, fmt.Errorf("usage: scan metered weekly quota probe: %w", err)
		}
		subjectID = strings.TrimSpace(subjectID)
		quotaKey = strings.TrimSpace(quotaKey)
		if subjectID == "" || quotaKey == "" || !reset.Valid || !recorded.Valid || windowSeconds <= 0 {
			continue
		}
		// Ascending order means the last row written per key is the newest one.
		out[aiAccountSubjectQuotaProbeKey(subjectID, quotaKey)] = meteredWeeklyQuotaProbe{
			resetAt:       reset.Time.UTC(),
			windowSeconds: windowSeconds,
			recordedAt:    recorded.Time.UTC(),
		}
	}
	return out, rows.Err()
}

// loadLatestWeeklyQuotaRefillProbes keeps, per window, the newest probe that found
// a full allowance directly after a metered one.
//
// The metered-probe pass cannot reach a period that was refilled rather than spent
// out: its newest metered probe still describes the period that was consumed, which
// is the one already stored. The step up into a full allowance is the only record
// that the upstream ended that period, and it stays in the history for as long as
// the fresh allowance goes untouched.
//
// The step is tracked in Go rather than with a window function because this store
// runs on both SQLite and Postgres, and it reuses the running server's observation
// rule so a repaired anchor is one the server would also have rolled.
func loadLatestWeeklyQuotaRefillProbes(db *sql.DB) (map[string]meteredWeeklyQuotaProbe, error) {
	rows, err := db.Query(`
		SELECT auth_subject_id, quota_key, percent, reset_at, window_seconds, recorded_at
		FROM ai_account_subject_quota_points
		WHERE window_seconds >= ?
		ORDER BY recorded_at ASC
	`, aiAccountSubjectWeeklyWindowSeconds)
	if err != nil {
		return nil, fmt.Errorf("usage: query weekly quota refill probes: %w", err)
	}
	defer rows.Close()

	out := make(map[string]meteredWeeklyQuotaProbe)
	previousPercent := make(map[string]*float64)
	for rows.Next() {
		var subjectID, quotaKey string
		var percent sql.NullFloat64
		var reset, recorded storedTime
		var windowSeconds int64
		if err := rows.Scan(&subjectID, &quotaKey, &percent, &reset, &windowSeconds, &recorded); err != nil {
			return nil, fmt.Errorf("usage: scan weekly quota refill probe: %w", err)
		}
		subjectID = strings.TrimSpace(subjectID)
		quotaKey = strings.TrimSpace(quotaKey)
		if subjectID == "" || quotaKey == "" || windowSeconds <= 0 {
			continue
		}
		key := aiAccountSubjectQuotaProbeKey(subjectID, quotaKey)
		current := nullableProbePercent(percent)
		observed := newAIAccountSubjectQuotaObservation(previousPercent[key], current)
		previousPercent[key] = current
		if !observed.refilled || !reset.Valid || !recorded.Valid {
			continue
		}
		// Ascending order means a later refill replaces an earlier one.
		out[key] = meteredWeeklyQuotaProbe{
			resetAt:       reset.Time.UTC(),
			windowSeconds: windowSeconds,
			recordedAt:    recorded.Time.UTC(),
		}
	}
	return out, rows.Err()
}

// aiAccountSubjectCycleRolloverFromProbe applies the live anchoring rule to a
// stored row, so the repair and the running server cannot disagree about which
// period an account is in.
func aiAccountSubjectCycleRolloverFromProbe(
	stored AIAccountSubjectQuotaCycle,
	probe meteredWeeklyQuotaProbe,
	observed aiAccountSubjectQuotaObservation,
) (AIAccountSubjectQuotaCycle, bool) {
	if stored.CycleStartAt.IsZero() || probe.windowSeconds != stored.WindowSeconds {
		return stored, false
	}
	incoming := stored
	incoming.CycleStartAt = probe.resetAt.Add(-time.Duration(probe.windowSeconds) * time.Second)
	incoming.ResetAt = probe.resetAt
	// The rule reads LastVerifiedAt as "when this observation was made", so judge
	// with the probe's own timestamp.
	incoming.LastVerifiedAt = probe.recordedAt
	// Only ever forward: a probe older than the stored anchor must not rewind a
	// period the server has already rolled into.
	if !incoming.CycleStartAt.After(stored.CycleStartAt) {
		return stored, false
	}
	if sameAIAccountSubjectCycle(incoming.CycleStartAt, stored.CycleStartAt, stored.WindowSeconds) {
		return stored, false
	}
	if !aiAccountSubjectCycleRollover(incoming, stored.ResetAt, observed) {
		return stored, false
	}
	// The metering probe a period is derived from can be older than the last probe
	// of any kind, and the stored verification time must not travel backwards.
	if stored.LastVerifiedAt.After(incoming.LastVerifiedAt) {
		incoming.LastVerifiedAt = stored.LastVerifiedAt
	}
	return incoming, true
}

func writeAIAccountSubjectQuotaCycleAnchorTx(tx *sql.Tx, cycle AIAccountSubjectQuotaCycle) error {
	if _, err := tx.Exec(`
		UPDATE ai_account_subject_quota_cycles
		SET cycle_start_at = ?, reset_at = ?, last_verified_at = ?
		WHERE auth_subject_id = ? AND quota_key = ?
	`, cycle.CycleStartAt.UTC().Format(time.RFC3339Nano),
		cycle.ResetAt.UTC().Format(time.RFC3339Nano),
		cycle.LastVerifiedAt.UTC().Format(time.RFC3339Nano),
		cycle.AuthSubjectID, cycle.QuotaKey); err != nil {
		return fmt.Errorf("usage: roll stored shared quota cycle: %w", err)
	}
	return nil
}

// repairAIAccountSubjectCycleBucketsTx moves the new period's usage out of the
// bucket the frozen anchor kept it in.
//
// The request log is the only record with the per-request timestamps needed to
// split them, so the new period is recomputed from it and only what its bucket
// was still missing is taken back off the previous one. Whatever the log no longer
// covers stays where it is: totals move between buckets but are never invented or
// lost.
//
// Charging the previous period the whole recomputed total would double-count a
// period that has already been partly moved, and that is the common case rather
// than a corner one: an anchor frozen on a refilled period rolls on its own as
// soon as the account spends into the fresh allowance, so its bucket already holds
// everything after the rollover and only the requests from before it are still
// misfiled. Production, 2026-08-25: a Codex account's new period held 6 requests
// against 162 in the log, the other 156 left behind in the previous week.
//
// Recomputing to a smaller total means the log has aged out from under the bucket,
// so the bucket is left alone: a repair may complete a period, never truncate one.
func repairAIAccountSubjectCycleBucketsTx(
	tx *sql.Tx,
	subjectID string,
	previousStart, newStart, now time.Time,
) (int64, error) {
	totals, err := sumAIAccountSubjectRequestLogsTx(tx, subjectID, newStart, now)
	if err != nil {
		return 0, err
	}
	if totals.requests == 0 {
		return 0, nil
	}
	stored, err := readAIAccountSubjectCycleBucketTotalsTx(tx, subjectID, newStart)
	if err != nil {
		return 0, err
	}
	if totals.requests <= stored.requests {
		return 0, nil
	}
	missing := aiAccountSubjectCycleTotalsDifference(totals, stored)
	if err := writeAIAccountSubjectCycleBucketTx(tx, subjectID, newStart, totals); err != nil {
		return 0, err
	}
	if err := subtractAIAccountSubjectCycleBucketTx(tx, subjectID, previousStart, missing); err != nil {
		return 0, err
	}
	return missing.requests, nil
}

func readAIAccountSubjectCycleBucketTotalsTx(
	tx *sql.Tx,
	subjectID string,
	start time.Time,
) (aiAccountSubjectCycleTotals, error) {
	var totals aiAccountSubjectCycleTotals
	err := tx.QueryRow(`
		SELECT request_count, success_count, failure_count, cost_total, total_tokens
		FROM ai_account_subject_usage_buckets
		WHERE auth_subject_id = ? AND bucket_kind = 'cycle' AND bucket_start = ?
	`, subjectID, formatAIAccountSubjectCycleBucketStart(start)).
		Scan(&totals.requests, &totals.successes, &totals.failures, &totals.cost, &totals.tokens)
	if err == sql.ErrNoRows {
		return aiAccountSubjectCycleTotals{}, nil
	}
	if err != nil {
		return totals, fmt.Errorf("usage: load repaired cycle bucket totals: %w", err)
	}
	return totals, nil
}

func aiAccountSubjectCycleTotalsDifference(from, subtract aiAccountSubjectCycleTotals) aiAccountSubjectCycleTotals {
	return aiAccountSubjectCycleTotals{
		requests:  clampNonNegativeInt64(from.requests - subtract.requests),
		successes: clampNonNegativeInt64(from.successes - subtract.successes),
		failures:  clampNonNegativeInt64(from.failures - subtract.failures),
		cost:      clampNonNegativeFloat64(from.cost - subtract.cost),
		tokens:    clampNonNegativeInt64(from.tokens - subtract.tokens),
	}
}

func sumAIAccountSubjectRequestLogsTx(
	tx *sql.Tx,
	subjectID string,
	from, to time.Time,
) (aiAccountSubjectCycleTotals, error) {
	var totals aiAccountSubjectCycleTotals
	var cost sql.NullFloat64
	var tokens sql.NullInt64
	var successes, failures sql.NullInt64
	var first, last storedTime
	err := tx.QueryRow(`
		SELECT COUNT(*),
			SUM(CASE WHEN failed = 0 THEN 1 ELSE 0 END),
			SUM(CASE WHEN failed = 0 THEN 0 ELSE 1 END),
			SUM(cost), SUM(total_tokens), MIN(timestamp), MAX(timestamp)
		FROM request_logs
		WHERE auth_subject_id = ? AND timestamp >= ? AND timestamp <= ?
	`, subjectID, from.UTC().Format(time.RFC3339Nano), to.UTC().Format(time.RFC3339Nano)).
		Scan(&totals.requests, &successes, &failures, &cost, &tokens, &first, &last)
	if err != nil {
		return totals, fmt.Errorf("usage: sum shared subject cycle request logs: %w", err)
	}
	totals.successes = successes.Int64
	totals.failures = failures.Int64
	totals.cost = cost.Float64
	totals.tokens = tokens.Int64
	if first.Valid {
		totals.firstEvent = first.Time.UTC()
	}
	if last.Valid {
		totals.lastEvent = last.Time.UTC()
	}
	return totals, nil
}

func writeAIAccountSubjectCycleBucketTx(
	tx *sql.Tx,
	subjectID string,
	start time.Time,
	totals aiAccountSubjectCycleTotals,
) error {
	firstEvent := totals.firstEvent
	if firstEvent.IsZero() {
		firstEvent = start
	}
	updatedAt := totals.lastEvent
	if updatedAt.IsZero() {
		updatedAt = start
	}
	// Overwrite rather than accumulate: the request log is authoritative for this
	// period, so a rerun that lands on an existing bucket must not double it.
	if _, err := tx.Exec(`
		INSERT INTO ai_account_subject_usage_buckets (
			auth_subject_id, bucket_kind, bucket_start, request_count,
			success_count, failure_count, cost_total, total_tokens, first_event_at, updated_at
		) VALUES (?, 'cycle', ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(auth_subject_id, bucket_kind, bucket_start) DO UPDATE SET
			request_count = excluded.request_count,
			success_count = excluded.success_count,
			failure_count = excluded.failure_count,
			cost_total = excluded.cost_total,
			total_tokens = excluded.total_tokens,
			first_event_at = excluded.first_event_at,
			updated_at = excluded.updated_at
	`, subjectID, formatAIAccountSubjectCycleBucketStart(start),
		totals.requests, totals.successes, totals.failures, totals.cost, totals.tokens,
		firstEvent.Format(time.RFC3339Nano), updatedAt.Format(time.RFC3339Nano)); err != nil {
		return fmt.Errorf("usage: write repaired cycle bucket: %w", err)
	}
	return nil
}

// subtractAIAccountSubjectCycleBucketTx clamps in Go rather than in SQL: the
// greatest-of-two spelling differs between the SQLite and Postgres backends this
// store runs on.
func subtractAIAccountSubjectCycleBucketTx(
	tx *sql.Tx,
	subjectID string,
	start time.Time,
	totals aiAccountSubjectCycleTotals,
) error {
	key := formatAIAccountSubjectCycleBucketStart(start)
	var requests, successes, failures, tokens int64
	var cost float64
	err := tx.QueryRow(`
		SELECT request_count, success_count, failure_count, cost_total, total_tokens
		FROM ai_account_subject_usage_buckets
		WHERE auth_subject_id = ? AND bucket_kind = 'cycle' AND bucket_start = ?
	`, subjectID, key).Scan(&requests, &successes, &failures, &cost, &tokens)
	if err == sql.ErrNoRows {
		return nil
	}
	if err != nil {
		return fmt.Errorf("usage: load previous cycle bucket for repair: %w", err)
	}
	if _, err := tx.Exec(`
		UPDATE ai_account_subject_usage_buckets
		SET request_count = ?, success_count = ?, failure_count = ?, cost_total = ?, total_tokens = ?
		WHERE auth_subject_id = ? AND bucket_kind = 'cycle' AND bucket_start = ?
	`, clampNonNegativeInt64(requests-totals.requests),
		clampNonNegativeInt64(successes-totals.successes),
		clampNonNegativeInt64(failures-totals.failures),
		clampNonNegativeFloat64(cost-totals.cost),
		clampNonNegativeInt64(tokens-totals.tokens),
		subjectID, key); err != nil {
		return fmt.Errorf("usage: subtract repaired usage from previous cycle bucket: %w", err)
	}
	return nil
}

func clampNonNegativeInt64(value int64) int64 {
	if value < 0 {
		return 0
	}
	return value
}

func clampNonNegativeFloat64(value float64) float64 {
	if value < 0 {
		return 0
	}
	return value
}
