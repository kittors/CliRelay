package usage

import (
	"testing"
	"time"
)

const (
	antigravityGeminiWeeklyKey = "antigravity:gemini_weekly"
	antigravity3pWeeklyKey     = "antigravity:3p_weekly"
	weeklyWindowSeconds        = int64(604800)
)

func projectSubjectRequest(t *testing.T, subjectID string, at time.Time, tokens int64) {
	t.Helper()
	tx, err := getDB().Begin()
	if err != nil {
		t.Fatal(err)
	}
	if err := projectAIAccountSubjectUsageTx(usageTestMetadataTx(tx), subjectID, false, 1, tokens, at); err != nil {
		_ = tx.Rollback()
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

func countCycleBuckets(t *testing.T, subjectID string) int {
	t.Helper()
	var count int
	if err := getDB().QueryRow(`
		SELECT COUNT(*) FROM ai_account_subject_usage_buckets
		WHERE auth_subject_id = ? AND bucket_kind = 'cycle'
	`, subjectID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

func readCycleSummary(t *testing.T, subjectID string) AuthSubjectUsageSummary {
	t.Helper()
	starts, err := QueryLatestAIAccountSubjectWeeklyCyclesBatch([]string{subjectID})
	if err != nil {
		t.Fatal(err)
	}
	summaries, err := QueryAIAccountSubjectUsageSummaries([]string{subjectID}, starts)
	if err != nil {
		t.Fatal(err)
	}
	return summaries[subjectID]
}

// Production: an antigravity account with 1084 lifetime calls showed 0 for the
// period. Its untouched third-party weekly bucket is answered as "a full window
// remaining", so the start derived from that countdown walked forward by hours on
// every probe, opening a usage bucket per step, while the readers resolved a
// single anchor that matched none of them.
func TestAntigravitySlidingWeeklyResetKeepsOnePeriod(t *testing.T) {
	initSharedSubjectTestDB(t)
	subjectID := "authsub_antigravity_slide"

	now := time.Now().UTC().Truncate(time.Second)
	// The bucket the account actually spends reports a fixed reset.
	geminiReset := now.Add(120 * time.Hour)
	geminiPercent, thirdPartyPercent := 87.0, 100.0

	for i := 0; i < 6; i++ {
		at := now.Add(time.Duration(i) * 12 * time.Hour)
		slidingReset := at.Add(7 * 24 * time.Hour)
		if err := RecordAIAccountSubjectQuotaPoints(subjectID, "antigravity", []QuotaSnapshotPoint{
			{
				RecordedAt: at, Provider: "antigravity", QuotaKey: antigravityGeminiWeeklyKey,
				QuotaLabel: "Gemini", Percent: &geminiPercent, ResetAt: &geminiReset,
				WindowSeconds: weeklyWindowSeconds,
			},
			{
				RecordedAt: at, Provider: "antigravity", QuotaKey: antigravity3pWeeklyKey,
				QuotaLabel: "Claude and GPT", Percent: &thirdPartyPercent, ResetAt: &slidingReset,
				WindowSeconds: weeklyWindowSeconds,
			},
		}); err != nil {
			t.Fatal(err)
		}
		projectSubjectRequest(t, subjectID, at, 10)
	}

	if buckets := countCycleBuckets(t, subjectID); buckets != 1 {
		t.Fatalf("cycle buckets = %d, want 1 (a sliding countdown must not open a period)", buckets)
	}

	got := readCycleSummary(t, subjectID)
	if !got.CycleKnown || got.CycleRequestTotal != 6 || got.CycleTotalTokens != 60 {
		t.Fatalf("cycle summary = %+v, want 6 requests / 60 tokens", got)
	}

	// The anchor must be the bucket that is actually metering, not the untouched
	// one whose window merely happens to be re-stamped on every probe.
	var start storedTime
	if err := getDB().QueryRow(`
		SELECT cycle_start_at FROM ai_account_subject_quota_cycles
		WHERE auth_subject_id = ? AND quota_key = ?
	`, subjectID, antigravity3pWeeklyKey).Scan(&start); err != nil {
		t.Fatal(err)
	}
	if !start.Valid || !start.Time.UTC().Equal(now) {
		t.Fatalf("third-party anchor = %v, want it pinned to its first probe %v", start.Time, now)
	}
}

// The projection reads the anchor from an in-memory map and the readers read it
// from a SQL result set. With several weekly windows re-stamped by one probe, any
// ordering by timestamp let the two sides disagree, and requests landed in a
// bucket nobody read back.
func TestWeeklyCycleSelectionIsIndependentOfCandidateOrder(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	gemini := AIAccountSubjectQuotaCycle{
		AuthSubjectID: "authsub_order", Provider: "antigravity", QuotaKey: antigravityGeminiWeeklyKey,
		CycleStartAt: now.Add(-48 * time.Hour), ResetAt: now.Add(120 * time.Hour),
		WindowSeconds: weeklyWindowSeconds, LastVerifiedAt: now,
	}
	thirdParty := AIAccountSubjectQuotaCycle{
		AuthSubjectID: "authsub_order", Provider: "antigravity", QuotaKey: antigravity3pWeeklyKey,
		CycleStartAt: now, ResetAt: now.Add(7 * 24 * time.Hour),
		WindowSeconds: weeklyWindowSeconds, LastVerifiedAt: now,
	}

	forward, okForward := selectAIAccountSubjectWeeklyCycle([]AIAccountSubjectQuotaCycle{gemini, thirdParty})
	reverse, okReverse := selectAIAccountSubjectWeeklyCycle([]AIAccountSubjectQuotaCycle{thirdParty, gemini})
	if !okForward || !okReverse {
		t.Fatal("expected a weekly cycle from both orderings")
	}
	if forward.QuotaKey != reverse.QuotaKey {
		t.Fatalf("selection depends on input order: %q vs %q", forward.QuotaKey, reverse.QuotaKey)
	}
	if forward.QuotaKey != antigravityGeminiWeeklyKey {
		t.Fatalf("selected %q, want the provider's named weekly window", forward.QuotaKey)
	}

	// An upstream rename must degrade to a stable choice rather than to no cycle.
	renamed := []AIAccountSubjectQuotaCycle{
		{
			AuthSubjectID: "authsub_order", Provider: "antigravity", QuotaKey: "antigravity:zz_weekly",
			CycleStartAt: now, ResetAt: now.Add(7 * 24 * time.Hour),
			WindowSeconds: weeklyWindowSeconds, LastVerifiedAt: now,
		},
		{
			AuthSubjectID: "authsub_order", Provider: "antigravity", QuotaKey: "antigravity:aa_weekly",
			CycleStartAt: now.Add(-time.Hour), ResetAt: now.Add(167 * time.Hour),
			WindowSeconds: weeklyWindowSeconds, LastVerifiedAt: now,
		},
	}
	fallback, ok := selectAIAccountSubjectWeeklyCycle(renamed)
	if !ok || fallback.QuotaKey != "antigravity:aa_weekly" {
		t.Fatalf("fallback selection = %+v, want the deterministic aa_weekly", fallback)
	}
}

// One batch mixes providers. Filtering candidates by a single list of quota keys
// built from that batch dropped every window belonging to a provider that names
// its buckets differently: with "seven_day" in the list, antigravity reported no
// cycle at all.
func TestWeeklyCycleBatchKeepsProvidersWithUnlistedQuotaKeys(t *testing.T) {
	initSharedSubjectTestDB(t)
	claudeSubject := "authsub_batch_claude"
	antigravitySubject := "authsub_batch_antigravity"

	now := time.Now().UTC().Truncate(time.Second)
	reset := now.Add(120 * time.Hour)
	percent := 50.0
	if err := RecordAIAccountSubjectQuotaPoints(claudeSubject, "claude", []QuotaSnapshotPoint{{
		RecordedAt: now, Provider: "claude", QuotaKey: "seven_day", QuotaLabel: "7d",
		Percent: &percent, ResetAt: &reset, WindowSeconds: weeklyWindowSeconds,
	}}); err != nil {
		t.Fatal(err)
	}
	if err := RecordAIAccountSubjectQuotaPoints(antigravitySubject, "antigravity", []QuotaSnapshotPoint{{
		RecordedAt: now, Provider: "antigravity", QuotaKey: antigravityGeminiWeeklyKey, QuotaLabel: "Gemini",
		Percent: &percent, ResetAt: &reset, WindowSeconds: weeklyWindowSeconds,
	}}); err != nil {
		t.Fatal(err)
	}

	starts, err := QueryLatestAIAccountSubjectWeeklyCyclesBatch([]string{claudeSubject, antigravitySubject})
	if err != nil {
		t.Fatal(err)
	}
	for _, subjectID := range []string{claudeSubject, antigravitySubject} {
		if starts[subjectID].IsZero() {
			t.Fatalf("subject %s lost its cycle in a mixed-provider batch: %+v", subjectID, starts)
		}
	}
}

// A period still has to roll when the upstream says the old one is over —
// otherwise the next week's traffic would be added to the previous week's total.
func TestWeeklyCycleRollsAfterStoredPeriodEnds(t *testing.T) {
	initSharedSubjectTestDB(t)
	subjectID := "authsub_rollover_after_end"

	now := time.Now().UTC().Truncate(time.Second)
	firstReset := now.Add(2 * time.Hour)
	percent := 10.0
	if err := RecordAIAccountSubjectQuotaPoints(subjectID, "antigravity", []QuotaSnapshotPoint{{
		RecordedAt: now, Provider: "antigravity", QuotaKey: antigravityGeminiWeeklyKey, QuotaLabel: "Gemini",
		Percent: &percent, ResetAt: &firstReset, WindowSeconds: weeklyWindowSeconds,
	}}); err != nil {
		t.Fatal(err)
	}

	// Probed after the stored period ended, reporting a window that is not
	// contiguous with it (the account was idle across the boundary).
	after := firstReset.Add(3 * time.Hour)
	secondReset := after.Add(7 * 24 * time.Hour)
	if err := RecordAIAccountSubjectQuotaPoints(subjectID, "antigravity", []QuotaSnapshotPoint{{
		RecordedAt: after, Provider: "antigravity", QuotaKey: antigravityGeminiWeeklyKey, QuotaLabel: "Gemini",
		Percent: &percent, ResetAt: &secondReset, WindowSeconds: weeklyWindowSeconds,
	}}); err != nil {
		t.Fatal(err)
	}

	var start storedTime
	if err := getDB().QueryRow(`
		SELECT cycle_start_at FROM ai_account_subject_quota_cycles
		WHERE auth_subject_id = ? AND quota_key = ?
	`, subjectID, antigravityGeminiWeeklyKey).Scan(&start); err != nil {
		t.Fatal(err)
	}
	want := secondReset.Add(-7 * 24 * time.Hour)
	if !start.Valid || !start.Time.UTC().Equal(want) {
		t.Fatalf("cycle start = %v, want %v (an ended period must roll)", start.Time, want)
	}
}

// The buckets already scattered on disk are repaired in place: waiting out a full
// window before totals become correct is not a fix an operator can use.
func TestCycleRealignFoldsFragmentsOntoLiveAnchor(t *testing.T) {
	initSharedSubjectTestDB(t)
	subjectID := "authsub_realign"

	anchorAt := time.Now().UTC().Truncate(time.Second).Add(-60 * time.Hour)
	resetAt := anchorAt.Add(7 * 24 * time.Hour)
	if _, err := getDB().Exec(`
		INSERT INTO ai_account_subject_quota_cycles
			(auth_subject_id, provider, quota_key, cycle_start_at, reset_at, window_seconds, last_verified_at)
		VALUES (?, 'antigravity', ?, ?, ?, ?, ?)
	`, subjectID, antigravityGeminiWeeklyKey,
		anchorAt.Format(time.RFC3339Nano), resetAt.Format(time.RFC3339Nano),
		weeklyWindowSeconds, anchorAt.Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	// The third-party window, whose sliding reset produced the fragments, is the
	// one a timestamp ordering would have picked.
	slidTo := anchorAt.Add(58 * time.Hour)
	if _, err := getDB().Exec(`
		INSERT INTO ai_account_subject_quota_cycles
			(auth_subject_id, provider, quota_key, cycle_start_at, reset_at, window_seconds, last_verified_at)
		VALUES (?, 'antigravity', ?, ?, ?, ?, ?)
	`, subjectID, antigravity3pWeeklyKey,
		slidTo.Format(time.RFC3339Nano), slidTo.Add(7*24*time.Hour).Format(time.RFC3339Nano),
		weeklyWindowSeconds, slidTo.Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}

	// Production's shape: six fragments of one week, plus a bucket from the
	// previous period that must survive untouched.
	previous := anchorAt.Add(-8 * 24 * time.Hour)
	fragments := []struct {
		at       time.Time
		requests int64
		tokens   int64
	}{
		{previous, 90, 900},
		{anchorAt, 545, 13258533},
		{anchorAt.Add(3 * time.Hour), 13, 186046},
		{anchorAt.Add(5 * time.Hour), 7, 246262},
		{anchorAt.Add(7 * time.Hour), 17, 408122},
		{anchorAt.Add(23 * time.Hour), 487, 27117844},
		{anchorAt.Add(48 * time.Hour), 15, 304158},
	}
	var wantRequests, wantTokens int64
	for _, fragment := range fragments {
		if _, err := getDB().Exec(`
			INSERT INTO ai_account_subject_usage_buckets (
				auth_subject_id, bucket_kind, bucket_start, request_count,
				success_count, failure_count, cost_total, total_tokens, first_event_at, updated_at
			) VALUES (?, 'cycle', ?, ?, ?, 0, 0, ?, ?, ?)
		`, subjectID, formatAIAccountSubjectCycleBucketStart(fragment.at),
			fragment.requests, fragment.requests, fragment.tokens,
			fragment.at.Format(time.RFC3339Nano), fragment.at.Format(time.RFC3339Nano)); err != nil {
			t.Fatal(err)
		}
		if !fragment.at.Before(anchorAt) {
			wantRequests += fragment.requests
			wantTokens += fragment.tokens
		}
	}

	if err := runAIAccountSubjectCycleRealignDB(getDB()); err != nil {
		t.Fatal(err)
	}

	if buckets := countCycleBuckets(t, subjectID); buckets != 2 {
		t.Fatalf("cycle buckets = %d, want 2 (this period folded, the previous kept)", buckets)
	}
	got := readCycleSummary(t, subjectID)
	if !got.CycleKnown || got.CycleRequestTotal != wantRequests || got.CycleTotalTokens != wantTokens {
		t.Fatalf("cycle summary = %+v, want %d requests / %d tokens", got, wantRequests, wantTokens)
	}

	// Idempotent: the marker stops a second pass from touching anything.
	if err := runAIAccountSubjectCycleRealignDB(getDB()); err != nil {
		t.Fatal(err)
	}
	if buckets := countCycleBuckets(t, subjectID); buckets != 2 {
		t.Fatalf("cycle buckets after rerun = %d, want 2", buckets)
	}
}

// One bucket keyed to a start no reader resolves reads as zero just as surely as
// six do, so the repair cannot be limited to subjects with several fragments.
func TestCycleRealignRekeysLoneMisplacedBucket(t *testing.T) {
	initSharedSubjectTestDB(t)
	subjectID := "authsub_realign_single"

	anchorAt := time.Now().UTC().Truncate(time.Second).Add(-40 * time.Hour)
	resetAt := anchorAt.Add(7 * 24 * time.Hour)
	if _, err := getDB().Exec(`
		INSERT INTO ai_account_subject_quota_cycles
			(auth_subject_id, provider, quota_key, cycle_start_at, reset_at, window_seconds, last_verified_at)
		VALUES (?, 'antigravity', ?, ?, ?, ?, ?)
	`, subjectID, antigravityGeminiWeeklyKey,
		anchorAt.Format(time.RFC3339Nano), resetAt.Format(time.RFC3339Nano),
		weeklyWindowSeconds, anchorAt.Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}

	strayAt := anchorAt.Add(30 * time.Hour)
	if _, err := getDB().Exec(`
		INSERT INTO ai_account_subject_usage_buckets (
			auth_subject_id, bucket_kind, bucket_start, request_count,
			success_count, failure_count, cost_total, total_tokens, first_event_at, updated_at
		) VALUES (?, 'cycle', ?, 42, 42, 0, 0, 4200, ?, ?)
	`, subjectID, formatAIAccountSubjectCycleBucketStart(strayAt),
		strayAt.Format(time.RFC3339Nano), strayAt.Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}

	if err := runAIAccountSubjectCycleRealignDB(getDB()); err != nil {
		t.Fatal(err)
	}
	got := readCycleSummary(t, subjectID)
	if !got.CycleKnown || got.CycleRequestTotal != 42 || got.CycleTotalTokens != 4200 {
		t.Fatalf("cycle summary = %+v, want 42 requests / 4200 tokens", got)
	}
}
