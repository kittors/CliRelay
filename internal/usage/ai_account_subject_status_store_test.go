package usage

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
)

func quotaWindow(key string, percent float64, observed *time.Time) QuotaWindowDTO {
	p := percent
	return QuotaWindowDTO{QuotaKey: key, Percent: &p, ObservedAt: observed}
}

func findWindow(windows []QuotaWindowDTO, key string) *QuotaWindowDTO {
	for i := range windows {
		if windows[i].QuotaKey == key {
			return &windows[i]
		}
	}
	return nil
}

// Codex answers with only `rate_limit` or only `additional_rate_limits` on a
// large minority of probes. Replacing the set wholesale made the other window
// vanish from the card until the next probe happened to include it again.
func TestMergeQuotaWindowsKeepsWindowsMissingFromPartialPayload(t *testing.T) {
	first := time.Date(2026, 8, 7, 8, 0, 0, 0, time.UTC)
	second := first.Add(30 * time.Minute)

	stored := mergeQuotaWindows(nil, []QuotaWindowDTO{
		quotaWindow("code_week", 15, nil),
		quotaWindow("additional:codex_bengalfox:week", 96, nil),
	}, first)
	if len(stored) != 2 {
		t.Fatalf("expected both windows stored, got %d", len(stored))
	}

	// Second probe reports only the primary window.
	merged := mergeQuotaWindows(stored, []QuotaWindowDTO{quotaWindow("code_week", 14, nil)}, second)
	if len(merged) != 2 {
		t.Fatalf("partial payload dropped a window: got %d windows", len(merged))
	}

	refreshed := findWindow(merged, "code_week")
	if refreshed == nil || *refreshed.Percent != 14 {
		t.Fatalf("confirmed window not refreshed: %+v", refreshed)
	}
	if refreshed.ObservedAt == nil || !refreshed.ObservedAt.Equal(second) {
		t.Fatalf("confirmed window should be stamped with the new probe time, got %v", refreshed.ObservedAt)
	}

	carried := findWindow(merged, "additional:codex_bengalfox:week")
	if carried == nil || *carried.Percent != 96 {
		t.Fatalf("carried window lost its value: %+v", carried)
	}
	// The whole point: a carried window must not inherit the new probe time, or the
	// UI cannot tell it apart from a value the upstream actually just confirmed.
	if carried.ObservedAt == nil || !carried.ObservedAt.Equal(first) {
		t.Fatalf("carried window must keep its original observation time, got %v", carried.ObservedAt)
	}
}

func TestMergeQuotaWindowsDropsWindowsUnconfirmedPastRetention(t *testing.T) {
	old := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	now := old.Add(quotaWindowRetention + time.Hour)

	stored := []QuotaWindowDTO{quotaWindow("retired_window", 50, &old)}
	merged := mergeQuotaWindows(stored, []QuotaWindowDTO{quotaWindow("code_week", 20, nil)}, now)

	if findWindow(merged, "retired_window") != nil {
		t.Fatal("window unconfirmed past retention should be dropped")
	}
	if findWindow(merged, "code_week") == nil {
		t.Fatal("confirmed window missing")
	}
}

// An empty payload confirms nothing. Windows stay (they are still the best known
// answer) but must keep aging so the UI can mark them stale.
func TestMergeQuotaWindowsEmptyPayloadDoesNotRefreshTimestamps(t *testing.T) {
	first := time.Date(2026, 8, 7, 8, 0, 0, 0, time.UTC)
	stored := mergeQuotaWindows(nil, []QuotaWindowDTO{quotaWindow("code_week", 15, nil)}, first)

	merged := mergeQuotaWindows(stored, nil, first.Add(time.Hour))
	window := findWindow(merged, "code_week")
	if window == nil {
		t.Fatal("empty payload should not clear the last known window")
	}
	if window.ObservedAt == nil || !window.ObservedAt.Equal(first) {
		t.Fatalf("empty payload must not restamp the window, got %v", window.ObservedAt)
	}
}

func TestMergeQuotaWindowsPreservesPreviousOrdering(t *testing.T) {
	at := time.Date(2026, 8, 7, 8, 0, 0, 0, time.UTC)
	stored := mergeQuotaWindows(nil, []QuotaWindowDTO{
		quotaWindow("code_5h", 40, nil),
		quotaWindow("code_week", 15, nil),
	}, at)

	// Upstream reports the same windows in the opposite order, plus a new one.
	merged := mergeQuotaWindows(stored, []QuotaWindowDTO{
		quotaWindow("code_week", 14, nil),
		quotaWindow("code_5h", 39, nil),
		quotaWindow("review_week", 88, nil),
	}, at.Add(time.Minute))

	got := []string{merged[0].QuotaKey, merged[1].QuotaKey, merged[2].QuotaKey}
	want := []string{"code_5h", "code_week", "review_week"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("card ordering churned: got %v want %v", got, want)
		}
	}
}

// A failing probe must move upstream_checked_at (the attempt happened) while
// leaving quota_observed_at behind, so "checked seconds ago, data from days ago"
// is representable instead of collapsing into one misleading timestamp.
func TestProbeFailureKeepsQuotaObservationTimeBehind(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "usage.db")
	if err := InitDB(dbPath, config.RequestLogStorageConfig{}, time.UTC); err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	t.Cleanup(func() {
		CloseDB()
		_ = os.Remove(dbPath)
	})
	if err := UpsertAIAccountSubject(&AuthSubjectIdentity{
		ID: "sub-fresh", Provider: "codex", SubjectScope: AIAccountSubjectScopeShared,
		SeedKind: "test", SeedHash: "hash-fresh",
	}); err != nil {
		t.Fatalf("upsert subject: %v", err)
	}

	observed := time.Now().UTC().Add(-6 * time.Hour)
	if err := UpsertAIAccountSubjectStatus(AIAccountSubjectStatusRecord{
		AuthSubjectID: "sub-fresh", Provider: "codex", LastProbeState: "success", HealthStatus: "ok",
		Quotas:            []QuotaWindowDTO{quotaWindow("code_week", 15, nil)},
		UpstreamCheckedAt: &observed, UpdatedAt: observed,
	}); err != nil {
		t.Fatalf("seed status: %v", err)
	}

	failedAt := time.Now().UTC()
	if err := UpdateAIAccountSubjectProbeFailure("sub-fresh", "codex", "probe_failed", "http 401 unauthorized", failedAt); err != nil {
		t.Fatalf("probe failure: %v", err)
	}

	rows, err := ListAIAccountSubjectStatus([]string{"sub-fresh"})
	if err != nil || len(rows) != 1 {
		t.Fatalf("list status: %v rows=%d", err, len(rows))
	}
	row := rows[0]
	if row.LastProbeState != "error" {
		t.Fatalf("expected error state, got %q", row.LastProbeState)
	}
	if len(row.Quotas) != 1 {
		t.Fatalf("failed probe must keep the last known quota, got %d windows", len(row.Quotas))
	}
	if row.UpstreamCheckedAt == nil || row.UpstreamCheckedAt.Before(failedAt.Add(-time.Second)) {
		t.Fatalf("attempt time should advance to the failure, got %v", row.UpstreamCheckedAt)
	}
	if row.QuotaObservedAt == nil {
		t.Fatal("quota observation time missing")
	}
	if row.QuotaObservedAt.After(observed.Add(time.Second)) {
		t.Fatalf("failed probe must not restamp quota observation: observed=%v got=%v", observed, row.QuotaObservedAt)
	}
	if !row.UpstreamCheckedAt.After(*row.QuotaObservedAt) {
		t.Fatal("attempt time must be strictly newer than the quota it describes")
	}
}
