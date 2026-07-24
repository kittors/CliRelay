package usage

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
)

func TestAIAccountStatusUpsertAndListTenantIsolation(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "usage.db")
	if err := InitDB(dbPath, config.RequestLogStorageConfig{}, time.UTC); err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	t.Cleanup(func() {
		CloseDB()
		_ = os.Remove(dbPath)
	})

	pct := 80.0
	now := time.Now().UTC()
	if err := UpsertAIAccountStatus(AIAccountStatusRecord{
		TenantID: "tenant-a", AuthSubjectID: "sub-1", AuthIndex: "auth-a", Provider: "codex",
		RefreshState: "success", HealthStatus: "active",
		Quotas:            []QuotaWindowDTO{{QuotaKey: "code_week", Percent: &pct}},
		UpstreamCheckedAt: &now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("upsert a: %v", err)
	}
	if err := UpsertAIAccountStatus(AIAccountStatusRecord{
		TenantID: "tenant-b", AuthSubjectID: "sub-1", AuthIndex: "auth-b", Provider: "codex",
		RefreshState: "error", UpdatedAt: now,
	}); err != nil {
		t.Fatalf("upsert b: %v", err)
	}

	rowsA, err := ListAIAccountStatusForTenant("tenant-a", nil)
	if err != nil {
		t.Fatalf("list a: %v", err)
	}
	if len(rowsA) != 1 || rowsA[0].RefreshState != "success" {
		t.Fatalf("tenant-a rows = %+v", rowsA)
	}
}

func TestUpdateRefreshStatePreservesQuotas(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "usage.db")
	if err := InitDB(dbPath, config.RequestLogStorageConfig{}, time.UTC); err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	t.Cleanup(func() { CloseDB(); _ = os.Remove(dbPath) })

	pct := 70.0
	now := time.Now().UTC()
	if err := UpsertAIAccountStatus(AIAccountStatusRecord{
		TenantID: systemTenantID, AuthSubjectID: "sub-1", AuthIndex: "auth-1", Provider: "codex",
		RefreshState: "success", PlanType: "plus",
		Quotas:            []QuotaWindowDTO{{QuotaKey: "code_week", Percent: &pct}},
		UpstreamCheckedAt: &now, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	if err := UpdateAIAccountRefreshState(systemTenantID, "sub-1", "auth-1", "codex", "running", "active", "", ""); err != nil {
		t.Fatal(err)
	}
	rows, err := ListAIAccountStatusForTenant(systemTenantID, []string{"sub-1"})
	if err != nil || len(rows) != 1 {
		t.Fatalf("rows=%v err=%v", rows, err)
	}
	if rows[0].RefreshState != "running" {
		t.Fatalf("refresh=%s", rows[0].RefreshState)
	}
	if len(rows[0].Quotas) != 1 || rows[0].PlanType != "plus" {
		t.Fatalf("preserved fields lost: %+v", rows[0])
	}
}

func TestAIAccountSubjectUsageSameTxProjection(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "usage.db")
	if err := InitDB(dbPath, config.RequestLogStorageConfig{}, time.UTC); err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	t.Cleanup(func() { CloseDB(); _ = os.Remove(dbPath) })

	subject := "authsub_testsubj"
	ts := time.Now().UTC()
	InsertLogWithDetailsIdentitySubjectUpstreamVision(
		"key", "kid", subject, "name", "gpt", "", "", "src", "ch", "idx",
		false, ts, 10, 0, TokenStats{InputTokens: 1, OutputTokens: 1, TotalTokens: 2},
		"", "", "",
	)
	InsertLogWithDetailsIdentitySubjectUpstreamVision(
		"key", "kid", subject, "name", "gpt", "", "", "src", "ch", "idx",
		true, ts.Add(time.Second), 10, 0, TokenStats{InputTokens: 1, OutputTokens: 1, TotalTokens: 2},
		"", "", "",
	)
	sums, err := QueryAIAccountSubjectUsageSummaries([]string{subject}, nil)
	if err != nil {
		t.Fatal(err)
	}
	s := sums[subject]
	if s.RequestTotal != 2 || s.RequestTotal30d != 2 || s.SuccessTotal30d != 1 || s.FailureTotal30d != 1 {
		t.Fatalf("summary=%+v", s)
	}
	var legacyRows int64
	if err := getDB().QueryRow(`SELECT COUNT(*) FROM auth_subject_usage_daily WHERE auth_subject_id = ?`, subject).Scan(&legacyRows); err != nil {
		t.Fatal(err)
	}
	if legacyRows != 0 {
		t.Fatalf("legacy daily rows=%d want 0", legacyRows)
	}
}

