package usage

import (
	"testing"
	"time"
)

const codexWeeklyQuotaKey = "code_week"

func readCycleAnchor(t *testing.T, subjectID, quotaKey string) (time.Time, time.Time) {
	t.Helper()
	var start, reset storedTime
	if err := getDB().QueryRow(`
		SELECT cycle_start_at, reset_at FROM ai_account_subject_quota_cycles
		WHERE auth_subject_id = ? AND quota_key = ?
	`, subjectID, quotaKey).Scan(&start, &reset); err != nil {
		t.Fatal(err)
	}
	if !start.Valid || !reset.Valid {
		t.Fatalf("anchor for %s/%s is not stored", subjectID, quotaKey)
	}
	return start.Time.UTC(), reset.Time.UTC()
}

func readCycleBucket(t *testing.T, subjectID string, start time.Time) (int64, float64, int64) {
	t.Helper()
	var requests, tokens int64
	var cost float64
	err := getDB().QueryRow(`
		SELECT request_count, cost_total, total_tokens
		FROM ai_account_subject_usage_buckets
		WHERE auth_subject_id = ? AND bucket_kind = 'cycle' AND bucket_start = ?
	`, subjectID, formatAIAccountSubjectCycleBucketStart(start)).Scan(&requests, &cost, &tokens)
	if err != nil {
		t.Fatalf("read cycle bucket at %v: %v", start, err)
	}
	return requests, cost, tokens
}

// Production, 2026-08-24: a Codex account reported 41% of code_week spent against
// a reset on the 27th, then hours later a full allowance against a reset on the
// 31st — the upstream ended the period four days before it was due. Refusing to
// leave a period until its own reset froze the anchor on the previous week: the
// card kept showing the old start while the new week's requests kept landing in
// the old week's bucket, so 12735 requests and $702 were reported for a window
// the same probe said was 1% used.
func TestWeeklyCycleRollsWhenUpstreamEndsPeriodEarly(t *testing.T) {
	initSharedSubjectTestDB(t)
	subjectID := "authsub_codex_early_reset"

	firstProbe := time.Now().UTC().Truncate(time.Second).Add(-8 * time.Hour)
	firstReset := firstProbe.Add(76 * time.Hour)
	spent := 59.0
	if err := RecordAIAccountSubjectQuotaPoints(subjectID, "codex", []QuotaSnapshotPoint{{
		RecordedAt: firstProbe, Provider: "codex", QuotaKey: codexWeeklyQuotaKey, QuotaLabel: "Weekly",
		Percent: &spent, ResetAt: &firstReset, WindowSeconds: weeklyWindowSeconds,
	}}); err != nil {
		t.Fatal(err)
	}

	// The allowance came back before the stored reset, against a reset a week past
	// it, and the account has already drawn on the new period.
	secondProbe := firstProbe.Add(7 * time.Hour)
	secondReset := firstReset.Add(4*24*time.Hour - 45*time.Minute)
	fresh := 99.0
	if err := RecordAIAccountSubjectQuotaPoints(subjectID, "codex", []QuotaSnapshotPoint{{
		RecordedAt: secondProbe, Provider: "codex", QuotaKey: codexWeeklyQuotaKey, QuotaLabel: "Weekly",
		Percent: &fresh, ResetAt: &secondReset, WindowSeconds: weeklyWindowSeconds,
	}}); err != nil {
		t.Fatal(err)
	}

	start, reset := readCycleAnchor(t, subjectID, codexWeeklyQuotaKey)
	wantStart := secondReset.Add(-7 * 24 * time.Hour)
	if !start.Equal(wantStart) || !reset.Equal(secondReset) {
		t.Fatalf("anchor = %v..%v, want %v..%v (an early upstream reset is a real new period)",
			start, reset, wantStart, secondReset)
	}

	// The period the card reports and the period the projection writes into must
	// be the same one, or the new week's usage is added to the old week's total.
	projectSubjectRequest(t, subjectID, secondProbe.Add(time.Minute), 10)
	got := readCycleSummary(t, subjectID)
	if !got.CycleKnown || got.CycleRequestTotal != 1 || got.CycleTotalTokens != 10 {
		t.Fatalf("cycle summary = %+v, want the new period's single request", got)
	}
}

