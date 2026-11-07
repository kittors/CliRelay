package enduser

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/identity"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/quota"
	sqlapikey "github.com/router-for-me/CLIProxyAPI/v6/internal/storage/sqlstore/apikey"
)

const policyTestTenant = "00000000-0000-0000-0000-000000000001"

func policyTestActor() identity.Principal {
	return identity.Principal{EffectiveTenant: identity.Tenant{ID: policyTestTenant}, Permissions: map[string]bool{"end_users.write": true}}
}

func insertPolicyTestUser(t *testing.T, db *sql.DB, name string) string {
	t.Helper()
	id := uuid.NewString()
	now := time.Now().UTC()
	if _, err := db.Exec(`
		INSERT INTO end_users (id, tenant_id, username, username_normalized, display_name, password_hash, status, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, 'x', 'active', ?, ?)
	`, id, policyTestTenant, name, name, name, now, now); err != nil {
		t.Fatalf("insert user: %v", err)
	}
	return id
}

func cell(metric quota.Metric, window quota.Period, value float64) *quota.MatrixPatch {
	p := &quota.MatrixPatch{}
	p.Set(metric, window, value)
	return p
}

func matrixOf(metric quota.Metric, window quota.Period, value float64) quota.Matrix {
	m := quota.Matrix{}
	m.Set(metric, window, value)
	return m
}

func TestUpdateUserStoresQuotaMatrixRulesAndRoutes(t *testing.T) {
	db := openEndUserTestDB(t)
	svc := NewService(db)
	ctx := context.Background()
	userID := insertPolicyTestUser(t, db, "matrix")

	limits := &quota.MatrixPatch{}
	limits.Set(quota.MetricCost, quota.PeriodWeek, 80)          // 固定列
	limits.Set(quota.MetricRequests, quota.PeriodDay, 300)      // 固定列 daily_limit
	limits.Set(quota.MetricRequests, quota.PeriodFiveHour, 120) // 策略
	limits.Set(quota.MetricTokens, quota.PeriodMonth, 5_000_000)
	rules := []quota.ModelRule{{Name: "Opus", Models: []string{"claude-opus-*"}, Limits: matrixOf(quota.MetricCost, quota.PeriodWeek, 30), FallbackModel: "claude-sonnet-4-5"}}
	routes := []quota.ModelRoute{{From: []string{"gpt-5"}, To: "gpt-5-mini"}}
	updated, err := svc.UpdateUser(ctx, policyTestActor(), policyTestTenant, userID, nil, nil, nil, nil,
		&QuotaPatch{QuotaLimits: limits, ModelRules: &rules, ModelRoutes: &routes})
	if err != nil {
		t.Fatalf("UpdateUser: %v", err)
	}
	if updated.QuotaLimits.Cost.Week != 80 || updated.QuotaLimits.Requests.Day != 300 ||
		updated.QuotaLimits.Requests.FiveHour != 120 || updated.QuotaLimits.Tokens.Month != 5_000_000 {
		t.Fatalf("quota limits = %+v", updated.QuotaLimits)
	}
	if updated.DailyLimit != 300 || updated.PeriodSpendingLimits.Week != 80 {
		t.Fatalf("legacy fields must mirror the column-backed cells: daily=%d week=%v", updated.DailyLimit, updated.PeriodSpendingLimits.Week)
	}
	if len(updated.ModelRules) != 1 || updated.ModelRules[0].ID == "" || updated.ModelRules[0].FallbackModel != "claude-sonnet-4-5" {
		t.Fatalf("rules = %+v", updated.ModelRules)
	}
	if len(updated.ModelRoutes) != 1 || updated.ModelRoutes[0].To != "gpt-5-mini" {
		t.Fatalf("routes = %+v", updated.ModelRoutes)
	}
	// 策略里不重复存固定列承载的格子。
	var raw string
	if err := db.QueryRow(`SELECT access_policy FROM end_users WHERE id = ?`, userID).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	policy, err := quota.DecodeAccessPolicy(raw)
	if err != nil || policy.Limits.Cost.Week != 0 || policy.Limits.Requests.Day != 0 || policy.Limits.Requests.FiveHour != 120 {
		t.Fatalf("stored policy = %+v (%v)", policy.Limits, err)
	}

	// 旧字段与 quota-limits 指向同一格却不一致：拒绝，不让后一个悄悄覆盖前一个。
	daily := 10
	if _, err := svc.UpdateUser(ctx, policyTestActor(), policyTestTenant, userID, nil, nil, nil, nil,
		&QuotaPatch{DailyLimit: &daily, QuotaLimits: cell(quota.MetricRequests, quota.PeriodDay, 20)}); !errors.Is(err, ErrQuotaLegacyConflict) {
		t.Fatalf("conflict err = %v", err)
	}
	// 只改其它字段时，规则与路由保持不变。
	name := "Matrix Renamed"
	after, err := svc.UpdateUser(ctx, policyTestActor(), policyTestTenant, userID, nil, &name, nil, nil, nil)
	if err != nil || len(after.ModelRules) != 1 || len(after.ModelRoutes) != 1 || after.QuotaLimits.Tokens.Month != 5_000_000 {
		t.Fatalf("after unrelated edit = %+v, %v", after, err)
	}
}