func TestQuotaSnapshotPointDedupeHeartbeat(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "usage.db")
	if err := InitDB(dbPath, config.RequestLogStorageConfig{}, time.UTC); err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	t.Cleanup(func() { CloseDB(); _ = os.Remove(dbPath) })

	pct := 90.0
	now := time.Now().UTC()
	point := QuotaSnapshotPoint{RecordedAt: now, QuotaKey: "code_week", QuotaLabel: "week", Percent: &pct, WindowSeconds: 604800}
	if err := RecordQuotaSnapshotPointsIdentityForTenant(systemTenantID, "auth-1", "sub-1", "codex", []QuotaSnapshotPoint{point}); err != nil {
		t.Fatal(err)
	}
	point.RecordedAt = now.Add(time.Minute)
	if err := RecordQuotaSnapshotPointsIdentityForTenant(systemTenantID, "auth-1", "sub-1", "codex", []QuotaSnapshotPoint{point}); err != nil {
		t.Fatal(err)
	}
	points, err := QueryQuotaSnapshotPointsByAuthSubjectForTenant(systemTenantID, AuthSubjectMatcher{SubjectID: "sub-1"}, now.Add(-time.Hour), now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(points) != 1 {
		t.Fatalf("points=%d want 1", len(points))
	}
}

func TestNormalizeStaleRefreshStates(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "usage.db")
	if err := InitDB(dbPath, config.RequestLogStorageConfig{}, time.UTC); err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	t.Cleanup(func() { CloseDB(); _ = os.Remove(dbPath) })

	old := time.Now().UTC().Add(-time.Hour)
	if err := UpsertAIAccountStatus(AIAccountStatusRecord{
		TenantID: systemTenantID, AuthSubjectID: "sub-stale", AuthIndex: "a", Provider: "codex",
		RefreshState: "running", UpdatedAt: old,
	}); err != nil {
		t.Fatal(err)
	}
	// Force updated_at older via direct SQL (Upsert sets now).
	db := getDB()
	_, _ = db.Exec(`UPDATE ai_account_status SET updated_at = ? WHERE auth_subject_id = ?`, old.Format(time.RFC3339Nano), "sub-stale")
	n, err := NormalizeStaleAIAccountRefreshStates(systemTenantID, 15*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if n < 1 {
		t.Fatalf("normalized=%d", n)
	}
	rows, _ := ListAIAccountStatusForTenant(systemTenantID, []string{"sub-stale"})
	if rows[0].RefreshState != "error" {
		t.Fatalf("state=%s", rows[0].RefreshState)
	}
}

func TestBackfillMarkerIdempotent(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "usage.db")
	if err := InitDB(dbPath, config.RequestLogStorageConfig{}, time.UTC); err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	t.Cleanup(func() { CloseDB(); _ = os.Remove(dbPath) })
	if err := RunAuthSubjectUsageDailyBackfillAtInit(7); err != nil {
		t.Fatal(err)
	}
	if err := RunAuthSubjectUsageDailyBackfillAtInit(7); err != nil {
		t.Fatal(err)
	}
	if projectionMarkerValue(getDB(), authSubjectUsageDailyBackfillMarker) != "done" {
		t.Fatal("marker not set")
	}
}

