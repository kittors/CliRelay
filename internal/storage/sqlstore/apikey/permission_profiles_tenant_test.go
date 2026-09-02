package apikey

import (
	"database/sql"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/quota"

	_ "modernc.org/sqlite"
)

func TestPermissionProfilesMigrateToTenantScopedPrimaryKeyAndSyncIsolatedAccounts(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.Exec(`
		CREATE TABLE api_key_permission_profiles (
			id TEXT PRIMARY KEY NOT NULL,
			tenant_id TEXT NOT NULL DEFAULT '00000000-0000-0000-0000-000000000001',
			name TEXT NOT NULL DEFAULT '', daily_limit INTEGER NOT NULL DEFAULT 0,
			total_quota INTEGER NOT NULL DEFAULT 0, daily_spending_limit REAL NOT NULL DEFAULT 0,
			concurrency_limit INTEGER NOT NULL DEFAULT 0, rpm_limit INTEGER NOT NULL DEFAULT 0,
			tpm_limit INTEGER NOT NULL DEFAULT 0, allowed_models TEXT NOT NULL DEFAULT '[]',
			allowed_channels TEXT NOT NULL DEFAULT '[]', allowed_channel_groups TEXT NOT NULL DEFAULT '[]',
			system_prompt TEXT NOT NULL DEFAULT '', created_at TEXT NOT NULL DEFAULT '', updated_at TEXT NOT NULL DEFAULT ''
		);
		INSERT INTO api_key_permission_profiles (tenant_id, id, name)
		VALUES ('00000000-0000-0000-0000-000000000001', 'standard', 'Legacy system');
		CREATE TABLE end_users (
			id TEXT PRIMARY KEY, tenant_id TEXT NOT NULL, permission_profile_id TEXT NOT NULL DEFAULT '',
			daily_limit INTEGER NOT NULL DEFAULT 0, total_quota INTEGER NOT NULL DEFAULT 0,
			spending_limit REAL NOT NULL DEFAULT 0, daily_spending_limit REAL NOT NULL DEFAULT 0,
			concurrency_limit INTEGER NOT NULL DEFAULT 0, rpm_limit INTEGER NOT NULL DEFAULT 0,
			tpm_limit INTEGER NOT NULL DEFAULT 0, allowed_models TEXT NOT NULL DEFAULT '[]',
			allowed_channels TEXT NOT NULL DEFAULT '[]', allowed_channel_groups TEXT NOT NULL DEFAULT '[]',
			system_prompt TEXT NOT NULL DEFAULT '', updated_at TEXT NOT NULL DEFAULT '', version INTEGER NOT NULL DEFAULT 1
		)
	`); err != nil {
		t.Fatalf("create legacy schema: %v", err)
	}

	InitPermissionProfilesTable(db)
	const tenantA = "00000000-0000-0000-0000-00000000000a"
	const tenantB = "00000000-0000-0000-0000-00000000000b"
	for _, row := range []struct {
		id       string
		tenantID string
	}{
		{id: "user-a", tenantID: tenantA},
		{id: "user-b", tenantID: tenantB},
	} {
		if _, err := db.Exec(`
			INSERT INTO end_users (id, tenant_id, permission_profile_id)
			VALUES (?, ?, 'standard')
		`, row.id, row.tenantID); err != nil {
			t.Fatalf("insert %s: %v", row.id, err)
		}
	}

	storeA := NewTenantStore(db, tenantA)
	storeB := NewTenantStore(db, tenantB)
	if count, err := storeA.ReplaceAllPermissionProfilesAndSyncEndUsers([]PermissionProfileRow{
		{ID: "standard", Name: "Tenant A", DailyLimit: 10},
	}); err != nil || count != 1 {
		t.Fatalf("tenant A replace+sync = count:%d err:%v", count, err)
	}
	if count, err := storeB.ReplaceAllPermissionProfilesAndSyncEndUsers([]PermissionProfileRow{
		{ID: "standard", Name: "Tenant B", DailyLimit: 20},
	}); err != nil || count != 1 {
		t.Fatalf("tenant B replace+sync = count:%d err:%v", count, err)
	}

	if got := storeA.ListPermissionProfiles(); len(got) != 1 || got[0].Name != "Tenant A" || got[0].DailyLimit != 10 {
		t.Fatalf("tenant A profiles = %#v", got)
	}
	if got := storeB.ListPermissionProfiles(); len(got) != 1 || got[0].Name != "Tenant B" || got[0].DailyLimit != 20 {
		t.Fatalf("tenant B profiles = %#v", got)
	}
	if got := NewStore(db).ListPermissionProfiles(); len(got) != 1 || got[0].Name != "Legacy system" {
		t.Fatalf("system profile was not preserved by migration: %#v", got)
	}

	for _, want := range []struct {
		id    string
		limit int
	}{
		{id: "user-a", limit: 10},
		{id: "user-b", limit: 20},
	} {
		var limit int
		if err := db.QueryRow(`SELECT daily_limit FROM end_users WHERE id = ?`, want.id).Scan(&limit); err != nil {
			t.Fatalf("query %s: %v", want.id, err)
		}
		if limit != want.limit {
			t.Fatalf("%s daily_limit = %d, want %d", want.id, limit, want.limit)
		}
	}
}

