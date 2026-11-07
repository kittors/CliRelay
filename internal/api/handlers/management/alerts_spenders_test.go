package management

import (
	"context"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/alerting"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/usage"
)

const (
	spendTenant      = "00000000-0000-0000-0000-000000000001"
	spendOtherTenant = "00000000-0000-0000-0000-0000000000f2"
)

// endUsersDDL is the end_users table with every column the quota reader selects.
// The runtime database creates it elsewhere; the usage tests build it by hand too.
const endUsersDDL = `
CREATE TABLE IF NOT EXISTS end_users (
	id TEXT PRIMARY KEY, tenant_id TEXT NOT NULL, username TEXT NOT NULL, username_normalized TEXT NOT NULL,
	display_name TEXT NOT NULL, password_hash TEXT NOT NULL DEFAULT 'x', status TEXT NOT NULL DEFAULT 'active',
	permission_profile_id TEXT NOT NULL DEFAULT '',
	daily_limit INTEGER NOT NULL DEFAULT 0, total_quota INTEGER NOT NULL DEFAULT 0,
	spending_limit REAL NOT NULL DEFAULT 0, daily_spending_limit REAL NOT NULL DEFAULT 0,
	five_hour_spending_limit REAL NOT NULL DEFAULT 0, weekly_spending_limit REAL NOT NULL DEFAULT 0,
	monthly_spending_limit REAL NOT NULL DEFAULT 0,
	concurrency_limit INTEGER NOT NULL DEFAULT 0, rpm_limit INTEGER NOT NULL DEFAULT 0, tpm_limit INTEGER NOT NULL DEFAULT 0,
	allowed_models TEXT NOT NULL DEFAULT '[]', allowed_channels TEXT NOT NULL DEFAULT '[]',
	allowed_channel_groups TEXT NOT NULL DEFAULT '[]', system_prompt TEXT NOT NULL DEFAULT '',
	access_policy TEXT NOT NULL DEFAULT '{}'
)`

func insertEndUser(t *testing.T, id, tenant, name, status string, dailyLimit int) {
	t.Helper()
	if _, err := usage.RuntimeDB().Exec(`
		INSERT INTO end_users (id, tenant_id, username, username_normalized, display_name, status, daily_limit)
		VALUES (?, ?, ?, ?, ?, ?, ?)`, id, tenant, id, id, name, status, dailyLimit); err != nil {
		t.Fatalf("insert end user %s: %v", id, err)
	}
}

func upsertKey(t *testing.T, row usage.APIKeyRow) {
	t.Helper()
	now := time.Now().UTC().Format(time.RFC3339)
	row.CreatedAt, row.UpdatedAt = now, now
	if err := usage.UpsertAPIKeyForTenant(row.TenantID, row); err != nil {
		t.Fatalf("upsert key %s: %v", row.ID, err)
	}
}

func spendRequests(t *testing.T, tenant string, row usage.APIKeyRow, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		usage.InsertRequestLog(usage.RequestLogEntry{
			TrustedTenantID: tenant, APIKey: row.Key, APIKeyID: row.ID, APIKeyName: row.Name,
			Model: "test-model", Source: "test", Timestamp: time.Now().UTC(),
			Tokens: usage.TokenStats{TotalTokens: 1},
		})
	}
}

func findSpender(samples []alerting.SpendSample, kind, id string) (alerting.SpendSample, bool) {
	for _, s := range samples {
		if s.Kind == kind && s.ID == id {
			return s, true
		}
	}
	return alerting.SpendSample{}, false
}

