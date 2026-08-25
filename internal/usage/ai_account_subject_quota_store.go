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
		// The previous probe is loaded before anchoring, not only for the heartbeat
		// de-duplication below: a period's end is a transition between two probes —
		// a window that was metering and now reports a full allowance has been reset
		// upstream — and a single probe cannot show it.
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
		var previousPercent *float64
		if err == nil {
			previousPercent = nullableProbePercent(latestPercent)
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
			anchored, upsertErr := upsertAIAccountSubjectQuotaCycleTx(tx, cycle,
				newAIAccountSubjectQuotaObservation(previousPercent, point.Percent))
			if upsertErr != nil {
				return upsertErr
			}
			cycles = append(cycles, anchored)
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
func anchorAIAccountSubjectQuotaCycleTx(tx *sql.Tx, cycle AIAccountSubjectQuotaCycle, observed aiAccountSubjectQuotaObservation) (AIAccountSubjectQuotaCycle, error) {
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
	storedReset := time.Time{}
	if reset.Valid {
		storedReset = reset.Time.UTC()
	}
	if !sameAIAccountSubjectCycle(cycle.CycleStartAt, start.Time, cycle.WindowSeconds) &&
		aiAccountSubjectCycleRollover(cycle, storedReset, observed) {
		return cycle, nil
	}
	cycle.CycleStartAt = start.Time.UTC()
	if !storedReset.IsZero() {
		// Keep reset_at consistent with the anchor so the card countdown stops
		// jittering by a second or two on every probe.
		cycle.ResetAt = storedReset
	}
	return cycle, nil
}

// aiAccountSubjectCycleRollover reports whether a probe describes a genuinely new
// period rather than the stored one seen through a moving countdown.
//
// Drift tolerance alone was not enough. A quota bucket the account has not touched
// is answered as "a full window remaining", so its reset — and the start derived
// from it — advances by however long has passed since the last probe. Hours of
// that is far outside any tolerance, yet nothing has rolled: it is the same
// untouched period. Treating each probe as a new period is what re-keyed the
// usage bucket every few hours and scattered one week's requests across six of
// them.
//
// A period may therefore be replaced when the upstream shows the old one is
// finished: the probe ran after it ended, the new period starts where it ended,
// or the reset moved closer (credits reset, plan change).
//
// It may also be replaced when the probe is metering. An upstream can end a
// period early — production had a Codex account report 41% of code_week spent
// against a reset four days out, then, hours later, a full allowance against a
// reset a week past the old one. That is a real new period arriving before the
// stored one was due, and holding the old anchor froze the card on the previous
// week while its usage kept accruing into the previous week's bucket. A window
// that reports consumption is one the upstream is actually counting, so its
// reset is a real period edge rather than an idle bucket's countdown.
//
// Metering alone still froze the moment it was meant to catch. A period that has
// just been refilled is at a full allowance by definition, so the probe that first
// sees the new period never meters, and rollover waited for the account to spend
// into it. Production, 2026-08-25: two Codex accounts had code_week drop from 55%
// and 70% to a full allowance against resets a week past the stored ones, and both
// anchors stayed on the period that started on the 24th — one card counted 12311
// requests and 1.4B tokens of two periods added together.
//
// The refill itself is therefore the evidence, and it is checked before the
// countdown guard because the two are indistinguishable by shape: of those two
// accounts one reported the new period against a fixed edge and the other against
// "this instant plus a window", which is exactly what an untouched bucket reports.
// Only the transition out of metering separates a period that just began from one
// that never started.
func aiAccountSubjectCycleRollover(incoming AIAccountSubjectQuotaCycle, storedReset time.Time, observed aiAccountSubjectQuotaObservation) bool {
	if storedReset.IsZero() {
		return true
	}
	if !incoming.LastVerifiedAt.IsZero() && !incoming.LastVerifiedAt.UTC().Before(storedReset) {
		return true
	}
	if sameAIAccountSubjectCycle(incoming.CycleStartAt, storedReset, incoming.WindowSeconds) {
		return true
	}
	if incoming.ResetAt.UTC().Before(storedReset) {
		return true
	}
	if observed.refilled {
		return true
	}
	return observed.metering && !aiAccountSubjectCycleFullWindowCountdown(incoming)
}

// aiAccountSubjectQuotaObservation is what two consecutive probes say about one
// window. Anchoring is judged on the pair because the end of a period is a
// transition — no single probe carries it.
type aiAccountSubjectQuotaObservation struct {
	// metering reports that this probe shows consumption drawn from the period it
	// describes, which makes its reset a real period edge.
	metering bool
	// refilled reports that the previous probe was metering and this one is back to
	// a full allowance: the upstream ended the period it had been counting.
	refilled bool
}

func newAIAccountSubjectQuotaObservation(previous, current *float64) aiAccountSubjectQuotaObservation {
	return aiAccountSubjectQuotaObservation{
		metering: aiAccountSubjectQuotaWindowMetering(current),
		refilled: aiAccountSubjectQuotaWindowMetering(previous) && current != nil && *current >= 100,
	}
}

// nullableProbePercent adapts a stored percentage for comparison. It is absent
// both when no probe has been recorded yet and when the upstream reports no
// percentage, and neither case says anything about a period ending.
func nullableProbePercent(stored sql.NullFloat64) *float64 {
	if !stored.Valid {
		return nil
	}
	value := stored.Float64
	return &value
}

// aiAccountSubjectQuotaWindowMetering reports whether a probe shows the window
// counting usage. Percent is the share remaining, so 100 (or an upstream that
// reports no percentage at all) means nothing has been drawn from the period the
// probe describes, and its reset carries no evidence about where that period began.
func aiAccountSubjectQuotaWindowMetering(percent *float64) bool {
	return percent != nil && *percent < 100
}

// aiAccountSubjectCycleFullWindowCountdown reports whether the probe's reset is
// simply "one full window from this probe".
//
// A metering percentage alone would still admit an upstream that answers every
// probe with a whole window remaining; the derived start would then track the
// probe clock and re-key the usage bucket on each pass. A genuine period began
// before the probe that observed it, so its derived start sits behind the probe
// by however long the period has been running. The slack is small on purpose:
// an idle bucket's countdown lands on the probe instant to the second, and a real
// period caught within the slack of its own start is recognised on the next probe,
// once the clock has moved on and the reset has not.
func aiAccountSubjectCycleFullWindowCountdown(incoming AIAccountSubjectQuotaCycle) bool {
	if incoming.LastVerifiedAt.IsZero() {
		return false
	}
	drift := absDuration(incoming.CycleStartAt.UTC().Sub(incoming.LastVerifiedAt.UTC()))
	return drift <= aiAccountSubjectFullWindowCountdownSlack
}

const aiAccountSubjectFullWindowCountdownSlack = 2 * time.Minute

func upsertAIAccountSubjectQuotaCycleTx(tx *sql.Tx, cycle AIAccountSubjectQuotaCycle, observed aiAccountSubjectQuotaObservation) (AIAccountSubjectQuotaCycle, error) {
	anchored, err := anchorAIAccountSubjectQuotaCycleTx(tx, cycle, observed)
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

// QueryLatestAIAccountSubjectWeeklyCyclesBatch resolves each subject's cycle
// anchor. Candidate filtering deliberately happens in
// selectAIAccountSubjectWeeklyCycle rather than in SQL: the preference is
// per-provider, and a single IN list built from a mixed batch dropped every
// window belonging to a provider that names its buckets differently — with the
// list holding "seven_day" for Claude, antigravity's own weekly rows never came
// back and its accounts reported no cycle at all.
func QueryLatestAIAccountSubjectWeeklyCyclesBatch(subjectIDs []string) (map[string]time.Time, error) {
	db := getReadDB()
	ids := dedupeExactStrings(subjectIDs)
	out := make(map[string]time.Time)
	if db == nil || len(ids) == 0 {
		return out, nil
	}
	args := make([]any, 0, len(ids)+1)
	for _, id := range ids {
		args = append(args, id)
	}
	args = append(args, aiAccountSubjectWeeklyWindowSeconds)
	query := `
		SELECT auth_subject_id, provider, quota_key, cycle_start_at, reset_at, window_seconds, last_verified_at
		FROM ai_account_subject_quota_cycles
		WHERE auth_subject_id IN (` + strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",") + `)
		  AND window_seconds >= ?`
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
	now := time.Now().UTC()
	for subjectID, cycles := range cyclesBySubject {
		if cycle, ok := selectAIAccountSubjectWeeklyCycle(cycles); ok {
			out[subjectID] = advanceAIAccountSubjectCycleTo(cycle, now).CycleStartAt
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