func TestUpdateUserRejectsRouteTargetsOutsideAllowedModels(t *testing.T) {
	db := openEndUserTestDB(t)
	svc := NewService(db)
	userID := insertPolicyTestUser(t, db, "allowlist")
	allowed := []string{"claude-opus-4-5"}
	routes := []quota.ModelRoute{{From: []string{"claude-opus-4-5"}, To: "claude-sonnet-4-5"}}
	_, err := svc.UpdateUser(context.Background(), policyTestActor(), policyTestTenant, userID, nil, nil, nil, nil,
		&QuotaPatch{AllowedModels: &allowed, ModelRoutes: &routes})
	var target *PolicyTargetNotAllowedError
	if !errors.As(err, &target) || target.Model != "claude-sonnet-4-5" || target.Field != "model-routes[0].to" {
		t.Fatalf("err = %v", err)
	}
	allowed = append(allowed, "claude-sonnet-4-5")
	if _, err := svc.UpdateUser(context.Background(), policyTestActor(), policyTestTenant, userID, nil, nil, nil, nil,
		&QuotaPatch{AllowedModels: &allowed, ModelRoutes: &routes}); err != nil {
		t.Fatalf("allowed target err = %v", err)
	}
}

func seedPolicyProfile(t *testing.T, db *sql.DB, profile sqlapikey.PermissionProfileRow) {
	t.Helper()
	if err := sqlapikey.NewTenantStore(db, policyTestTenant).ReplaceAllPermissionProfiles([]sqlapikey.PermissionProfileRow{profile}); err != nil {
		t.Fatalf("seed profile: %v", err)
	}
}