// Production, 2026-08-25: two Codex accounts had code_week step from 55% and 70%
// straight back to a full allowance, against resets a week past the stored ones.
// The upstream had ended those periods early, but a refilled period is at a full
// allowance by definition, so the metering rule could never admit the very probe
// that announces one: both anchors stayed on the period that began on the 24th
// while the new period's requests kept being added to it, and one card reported
// 12311 requests and 1.4B tokens for two periods added together.
func TestWeeklyCycleRollsWhenUpstreamRefillsTheAllowance(t *testing.T) {
	// The two accounts described the same refill differently, so the anchor has to
	// take both shapes: one reported a fixed period edge, the other "this instant
	// plus a whole window" — which is what an untouched bucket reports too, and is
	// why the refill itself has to be the evidence.
	for _, shape := range []struct {
		name           string
		resetFromProbe time.Duration
	}{
		{name: "fixed period edge", resetFromProbe: 7*24*time.Hour - 14*time.Minute},
		{name: "full window from the probe", resetFromProbe: 7 * 24 * time.Hour},
	} {
		t.Run(shape.name, func(t *testing.T) {
			initSharedSubjectTestDB(t)
			subjectID := "authsub_codex_refill"

			spentProbe := time.Now().UTC().Truncate(time.Second).Add(-3 * time.Hour)
			spentReset := spentProbe.Add(5 * 24 * time.Hour)
			spent := 55.0
			if err := RecordAIAccountSubjectQuotaPoints(subjectID, "codex", []QuotaSnapshotPoint{{
				RecordedAt: spentProbe, Provider: "codex", QuotaKey: codexWeeklyQuotaKey, QuotaLabel: "Weekly",
				Percent: &spent, ResetAt: &spentReset, WindowSeconds: weeklyWindowSeconds,
			}}); err != nil {
				t.Fatal(err)
			}

			refillProbe := spentProbe.Add(time.Hour)
			refillReset := refillProbe.Add(shape.resetFromProbe)
			full := 100.0
			if err := RecordAIAccountSubjectQuotaPoints(subjectID, "codex", []QuotaSnapshotPoint{{
				RecordedAt: refillProbe, Provider: "codex", QuotaKey: codexWeeklyQuotaKey, QuotaLabel: "Weekly",
				Percent: &full, ResetAt: &refillReset, WindowSeconds: weeklyWindowSeconds,
			}}); err != nil {
				t.Fatal(err)
			}

			start, reset := readCycleAnchor(t, subjectID, codexWeeklyQuotaKey)
			wantStart := refillReset.Add(-7 * 24 * time.Hour)
			if !start.Equal(wantStart) || !reset.Equal(refillReset) {
				t.Fatalf("anchor = %v..%v, want the refilled period %v..%v",
					start, reset, wantStart, refillReset)
			}

			// The period the card reports and the one the projection writes into have
			// to be the same, or the fresh allowance's usage is added to the period
			// the upstream just ended.
			projectSubjectRequest(t, subjectID, refillProbe.Add(time.Minute), 40)
			got := readCycleSummary(t, subjectID)
			if !got.CycleKnown || got.CycleRequestTotal != 1 || got.CycleTotalTokens != 40 {
				t.Fatalf("cycle summary = %+v, want the refilled period's single request", got)
			}
		})
	}
}

