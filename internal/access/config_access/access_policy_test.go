package configaccess

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/accesspolicy"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/enduser"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/identity"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/quota"
	sqlapikey "github.com/router-for-me/CLIProxyAPI/v6/internal/storage/sqlstore/apikey"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/usage"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v6/sdk/config"
)

// 鉴权快照重建时一并发布每把 Key 的结构化策略：账号下的 Key 拿到模板覆盖后的账号额度、
// 账号规则与路由，以及 Key 自己的子额度与规则。
func TestRegisterPublishesProfileAppliedAccessPolicies(t *testing.T) {
	if err := usage.InitDB(filepath.Join(t.TempDir(), "usage.db"), config.RequestLogStorageConfig{}, time.UTC); err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	t.Cleanup(usage.CloseDB)
	t.Cleanup(func() { accesspolicy.Publish(nil) })
	db := usage.RuntimeDB()
	if _, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS end_users (
			id TEXT PRIMARY KEY, tenant_id TEXT NOT NULL, username TEXT NOT NULL,
			username_normalized TEXT NOT NULL UNIQUE, display_name TEXT NOT NULL, password_hash TEXT NOT NULL,
			status TEXT NOT NULL DEFAULT 'active', must_change_password INTEGER NOT NULL DEFAULT 0,
			password_changed_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP, last_login_at TEXT,
			failed_login_count INTEGER NOT NULL DEFAULT 0, lock_stage INTEGER NOT NULL DEFAULT 0, locked_until TEXT,
			created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP, updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
			version INTEGER NOT NULL DEFAULT 1
		)
	`); err != nil {
		t.Fatalf("create end_users: %v", err)
	}
	if err := usage.EnsureEndUserQuotaColumns(db); err != nil {
		t.Fatal(err)
	}
	tenantID := identity.SystemTenantID
	weekRequests := quota.Matrix{}
	weekRequests.Set(quota.MetricRequests, quota.PeriodWeek, 900)
	if err := sqlapikey.NewTenantStore(db, tenantID).ReplaceAllPermissionProfiles([]sqlapikey.PermissionProfileRow{{
		ID: "gold", Name: "Gold",
		AccessPolicy: quota.AccessPolicy{
			Limits:      weekRequests,
			ModelRules:  []quota.ModelRule{{ID: "opus", Models: []string{"claude-opus-*"}, Limits: weekRequests}},
			ModelRoutes: []quota.ModelRoute{{ID: "r", From: []string{"gpt-5"}, To: "gpt-5-mini"}},
		},
	}}); err != nil {
		t.Fatalf("profiles: %v", err)
	}
	userID := uuid.NewString()
	if _, err := db.Exec(`INSERT INTO end_users (id, tenant_id, username, username_normalized, display_name, password_hash, permission_profile_id)
		VALUES (?, ?, 'gold-user', 'gold-user', 'Gold User', 'x', 'gold')`, userID, tenantID); err != nil {
		t.Fatalf("insert account: %v", err)
	}
	svc := enduser.NewService(db)
	name := "worker"
	tokens := &quota.MatrixPatch{}
	tokens.Set(quota.MetricTokens, quota.PeriodDay, 5000)
	keyRules := []quota.ModelRule{{ID: "k", Models: []string{"gpt-5-mini"}, Limits: weekRequests}}
	key, err := svc.CreateKeyWithPatch(context.Background(), tenantID, userID, enduser.KeyPatch{Name: &name, QuotaLimits: tokens, ModelRules: &keyRules})
	if err != nil {
		t.Fatalf("create key: %v", err)
	}

	Register(&sdkconfig.SDKConfig{})
	policy := accesspolicy.Lookup(key.PlaintextKey)
	if policy == nil {
		t.Fatal("owned key with a bound profile must publish a policy")
	}
	if policy.EndUserID != userID || policy.APIKeyID != key.APIKey.ID {
		t.Fatalf("policy identity = %+v", policy)
	}
	if policy.AccountLimits.Requests.Week != 900 || len(policy.AccountRules) != 1 || len(policy.Routes) != 1 {
		t.Fatalf("account layer must come from the profile: %+v", policy)
	}
	if policy.KeyLimits.Tokens.Day != 5000 || len(policy.KeyRules) != 1 || policy.KeyRules[0].ID != "k" {
		t.Fatalf("key layer = %+v", policy)
	}

	// 删除 Key 后重建，旧明文不再查得到策略。
	other := "spare"
	if _, err := svc.CreateKeyWithPatch(context.Background(), tenantID, userID, enduser.KeyPatch{Name: &other}); err != nil {
		t.Fatal(err)
	}
	if err := svc.DeleteKey(context.Background(), tenantID, userID, key.APIKey.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	Register(&sdkconfig.SDKConfig{})
	if accesspolicy.Lookup(key.PlaintextKey) != nil {
		t.Fatal("a deleted key must not keep its policy")
	}
}