func TestBindingProfileCopiesSnapshotAndLocksProfileOwnedCells(t *testing.T) {
	db := openEndUserTestDB(t)
	svc := NewService(db)
	ctx := context.Background()
	userID := insertPolicyTestUser(t, db, "bound")
	seedPolicyProfile(t, db, sqlapikey.PermissionProfileRow{
		ID: "std", Name: "Standard", DailyLimit: 500, AllowedModels: []string{"claude-sonnet-4-5", "claude-opus-4-5"},
		AccessPolicy: quota.AccessPolicy{
			Limits:      matrixOf(quota.MetricTokens, quota.PeriodDay, 1_000_000),
			ModelRules:  []quota.ModelRule{{ID: "opus", Models: []string{"claude-opus-*"}, Limits: matrixOf(quota.MetricRequests, quota.PeriodDay, 50)}},
			ModelRoutes: []quota.ModelRoute{{ID: "r", From: []string{"claude-opus-4-1"}, To: "claude-opus-4-5"}},
		},
	})
	std := "std"
	if _, err := svc.UpdateUser(ctx, policyTestActor(), policyTestTenant, userID, nil, nil, nil, nil, &QuotaPatch{PermissionProfileID: &std}); err != nil {
		t.Fatalf("bind: %v", err)
	}
	row, err := svc.GetUser(ctx, policyTestTenant, userID)
	if err != nil {
		t.Fatal(err)
	}
	if row.DailyLimit != 500 || row.QuotaLimits.Tokens.Day != 1_000_000 || len(row.ModelRules) != 1 || len(row.ModelRoutes) != 1 {
		t.Fatalf("snapshot after bind = %+v", row)
	}

	// 模板拥有的格子、规则与路由不能在绑定的账号上单独改；累计消费属于账号，可以。
	if _, err := svc.UpdateUser(ctx, policyTestActor(), policyTestTenant, userID, nil, nil, nil, nil,
		&QuotaPatch{QuotaLimits: cell(quota.MetricTokens, quota.PeriodDay, 5)}); !errors.Is(err, ErrValidation) {
		t.Fatalf("profile-owned cell err = %v", err)
	}
	if _, err := svc.UpdateUser(ctx, policyTestActor(), policyTestTenant, userID, nil, nil, nil, nil,
		&QuotaPatch{QuotaLimits: cell(quota.MetricCost, quota.PeriodLifetime, 99)}); err != nil {
		t.Fatalf("lifetime cost on bound account: %v", err)
	}

	// 解绑：额度快照保留并转为可编辑；规则、路由、模型白名单这类访问范围没有显式给出就清掉。
	empty := ""
	if _, err := svc.UpdateUser(ctx, policyTestActor(), policyTestTenant, userID, nil, nil, nil, nil, &QuotaPatch{PermissionProfileID: &empty}); err != nil {
		t.Fatalf("unbind: %v", err)
	}
	row, err = svc.GetUser(ctx, policyTestTenant, userID)
	if err != nil {
		t.Fatal(err)
	}
	if row.QuotaLimits.Tokens.Day != 1_000_000 || row.QuotaLimits.Cost.Lifetime != 99 || len(row.ModelRules) != 0 || len(row.ModelRoutes) != 0 || len(row.AllowedModels) != 0 {
		t.Fatalf("after unbind = limits %+v rules %+v routes %+v allowed %v", row.QuotaLimits, row.ModelRules, row.ModelRoutes, row.AllowedModels)
	}
}

