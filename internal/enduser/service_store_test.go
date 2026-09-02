package enduser

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/identity"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/quota"
	sqlapikey "github.com/router-for-me/CLIProxyAPI/v6/internal/storage/sqlstore/apikey"
	_ "modernc.org/sqlite"
)

func openEndUserTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+t.Name()+"?mode=memory&cache=shared")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	sqlapikey.InitTable(db)
	sqlapikey.InitPermissionProfilesTable(db)
	if _, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS end_users (
			id TEXT PRIMARY KEY,
			tenant_id TEXT NOT NULL,
			username TEXT NOT NULL,
			username_normalized TEXT NOT NULL UNIQUE,
			display_name TEXT NOT NULL,
			password_hash TEXT NOT NULL,
			status TEXT NOT NULL DEFAULT 'active',
			must_change_password INTEGER NOT NULL DEFAULT 0,
			password_changed_at TEXT,
			last_login_at TEXT,
			failed_login_count INTEGER NOT NULL DEFAULT 0,
			lock_stage INTEGER NOT NULL DEFAULT 0,
			locked_until TEXT,
			created_at DATETIME NOT NULL,
			updated_at DATETIME NOT NULL,
			version INTEGER NOT NULL DEFAULT 1,
			permission_profile_id TEXT NOT NULL DEFAULT '',
			daily_limit INTEGER NOT NULL DEFAULT 0,
			total_quota INTEGER NOT NULL DEFAULT 0,
			spending_limit REAL NOT NULL DEFAULT 0,
			daily_spending_limit REAL NOT NULL DEFAULT 0,
			concurrency_limit INTEGER NOT NULL DEFAULT 0,
			rpm_limit INTEGER NOT NULL DEFAULT 0,
			tpm_limit INTEGER NOT NULL DEFAULT 0,
			allowed_models TEXT NOT NULL DEFAULT '[]',
			allowed_channels TEXT NOT NULL DEFAULT '[]',
			allowed_channel_groups TEXT NOT NULL DEFAULT '[]',
			system_prompt TEXT NOT NULL DEFAULT ''
		)
	`); err != nil {
		t.Fatalf("create end_users: %v", err)
	}
	return db
}

func TestDeleteKeyPromotesDefaultOnSQLite(t *testing.T) {
	t.Parallel()
	db := openEndUserTestDB(t)
	svc := NewService(db)
	ctx := context.Background()
	tenantID := "00000000-0000-0000-0000-000000000001"
	userID := uuid.NewString()
	if _, err := db.Exec(`
		INSERT INTO end_users (id, tenant_id, username, username_normalized, display_name, password_hash, status, created_at, updated_at)
		VALUES (?, ?, 'alice', 'alice', 'Alice', 'x', 'active', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z')
	`, userID, tenantID); err != nil {
		t.Fatalf("insert user: %v", err)
	}

	first, err := svc.CreateKey(ctx, tenantID, userID, "first")
	if err != nil {
		t.Fatalf("create first: %v", err)
	}
	second, err := svc.CreateKey(ctx, tenantID, userID, "second")
	if err != nil {
		t.Fatalf("create second: %v", err)
	}
	if !first.APIKey.IsDefault {
		t.Fatal("first key should be default")
	}
	if second.APIKey.IsDefault {
		t.Fatal("second key should not be default")
	}

	if err := svc.DeleteKey(ctx, tenantID, userID, first.APIKey.ID); err != nil {
		t.Fatalf("delete default key: %v", err)
	}
	keys, err := svc.ListKeys(ctx, tenantID, userID)
	if err != nil {
		t.Fatalf("list keys: %v", err)
	}
	if len(keys) != 1 {
		t.Fatalf("keys after delete = %d, want 1", len(keys))
	}
	if !keys[0].IsDefault {
		t.Fatal("remaining key must be promoted to default")
	}
	var disabled int
	var storedSecret string
	if err := db.QueryRow(`SELECT disabled, key FROM api_keys WHERE tenant_id = ? AND id = ?`, tenantID, first.APIKey.ID).Scan(&disabled, &storedSecret); err != nil {
		t.Fatalf("query soft-deleted key: %v", err)
	}
	if disabled != 1 {
		t.Fatalf("disabled = %d, want 1", disabled)
	}
	if storedSecret == first.PlaintextKey || !strings.HasPrefix(storedSecret, "sk-deleted-") {
		t.Fatalf("soft-deleted secret not invalidated: %q", storedSecret)
	}
	if err := svc.DeleteKey(ctx, tenantID, userID, keys[0].ID); !errors.Is(err, ErrLastKey) {
		t.Fatalf("delete last key err = %v, want ErrLastKey", err)
	}
}

func TestSetDefaultKeyOnSQLite(t *testing.T) {
	t.Parallel()
	db := openEndUserTestDB(t)
	svc := NewService(db)
	ctx := context.Background()
	tenantID := "00000000-0000-0000-0000-000000000001"
	userID := uuid.NewString()
	if _, err := db.Exec(`
		INSERT INTO end_users (id, tenant_id, username, username_normalized, display_name, password_hash, status, created_at, updated_at)
		VALUES (?, ?, 'bob', 'bob', 'Bob', 'x', 'active', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z')
	`, userID, tenantID); err != nil {
		t.Fatalf("insert user: %v", err)
	}
	a, err := svc.CreateKey(ctx, tenantID, userID, "a")
	if err != nil {
		t.Fatalf("create a: %v", err)
	}
	b, err := svc.CreateKey(ctx, tenantID, userID, "b")
	if err != nil {
		t.Fatalf("create b: %v", err)
	}
	if _, err := svc.CreateKey(ctx, tenantID, userID, "A"); !errors.Is(err, ErrDuplicateKeyName) {
		t.Fatalf("duplicate name err = %v, want ErrDuplicateKeyName", err)
	}
	if err := svc.UpdateKeyName(ctx, tenantID, userID, b.APIKey.ID, "a"); !errors.Is(err, ErrDuplicateKeyName) {
		t.Fatalf("rename to duplicate err = %v, want ErrDuplicateKeyName", err)
	}
	if err := svc.UpdateKeyName(ctx, tenantID, userID, a.APIKey.ID, "a"); err != nil {
		t.Fatalf("keep same name: %v", err)
	}
	if err := svc.SetDefaultKey(ctx, tenantID, userID, b.APIKey.ID); err != nil {
		t.Fatalf("set default: %v", err)
	}
	keys, err := svc.ListKeys(ctx, tenantID, userID)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	var defaultCount int
	for _, k := range keys {
		if k.IsDefault {
			defaultCount++
			if k.ID != b.APIKey.ID {
				t.Fatalf("default key id = %s, want %s", k.ID, b.APIKey.ID)
			}
		}
	}
	if defaultCount != 1 {
		t.Fatalf("default count = %d, want 1", defaultCount)
	}
	_ = a
}

func TestUpdateUserAutoCapsOwnedKeyPeriodLimits(t *testing.T) {
	db := openEndUserTestDB(t)
	svc := NewService(db)
	ctx := context.Background()
	tenantID := "00000000-0000-0000-0000-000000000001"
	userID := uuid.NewString()
	now := time.Now().UTC()
	if _, err := db.Exec(`
		INSERT INTO end_users (id, tenant_id, username, username_normalized, display_name, password_hash, status, daily_spending_limit, created_at, updated_at)
		VALUES (?, ?, 'cap', 'cap', 'Cap', 'x', 'active', 200, ?, ?)
	`, userID, tenantID, now, now); err != nil {
		t.Fatalf("insert user: %v", err)
	}
	day := 150.0
	created, err := svc.CreateKeyWithPeriodLimits(ctx, tenantID, userID, "limited", &quota.PeriodSpendingLimitsPatch{Day: &day})
	if err != nil {
		t.Fatalf("create key: %v", err)
	}
	newDay := 100.0
	actor := identity.Principal{EffectiveTenant: identity.Tenant{ID: tenantID}, Permissions: map[string]bool{"end_users.write": true}}
	updated, err := svc.UpdateUser(ctx, actor, tenantID, userID, nil, nil, nil, nil, &QuotaPatch{PeriodSpendingLimits: &quota.PeriodSpendingLimitsPatch{Day: &newDay}})
	if err != nil {
		t.Fatalf("UpdateUser: %v", err)
	}
	if len(updated.CappedKeys) != 1 || updated.CappedKeys[0].ID != created.APIKey.ID || updated.CappedKeys[0].Period != quota.PeriodDay || updated.CappedKeys[0].From != 150 || updated.CappedKeys[0].To != 100 {
		t.Fatalf("capped keys = %+v", updated.CappedKeys)
	}
	var stored float64
	if err := db.QueryRow(`SELECT daily_spending_limit FROM api_keys WHERE id = ?`, created.APIKey.ID).Scan(&stored); err != nil {
		t.Fatalf("query key: %v", err)
	}
	if stored != 100 {
		t.Fatalf("stored day = %v, want 100", stored)
	}
}

// Unbinding a permission profile used to leave the profile's model/channel
// scopes on the account. The console renders those scopes only while a profile
// is attached, so the account read "unrestricted" while a stale channel-group
// whitelist kept every credential outside that group unreachable — with nowhere
// in the UI to see it, and no error message that named it.
func TestUnbindingPermissionProfileClearsInheritedScopes(t *testing.T) {
	db := openEndUserTestDB(t)
	svc := NewService(db)
	ctx := context.Background()
	tenantID := "00000000-0000-0000-0000-000000000001"
	userID := uuid.NewString()
	now := time.Now().UTC()
	if _, err := db.Exec(`
		INSERT INTO end_users (id, tenant_id, username, username_normalized, display_name, password_hash, status,
			permission_profile_id, allowed_models, allowed_channels, allowed_channel_groups, system_prompt, created_at, updated_at)
		VALUES (?, ?, 'scoped', 'scoped', 'Scoped', 'x', 'active',
			'profile-1', '["grok-4.5"]', '["Grok Account"]', '["group"]', 'be brief', ?, ?)
	`, userID, tenantID, now, now); err != nil {
		t.Fatalf("insert user: %v", err)
	}

	actor := identity.Principal{EffectiveTenant: identity.Tenant{ID: tenantID}, Permissions: map[string]bool{"end_users.write": true}}
	empty := ""
	if _, err := svc.UpdateUser(ctx, actor, tenantID, userID, nil, nil, nil, nil, &QuotaPatch{PermissionProfileID: &empty}); err != nil {
		t.Fatalf("UpdateUser: %v", err)
	}

	var profileID, models, channels, groups, prompt string
	if err := db.QueryRow(`SELECT permission_profile_id, allowed_models, allowed_channels, allowed_channel_groups, system_prompt
		FROM end_users WHERE id = ?`, userID).Scan(&profileID, &models, &channels, &groups, &prompt); err != nil {
		t.Fatalf("query user: %v", err)
	}
	if profileID != "" {
		t.Fatalf("permission_profile_id = %q, want cleared", profileID)
	}
	for name, got := range map[string]string{
		"allowed_models":         models,
		"allowed_channels":       channels,
		"allowed_channel_groups": groups,
	} {
		if got != "[]" {
			t.Errorf("%s = %q, want an empty list after unbinding", name, got)
		}
	}
	if prompt != "" {
		t.Errorf("system_prompt = %q, want cleared after unbinding", prompt)
	}
}

// Switching between profiles must not trip the unbind cleanup, and an explicit
// scope in the same patch still wins over it.
func TestProfileScopesSurviveWhenNotUnbinding(t *testing.T) {
	db := openEndUserTestDB(t)
	svc := NewService(db)
	ctx := context.Background()
	tenantID := "00000000-0000-0000-0000-000000000001"
	actor := identity.Principal{EffectiveTenant: identity.Tenant{ID: tenantID}, Permissions: map[string]bool{"end_users.write": true}}
	now := time.Now().UTC()

	rebind := uuid.NewString()
	if _, err := db.Exec(`
		INSERT INTO end_users (id, tenant_id, username, username_normalized, display_name, password_hash, status,
			permission_profile_id, allowed_channel_groups, created_at, updated_at)
		VALUES (?, ?, 'rebind', 'rebind', 'Rebind', 'x', 'active', 'profile-1', '["group"]', ?, ?)
	`, rebind, tenantID, now, now); err != nil {
		t.Fatalf("insert rebind user: %v", err)
	}
	other := "profile-2"
	if _, err := svc.UpdateUser(ctx, actor, tenantID, rebind, nil, nil, nil, nil, &QuotaPatch{PermissionProfileID: &other}); err != nil {
		t.Fatalf("UpdateUser rebind: %v", err)
	}
	var groups string
	if err := db.QueryRow(`SELECT allowed_channel_groups FROM end_users WHERE id = ?`, rebind).Scan(&groups); err != nil {
		t.Fatalf("query rebind: %v", err)
	}
	if groups != `["group"]` {
		t.Fatalf("allowed_channel_groups = %q, want untouched when switching profiles", groups)
	}

	explicit := uuid.NewString()
	if _, err := db.Exec(`
		INSERT INTO end_users (id, tenant_id, username, username_normalized, display_name, password_hash, status,
			permission_profile_id, allowed_channel_groups, created_at, updated_at)
		VALUES (?, ?, 'explicit', 'explicit', 'Explicit', 'x', 'active', 'profile-1', '["group"]', ?, ?)
	`, explicit, tenantID, now, now); err != nil {
		t.Fatalf("insert explicit user: %v", err)
	}
	empty := ""
	kept := []string{"default"}
	if _, err := svc.UpdateUser(ctx, actor, tenantID, explicit, nil, nil, nil, nil, &QuotaPatch{
		PermissionProfileID:  &empty,
		AllowedChannelGroups: &kept,
	}); err != nil {
		t.Fatalf("UpdateUser explicit: %v", err)
	}
	if err := db.QueryRow(`SELECT allowed_channel_groups FROM end_users WHERE id = ?`, explicit).Scan(&groups); err != nil {
		t.Fatalf("query explicit: %v", err)
	}
	if groups != `["default"]` {
		t.Fatalf("allowed_channel_groups = %q, want the explicit value in the same patch", groups)
	}
}