func TestRefreshFailurePreservesLastSuccessfulSnapshot(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "usage.db")
	if err := InitDB(dbPath, config.RequestLogStorageConfig{}, time.UTC); err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	t.Cleanup(func() { CloseDB(); _ = os.Remove(dbPath) })

	pct := 72.0
	credits := int64(2)
	now := time.Now().UTC()
	if err := UpsertAIAccountStatus(AIAccountStatusRecord{
		TenantID: systemTenantID, AuthSubjectID: "sub-preserve", AuthIndex: "auth-preserve", Provider: "codex",
		RefreshState: "success", HealthStatus: "active", PlanType: "plus",
		Quotas:                 []QuotaWindowDTO{{QuotaKey: "code_week", Percent: &pct}},
		ResetCreditCount:       &credits,
		ResetCreditExpirations: []string{"2026-07-20T00:00:00Z"},
		UpstreamCheckedAt:      &now,
		UpdatedAt:              now,
	}); err != nil {
		t.Fatal(err)
	}
	failedAt := now.Add(time.Minute)
	if err := UpdateAIAccountRefreshFailure(
		systemTenantID, "sub-preserve", "auth-preserve", "codex", "degraded",
		"probe_failed", "upstream http 503", failedAt,
	); err != nil {
		t.Fatal(err)
	}
	rows, err := ListAIAccountStatusForTenant(systemTenantID, []string{"sub-preserve"})
	if err != nil || len(rows) != 1 {
		t.Fatalf("rows=%v err=%v", rows, err)
	}
	row := rows[0]
	if row.RefreshState != "error" || row.ErrorCode != "probe_failed" {
		t.Fatalf("lifecycle=%+v", row)
	}
	if row.PlanType != "plus" || len(row.Quotas) != 1 || row.Quotas[0].Percent == nil || *row.Quotas[0].Percent != pct {
		t.Fatalf("last successful quota/plan was lost: %+v", row)
	}
	if row.ResetCreditCount == nil || *row.ResetCreditCount != 2 || len(row.ResetCreditExpirations) != 1 {
		t.Fatalf("reset credits were lost: %+v", row)
	}
	if row.UpstreamCheckedAt == nil || !row.UpstreamCheckedAt.Equal(failedAt) {
		t.Fatalf("checkedAt=%v want %v", row.UpstreamCheckedAt, failedAt)
	}
}

func TestSuccessfulEmptySnapshotClearsPriorQuotaAndResetCredits(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "usage.db")
	if err := InitDB(dbPath, config.RequestLogStorageConfig{}, time.UTC); err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	t.Cleanup(func() { CloseDB(); _ = os.Remove(dbPath) })

	pct := 55.0
	credits := int64(3)
	now := time.Now().UTC()
	if err := UpsertAIAccountStatus(AIAccountStatusRecord{
		TenantID: systemTenantID, AuthSubjectID: "sub-clear", AuthIndex: "auth-clear", Provider: "codex",
		RefreshState: "success", PlanType: "plus",
		Quotas:                 []QuotaWindowDTO{{QuotaKey: "code_week", Percent: &pct}},
		ResetCreditCount:       &credits,
		ResetCreditExpirations: []string{"2026-07-20T00:00:00Z"},
		UpdatedAt:              now,
	}); err != nil {
		t.Fatal(err)
	}
	zero := int64(0)
	if err := UpsertAIAccountStatus(AIAccountStatusRecord{
		TenantID: systemTenantID, AuthSubjectID: "sub-clear", AuthIndex: "auth-clear", Provider: "codex",
		RefreshState: "success", Quotas: []QuotaWindowDTO{}, ResetCreditCount: &zero,
		ResetCreditExpirations: []string{}, UpdatedAt: now.Add(time.Minute),
	}); err != nil {
		t.Fatal(err)
	}
	rows, err := ListAIAccountStatusForTenant(systemTenantID, []string{"sub-clear"})
	if err != nil || len(rows) != 1 {
		t.Fatalf("rows=%v err=%v", rows, err)
	}
	row := rows[0]
	if row.PlanType != "" || len(row.Quotas) != 0 {
		t.Fatalf("successful empty payload did not clear old snapshot: %+v", row)
	}
	if row.ResetCreditCount == nil || *row.ResetCreditCount != 0 || len(row.ResetCreditExpirations) != 0 {
		t.Fatalf("reset credits were not cleared: %+v", row)
	}
}