// The guard that made the freeze possible has to keep working: a window the
// account never touched answers every probe with a full window remaining, and
// following that countdown re-keys the usage bucket on each pass.
func TestUntouchedWeeklyWindowStillDoesNotRollOnCountdown(t *testing.T) {
	initSharedSubjectTestDB(t)
	subjectID := "authsub_codex_untouched"

	firstProbe := time.Now().UTC().Truncate(time.Second).Add(-9 * time.Hour)
	untouched := 100.0
	first := firstProbe.Add(7 * 24 * time.Hour)
	if err := RecordAIAccountSubjectQuotaPoints(subjectID, "codex", []QuotaSnapshotPoint{{
		RecordedAt: firstProbe, Provider: "codex", QuotaKey: "additional:extra:week", QuotaLabel: "Extra",
		Percent: &untouched, ResetAt: &first, WindowSeconds: weeklyWindowSeconds,
	}}); err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 3; i++ {
		at := firstProbe.Add(time.Duration(i) * 2 * time.Hour)
		sliding := at.Add(7 * 24 * time.Hour)
		if err := RecordAIAccountSubjectQuotaPoints(subjectID, "codex", []QuotaSnapshotPoint{{
			RecordedAt: at, Provider: "codex", QuotaKey: "additional:extra:week", QuotaLabel: "Extra",
			Percent: &untouched, ResetAt: &sliding, WindowSeconds: weeklyWindowSeconds,
		}}); err != nil {
			t.Fatal(err)
		}
	}

	start, _ := readCycleAnchor(t, subjectID, "additional:extra:week")
	if !start.Equal(firstProbe) {
		t.Fatalf("anchor = %v, want it pinned to the first probe %v", start, firstProbe)
	}
}

// A metering probe whose reset is still a whole window away from the probe itself
// is that same countdown wearing a percentage, so it must not move the anchor
// either. Once the clock moves on and the reset does not, it is recognised.
func TestMeteringProbeWithFullWindowCountdownDefersRollover(t *testing.T) {
	initSharedSubjectTestDB(t)
	subjectID := "authsub_countdown_metering"

	firstProbe := time.Now().UTC().Truncate(time.Second).Add(-30 * time.Hour)
	firstReset := firstProbe.Add(100 * time.Hour)
	percent := 80.0
	if err := RecordAIAccountSubjectQuotaPoints(subjectID, "codex", []QuotaSnapshotPoint{{
		RecordedAt: firstProbe, Provider: "codex", QuotaKey: codexWeeklyQuotaKey, QuotaLabel: "Weekly",
		Percent: &percent, ResetAt: &firstReset, WindowSeconds: weeklyWindowSeconds,
	}}); err != nil {
		t.Fatal(err)
	}

	countdownProbe := firstProbe.Add(10 * time.Hour)
	countdownReset := countdownProbe.Add(7 * 24 * time.Hour)
	if err := RecordAIAccountSubjectQuotaPoints(subjectID, "codex", []QuotaSnapshotPoint{{
		RecordedAt: countdownProbe, Provider: "codex", QuotaKey: codexWeeklyQuotaKey, QuotaLabel: "Weekly",
		Percent: &percent, ResetAt: &countdownReset, WindowSeconds: weeklyWindowSeconds,
	}}); err != nil {
		t.Fatal(err)
	}
	if start, _ := readCycleAnchor(t, subjectID, codexWeeklyQuotaKey); !start.Equal(firstProbe.Add(100*time.Hour - 7*24*time.Hour)) {
		t.Fatalf("anchor = %v, want the stored period kept while the reset only counts down", start)
	}

	// Same reset, later probe: the period is now visibly older than the probe.
	settledProbe := countdownProbe.Add(3 * time.Hour)
	if err := RecordAIAccountSubjectQuotaPoints(subjectID, "codex", []QuotaSnapshotPoint{{
		RecordedAt: settledProbe, Provider: "codex", QuotaKey: codexWeeklyQuotaKey, QuotaLabel: "Weekly",
		Percent: &percent, ResetAt: &countdownReset, WindowSeconds: weeklyWindowSeconds,
	}}); err != nil {
		t.Fatal(err)
	}
	start, _ := readCycleAnchor(t, subjectID, codexWeeklyQuotaKey)
	if !start.Equal(countdownReset.Add(-7 * 24 * time.Hour)) {
		t.Fatalf("anchor = %v, want it rolled once the reset held still across probes", start)
	}
}