func TestOwnedKeyQuotaMatrixStaysWithinAccountAndIsCapped(t *testing.T) {
	db := openEndUserTestDB(t)
	svc := NewService(db)
	ctx := context.Background()
	userID := insertPolicyTestUser(t, db, "keycap")
	accountLimits := &quota.MatrixPatch{}
	accountLimits.Set(quota.MetricTokens, quota.PeriodDay, 1000)
	accountLimits.Set(quota.MetricCost, quota.PeriodWeek, 50)
	if _, err := svc.UpdateUser(ctx, policyTestActor(), policyTestTenant, userID, nil, nil, nil, nil, &QuotaPatch{QuotaLimits: accountLimits}); err != nil {
		t.Fatalf("account limits: %v", err)
	}
	name := "worker"
	var cellErr *quota.QuotaLimitExceedsAccountError
	if _, err := svc.CreateKeyWithPatch(ctx, policyTestTenant, userID, KeyPatch{Name: &name, QuotaLimits: cell(quota.MetricTokens, quota.PeriodDay, 2000)}); !errors.As(err, &cellErr) {
		t.Fatalf("over-account token err = %v", err)
	}
	var periodErr *quota.LimitExceedsAccountError
	if _, err := svc.CreateKeyWithPatch(ctx, policyTestTenant, userID, KeyPatch{Name: &name, QuotaLimits: cell(quota.MetricCost, quota.PeriodWeek, 60)}); !errors.As(err, &periodErr) {
		t.Fatalf("over-account weekly cost err = %v", err)
	}
	keyLimits := &quota.MatrixPatch{}
	keyLimits.Set(quota.MetricTokens, quota.PeriodDay, 800)
	keyLimits.Set(quota.MetricRequests, quota.PeriodLifetime, 30)
	keyLimits.Set(quota.MetricCost, quota.PeriodWeek, 40)
	keyRules := []quota.ModelRule{{ID: "k-opus", Models: []string{"claude-opus-*"}, Limits: matrixOf(quota.MetricRequests, quota.PeriodDay, 5)}}
	created, err := svc.CreateKeyWithPatch(ctx, policyTestTenant, userID, KeyPatch{Name: &name, QuotaLimits: keyLimits, ModelRules: &keyRules})
	if err != nil {
		t.Fatalf("create key: %v", err)
	}
	if created.APIKey.QuotaLimits.Tokens.Day != 800 || created.APIKey.PeriodSpendingLimits.Week != 40 || len(created.APIKey.ModelRules) != 1 {
		t.Fatalf("created = %+v", created.APIKey)
	}
	keys, err := svc.ListKeys(ctx, policyTestTenant, userID)
	if err != nil || len(keys) != 1 {
		t.Fatalf("list keys = %+v, %v", keys, err)
	}
	if keys[0].QuotaLimits.Requests.Lifetime != 30 || keys[0].QuotaLimits.Cost.Week != 40 || keys[0].ModelRules[0].ID != "k-opus" {
		t.Fatalf("listed key = %+v", keys[0])
	}

	// 账号降额：Key 超出的格子被压到新的天花板，包括策略里的格子。
	lower := &quota.MatrixPatch{}
	lower.Set(quota.MetricTokens, quota.PeriodDay, 500)
	lower.Set(quota.MetricCost, quota.PeriodWeek, 30)
	updated, err := svc.UpdateUser(ctx, policyTestActor(), policyTestTenant, userID, nil, nil, nil, nil, &QuotaPatch{QuotaLimits: lower})
	if err != nil {
		t.Fatalf("lower account: %v", err)
	}
	if len(updated.CappedCells) != 2 || len(updated.CappedKeys) != 1 || updated.CappedKeys[0].Period != quota.PeriodWeek {
		t.Fatalf("capped cells = %+v legacy = %+v", updated.CappedCells, updated.CappedKeys)
	}
	keys, _ = svc.ListKeys(ctx, policyTestTenant, userID)
	if keys[0].QuotaLimits.Tokens.Day != 500 || keys[0].QuotaLimits.Cost.Week != 30 || keys[0].QuotaLimits.Requests.Lifetime != 30 {
		t.Fatalf("capped key = %+v", keys[0].QuotaLimits)
	}
}