func TestStaleRefreshNormalizationPreservesSnapshotPayload(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "usage.db")
	if err := InitDB(dbPath, config.RequestLogStorageConfig{}, time.UTC); err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	t.Cleanup(func() { CloseDB(); _ = os.Remove(dbPath) })

	pct := 88.0
	old := time.Now().UTC().Add(-time.Hour)
	if err := UpsertAIAccountStatus(AIAccountStatusRecord{
		TenantID: systemTenantID, AuthSubjectID: "sub-stale-payload", AuthIndex: "auth-stale", Provider: "codex",
		RefreshState: "running", PlanType: "team", Quotas: []QuotaWindowDTO{{QuotaKey: "code_week", Percent: &pct}}, UpdatedAt: old,
	}); err != nil {
		t.Fatal(err)
	}
	_, _ = getDB().Exec(`UPDATE ai_account_status SET updated_at = ? WHERE tenant_id = ? AND auth_subject_id = ?`, old.Format(time.RFC3339Nano), systemTenantID, "sub-stale-payload")
	if _, err := NormalizeStaleAIAccountRefreshStates(systemTenantID, 15*time.Minute); err != nil {
		t.Fatal(err)
	}
	rows, _ := ListAIAccountStatusForTenant(systemTenantID, []string{"sub-stale-payload"})
	if len(rows) != 1 || rows[0].RefreshState != "error" || rows[0].PlanType != "team" || len(rows[0].Quotas) != 1 {
		t.Fatalf("stale normalization lost snapshot: %+v", rows)
	}
}

func TestAIAccountSubjectDayProjectionUsesConfiguredTimezone(t *testing.T) {
	loc, err := time.LoadLocation("Asia/Singapore")
	if err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(t.TempDir(), "usage.db")
	if err := InitDB(dbPath, config.RequestLogStorageConfig{}, loc); err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	t.Cleanup(func() { CloseDB(); _ = os.Remove(dbPath) })

	now := time.Now().UTC()
	at := time.Date(now.Year(), now.Month(), now.Day(), 16, 30, 0, 0, time.UTC)
	if at.After(now) {
		at = at.AddDate(0, 0, -1)
	}
	tx, err := getDB().Begin()
	if err != nil {
		t.Fatal(err)
	}
	if err := projectAIAccountSubjectUsageTx(tx, "sub-timezone", false, 1.25, 123, at); err != nil {
		_ = tx.Rollback()
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	var dayKey string
	if err := getDB().QueryRow(`SELECT bucket_start FROM ai_account_subject_usage_buckets WHERE auth_subject_id = ? AND bucket_kind = 'day'`, "sub-timezone").Scan(&dayKey); err != nil {
		t.Fatal(err)
	}
	if want := at.In(loc).Format("2006-01-02"); dayKey != want {
		t.Fatalf("dayKey=%q want %q", dayKey, want)
	}
}

func TestBackfillMarkerPreventsRepeatedRequestLogScan(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "usage.db")
	if err := InitDB(dbPath, config.RequestLogStorageConfig{}, time.UTC); err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	t.Cleanup(func() { CloseDB(); _ = os.Remove(dbPath) })

	subject := "sub-backfill"
	at := time.Now().UTC().Add(-time.Hour)
	InsertLogWithDetailsIdentitySubjectUpstreamVision(
		"key", "kid", subject, "name", "gpt", "", "", "src", "ch", "idx",
		false, at, 10, 0, TokenStats{InputTokens: 1, OutputTokens: 1, TotalTokens: 2},
		"", "", "",
	)
	db := getDB()
	if _, err := db.Exec(`UPDATE request_logs SET cost = 2.5, failed = 0 WHERE tenant_id = ? AND auth_subject_id = ?`, systemTenantID, subject); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`DELETE FROM auth_subject_usage_daily WHERE tenant_id = ? AND auth_subject_id = ?`, systemTenantID, subject); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`DELETE FROM usage_projection_markers WHERE marker_key = ?`, authSubjectUsageDailyBackfillMarker); err != nil {
		t.Fatal(err)
	}
	if err := RunAuthSubjectUsageDailyBackfillAtInit(30); err != nil {
		t.Fatal(err)
	}
	summaries, err := QueryAuthSubjectUsageSummaries(systemTenantID, []string{subject}, nil)
	if err != nil {
		t.Fatal(err)
	}
	before := summaries[subject]
	if before.RequestTotal30d != 1 || before.CostTotal7d != 2.5 {
		t.Fatalf("backfill summary=%+v", before)
	}
	if _, err := db.Exec(`UPDATE request_logs SET cost = 99 WHERE tenant_id = ? AND auth_subject_id = ?`, systemTenantID, subject); err != nil {
		t.Fatal(err)
	}
	if err := RunAuthSubjectUsageDailyBackfillAtInit(30); err != nil {
		t.Fatal(err)
	}
	afterMap, _ := QueryAuthSubjectUsageSummaries(systemTenantID, []string{subject}, nil)
	after := afterMap[subject]
	if after.CostTotal7d != before.CostTotal7d {
		t.Fatalf("marker did not prevent repeated backfill: before=%+v after=%+v", before, after)
	}
}