func TestPermissionProfileSaveCapsBoundOwnedKeysWithoutSyncAccounts(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	InitTable(db)
	if _, err := db.Exec(`CREATE TABLE end_users (
		id TEXT PRIMARY KEY, tenant_id TEXT NOT NULL, permission_profile_id TEXT NOT NULL DEFAULT '',
		daily_limit INTEGER NOT NULL DEFAULT 0, total_quota INTEGER NOT NULL DEFAULT 0,
		spending_limit REAL NOT NULL DEFAULT 0, daily_spending_limit REAL NOT NULL DEFAULT 0,
		five_hour_spending_limit REAL NOT NULL DEFAULT 0, weekly_spending_limit REAL NOT NULL DEFAULT 0, monthly_spending_limit REAL NOT NULL DEFAULT 0,
		concurrency_limit INTEGER NOT NULL DEFAULT 0, rpm_limit INTEGER NOT NULL DEFAULT 0, tpm_limit INTEGER NOT NULL DEFAULT 0,
		allowed_models TEXT NOT NULL DEFAULT '[]', allowed_channels TEXT NOT NULL DEFAULT '[]', allowed_channel_groups TEXT NOT NULL DEFAULT '[]', system_prompt TEXT NOT NULL DEFAULT '',
		updated_at TEXT NOT NULL DEFAULT '', version INTEGER NOT NULL DEFAULT 1
	)`); err != nil {
		t.Fatalf("create users: %v", err)
	}
	InitPermissionProfilesTable(db)
	tenantID := "00000000-0000-0000-0000-000000000001"
	if _, err := db.Exec(`INSERT INTO end_users(id,tenant_id,permission_profile_id,daily_spending_limit) VALUES('user-a',?,'standard',200)`, tenantID); err != nil {
		t.Fatalf("user: %v", err)
	}
	store := NewTenantStore(db, tenantID)
	if err := store.Upsert(APIKeyRow{ID: "key-a", Key: "sk-a", EndUserID: "user-a", DailySpendingLimit: 200, PeriodSpendingLimits: quota.PeriodSpendingLimits{Day: 200}}); err != nil {
		t.Fatalf("key: %v", err)
	}
	result, err := store.ReplaceAllPermissionProfilesWithCaps([]PermissionProfileRow{{ID: "standard", Name: "Standard", DailySpendingLimit: 100}}, false)
	if err != nil {
		t.Fatalf("replace: %v", err)
	}
	if result.AppliedCount != 0 || len(result.CappedKeys) != 1 || result.CappedKeys[0].ID != "key-a" || result.CappedKeys[0].Period != quota.PeriodDay || result.CappedKeys[0].From != 200 || result.CappedKeys[0].To != 100 {
		t.Fatalf("result = %+v", result)
	}
	if got := store.GetByID("key-a"); got == nil || got.DailySpendingLimit != 100 {
		t.Fatalf("stored key = %+v", got)
	}
}