func TestPausingAndResumingOwnedKeys(t *testing.T) {
	db := openEndUserTestDB(t)
	svc := NewService(db)
	ctx := context.Background()
	userID := insertPolicyTestUser(t, db, "pause")
	first, err := svc.CreateKey(ctx, policyTestTenant, userID, "first")
	if err != nil {
		t.Fatal(err)
	}
	paused := true
	if err := svc.UpdateKeyWithPatch(ctx, policyTestTenant, userID, first.APIKey.ID, KeyPatch{Disabled: &paused}); !errors.Is(err, ErrLastKey) {
		t.Fatalf("pause last key err = %v", err)
	}
	second, err := svc.CreateKey(ctx, policyTestTenant, userID, "second")
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.UpdateKeyWithPatch(ctx, policyTestTenant, userID, first.APIKey.ID, KeyPatch{Disabled: &paused}); err != nil {
		t.Fatalf("pause default key: %v", err)
	}
	keys, err := svc.ListKeys(ctx, policyTestTenant, userID)
	if err != nil || len(keys) != 2 {
		t.Fatalf("keys = %+v, %v", keys, err)
	}
	byID := map[string]APIKey{keys[0].ID: keys[0], keys[1].ID: keys[1]}
	if !byID[first.APIKey.ID].Disabled || byID[first.APIKey.ID].IsDefault || !byID[second.APIKey.ID].IsDefault {
		t.Fatalf("after pause = %+v", keys)
	}
	// 暂停的 Key 仍占用名称，恢复时不会出现两把同名的 Key。
	dup := "FIRST"
	if _, err := svc.CreateKeyWithPatch(ctx, policyTestTenant, userID, KeyPatch{Name: &dup}); !errors.Is(err, ErrDuplicateKeyName) {
		t.Fatalf("duplicate of paused key name err = %v", err)
	}
	resumed := false
	if err := svc.UpdateKeyWithPatch(ctx, policyTestTenant, userID, first.APIKey.ID, KeyPatch{Disabled: &resumed}); err != nil {
		t.Fatalf("resume: %v", err)
	}
	var disabled int
	var secret string
	if err := db.QueryRow(`SELECT disabled, key FROM api_keys WHERE id = ?`, first.APIKey.ID).Scan(&disabled, &secret); err != nil {
		t.Fatal(err)
	}
	if disabled != 0 || secret != first.PlaintextKey {
		t.Fatalf("resumed key disabled=%d secret kept=%v", disabled, secret == first.PlaintextKey)
	}
}

func TestBatchBindingAndCreateWithProfile(t *testing.T) {
	db := openEndUserTestDB(t)
	svc := NewService(db)
	ctx := context.Background()
	seedPolicyProfile(t, db, sqlapikey.PermissionProfileRow{ID: "team", Name: "Team", RPMLimit: 60,
		AccessPolicy: quota.AccessPolicy{Limits: matrixOf(quota.MetricRequests, quota.PeriodWeek, 700)}})
	a := insertPolicyTestUser(t, db, "batch-a")
	b := insertPolicyTestUser(t, db, "batch-b")

	if _, err := svc.BindPermissionProfile(ctx, policyTestActor(), policyTestTenant, []string{a}, "missing"); !errors.Is(err, ErrValidation) {
		t.Fatalf("missing profile err = %v", err)
	}
	if _, err := svc.BindPermissionProfile(ctx, policyTestActor(), policyTestTenant, nil, "team"); !errors.Is(err, ErrValidation) {
		t.Fatalf("empty ids err = %v", err)
	}
	result, err := svc.BindPermissionProfile(ctx, policyTestActor(), policyTestTenant, []string{a, b, a}, "team")
	if err != nil || result.Updated != 2 {
		t.Fatalf("bind batch = %+v, %v", result, err)
	}
	for _, id := range []string{a, b} {
		row, err := svc.GetUser(ctx, policyTestTenant, id)
		if err != nil || row.PermissionProfileID != "team" || row.RPMLimit != 60 || row.QuotaLimits.Requests.Week != 700 {
			t.Fatalf("bound %s = %+v, %v", id, row, err)
		}
	}
	if _, err := svc.BindPermissionProfile(ctx, policyTestActor(), policyTestTenant, []string{a, uuid.NewString()}, "team"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown account in batch err = %v", err)
	}

	created, err := svc.CreateUser(ctx, policyTestActor(), policyTestTenant, "", "Provisioned", "", WithPermissionProfile("team"))
	if err != nil {
		t.Fatalf("create with profile: %v", err)
	}
	if created.User.PermissionProfileID != "team" || created.User.QuotaLimits.Requests.Week != 700 {
		t.Fatalf("created user = %+v", created.User)
	}
	if _, err := svc.CreateUser(ctx, policyTestActor(), policyTestTenant, "", "Bad", "", WithPermissionProfile("missing")); !errors.Is(err, ErrValidation) {
		t.Fatalf("create with missing profile err = %v", err)
	}
}