// Probes stop for disabled, rate limited or unreachable accounts. The projection
// keys a request's bucket to the period the request happened in, so a reader that
// reports the stored period verbatim answers with the previous period's start and
// totals for as long as the outage lasts.
func TestExpiredAnchorRollsForReadersWithoutAProbe(t *testing.T) {
	initSharedSubjectTestDB(t)
	subjectID := "authsub_probe_outage"

	now := time.Now().UTC().Truncate(time.Second)
	staleStart := now.Add(-9 * 24 * time.Hour)
	staleReset := staleStart.Add(7 * 24 * time.Hour)
	if _, err := getDB().Exec(`
		INSERT INTO ai_account_subject_quota_cycles
			(auth_subject_id, provider, quota_key, cycle_start_at, reset_at, window_seconds, last_verified_at)
		VALUES (?, 'codex', ?, ?, ?, ?, ?)
	`, subjectID, codexWeeklyQuotaKey,
		staleStart.Format(time.RFC3339Nano), staleReset.Format(time.RFC3339Nano),
		weeklyWindowSeconds, staleReset.Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}

	starts, err := QueryLatestAIAccountSubjectWeeklyCyclesBatch([]string{subjectID})
	if err != nil {
		t.Fatal(err)
	}
	if !starts[subjectID].UTC().Equal(staleReset) {
		t.Fatalf("reader cycle start = %v, want the period after the expired one (%v)",
			starts[subjectID].UTC(), staleReset)
	}

	// The writer must agree, so the request lands in the bucket the reader totals.
	projectSubjectRequest(t, subjectID, now, 25)
	got := readCycleSummary(t, subjectID)
	if !got.CycleKnown || got.CycleRequestTotal != 1 || got.CycleTotalTokens != 25 {
		t.Fatalf("cycle summary = %+v, want the rolled period's single request", got)
	}
}

