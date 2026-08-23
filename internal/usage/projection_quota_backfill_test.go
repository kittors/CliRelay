package usage

import (
	"testing"
	"time"
)

// Existing xAI accounts already have product snapshots stored without a window,
// because the probe did not tag them until now. Those rows are what makes the
// projection fall back to the pool-wide figure, so the first probe after the
// upgrade has to write a windowed point even when the percentage has not moved
// — otherwise the dedupe swallows it and the account never heals.
func TestQuotaSnapshotWritesWindowChangeAtUnchangedPercent(t *testing.T) {
	initSharedSubjectTestDB(t)

	const subjectID = "authsub_xai_backfill"
	percent := 84.0
	reset := time.Date(2026, 8, 27, 6, 45, 51, 0, time.UTC)
	recorded := time.Date(2026, 8, 24, 0, 0, 0, 0, time.UTC)

	// Pre-upgrade shape: a product percentage with no window and no reset.
	if err := RecordAIAccountSubjectQuotaPoints(subjectID, "xai", []QuotaSnapshotPoint{{
		RecordedAt: recorded,
		AuthIndex:  "auth-1",
		Provider:   "xai",
		QuotaKey:   "product:GrokBuild",
		QuotaLabel: "xai_quota.product_usage_named::GrokBuild",
		Percent:    &percent,
	}}); err != nil {
		t.Fatalf("record legacy point: %v", err)
	}

	// Post-upgrade probe, minutes later and at the very same percentage.
	samePercent := percent
	if err := RecordAIAccountSubjectQuotaPoints(subjectID, "xai", []QuotaSnapshotPoint{{
		RecordedAt:    recorded.Add(2 * time.Minute),
		AuthIndex:     "auth-1",
		Provider:      "xai",
		QuotaKey:      "product:GrokBuild",
		QuotaLabel:    "xai_quota.product_usage_named::GrokBuild",
		Percent:       &samePercent,
		ResetAt:       &reset,
		WindowSeconds: WeeklyQuotaWindowSeconds,
	}}); err != nil {
		t.Fatalf("record windowed point: %v", err)
	}

	db := getDB()
	if db == nil {
		t.Fatal("db unavailable")
	}
	var windowed int
	if err := db.QueryRow(`
		SELECT COUNT(*) FROM ai_account_subject_quota_points
		WHERE auth_subject_id = ? AND quota_key = ? AND window_seconds = ?
	`, subjectID, "product:GrokBuild", WeeklyQuotaWindowSeconds).Scan(&windowed); err != nil {
		t.Fatalf("count windowed points: %v", err)
	}
	if windowed != 1 {
		t.Fatalf("windowed points = %d, want 1 (dedupe must not swallow a window change)", windowed)
	}
}