func TestUsageSummaryMarksKnownCycleWithZeroRequests(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "usage.db")
	if err := InitDB(dbPath, config.RequestLogStorageConfig{}, time.UTC); err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	t.Cleanup(func() { CloseDB(); _ = os.Remove(dbPath) })

	cycleStart := time.Now().UTC().Add(-time.Hour)
	summaries, err := QueryAuthSubjectUsageSummaries(
		systemTenantID,
		[]string{"sub-zero-cycle"},
		map[string]time.Time{"sub-zero-cycle": cycleStart},
	)
	if err != nil {
		t.Fatal(err)
	}
	summary := summaries["sub-zero-cycle"]
	if !summary.CycleKnown || summary.CycleRequestTotal != 0 || summary.CycleStart != cycleStart.Format(time.RFC3339) {
		t.Fatalf("summary=%+v", summary)
	}
}

func TestStatusReadsUseReadPoolWhileWriterBusy(t *testing.T) {
	// GET status SELECTs must not queue behind the single writer connection.
	dbPath := filepath.Join(t.TempDir(), "usage.db")
	if err := InitDB(dbPath, config.RequestLogStorageConfig{}, time.UTC); err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	t.Cleanup(func() { CloseDB(); _ = os.Remove(dbPath) })

	pct := 42.0
	now := time.Now().UTC()
	if err := UpsertAIAccountStatus(AIAccountStatusRecord{
		TenantID: systemTenantID, AuthSubjectID: "sub-readpool", AuthIndex: "auth-readpool", Provider: "codex",
		RefreshState: "success", PlanType: "plus",
		Quotas:    []QuotaWindowDTO{{QuotaKey: "code_week", Percent: &pct}},
		UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	// Seed daily projection via request insert (same-tx projection).
	InsertLogWithDetailsIdentitySubjectUpstreamVision(
		"key", "kid", "sub-readpool", "name", "gpt", "", "", "src", "ch", "auth-readpool",
		false, now, 10, 0, TokenStats{InputTokens: 1, OutputTokens: 1, TotalTokens: 2},
		"", "", "",
	)

	// Hold the writer connection so any SELECT on getDB() would block (MaxOpenConns=1).
	writeDB := getDB()
	if writeDB == nil {
		t.Fatal("nil write db")
	}
	tx, err := writeDB.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(`UPDATE ai_account_status SET version = version WHERE auth_subject_id = ?`, "sub-readpool"); err != nil {
		_ = tx.Rollback()
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()

	// Prove we are not falling back to request_logs: rename it on a side connection
	// is hard under open writer tx; instead assert SELECTs finish quickly on read pool.
	done := make(chan error, 1)
	go func() {
		rows, err := ListAIAccountStatusForTenant(systemTenantID, []string{"sub-readpool"})
		if err != nil {
			done <- err
			return
		}
		if len(rows) != 1 || rows[0].PlanType != "plus" {
			done <- fmt.Errorf("list status unexpected: %+v", rows)
			return
		}
		sums, err := QueryAIAccountSubjectUsageSummaries([]string{"sub-readpool"}, nil)
		if err != nil {
			done <- err
			return
		}
		if sums["sub-readpool"].RequestTotal30d < 1 {
			done <- fmt.Errorf("summary empty: %+v", sums["sub-readpool"])
			return
		}
		if _, err := QueryLatestWeeklyQuotaCyclesBatch(systemTenantID, []string{"sub-readpool"}, []string{"code_week"}); err != nil {
			done <- err
			return
		}
		done <- nil
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("read-path blocked or failed while writer busy: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("status SELECTs timed out — still using writer pool")
	}
}