// Anchors already frozen on disk cannot wait for the stored period to expire:
// their totals stay wrong for the rest of a week that upstream has already ended.
func TestCycleRolloverRepairMovesUsageToTheLivePeriod(t *testing.T) {
	initSharedSubjectTestDB(t)
	subjectID := "authsub_repair_frozen"

	now := time.Now().UTC().Truncate(time.Second)
	frozenStart := now.Add(-4 * 24 * time.Hour)
	frozenReset := frozenStart.Add(7 * 24 * time.Hour)
	if _, err := getDB().Exec(`
		INSERT INTO ai_account_subject_quota_cycles
			(auth_subject_id, provider, quota_key, cycle_start_at, reset_at, window_seconds, last_verified_at)
		VALUES (?, 'codex', ?, ?, ?, ?, ?)
	`, subjectID, codexWeeklyQuotaKey,
		frozenStart.Format(time.RFC3339Nano), frozenReset.Format(time.RFC3339Nano),
		weeklyWindowSeconds, now.Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}

	// The probe history already shows the live period: metered, reset a week past
	// the frozen one.
	liveStart := now.Add(-90 * time.Minute)
	liveReset := liveStart.Add(7 * 24 * time.Hour)
	if _, err := getDB().Exec(`
		INSERT INTO ai_account_subject_quota_points
			(auth_subject_id, provider, quota_key, quota_label, percent, reset_at, window_seconds, recorded_at)
		VALUES (?, 'codex', ?, 'Weekly', 99, ?, ?, ?)
	`, subjectID, codexWeeklyQuotaKey, liveReset.Format(time.RFC3339Nano),
		weeklyWindowSeconds, now.Add(-30*time.Minute).Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}

	// Two requests in the previous period, three in the live one — all five were
	// counted into the frozen period's bucket.
	events := []time.Time{
		frozenStart.Add(time.Hour), frozenStart.Add(30 * time.Hour),
		liveStart.Add(time.Minute), liveStart.Add(20 * time.Minute), liveStart.Add(80 * time.Minute),
	}
	for _, at := range events {
		if _, err := getDB().Exec(`
			INSERT INTO request_logs (tenant_id, timestamp, auth_subject_id, failed, cost, total_tokens)
			VALUES ('default', ?, ?, 0, 2, 100)
		`, at.Format(time.RFC3339Nano), subjectID); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := getDB().Exec(`
		INSERT INTO ai_account_subject_usage_buckets (
			auth_subject_id, bucket_kind, bucket_start, request_count,
			success_count, failure_count, cost_total, total_tokens, first_event_at, updated_at
		) VALUES (?, 'cycle', ?, 5, 5, 0, 10, 500, ?, ?)
	`, subjectID, formatAIAccountSubjectCycleBucketStart(frozenStart),
		events[0].Format(time.RFC3339Nano), events[4].Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}

	if err := runAIAccountSubjectCycleRolloverRepairDB(getDB()); err != nil {
		t.Fatal(err)
	}

	start, reset := readCycleAnchor(t, subjectID, codexWeeklyQuotaKey)
	if !start.Equal(liveStart) || !reset.Equal(liveReset) {
		t.Fatalf("anchor = %v..%v, want the live period %v..%v", start, reset, liveStart, liveReset)
	}
	if requests, cost, tokens := readCycleBucket(t, subjectID, liveStart); requests != 3 || cost != 6 || tokens != 300 {
		t.Fatalf("live bucket = %d requests / %v cost / %d tokens, want 3 / 6 / 300", requests, cost, tokens)
	}
	// The usage moved rather than being duplicated: the previous period keeps only
	// what actually happened in it.
	if requests, cost, tokens := readCycleBucket(t, subjectID, frozenStart); requests != 2 || cost != 4 || tokens != 200 {
		t.Fatalf("previous bucket = %d requests / %v cost / %d tokens, want 2 / 4 / 200", requests, cost, tokens)
	}
	got := readCycleSummary(t, subjectID)
	if !got.CycleKnown || got.CycleRequestTotal != 3 || got.CycleTotalTokens != 300 {
		t.Fatalf("cycle summary = %+v, want the live period's 3 requests / 300 tokens", got)
	}

	// Idempotent: the marker stops a second pass from moving the totals again.
	if err := runAIAccountSubjectCycleRolloverRepairDB(getDB()); err != nil {
		t.Fatal(err)
	}
	if requests, _, _ := readCycleBucket(t, subjectID, frozenStart); requests != 2 {
		t.Fatalf("previous bucket after rerun = %d requests, want 2", requests)
	}
}

func insertQuotaPoint(t *testing.T, subjectID string, percent float64, resetAt, recordedAt time.Time) {
	t.Helper()
	if _, err := getDB().Exec(`
		INSERT INTO ai_account_subject_quota_points
			(auth_subject_id, provider, quota_key, quota_label, percent, reset_at, window_seconds, recorded_at)
		VALUES (?, 'codex', ?, 'Weekly', ?, ?, ?, ?)
	`, subjectID, codexWeeklyQuotaKey, percent, resetAt.Format(time.RFC3339Nano),
		weeklyWindowSeconds, recordedAt.Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
}

func insertFrozenWeeklyAnchor(t *testing.T, subjectID string, start, reset, verifiedAt time.Time) {
	t.Helper()
	if _, err := getDB().Exec(`
		INSERT INTO ai_account_subject_quota_cycles
			(auth_subject_id, provider, quota_key, cycle_start_at, reset_at, window_seconds, last_verified_at)
		VALUES (?, 'codex', ?, ?, ?, ?, ?)
	`, subjectID, codexWeeklyQuotaKey, start.Format(time.RFC3339Nano), reset.Format(time.RFC3339Nano),
		weeklyWindowSeconds, verifiedAt.Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
}

// The metered-probe repair cannot reach an anchor frozen on a period the upstream
// refilled rather than spent out: every probe taken since the refill reports a full
// allowance, so the newest metered probe is the one describing the frozen period
// itself. Without a pass that reads the refill, these anchors stay wrong until the
// account spends into the fresh allowance.
func TestCycleRefillRepairMovesUsageToTheRefilledPeriod(t *testing.T) {
	initSharedSubjectTestDB(t)
	subjectID := "authsub_repair_refilled"

	now := time.Now().UTC().Truncate(time.Second)
	frozenStart := now.Add(-4 * 24 * time.Hour)
	frozenReset := frozenStart.Add(7 * 24 * time.Hour)
	insertFrozenWeeklyAnchor(t, subjectID, frozenStart, frozenReset, now)

	// The history holds the step the anchor missed: metered on the frozen period,
	// then a full allowance against a reset a week past it, and only full-allowance
	// probes after that.
	refillAt := now.Add(-90 * time.Minute)
	refillReset := refillAt.Add(7 * 24 * time.Hour)
	insertQuotaPoint(t, subjectID, 55, frozenReset, now.Add(-3*time.Hour))
	insertQuotaPoint(t, subjectID, 100, refillReset, refillAt)
	insertQuotaPoint(t, subjectID, 100, now.Add(-30*time.Minute).Add(7*24*time.Hour), now.Add(-30*time.Minute))

	// Two requests in the period that ended, three in the refilled one — all five
	// were counted into the frozen period's bucket.
	events := []time.Time{
		frozenStart.Add(time.Hour), frozenStart.Add(30 * time.Hour),
		refillAt.Add(time.Minute), refillAt.Add(20 * time.Minute), refillAt.Add(80 * time.Minute),
	}
	for _, at := range events {
		if _, err := getDB().Exec(`
			INSERT INTO request_logs (tenant_id, timestamp, auth_subject_id, failed, cost, total_tokens)
			VALUES ('default', ?, ?, 0, 2, 100)
		`, at.Format(time.RFC3339Nano), subjectID); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := getDB().Exec(`
		INSERT INTO ai_account_subject_usage_buckets (
			auth_subject_id, bucket_kind, bucket_start, request_count,
			success_count, failure_count, cost_total, total_tokens, first_event_at, updated_at
		) VALUES (?, 'cycle', ?, 5, 5, 0, 10, 500, ?, ?)
	`, subjectID, formatAIAccountSubjectCycleBucketStart(frozenStart),
		events[0].Format(time.RFC3339Nano), events[4].Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}

	if err := runAIAccountSubjectCycleRefillRepairDB(getDB()); err != nil {
		t.Fatal(err)
	}

	start, reset := readCycleAnchor(t, subjectID, codexWeeklyQuotaKey)
	if !start.Equal(refillAt) || !reset.Equal(refillReset) {
		t.Fatalf("anchor = %v..%v, want the refilled period %v..%v", start, reset, refillAt, refillReset)
	}
	if requests, cost, tokens := readCycleBucket(t, subjectID, refillAt); requests != 3 || cost != 6 || tokens != 300 {
		t.Fatalf("refilled bucket = %d requests / %v cost / %d tokens, want 3 / 6 / 300", requests, cost, tokens)
	}
	if requests, cost, tokens := readCycleBucket(t, subjectID, frozenStart); requests != 2 || cost != 4 || tokens != 200 {
		t.Fatalf("previous bucket = %d requests / %v cost / %d tokens, want 2 / 4 / 200", requests, cost, tokens)
	}
	got := readCycleSummary(t, subjectID)
	if !got.CycleKnown || got.CycleRequestTotal != 3 || got.CycleTotalTokens != 300 {
		t.Fatalf("cycle summary = %+v, want the refilled period's 3 requests / 300 tokens", got)
	}

	if err := runAIAccountSubjectCycleRefillRepairDB(getDB()); err != nil {
		t.Fatal(err)
	}
	if requests, _, _ := readCycleBucket(t, subjectID, frozenStart); requests != 2 {
		t.Fatalf("previous bucket after rerun = %d requests, want 2", requests)
	}
}

// A frozen anchor rolls on its own the moment the account spends into the fresh
// allowance, which leaves the anchor right and the buckets wrong: everything
// served while it was frozen is still filed under the period before it, and the
// anchor no longer needs moving so nothing goes looking for it. Production,
// 2026-08-25: a Codex account's refilled period held 6 requests against 162 in
// the request log, the other 156 left in the previous week's bucket.
func TestCycleRefillRepairCompletesAPeriodTheAnchorRolledIntoLate(t *testing.T) {
	initSharedSubjectTestDB(t)
	subjectID := "authsub_repair_late_roll"

	now := time.Now().UTC().Truncate(time.Second)
	previousStart := now.Add(-2 * 24 * time.Hour)
	refillAt := now.Add(-2 * time.Hour)
	refillReset := refillAt.Add(7 * 24 * time.Hour)

	insertFrozenWeeklyAnchor(t, subjectID, refillAt, refillReset, now)
	insertQuotaPoint(t, subjectID, 55, previousStart.Add(7*24*time.Hour), now.Add(-3*time.Hour))
	insertQuotaPoint(t, subjectID, 100, refillReset, refillAt)

	// One request in the period that ended and five in the refilled one, of which
	// the bucket only caught the two served after the anchor rolled.
	events := []time.Time{
		previousStart.Add(time.Hour),
		refillAt.Add(time.Minute), refillAt.Add(2 * time.Minute), refillAt.Add(3 * time.Minute),
		refillAt.Add(90 * time.Minute), refillAt.Add(100 * time.Minute),
	}
	for _, at := range events {
		if _, err := getDB().Exec(`
			INSERT INTO request_logs (tenant_id, timestamp, auth_subject_id, failed, cost, total_tokens)
			VALUES ('default', ?, ?, 0, 2, 100)
		`, at.Format(time.RFC3339Nano), subjectID); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := getDB().Exec(`
		INSERT INTO ai_account_subject_usage_buckets (
			auth_subject_id, bucket_kind, bucket_start, request_count,
			success_count, failure_count, cost_total, total_tokens, first_event_at, updated_at
		) VALUES (?, 'cycle', ?, 4, 4, 0, 8, 400, ?, ?), (?, 'cycle', ?, 2, 2, 0, 4, 200, ?, ?)
	`, subjectID, formatAIAccountSubjectCycleBucketStart(previousStart),
		events[0].Format(time.RFC3339Nano), events[3].Format(time.RFC3339Nano),
		subjectID, formatAIAccountSubjectCycleBucketStart(refillAt),
		events[4].Format(time.RFC3339Nano), events[5].Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}

	if err := runAIAccountSubjectCycleRefillRepairDB(getDB()); err != nil {
		t.Fatal(err)
	}

	if start, _ := readCycleAnchor(t, subjectID, codexWeeklyQuotaKey); !start.Equal(refillAt) {
		t.Fatalf("anchor = %v, want it left on the refilled period %v", start, refillAt)
	}
	if requests, cost, tokens := readCycleBucket(t, subjectID, refillAt); requests != 5 || cost != 10 || tokens != 500 {
		t.Fatalf("refilled bucket = %d requests / %v cost / %d tokens, want 5 / 10 / 500", requests, cost, tokens)
	}
	// Only the three that were misfiled came off the previous period, not all five.
	if requests, cost, tokens := readCycleBucket(t, subjectID, previousStart); requests != 1 || cost != 2 || tokens != 100 {
		t.Fatalf("previous bucket = %d requests / %v cost / %d tokens, want 1 / 2 / 100", requests, cost, tokens)
	}
}

// A window that has only ever answered "a full window remaining" never refilled,
// because it never metered. Repairing it would move the anchor onto whatever the
// last probe's countdown happened to say and re-key the usage bucket with it.
func TestCycleRefillRepairLeavesNeverMeteredWindowsAlone(t *testing.T) {
	initSharedSubjectTestDB(t)
	subjectID := "authsub_repair_untouched"

	now := time.Now().UTC().Truncate(time.Second)
	storedStart := now.Add(-3 * 24 * time.Hour)
	storedReset := storedStart.Add(7 * 24 * time.Hour)
	insertFrozenWeeklyAnchor(t, subjectID, storedStart, storedReset, now)

	for i := 3; i >= 1; i-- {
		at := now.Add(-time.Duration(i) * time.Hour)
		insertQuotaPoint(t, subjectID, 100, at.Add(7*24*time.Hour), at)
	}

	if err := runAIAccountSubjectCycleRefillRepairDB(getDB()); err != nil {
		t.Fatal(err)
	}
	start, reset := readCycleAnchor(t, subjectID, codexWeeklyQuotaKey)
	if !start.Equal(storedStart) || !reset.Equal(storedReset) {
		t.Fatalf("anchor = %v..%v, want the stored period %v..%v left alone",
			start, reset, storedStart, storedReset)
	}
}
