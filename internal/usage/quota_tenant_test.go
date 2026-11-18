package usage

import (
	"database/sql"
	"slices"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

func TestMigrateQuotaTenantPrimaryKeysRebuildsLegacyTables(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err = db.Exec(`
		CREATE TABLE auth_file_quota_snapshots (
			tenant_id TEXT NOT NULL, date_key TEXT NOT NULL, auth_index TEXT NOT NULL,
			auth_subject_id TEXT NOT NULL DEFAULT '', provider TEXT NOT NULL DEFAULT '',
			quota_key TEXT NOT NULL, percent REAL, recorded_at DATETIME NOT NULL,
			PRIMARY KEY (date_key, auth_index, quota_key));
		CREATE TABLE auth_subject_quota_cycles (
			tenant_id TEXT NOT NULL, subject_id TEXT NOT NULL, auth_index TEXT NOT NULL DEFAULT '',
			provider TEXT NOT NULL DEFAULT '', quota_key TEXT NOT NULL, cycle_start_at DATETIME NOT NULL,
			reset_at DATETIME NOT NULL, window_seconds INTEGER NOT NULL DEFAULT 0,
			last_verified_at DATETIME NOT NULL, PRIMARY KEY (subject_id, quota_key));
	`); err != nil {
		t.Fatal(err)
	}

	migrateQuotaTenantPrimaryKeys(db)
	for table, want := range map[string][]string{
		"auth_file_quota_snapshots": {"tenant_id", "date_key", "auth_index", "quota_key"},
		"auth_subject_quota_cycles": {"tenant_id", "subject_id", "quota_key"},
	} {
		got, err := sqlitePrimaryKeyColumns(db, table)
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(got, want) {
			t.Fatalf("%s primary key = %v, want %v", table, got, want)
		}
	}
	for _, tenantID := range []string{"tenant-a", "tenant-b"} {
		if _, err = db.Exec(`INSERT INTO auth_file_quota_snapshots
			(tenant_id,date_key,auth_index,quota_key,recorded_at) VALUES (?,?,?,?,?)`,
			tenantID, "2026-07-11", "same-auth", "weekly", time.Now().UTC()); err != nil {
			t.Fatalf("insert snapshot for %s: %v", tenantID, err)
		}
	}
}

func TestQuotaSnapshotsAreTenantScoped(t *testing.T) {
	initTestUsageDB(t, config.RequestLogStorageConfig{})
	const tenantA = "00000000-0000-0000-0000-00000000000a"
	const tenantB = "00000000-0000-0000-0000-00000000000b"
	percentA, percentB := 10.0, 90.0
	now := time.Now().UTC()
	reset := now.Add(7 * 24 * time.Hour)
	for tenantID, percent := range map[string]*float64{tenantA: &percentA, tenantB: &percentB} {
		if err := RecordQuotaSnapshotPointsIdentityForTenant(tenantID, "same-auth", "same-subject", "codex", []QuotaSnapshotPoint{{
			RecordedAt: now, QuotaKey: "weekly", QuotaLabel: "Weekly", Percent: percent,
			ResetAt: &reset, WindowSeconds: 7 * 24 * 60 * 60,
		}}); err != nil {
			t.Fatalf("record %s: %v", tenantID, err)
		}
	}

	pointsA, err := QueryQuotaSnapshotPointsByAuthSubjectForTenant(tenantA, AuthSubjectMatcher{SubjectID: "same-subject"}, now.Add(-time.Minute), now.Add(time.Minute))
	if err != nil || len(pointsA) != 1 || pointsA[0].Percent == nil || *pointsA[0].Percent != percentA {
		t.Fatalf("tenant A points = %#v, err=%v", pointsA, err)
	}
	pointsB, err := QueryQuotaSnapshotPointsByAuthSubjectForTenant(tenantB, AuthSubjectMatcher{SubjectID: "same-subject"}, now.Add(-time.Minute), now.Add(time.Minute))
	if err != nil || len(pointsB) != 1 || pointsB[0].Percent == nil || *pointsB[0].Percent != percentB {
		t.Fatalf("tenant B points = %#v, err=%v", pointsB, err)
	}
	cycleA, err := QueryLatestWeeklyQuotaCycleByAuthSubjectForTenant(tenantA, "same-subject", "weekly")
	if err != nil || cycleA == nil || cycleA.SubjectID != "same-subject" {
		t.Fatalf("tenant A cycle = %#v, err=%v", cycleA, err)
	}
}
