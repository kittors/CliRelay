package usage

import (
	"testing"
	"time"
)

// Production reproduced: one weekly period held three cycle buckets whose starts
// were 00:51:21 / :23 / :24, carrying 404 / 114 / 1 requests. Whichever fragment
// a reader's timestamp matched became "the" cycle total, so the card said 19 and
// the detail dialog said 436 for the same account in the same minute.
func TestRecordQuotaPointsKeepsOneCycleAcrossProbeJitter(t *testing.T) {
	initSharedSubjectTestDB(t)
	auth := sharedSubjectTestAuth(sharedSubjectTenantA, "jitter", "acct-jitter", "jitter@example.com")
	identity := ResolveAuthSubjectIdentity(auth)
	if err := UpsertAIAccountTenantBinding(auth, identity); err != nil {
		t.Fatal(err)
	}

	now := time.Now().UTC().Truncate(time.Second)
	baseReset := now.Add(120 * time.Hour)
	pct := 50.0
	// Three probes, each reporting the reset a couple of seconds off the last.
	for i, drift := range []time.Duration{0, 2 * time.Second, 3 * time.Second} {
		resetAt := baseReset.Add(drift)
		if err := RecordAIAccountSubjectQuotaPoints(identity.ID, "codex", []QuotaSnapshotPoint{{
			RecordedAt: now.Add(time.Duration(i) * time.Minute), Provider: "codex",
			QuotaKey: "code_week", QuotaLabel: "Week", Percent: &pct,
			ResetAt: &resetAt, WindowSeconds: 604800,
		}}); err != nil {
			t.Fatal(err)
		}
		tx, err := getDB().Begin()
		if err != nil {
			t.Fatal(err)
		}
		if err := projectAIAccountSubjectUsageTx(tx, identity.ID, false, 1, 10, now.Add(time.Duration(i)*time.Minute)); err != nil {
			_ = tx.Rollback()
			t.Fatal(err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
	}

	var buckets int
	if err := getDB().QueryRow(`
		SELECT COUNT(*) FROM ai_account_subject_usage_buckets
		WHERE auth_subject_id = ? AND bucket_kind = 'cycle'
	`, identity.ID).Scan(&buckets); err != nil {
		t.Fatal(err)
	}
	if buckets != 1 {
		t.Fatalf("cycle buckets = %d, want 1 (jitter must not open a new period)", buckets)
	}

	anchor := baseReset.Add(-7 * 24 * time.Hour)
	summaries, err := QueryAIAccountSubjectUsageSummaries([]string{identity.ID}, map[string]time.Time{identity.ID: anchor})
	if err != nil {
		t.Fatal(err)
	}
	got := summaries[identity.ID]
	if !got.CycleKnown || got.CycleRequestTotal != 3 || got.CycleTotalTokens != 30 {
		t.Fatalf("cycle summary = %+v, want 3 requests / 30 tokens", got)
	}
}

// A real rollover moves the start by a whole window, far outside the tolerance,
// so the period must still roll instead of absorbing the next week's traffic.
func TestRecordQuotaPointsRollsCycleOnGenuineReset(t *testing.T) {
	initSharedSubjectTestDB(t)
	auth := sharedSubjectTestAuth(sharedSubjectTenantA, "rollover", "acct-rollover", "roll@example.com")
	identity := ResolveAuthSubjectIdentity(auth)
	if err := UpsertAIAccountTenantBinding(auth, identity); err != nil {
		t.Fatal(err)
	}

	now := time.Now().UTC().Truncate(time.Second)
	firstReset := now.Add(24 * time.Hour)
	secondReset := firstReset.Add(7 * 24 * time.Hour)
	pct := 50.0
	for _, resetAt := range []time.Time{firstReset, secondReset} {
		reset := resetAt
		if err := RecordAIAccountSubjectQuotaPoints(identity.ID, "codex", []QuotaSnapshotPoint{{
			RecordedAt: now, Provider: "codex", QuotaKey: "code_week", QuotaLabel: "Week",
			Percent: &pct, ResetAt: &reset, WindowSeconds: 604800,
		}}); err != nil {
			t.Fatal(err)
		}
	}

	var start storedTime
	if err := getDB().QueryRow(`
		SELECT cycle_start_at FROM ai_account_subject_quota_cycles
		WHERE auth_subject_id = ? AND quota_key = 'code_week'
	`, identity.ID).Scan(&start); err != nil {
		t.Fatal(err)
	}
	want := secondReset.Add(-7 * 24 * time.Hour)
	if !start.Valid || !start.Time.UTC().Equal(want) {
		t.Fatalf("cycle start = %v, want %v (a real reset must roll the period)", start.Time, want)
	}
}

// Buckets written before anchoring landed are still fragmented on disk; the
// migration folds them onto the earliest start and repoints the live cycle row.
func TestCycleBucketMergeFoldsFragmentedPeriod(t *testing.T) {
	initSharedSubjectTestDB(t)
	auth := sharedSubjectTestAuth(sharedSubjectTenantA, "merge", "acct-merge", "merge@example.com")
	identity := ResolveAuthSubjectIdentity(auth)
	if err := UpsertAIAccountTenantBinding(auth, identity); err != nil {
		t.Fatal(err)
	}

	now := time.Now().UTC().Truncate(time.Second)
	anchor := now.Add(-48 * time.Hour)
	fragments := []struct {
		at       time.Time
		requests int64
		tokens   int64
		cost     float64
	}{
		{anchor, 404, 57702997, 52.5},
		{anchor.Add(2 * time.Second), 114, 23291786, 16.8},
		{anchor.Add(3 * time.Second), 1, 271753, 0.15},
	}
	for _, fragment := range fragments {
		if _, err := getDB().Exec(`
			INSERT INTO ai_account_subject_usage_buckets (
				auth_subject_id, bucket_kind, bucket_start, request_count, success_count,
				failure_count, cost_total, total_tokens, first_event_at, updated_at
			) VALUES (?, 'cycle', ?, ?, ?, 0, ?, ?, ?, ?)
		`, identity.ID, formatAIAccountSubjectCycleBucketStart(fragment.at), fragment.requests,
			fragment.requests, fragment.cost, fragment.tokens,
			fragment.at.Format(time.RFC3339Nano), now.Format(time.RFC3339Nano)); err != nil {
			t.Fatal(err)
		}
	}
	// The live cycle row points at the newest fragment, which is exactly why a
	// reader saw only one request instead of the period's 519.
	newest := fragments[len(fragments)-1].at
	if _, err := getDB().Exec(`
		INSERT INTO ai_account_subject_quota_cycles
			(auth_subject_id, provider, quota_key, cycle_start_at, reset_at, window_seconds, last_verified_at)
		VALUES (?, 'codex', 'code_week', ?, ?, 604800, ?)
	`, identity.ID, newest.Format(time.RFC3339Nano),
		newest.Add(7*24*time.Hour).Format(time.RFC3339Nano), now.Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}

	if err := runAIAccountSubjectCycleBucketMergeDB(getDB()); err != nil {
		t.Fatal(err)
	}

	var buckets int
	var requests, tokens int64
	if err := getDB().QueryRow(`
		SELECT COUNT(*), COALESCE(SUM(request_count), 0), COALESCE(SUM(total_tokens), 0)
		FROM ai_account_subject_usage_buckets
		WHERE auth_subject_id = ? AND bucket_kind = 'cycle'
	`, identity.ID).Scan(&buckets, &requests, &tokens); err != nil {
		t.Fatal(err)
	}
	if buckets != 1 || requests != 519 || tokens != 81266536 {
		t.Fatalf("merged buckets=%d requests=%d tokens=%d, want 1/519/81266536", buckets, requests, tokens)
	}

	var start storedTime
	if err := getDB().QueryRow(`
		SELECT cycle_start_at FROM ai_account_subject_quota_cycles
		WHERE auth_subject_id = ? AND quota_key = 'code_week'
	`, identity.ID).Scan(&start); err != nil {
		t.Fatal(err)
	}
	if !start.Valid || !start.Time.UTC().Equal(anchor) {
		t.Fatalf("cycle row start = %v, want realigned to %v", start.Time, anchor)
	}

	summaries, err := QueryAIAccountSubjectUsageSummaries([]string{identity.ID}, map[string]time.Time{identity.ID: anchor})
	if err != nil {
		t.Fatal(err)
	}
	if got := summaries[identity.ID]; got.CycleRequestTotal != 519 || got.CycleTotalTokens != 81266536 {
		t.Fatalf("summary after merge = %+v, want 519 requests / 81266536 tokens", got)
	}
}

// Distinct periods must survive the merge: the grouping compares each bucket to
// its group anchor, so a chain of small drifts cannot swallow a whole week.
func TestCycleBucketMergeKeepsDistinctPeriodsApart(t *testing.T) {
	initSharedSubjectTestDB(t)
	auth := sharedSubjectTestAuth(sharedSubjectTenantA, "distinct", "acct-distinct", "distinct@example.com")
	identity := ResolveAuthSubjectIdentity(auth)
	if err := UpsertAIAccountTenantBinding(auth, identity); err != nil {
		t.Fatal(err)
	}

	now := time.Now().UTC().Truncate(time.Second)
	lastWeek := now.Add(-7 * 24 * time.Hour)
	for _, at := range []time.Time{lastWeek, lastWeek.Add(time.Second), now, now.Add(time.Second)} {
		if _, err := getDB().Exec(`
			INSERT INTO ai_account_subject_usage_buckets (
				auth_subject_id, bucket_kind, bucket_start, request_count, success_count,
				failure_count, cost_total, total_tokens, first_event_at, updated_at
			) VALUES (?, 'cycle', ?, 10, 10, 0, 1, 100, ?, ?)
		`, identity.ID, formatAIAccountSubjectCycleBucketStart(at),
			at.Format(time.RFC3339Nano), now.Format(time.RFC3339Nano)); err != nil {
			t.Fatal(err)
		}
	}

	if err := runAIAccountSubjectCycleBucketMergeDB(getDB()); err != nil {
		t.Fatal(err)
	}

	var buckets int
	if err := getDB().QueryRow(`
		SELECT COUNT(*) FROM ai_account_subject_usage_buckets
		WHERE auth_subject_id = ? AND bucket_kind = 'cycle'
	`, identity.ID).Scan(&buckets); err != nil {
		t.Fatal(err)
	}
	if buckets != 2 {
		t.Fatalf("cycle buckets = %d, want 2 (one per real period)", buckets)
	}
}

func TestCycleBucketMergeRunsOncePerMarker(t *testing.T) {
	initSharedSubjectTestDB(t)
	if err := runAIAccountSubjectCycleBucketMergeDB(getDB()); err != nil {
		t.Fatal(err)
	}
	if got := projectionMarkerValue(getDB(), aiAccountSubjectCycleBucketMergeMarker); got != rollupMarkerDone {
		t.Fatalf("marker = %q, want %q", got, rollupMarkerDone)
	}
	if err := runAIAccountSubjectCycleBucketMergeDB(getDB()); err != nil {
		t.Fatalf("rerun: %v", err)
	}
}