func TestAlertSourcesSpenders(t *testing.T) {
	initAlertSourcesDB(t)
	if _, err := usage.RuntimeDB().Exec(endUsersDDL); err != nil {
		t.Fatalf("create end_users: %v", err)
	}
	now := time.Now().UTC()

	limited := usage.APIKeyRow{TenantID: spendTenant, ID: "key-limited", Key: "sk-limited-secret-value", Name: "Ops key", DailyLimit: 10}
	free := usage.APIKeyRow{TenantID: spendTenant, ID: "key-free", Key: "sk-free-secret-value", Name: "No limit"}
	off := usage.APIKeyRow{TenantID: spendTenant, ID: "key-off", Key: "sk-off-secret-value", Name: "Switched off", DailyLimit: 5, Disabled: true}
	elsewhere := usage.APIKeyRow{TenantID: spendOtherTenant, ID: "key-elsewhere", Key: "sk-elsewhere-secret-value", Name: "Other tenant", DailyLimit: 5}
	// A key follows its permission profile, whatever limit it carries itself: the
	// profile's 6 a day is what the gateway enforces, so that is what it is judged against.
	if err := usage.ReplaceAllAPIKeyPermissionProfilesForTenant(spendTenant, []usage.APIKeyPermissionProfileRow{
		{TenantID: spendTenant, ID: "profile-small", Name: "Small", DailyLimit: 6},
	}); err != nil {
		t.Fatalf("create the permission profile: %v", err)
	}
	profiled := usage.APIKeyRow{TenantID: spendTenant, ID: "key-profiled", Key: "sk-profiled-secret-value", Name: "Profiled", DailyLimit: 1000, PermissionProfileID: "profile-small"}
	for _, row := range []usage.APIKeyRow{limited, free, off, elsewhere, profiled} {
		upsertKey(t, row)
	}
	spendRequests(t, spendTenant, limited, 9)
	spendRequests(t, spendTenant, profiled, 6)
	spendRequests(t, spendOtherTenant, elsewhere, 5)

	insertEndUser(t, "eu-active", spendTenant, "Chen Long", "active", 20)
	insertEndUser(t, "eu-suspended", spendTenant, "Suspended", "disabled", 20)
	insertEndUser(t, "eu-foreign", spendOtherTenant, "Foreign", "active", 20)
	insertEndUser(t, "eu-unlimited", spendTenant, "Unlimited", "active", 0)
	owned := usage.APIKeyRow{TenantID: spendTenant, ID: "key-owned", Key: "sk-owned-secret-value", Name: "laptop", EndUserID: "eu-active"}
	for _, row := range []usage.APIKeyRow{
		owned,
		{TenantID: spendTenant, ID: "key-owned-suspended", Key: "sk-suspended-secret-value", Name: "x", EndUserID: "eu-suspended"},
		{TenantID: spendTenant, ID: "key-owned-foreign", Key: "sk-foreign-secret-value", Name: "y", EndUserID: "eu-foreign"},
		{TenantID: spendTenant, ID: "key-owned-unlimited", Key: "sk-unlimited-secret-value", Name: "z", EndUserID: "eu-unlimited"},
	} {
		upsertKey(t, row)
	}
	spendRequests(t, spendTenant, owned, 18)

	samples, err := alertSources{}.Spenders(context.Background(), spendTenant, now)
	if err != nil {
		t.Fatal(err)
	}

	key, ok := findSpender(samples, alerting.SubjectKey, "key-limited")
	if !ok {
		t.Fatalf("samples = %+v, want the limited key", samples)
	}
	if key.Name != "Ops key" || len(key.Cells) != 1 {
		t.Fatalf("limited key = %+v", key)
	}
	cell := key.Cells[0]
	if cell.Metric != "requests" || cell.Window != "day" || cell.Used != 9 || cell.Limit != 10 {
		t.Fatalf("cell = %+v, want 9 of 10 requests today", cell)
	}
	if !cell.ResetAt.After(now) || cell.ResetAt.Sub(now) > 24*time.Hour {
		t.Fatalf("cell renews at %v, want within the next day", cell.ResetAt)
	}

	viaProfile, ok := findSpender(samples, alerting.SubjectKey, "key-profiled")
	if !ok || len(viaProfile.Cells) != 1 || viaProfile.Cells[0].Limit != 6 || viaProfile.Cells[0].Used != 6 {
		t.Fatalf("profiled key = %+v ok=%v, want 6 of the profile's 6 a day, not the key's own 1000", viaProfile, ok)
	}

	user, ok := findSpender(samples, alerting.SubjectEndUser, "eu-active")
	if !ok {
		t.Fatalf("samples = %+v, want the end user", samples)
	}
	if user.Name != "Chen Long" || len(user.Cells) != 1 || user.Cells[0].Used != 18 || user.Cells[0].Limit != 20 {
		t.Fatalf("end user = %+v, want 18 of 20 requests today", user)
	}

	for _, absent := range []struct{ kind, id, why string }{
		{alerting.SubjectKey, "key-free", "a key with no limit"},
		{alerting.SubjectKey, "key-off", "a key that is switched off"},
		{alerting.SubjectKey, "key-elsewhere", "another tenant's key"},
		{alerting.SubjectKey, "key-owned", "a key owned by an end user (its limit is the user's)"},
		{alerting.SubjectEndUser, "eu-suspended", "a suspended end user"},
		{alerting.SubjectEndUser, "eu-foreign", "an end user of another tenant, even when one of this tenant's keys names it"},
		{alerting.SubjectEndUser, "eu-unlimited", "an end user with no limit"},
	} {
		if _, found := findSpender(samples, absent.kind, absent.id); found {
			t.Errorf("%s must not be sampled", absent.why)
		}
	}
	if len(samples) != 3 {
		t.Errorf("samples = %+v, want exactly the limited key, the profiled key and the active end user", samples)
	}

	// No sample may carry a key's secret: the engine copies names into messages.
	for _, s := range samples {
		for _, secret := range []string{"sk-limited-secret-value", "sk-owned-secret-value"} {
			if s.Name == secret || s.ID == secret {
				t.Errorf("sample %+v carries a key's secret", s)
			}
		}
	}

	// The same call for the other tenant sees only its own key.
	other, err := alertSources{}.Spenders(context.Background(), spendOtherTenant, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(other) != 1 || other[0].ID != "key-elsewhere" {
		t.Fatalf("other tenant's samples = %+v, want only its own key", other)
	}
	if other[0].Cells[0].Used != 5 || other[0].Cells[0].Limit != 5 {
		t.Fatalf("other tenant's cell = %+v, want 5 of 5", other[0].Cells[0])
	}
}

func TestAlertSourcesSpendersWithNothingToWatch(t *testing.T) {
	initAlertSourcesDB(t)
	samples, err := alertSources{}.Spenders(context.Background(), spendTenant, time.Now())
	if err != nil || len(samples) != 0 {
		t.Fatalf("samples = %+v err = %v, want none and no error", samples, err)
	}
}
