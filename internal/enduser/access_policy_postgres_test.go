package enduser

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/identity"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/quota"
	sqlapikey "github.com/router-for-me/CLIProxyAPI/v6/internal/storage/sqlstore/apikey"
)

// 生产跑在 PostgreSQL 上（uuid 列、FOR UPDATE、compatdriver 改写）：账号与 Key 的访问
// 策略在这里完整走一遍，SQLite 测不出的类型与语法问题在这里暴露。
func TestPostgresAccessPolicyLifecycle(t *testing.T) {
	db := openLegacyLockPostgresDB(t)
	svc := NewService(db)
	ctx := context.Background()
	actor := identity.Principal{EffectiveTenant: identity.Tenant{ID: identity.SystemTenantID}, Permissions: map[string]bool{"end_users.write": true}}
	userID := uuid.NewString()
	if _, err := db.ExecContext(ctx, `INSERT INTO end_users (id, tenant_id, username, username_normalized, display_name, password_hash)
		VALUES (?, ?, 'policy_pg', 'policy_pg', 'Policy PG', 'x')`, userID, identity.SystemTenantID); err != nil {
		t.Fatalf("insert account: %v", err)
	}

	limits := &quota.MatrixPatch{}
	limits.Set(quota.MetricTokens, quota.PeriodDay, 1000)
	limits.Set(quota.MetricRequests, quota.PeriodDay, 50)
	limits.Set(quota.MetricCost, quota.PeriodLifetime, 300)
	rules := []quota.ModelRule{{Name: "Opus", Models: []string{"claude-opus-*"}, Limits: matrixOf(quota.MetricCost, quota.PeriodWeek, 20), FallbackModel: "claude-sonnet-4-5"}}
	routes := []quota.ModelRoute{{From: []string{"gpt-5"}, To: "gpt-5-mini"}}
	updated, err := svc.UpdateUser(ctx, actor, identity.SystemTenantID, userID, nil, nil, nil, nil,
		&QuotaPatch{QuotaLimits: limits, ModelRules: &rules, ModelRoutes: &routes})
	if err != nil {
		t.Fatalf("UpdateUser: %v", err)
	}
	if updated.QuotaLimits.Tokens.Day != 1000 || updated.DailyLimit != 50 || updated.SpendingLimit != 300 || len(updated.ModelRules) != 1 {
		t.Fatalf("updated = %+v", updated)
	}

	name := "pg-key"
	keyLimits := &quota.MatrixPatch{}
	keyLimits.Set(quota.MetricTokens, quota.PeriodDay, 800)
	keyLimits.Set(quota.MetricCost, quota.PeriodLifetime, 100)
	if _, err := svc.CreateKeyWithPatch(ctx, identity.SystemTenantID, userID, KeyPatch{Name: &name, QuotaLimits: cell(quota.MetricTokens, quota.PeriodDay, 5000)}); err == nil {
		t.Fatal("a key over the account token limit must be rejected")
	}
	created, err := svc.CreateKeyWithPatch(ctx, identity.SystemTenantID, userID, KeyPatch{Name: &name, QuotaLimits: keyLimits})
	if err != nil {
		t.Fatalf("create key: %v", err)
	}
	spare := "pg-spare"
	if _, err := svc.CreateKeyWithPatch(ctx, identity.SystemTenantID, userID, KeyPatch{Name: &spare}); err != nil {
		t.Fatalf("create spare: %v", err)
	}
	paused := true
	if err := svc.UpdateKeyWithPatch(ctx, identity.SystemTenantID, userID, created.APIKey.ID, KeyPatch{Disabled: &paused}); err != nil {
		t.Fatalf("pause: %v", err)
	}
	keys, err := svc.ListKeys(ctx, identity.SystemTenantID, userID)
	if err != nil || len(keys) != 2 {
		t.Fatalf("list keys = %+v, %v", keys, err)
	}
	if !keys[1].Disabled || keys[1].QuotaLimits.Tokens.Day != 800 || keys[1].QuotaLimits.Cost.Lifetime != 100 {
		t.Fatalf("paused key sorts last with its limits: %+v", keys)
	}

	lower := &quota.MatrixPatch{}
	lower.Set(quota.MetricTokens, quota.PeriodDay, 600)
	lowered, err := svc.UpdateUser(ctx, actor, identity.SystemTenantID, userID, nil, nil, nil, nil, &QuotaPatch{QuotaLimits: lower})
	if err != nil || len(lowered.CappedCells) != 1 || lowered.CappedCells[0].To != 600 {
		t.Fatalf("lower = %+v, %v", lowered.CappedCells, err)
	}

	if err := sqlapikey.NewTenantStore(db, identity.SystemTenantID).ReplaceAllPermissionProfiles([]sqlapikey.PermissionProfileRow{{
		ID: "pg-profile", Name: "PG Profile", AccessPolicy: quota.AccessPolicy{Limits: matrixOf(quota.MetricTokens, quota.PeriodDay, 400)},
	}}); err != nil {
		t.Fatalf("profiles: %v", err)
	}
	result, err := svc.BindPermissionProfile(ctx, actor, identity.SystemTenantID, []string{userID}, "pg-profile")
	if err != nil || result.Updated != 1 || len(result.CappedCells) != 1 || result.CappedCells[0].To != 400 {
		t.Fatalf("bind = %+v, %v", result, err)
	}
	row, err := svc.GetUser(ctx, identity.SystemTenantID, userID)
	if err != nil || row.PermissionProfileID != "pg-profile" || row.QuotaLimits.Tokens.Day != 400 || row.SpendingLimit != 300 {
		t.Fatalf("bound row = %+v, %v", row, err)
	}
	// 账号累计消费不归模板管：绑定后仍是账号自己的 300。
	if _, err := svc.UpdateUser(ctx, actor, identity.SystemTenantID, userID, nil, nil, nil, nil,
		&QuotaPatch{QuotaLimits: cell(quota.MetricTokens, quota.PeriodDay, 10)}); !errors.Is(err, ErrValidation) {
		t.Fatalf("profile-owned cell on bound account err = %v", err)
	}
}
