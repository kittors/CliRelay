package usage

import (
	"database/sql"
	"math"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/identityfingerprint"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

const (
	sharedSubjectTenantA = "11111111-1111-1111-1111-111111111111"
	sharedSubjectTenantB = "22222222-2222-2222-2222-222222222222"
)

func initSharedSubjectTestDB(t *testing.T) {
	t.Helper()
	CloseDB()
	if err := InitDB(filepath.Join(t.TempDir(), "usage.db"), config.RequestLogStorageConfig{}, time.UTC); err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	t.Cleanup(CloseDB)
}

func TestInitDBAddsTotalTokensToExistingSharedUsageBuckets(t *testing.T) {
	CloseDB()
	dbPath := filepath.Join(t.TempDir(), "usage.db")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`
		CREATE TABLE ai_account_subject_usage_buckets (
			auth_subject_id TEXT NOT NULL,
			bucket_kind TEXT NOT NULL,
			bucket_start TEXT NOT NULL,
			request_count INTEGER NOT NULL DEFAULT 0,
			success_count INTEGER NOT NULL DEFAULT 0,
			failure_count INTEGER NOT NULL DEFAULT 0,
			cost_total REAL NOT NULL DEFAULT 0,
			first_event_at DATETIME NOT NULL,
			updated_at DATETIME NOT NULL,
			PRIMARY KEY (auth_subject_id, bucket_kind, bucket_start)
		)
	`); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if err := InitDB(dbPath, config.RequestLogStorageConfig{}, time.UTC); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(CloseDB)

	rows, err := getDB().Query(`PRAGMA table_info(ai_account_subject_usage_buckets)`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	found := false
	for rows.Next() {
		var cid, notNull, primaryKey int
		var name, columnType string
		var defaultValue any
		if err := rows.Scan(&cid, &name, &columnType, &notNull, &defaultValue, &primaryKey); err != nil {
			t.Fatal(err)
		}
		if name == "total_tokens" {
			found = true
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if !found {
		t.Fatal("total_tokens column missing after SQLite additive migration")
	}
}

func sharedSubjectTestAuth(tenantID, authID, accountID, email string) *coreauth.Auth {
	metadata := map[string]any{}
	if accountID != "" {
		metadata["account_id"] = accountID
	}
	if email != "" {
		metadata["email"] = email
	}
	return &coreauth.Auth{
		ID:       authID,
		TenantID: tenantID,
		Provider: "codex",
		FileName: authID + ".json",
		Metadata: metadata,
	}
}

func TestResolveAuthSubjectIdentitySharesOnlyStableAccountID(t *testing.T) {
	authA := sharedSubjectTestAuth(sharedSubjectTenantA, "auth-a", "acct-shared", "a@example.com")
	authB := sharedSubjectTestAuth(sharedSubjectTenantB, "auth-b", "acct-shared", "b@example.com")
	identityA := ResolveAuthSubjectIdentity(authA)
	identityB := ResolveAuthSubjectIdentity(authB)
	if identityA == nil || identityB == nil {
		t.Fatal("expected account identities")
	}
	if identityA.ID != identityB.ID {
		t.Fatalf("stable account_id subjects differ: %q != %q", identityA.ID, identityB.ID)
	}
	if identityA.SubjectScope != AIAccountSubjectScopeShared || !identityA.ShareEligible || identityA.SeedKind != "account_id" {
		t.Fatalf("shared identity contract = %+v", identityA)
	}
	// Fingerprint account_key uses this exact ID; no second account key exists.
	if identityA.ID == "" {
		t.Fatal("account_key/auth_subject_id must be non-empty")
	}

	fallbackA := ResolveAuthSubjectIdentity(sharedSubjectTestAuth(sharedSubjectTenantA, "fallback-a", "", "same@example.com"))
	fallbackB := ResolveAuthSubjectIdentity(sharedSubjectTestAuth(sharedSubjectTenantB, "fallback-b", "", "same@example.com"))
	if fallbackA == nil || fallbackB == nil {
		t.Fatal("expected fallback identities")
	}
	if fallbackA.ID == fallbackB.ID {
		t.Fatalf("email fallback crossed tenant boundary: %q", fallbackA.ID)
	}
	for _, identity := range []*AuthSubjectIdentity{fallbackA, fallbackB} {
		if identity.SubjectScope != AIAccountSubjectScopeTenant || identity.ShareEligible || identity.SeedKind != "email" {
			t.Fatalf("tenant fallback contract = %+v", identity)
		}
	}
}

func TestUpsertAIAccountSubjectWritesBooleanHistoryComplete(t *testing.T) {
	// Regression: PG column usage_history_complete is BOOLEAN; integer 0 fails SQLSTATE 42804
	// and empties shared subject tables, which 500s /ai-accounts/status and blanks status-refresh job_id.
	initSharedSubjectTestDB(t)
	auth := sharedSubjectTestAuth(sharedSubjectTenantA, "auth-bool", "acct-bool", "bool@example.com")
	identity := ResolveAuthSubjectIdentity(auth)
	if err := UpsertAIAccountSubject(identity); err != nil {
		t.Fatalf("UpsertAIAccountSubject: %v", err)
	}
	var complete bool
	if err := getDB().QueryRow(
		`SELECT usage_history_complete FROM ai_account_subjects WHERE auth_subject_id = ?`,
		identity.ID,
	).Scan(&complete); err != nil {
		t.Fatalf("scan usage_history_complete: %v", err)
	}
	if complete {
		t.Fatalf("usage_history_complete = true, want false on insert")
	}
	if err := UpsertAIAccountTenantBinding(auth, identity); err != nil {
		t.Fatalf("UpsertAIAccountTenantBinding: %v", err)
	}
	subjects, err := ListAIAccountSubjects([]string{identity.ID})
	if err != nil || subjects[identity.ID].AuthSubjectID == "" {
		t.Fatalf("subject missing after binding upsert: %+v err=%v", subjects, err)
	}
}

func TestAIAccountTenantBindingsDeleteOneTenantOnly(t *testing.T) {
	initSharedSubjectTestDB(t)
	authA := sharedSubjectTestAuth(sharedSubjectTenantA, "auth-a", "acct-bind", "a@example.com")
	authB := sharedSubjectTestAuth(sharedSubjectTenantB, "auth-b", "acct-bind", "b@example.com")
	identityA := ResolveAuthSubjectIdentity(authA)
	identityB := ResolveAuthSubjectIdentity(authB)
	if err := UpsertAIAccountTenantBinding(authA, identityA); err != nil {
		t.Fatal(err)
	}
	if err := UpsertAIAccountTenantBinding(authB, identityB); err != nil {
		t.Fatal(err)
	}
	if identityA.ID != identityB.ID {
		t.Fatalf("subject mismatch: %q != %q", identityA.ID, identityB.ID)
	}
	if err := MarkAIAccountTenantBindingDeleted(sharedSubjectTenantA, authA.ID); err != nil {
		t.Fatal(err)
	}

	rowsA, err := ListAIAccountBindingsForTenantAuths(sharedSubjectTenantA, []string{authA.ID})
	if err != nil || len(rowsA) != 1 || rowsA[0].BindingState != "deleted" || rowsA[0].BindingRevision != 2 {
		t.Fatalf("tenant A binding=%+v err=%v", rowsA, err)
	}
	rowsB, err := ListAIAccountBindingsForTenantAuths(sharedSubjectTenantB, []string{authB.ID})
	if err != nil || len(rowsB) != 1 || rowsB[0].BindingState != "active" || rowsB[0].BindingRevision != 1 {
		t.Fatalf("tenant B binding=%+v err=%v", rowsB, err)
	}
	countsB, err := CountAIAccountTenantBindings(sharedSubjectTenantB, []string{identityB.ID})
	if err != nil || countsB[identityB.ID] != 1 {
		t.Fatalf("tenant B counts=%v err=%v", countsB, err)
	}
	subjects, err := ListAIAccountSubjects([]string{identityB.ID})
	if err != nil || subjects[identityB.ID].AuthSubjectID == "" {
		t.Fatalf("shared subject deleted with tenant A binding: %+v err=%v", subjects, err)
	}
}

func TestRequestProjectionSharesUsageWithoutTouchingBindingAndSurvivesLogCleanup(t *testing.T) {
	initSharedSubjectTestDB(t)
	authA := sharedSubjectTestAuth(sharedSubjectTenantA, "auth-a", "acct-usage", "a@example.com")
	authB := sharedSubjectTestAuth(sharedSubjectTenantB, "auth-b", "acct-usage", "b@example.com")
	identity := ResolveAuthSubjectIdentity(authA)
	if err := UpsertAIAccountTenantBinding(authA, identity); err != nil {
		t.Fatal(err)
	}
	if err := UpsertAIAccountTenantBinding(authB, ResolveAuthSubjectIdentity(authB)); err != nil {
		t.Fatal(err)
	}

	before, err := ListAIAccountBindingsForTenantAuths(sharedSubjectTenantA, []string{authA.ID})
	if err != nil || len(before) != 1 {
		t.Fatalf("binding before=%+v err=%v", before, err)
	}

	now := time.Now().UTC()
	resetAt := now.Add(48 * time.Hour)
	pct := 40.0
	if err := RecordAIAccountSubjectQuotaPoints(identity.ID, "codex", []QuotaSnapshotPoint{{
		RecordedAt: now, Provider: "codex", QuotaKey: "code_week", QuotaLabel: "Week",
		Percent: &pct, ResetAt: &resetAt, WindowSeconds: int64((7 * 24 * time.Hour) / time.Second),
	}}); err != nil {
		t.Fatal(err)
	}

	keyA := "sk-shared-a"
	keyB := "sk-shared-b"
	for _, row := range []APIKeyRow{
		{TenantID: sharedSubjectTenantA, ID: uuid.NewString(), Key: keyA, Name: "A", CreatedAt: now.Format(time.RFC3339), UpdatedAt: now.Format(time.RFC3339)},
		{TenantID: sharedSubjectTenantB, ID: uuid.NewString(), Key: keyB, Name: "B", CreatedAt: now.Format(time.RFC3339), UpdatedAt: now.Format(time.RFC3339)},
	} {
		if err := UpsertAPIKeyForTenant(row.TenantID, row); err != nil {
			t.Fatalf("UpsertAPIKeyForTenant(%s): %v", row.Key, err)
		}
	}
	InsertLogWithDetailsIdentitySubject(keyA, "", identity.ID, "A", "gpt-5.4", "codex", "A", authA.EnsureIndex(), false, now, 10, 1, TokenStats{TotalTokens: 111}, "", "", "")
	InsertLogWithDetailsIdentitySubject(keyB, "", identity.ID, "B", "gpt-5.4", "codex", "B", authB.EnsureIndex(), true, now.Add(time.Second), 10, 1, TokenStats{TotalTokens: 222}, "", "", "")

	var dayRows, dayRequests, dayTokens int64
	if err := getDB().QueryRow(`SELECT COUNT(*), COALESCE(SUM(request_count), 0), COALESCE(SUM(total_tokens), 0) FROM ai_account_subject_usage_buckets WHERE auth_subject_id = ? AND bucket_kind = 'day'`, identity.ID).Scan(&dayRows, &dayRequests, &dayTokens); err != nil {
		t.Fatal(err)
	}
	if dayRows != 1 || dayRequests != 2 || dayTokens != 333 {
		t.Fatalf("day projection rows=%d requests=%d tokens=%d", dayRows, dayRequests, dayTokens)
	}
	var legacyRows int64
	if err := getDB().QueryRow(`SELECT COUNT(*) FROM auth_subject_usage_daily WHERE auth_subject_id = ?`, identity.ID).Scan(&legacyRows); err != nil {
		t.Fatal(err)
	}
	if legacyRows != 0 {
		t.Fatalf("legacy daily rows=%d want 0", legacyRows)
	}

	start7 := time.Now().UTC().AddDate(0, 0, -6).Format("2006-01-02")
	start30 := time.Now().UTC().AddDate(0, 0, -29).Format("2006-01-02")
	var directID string
	var direct7, direct30 int64
	if err := getDB().QueryRow(`SELECT auth_subject_id, SUM(CASE WHEN bucket_start >= ? THEN request_count ELSE 0 END), SUM(request_count) FROM ai_account_subject_usage_buckets WHERE bucket_kind='day' AND bucket_start >= ? AND auth_subject_id IN (?) GROUP BY auth_subject_id`, start7, start30, identity.ID).Scan(&directID, &direct7, &direct30); err != nil {
		t.Fatal(err)
	}
	if direct7 != 2 || direct30 != 2 {
		t.Fatalf("direct day summary id=%s 7d=%d 30d=%d start7=%s start30=%s", directID, direct7, direct30, start7, start30)
	}

	cycleStarts, err := QueryLatestAIAccountSubjectWeeklyCyclesBatch([]string{identity.ID}, []string{"code_week"})
	if err != nil {
		t.Fatal(err)
	}
	summaries, err := QueryAIAccountSubjectUsageSummaries([]string{identity.ID}, cycleStarts)
	if err != nil {
		t.Fatal(err)
	}
	got := summaries[identity.ID]
	if got.RequestTotal != 2 || got.SuccessTotal != 1 || got.FailureTotal != 1 || got.RequestTotal30d != 2 || got.CycleRequestTotal != 2 || got.CycleTotalTokens != 333 || !got.CycleKnown {
		t.Fatalf("shared usage=%+v", got)
	}
	if got.SuccessRate == nil || math.Abs(*got.SuccessRate-0.5) > 1e-9 {
		t.Fatalf("success rate=%v", got.SuccessRate)
	}
	for tenantID, want := range map[string]int64{sharedSubjectTenantA: 1, sharedSubjectTenantB: 1} {
		stats, err := QueryStats(LogQueryParams{TenantID: tenantID, Days: 30})
		if err != nil || stats.Total != want {
			t.Fatalf("tenant %s generic rollup total=%d err=%v", tenantID, stats.Total, err)
		}
	}

	after, err := ListAIAccountBindingsForTenantAuths(sharedSubjectTenantA, []string{authA.ID})
	if err != nil || len(after) != 1 {
		t.Fatalf("binding after=%+v err=%v", after, err)
	}
	if after[0].BindingRevision != before[0].BindingRevision || !after[0].LastSeenAt.Equal(before[0].LastSeenAt) {
		t.Fatalf("request path mutated binding: before=%+v after=%+v", before[0], after[0])
	}

	if _, err := getDB().Exec(`DELETE FROM request_logs`); err != nil {
		t.Fatal(err)
	}
	afterCleanup, err := QueryAIAccountSubjectUsageSummaries([]string{identity.ID}, cycleStarts)
	if err != nil {
		t.Fatal(err)
	}
	clean := afterCleanup[identity.ID]
	if clean.RequestTotal != got.RequestTotal || clean.SuccessTotal != got.SuccessTotal || clean.FailureTotal != got.FailureTotal || clean.CycleRequestTotal != got.CycleRequestTotal || clean.CycleTotalTokens != got.CycleTotalTokens {
		t.Fatalf("shared usage reset after request_logs cleanup: before=%+v after=%+v", got, clean)
	}
}

func TestSharedSubjectBackfillUsesOnlySmallTablesAndIsIdempotent(t *testing.T) {
	initSharedSubjectTestDB(t)
	authA := sharedSubjectTestAuth(sharedSubjectTenantA, "backfill-a", "acct-backfill", "a@example.com")
	authB := sharedSubjectTestAuth(sharedSubjectTenantB, "backfill-b", "acct-backfill", "b@example.com")
	identity := ResolveAuthSubjectIdentity(authA)
	if err := UpsertAIAccountTenantBinding(authA, identity); err != nil {
		t.Fatal(err)
	}
	if err := UpsertAIAccountTenantBinding(authB, ResolveAuthSubjectIdentity(authB)); err != nil {
		t.Fatal(err)
	}

	now := time.Now().UTC()
	older := now.Add(-time.Hour)
	pctOld, pctNew := 25.0, 75.0
	if err := UpsertAIAccountStatus(AIAccountStatusRecord{
		TenantID: sharedSubjectTenantA, AuthSubjectID: identity.ID, AuthIndex: authA.EnsureIndex(), Provider: "codex",
		RefreshState: "success", PlanType: "plus", Quotas: []QuotaWindowDTO{{QuotaKey: "code_week", Percent: &pctOld}},
		UpstreamCheckedAt: &older, UpdatedAt: older,
	}); err != nil {
		t.Fatal(err)
	}
	if err := UpsertAIAccountStatus(AIAccountStatusRecord{
		TenantID: sharedSubjectTenantB, AuthSubjectID: identity.ID, AuthIndex: authB.EnsureIndex(), Provider: "codex",
		RefreshState: "success", PlanType: "pro", Quotas: []QuotaWindowDTO{{QuotaKey: "code_week", Percent: &pctNew}},
		UpstreamCheckedAt: &now, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	day := now.Format("2006-01-02")
	for _, row := range []struct {
		tenant  string
		success int
		failure int
		cost    float64
	}{
		{sharedSubjectTenantA, 1, 0, 1.25},
		{sharedSubjectTenantB, 0, 1, 2.75},
	} {
		if _, err := getDB().Exec(`
			INSERT INTO auth_subject_usage_daily
				(tenant_id, auth_subject_id, day_key, request_count, success_count, failure_count, cost_total, updated_at)
			VALUES (?, ?, ?, 1, ?, ?, ?, ?)
		`, row.tenant, identity.ID, day, row.success, row.failure, row.cost, now.Format(time.RFC3339Nano)); err != nil {
			t.Fatal(err)
		}
	}
	for _, tenantID := range []string{sharedSubjectTenantA, sharedSubjectTenantB} {
		tx, err := getDB().Begin()
		if err != nil {
			t.Fatal(err)
		}
		if err := projectUsageRollupTx(tx, rollupEvent{
			TenantID: tenantID, AuthSubjectID: identity.ID, Model: "gpt-5.4", Source: tenantID,
			ChannelName: tenantID, At: now, Cost: 2, Tokens: TokenStats{TotalTokens: 1},
		}); err != nil {
			_ = tx.Rollback()
			t.Fatal(err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
	}
	resetOld := now.Add(24 * time.Hour)
	resetNew := now.Add(48 * time.Hour)
	if err := RecordQuotaSnapshotPointsIdentityForTenant(sharedSubjectTenantA, authA.EnsureIndex(), identity.ID, "codex", []QuotaSnapshotPoint{{
		RecordedAt: older, QuotaKey: "code_week", QuotaLabel: "Week", Percent: &pctOld, ResetAt: &resetOld, WindowSeconds: 604800,
	}}); err != nil {
		t.Fatal(err)
	}
	if err := RecordQuotaSnapshotPointsIdentityForTenant(sharedSubjectTenantB, authB.EnsureIndex(), identity.ID, "codex", []QuotaSnapshotPoint{{
		RecordedAt: now, QuotaKey: "code_week", QuotaLabel: "Week", Percent: &pctNew, ResetAt: &resetNew, WindowSeconds: 604800,
	}}); err != nil {
		t.Fatal(err)
	}
	if err := setProjectionMarker(getDB(), usageRollupBackfillMarker, rollupMarkerDone); err != nil {
		t.Fatal(err)
	}
	// A missing request_logs table proves the migration path never falls back to detail scans.
	if _, err := getDB().Exec(`ALTER TABLE request_logs RENAME TO request_logs_hidden`); err != nil {
		t.Fatal(err)
	}

	report, err := RunAIAccountSharedSubjectBackfillAtInit()
	if err != nil {
		t.Fatalf("RunAIAccountSharedSubjectBackfillAtInit: %v", err)
	}
	if report.Subjects != 1 || report.Bindings != 2 || report.StatusRows != 1 || report.StatusPayloadConflicts != 1 || report.QuotaCycles != 1 || report.QuotaPoints != 2 || report.Checksum == "" {
		t.Fatalf("report=%+v", report)
	}
	statuses, err := ListAIAccountSubjectStatus([]string{identity.ID})
	if err != nil || len(statuses) != 1 || statuses[0].PlanType != "pro" {
		t.Fatalf("status winner=%+v err=%v", statuses, err)
	}
	summaries, err := QueryAIAccountSubjectUsageSummaries([]string{identity.ID}, map[string]time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	summary := summaries[identity.ID]
	if summary.RequestTotal != 2 || summary.SuccessTotal != 2 || summary.FailureTotal != 0 || summary.RequestTotal30d != 2 || summary.SuccessTotal30d != 1 || summary.FailureTotal30d != 1 || math.Abs(summary.CostTotal-4) > 1e-9 || !summary.HistoryComplete {
		t.Fatalf("backfilled summary=%+v", summary)
	}
	var cycleReset string
	if err := getDB().QueryRow(`SELECT reset_at FROM ai_account_subject_quota_cycles WHERE auth_subject_id = ? AND quota_key = 'code_week'`, identity.ID).Scan(&cycleReset); err != nil {
		t.Fatal(err)
	}
	parsedReset, ok := parseStoredTimeString(cycleReset)
	if !ok || !parsedReset.Equal(resetNew) {
		t.Fatalf("cycle reset=%q want %s", cycleReset, resetNew)
	}
	cycleStarts, err := QueryLatestAIAccountSubjectWeeklyCyclesBatch([]string{identity.ID}, []string{"code_week"})
	if err != nil {
		t.Fatal(err)
	}
	cycleSummaries, err := QueryAIAccountSubjectUsageSummaries([]string{identity.ID}, cycleStarts)
	if err != nil {
		t.Fatal(err)
	}
	cycleSummary := cycleSummaries[identity.ID]
	if !cycleSummary.CycleKnown || cycleSummary.CycleRequestTotal != 0 || cycleSummary.CycleCostTotal != 0 {
		t.Fatalf("cycle usage must not be estimated from legacy days: %+v", cycleSummary)
	}

	second, err := RunAIAccountSharedSubjectBackfillAtInit()
	if err != nil || second.Checksum != report.Checksum || second.UsageRows != report.UsageRows || second.QuotaPoints != report.QuotaPoints {
		t.Fatalf("done marker rerun report=%+v err=%v first=%+v", second, err, report)
	}
	if err := setProjectionMarker(getDB(), aiAccountSharedBackfillMarker, "pending"); err != nil {
		t.Fatal(err)
	}
	third, err := RunAIAccountSharedSubjectBackfillAtInit()
	if err != nil || third.Checksum != report.Checksum || third.UsageRows != report.UsageRows || third.QuotaPoints != report.QuotaPoints {
		t.Fatalf("pending rerun report=%+v err=%v first=%+v", third, err, report)
	}
}

func TestProjectAIAccountSubjectUsageLoadsPrimaryCycleOnCacheMiss(t *testing.T) {
	initSharedSubjectTestDB(t)
	auth := sharedSubjectTestAuth(sharedSubjectTenantA, "cache-miss", "acct-cache-miss", "cache@example.com")
	identity := ResolveAuthSubjectIdentity(auth)
	if err := UpsertAIAccountTenantBinding(auth, identity); err != nil {
		t.Fatal(err)
	}

	now := time.Now().UTC()
	primaryReset := now.Add(48 * time.Hour)
	additionalReset := now.Add(96 * time.Hour)
	pct := 50.0
	if err := RecordAIAccountSubjectQuotaPoints(identity.ID, "codex", []QuotaSnapshotPoint{
		{RecordedAt: now, Provider: "codex", QuotaKey: "code_week", QuotaLabel: "Week", Percent: &pct, ResetAt: &primaryReset, WindowSeconds: 604800},
		{RecordedAt: now.Add(time.Second), Provider: "codex", QuotaKey: "other_week", QuotaLabel: "Other week", Percent: &pct, ResetAt: &additionalReset, WindowSeconds: 1209600},
	}); err != nil {
		t.Fatal(err)
	}
	resetAIAccountSubjectCycleCache()
	primaryStart := primaryReset.Add(-7 * 24 * time.Hour)

	tx, err := getDB().Begin()
	if err != nil {
		t.Fatal(err)
	}
	if err := projectAIAccountSubjectUsageTx(tx, identity.ID, false, 0.5, 111, primaryStart.Add(-time.Second)); err != nil {
		_ = tx.Rollback()
		t.Fatal(err)
	}
	if err := projectAIAccountSubjectUsageTx(tx, identity.ID, false, 1.5, 321, now); err != nil {
		_ = tx.Rollback()
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	var bucketStart string
	var requests, totalTokens int64
	if err := getDB().QueryRow(`
		SELECT bucket_start, request_count, total_tokens
		FROM ai_account_subject_usage_buckets
		WHERE auth_subject_id = ? AND bucket_kind = 'cycle'
	`, identity.ID).Scan(&bucketStart, &requests, &totalTokens); err != nil {
		t.Fatal(err)
	}
	if bucketStart != formatAIAccountSubjectCycleBucketStart(primaryStart) || requests != 1 || totalTokens != 321 {
		t.Fatalf("cycle bucket start=%q requests=%d tokens=%d want %q/1/321", bucketStart, requests, totalTokens, formatAIAccountSubjectCycleBucketStart(primaryStart))
	}
	summaries, err := QueryAIAccountSubjectUsageSummaries([]string{identity.ID}, map[string]time.Time{identity.ID: primaryStart})
	if err != nil {
		t.Fatal(err)
	}
	if got := summaries[identity.ID]; !got.CycleKnown || got.CycleRequestTotal != 1 || got.CycleTotalTokens != 321 {
		t.Fatalf("cycle summary=%+v", got)
	}
}

func TestAIAccountSubjectUsageTokensBackfillUsesRollupsAndExactCycle(t *testing.T) {
	initSharedSubjectTestDB(t)
	auth := sharedSubjectTestAuth(sharedSubjectTenantA, "token-backfill", "acct-token-backfill", "tokens@example.com")
	identity := ResolveAuthSubjectIdentity(auth)
	if err := UpsertAIAccountTenantBinding(auth, identity); err != nil {
		t.Fatal(err)
	}

	now := time.Now().UTC().Truncate(time.Second)
	cycleStart := now.Add(-48 * time.Hour)
	resetAt := cycleStart.Add(7 * 24 * time.Hour)
	pct := 50.0
	if err := RecordAIAccountSubjectQuotaPoints(identity.ID, "codex", []QuotaSnapshotPoint{{
		RecordedAt: now, Provider: "codex", QuotaKey: "code_week", QuotaLabel: "Week",
		Percent: &pct, ResetAt: &resetAt, WindowSeconds: 604800,
	}}); err != nil {
		t.Fatal(err)
	}

	beforeCycle := cycleStart.Add(-time.Hour)
	insideCycle := cycleStart.Add(time.Hour)
	for _, event := range []struct {
		at     time.Time
		tokens int64
	}{{beforeCycle, 100}, {insideCycle, 200}} {
		tx, err := getDB().Begin()
		if err != nil {
			t.Fatal(err)
		}
		if err := projectUsageRollupTx(tx, rollupEvent{
			TenantID: sharedSubjectTenantA, AuthSubjectID: identity.ID, At: event.at,
			Tokens: TokenStats{TotalTokens: event.tokens},
		}); err != nil {
			_ = tx.Rollback()
			t.Fatal(err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
		if _, err := getDB().Exec(`
			INSERT INTO request_logs (tenant_id, timestamp, auth_subject_id, total_tokens)
			VALUES (?, ?, ?, ?)
		`, sharedSubjectTenantA, event.at.Format(time.RFC3339Nano), identity.ID, event.tokens); err != nil {
			t.Fatal(err)
		}
	}

	assertTotals := func(wantDay, wantLifetime, wantCycle int64) {
		t.Helper()
		var day, lifetime, cycle int64
		if err := getDB().QueryRow(`
			SELECT COALESCE(SUM(total_tokens), 0)
			FROM ai_account_subject_usage_buckets
			WHERE auth_subject_id = ? AND bucket_kind = 'day'
		`, identity.ID).Scan(&day); err != nil {
			t.Fatal(err)
		}
		if err := getDB().QueryRow(`
			SELECT total_tokens
			FROM ai_account_subject_usage_buckets
			WHERE auth_subject_id = ? AND bucket_kind = 'lifetime' AND bucket_start = '1970-01-01'
		`, identity.ID).Scan(&lifetime); err != nil {
			t.Fatal(err)
		}
		if err := getDB().QueryRow(`
			SELECT total_tokens
			FROM ai_account_subject_usage_buckets
			WHERE auth_subject_id = ? AND bucket_kind = 'cycle' AND bucket_start = ?
		`, identity.ID, formatAIAccountSubjectCycleBucketStart(cycleStart)).Scan(&cycle); err != nil {
			t.Fatal(err)
		}
		if day != wantDay || lifetime != wantLifetime || cycle != wantCycle {
			t.Fatalf("backfilled tokens day=%d lifetime=%d cycle=%d want %d/%d/%d", day, lifetime, cycle, wantDay, wantLifetime, wantCycle)
		}
	}

	if err := RunAIAccountSubjectUsageTokensBackfillAtInit(); err != nil {
		t.Fatal(err)
	}
	assertTotals(300, 300, 200)
	if got := projectionMarkerValue(getDB(), aiAccountSubjectUsageTokensBackfillMarker); got != rollupMarkerDone {
		t.Fatalf("token backfill marker=%q want %q", got, rollupMarkerDone)
	}

	if _, err := getDB().Exec(`UPDATE ai_account_subject_usage_buckets SET total_tokens = 999 WHERE auth_subject_id = ?`, identity.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := getDB().Exec(`DELETE FROM usage_projection_markers WHERE marker_key = ?`, aiAccountSubjectUsageTokensBackfillMarker); err != nil {
		t.Fatal(err)
	}
	if err := RunAIAccountSubjectUsageTokensBackfillAtInit(); err != nil {
		t.Fatal(err)
	}
	assertTotals(300, 300, 200)
}

func TestAIAccountBindingHookTracksAuthLifecycle(t *testing.T) {
	initSharedSubjectTestDB(t)
	manager := coreauth.NewManager(nil, nil, NewAIAccountBindingHook())
	auth := sharedSubjectTestAuth(sharedSubjectTenantA, "hook-auth", "acct-hook-a", "hook@example.com")
	registered, err := manager.Register(t.Context(), auth)
	if err != nil {
		t.Fatal(err)
	}
	rows, err := ListAIAccountBindingsForTenantAuths(sharedSubjectTenantA, []string{auth.ID})
	if err != nil || len(rows) != 1 || rows[0].BindingState != "active" || rows[0].BindingRevision != 1 {
		t.Fatalf("registered binding=%+v err=%v", rows, err)
	}
	firstSubject := rows[0].AuthSubjectID

	registered.Metadata["account_id"] = "acct-hook-b"
	if _, err := manager.Update(t.Context(), registered); err != nil {
		t.Fatal(err)
	}
	rows, err = ListAIAccountBindingsForTenantAuths(sharedSubjectTenantA, []string{auth.ID})
	if err != nil || len(rows) != 1 || rows[0].BindingRevision != 2 || rows[0].AuthSubjectID == firstSubject {
		t.Fatalf("updated binding=%+v err=%v", rows, err)
	}

	if _, err := manager.Delete(t.Context(), auth.ID); err != nil {
		t.Fatal(err)
	}
	rows, err = ListAIAccountBindingsForTenantAuths(sharedSubjectTenantA, []string{auth.ID})
	if err != nil || len(rows) != 1 || rows[0].BindingState != "deleted" || rows[0].BindingRevision != 3 {
		t.Fatalf("deleted binding=%+v err=%v", rows, err)
	}
}

func TestFingerprintPolicyUsesSharedSubjectKeyAndTenantFallbackIsolation(t *testing.T) {
	initSharedSubjectTestDB(t)
	authA := sharedSubjectTestAuth(sharedSubjectTenantA, "fp-a", "acct-fingerprint", "a@example.com")
	authB := sharedSubjectTestAuth(sharedSubjectTenantB, "fp-b", "acct-fingerprint", "b@example.com")
	identityA := ResolveAuthSubjectIdentity(authA)
	identityB := ResolveAuthSubjectIdentity(authB)
	if identityA == nil || identityB == nil || identityA.ID != identityB.ID {
		t.Fatalf("shared identities A=%+v B=%+v", identityA, identityB)
	}
	invalidations := 0
	invalidatedKey := ""
	unregister := RegisterIdentityFingerprintInvalidationHook(func(provider identityfingerprint.Provider, accountKey string) {
		if provider == identityfingerprint.ProviderCodex {
			invalidations++
			invalidatedKey = accountKey
		}
	})
	defer unregister()

	saved, err := SaveIdentityFingerprintAccountPolicy(identityfingerprint.AccountPolicy{
		Provider: identityfingerprint.ProviderCodex, AccountKey: identityA.ID,
		Strategy: identityfingerprint.AccountStrategyCLIPreferred,
	}, 0)
	if err != nil {
		t.Fatal(err)
	}
	readByTenantBKey, err := GetIdentityFingerprintAccountPolicy(identityfingerprint.ProviderCodex, identityB.ID)
	if err != nil || readByTenantBKey.Revision != saved.Revision || readByTenantBKey.AccountKey != identityA.ID {
		t.Fatalf("tenant B policy=%+v saved=%+v err=%v", readByTenantBKey, saved, err)
	}
	if invalidations != 1 || invalidatedKey != identityA.ID {
		t.Fatalf("invalidation count=%d key=%q", invalidations, invalidatedKey)
	}

	fallbackA := ResolveAuthSubjectIdentity(sharedSubjectTestAuth(sharedSubjectTenantA, "fp-email-a", "", "same@example.com"))
	fallbackB := ResolveAuthSubjectIdentity(sharedSubjectTestAuth(sharedSubjectTenantB, "fp-email-b", "", "same@example.com"))
	if fallbackA.ID == fallbackB.ID {
		t.Fatal("tenant-scoped fingerprint fallback merged across tenants")
	}
	if _, err := SaveIdentityFingerprintAccountPolicy(identityfingerprint.AccountPolicy{
		Provider: identityfingerprint.ProviderCodex, AccountKey: fallbackA.ID,
		Strategy: identityfingerprint.AccountStrategyCLIPreferred,
	}, 0); err != nil {
		t.Fatal(err)
	}
	fallbackBPolicy, err := GetIdentityFingerprintAccountPolicy(identityfingerprint.ProviderCodex, fallbackB.ID)
	if err != nil || fallbackBPolicy.Revision != 0 {
		t.Fatalf("tenant B fallback saw tenant A policy: %+v err=%v", fallbackBPolicy, err)
	}
}

func TestNullableStoredTimeArgNeverReturnsEmptyString(t *testing.T) {
	if got := nullableStoredTimeArg(""); got != nil {
		t.Fatalf("empty -> %v, want nil", got)
	}
	if got := nullableStoredTimeArg("   "); got != nil {
		t.Fatalf("blank -> %v, want nil", got)
	}
	if _, ok := requiredStoredTimeArg(""); ok {
		t.Fatal("required empty should fail")
	}
	raw := "2026-07-20 16:01:43.169028+00"
	got := nullableStoredTimeArg(raw)
	s, ok := got.(string)
	if !ok || s == "" {
		t.Fatalf("parseable time -> %v", got)
	}
}
