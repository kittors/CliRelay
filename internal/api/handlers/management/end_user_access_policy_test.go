package management

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/enduser"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/identity"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/quota"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/quota/inflight"
	sqlapikey "github.com/router-for-me/CLIProxyAPI/v6/internal/storage/sqlstore/apikey"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/usage"
)

type accessPolicyFixture struct {
	db        *sql.DB
	svc       *enduser.Service
	h         *Handler
	principal identity.Principal
	userID    string
}

func newAccessPolicyFixture(t *testing.T) accessPolicyFixture {
	t.Helper()
	gin.SetMode(gin.TestMode)
	if err := usage.InitDB(filepath.Join(t.TempDir(), "usage.db"), config.RequestLogStorageConfig{}, time.UTC); err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	t.Cleanup(usage.CloseDB)
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
	svc := enduser.NewService(db)
	enduser.SetDefault(svc)
	t.Cleanup(func() { enduser.SetDefault(nil) })
	userID := uuid.NewString()
	if _, err := db.Exec(`INSERT INTO end_users (id, tenant_id, username, username_normalized, display_name, password_hash)
		VALUES (?, ?, 'policy', 'policy', 'Policy User', 'x')`, userID, identity.SystemTenantID); err != nil {
		t.Fatalf("insert user: %v", err)
	}
	return accessPolicyFixture{
		db: db, svc: svc, h: &Handler{}, userID: userID,
		principal: identity.Principal{PlatformAdmin: true, EffectiveTenant: identity.Tenant{ID: identity.SystemTenantID}},
	}
}

func (f accessPolicyFixture) call(t *testing.T, handler gin.HandlerFunc, method, target, body string, params gin.Params) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Set(managementPrincipalKey, f.principal)
	c.Params = params
	var reader *bytes.Buffer
	if body != "" {
		reader = bytes.NewBufferString(body)
	} else {
		reader = &bytes.Buffer{}
	}
	c.Request = httptest.NewRequest(method, target, reader)
	c.Request.Header.Set("Content-Type", "application/json")
	handler(c)
	return rec
}

func TestGetEndUsersReturnsEffectivePolicyUsageAndCapabilities(t *testing.T) {
	f := newAccessPolicyFixture(t)
	weekRequests := quota.Matrix{}
	weekRequests.Set(quota.MetricRequests, quota.PeriodWeek, 400)
	if err := sqlapikey.NewTenantStore(f.db, identity.SystemTenantID).ReplaceAllPermissionProfiles([]sqlapikey.PermissionProfileRow{{
		ID: "pro", Name: "Pro", AccessPolicy: quota.AccessPolicy{
			Limits:     weekRequests,
			ModelRules: []quota.ModelRule{{ID: "opus", Models: []string{"claude-opus-*"}, Limits: weekRequests}},
		},
	}}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.Exec(`UPDATE end_users SET permission_profile_id = 'pro' WHERE id = ?`, f.userID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.Exec(`INSERT INTO usage_rollup_buckets (tenant_id,bucket_kind,bucket_start,end_user_id,model,request_count,updated_at)
		VALUES (?,?,?,?,?,?,?)`, identity.SystemTenantID, "day", usage.LocalDayKeyAt(time.Now()), f.userID, "claude-opus-4-5", 7, time.Now()); err != nil {
		t.Fatal(err)
	}

	rec := f.call(t, f.h.GetEndUsers, http.MethodGet, "/v0/management/end-users", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body)
	}
	var body struct {
		Items        []enduser.User  `json:"items"`
		Capabilities map[string]bool `json:"capabilities"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if !body.Capabilities["access-policy"] || !body.Capabilities["model-fallback"] || !body.Capabilities["batch-profile-binding"] {
		t.Fatalf("capabilities = %+v", body.Capabilities)
	}
	if len(body.Items) != 1 {
		t.Fatalf("items = %+v", body.Items)
	}
	user := body.Items[0]
	// 账号行上没有这些值：它们来自绑定的模板。
	if user.QuotaLimits.Requests.Week != 400 || len(user.ModelRules) != 1 || user.QuotaUsage == nil {
		t.Fatalf("effective user = %+v", user)
	}
	if user.QuotaUsage.Total.Requests.Week != 7 || len(user.QuotaUsage.Rules) != 1 || user.QuotaUsage.Rules[0].Used.Requests.Day != 7 {
		t.Fatalf("usage view = %+v", user.QuotaUsage)
	}
}

func TestGetEndUsersShowsWhatTheAccountHasInFlight(t *testing.T) {
	f := newAccessPolicyFixture(t)
	list := func() *enduser.User {
		t.Helper()
		rec := f.call(t, f.h.GetEndUsers, http.MethodGet, "/v0/management/end-users", "", nil)
		var body struct {
			Items []enduser.User `json:"items"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || len(body.Items) != 1 {
			t.Fatalf("status = %d body = %s", rec.Code, rec.Body)
		}
		return &body.Items[0]
	}
	r := inflight.Default().Reserve("", inflight.Owner{Key: inflight.KeyName("key-in-flight"), Account: inflight.AccountName(f.userID)})
	t.Cleanup(r.Finish)
	if user := list(); user.QuotaUsage == nil || user.QuotaUsage.InFlight == nil || user.QuotaUsage.InFlight.Requests != 1 {
		t.Fatalf("usage view = %+v, want the account's request in flight", user.QuotaUsage)
	}
	r.Finish()
	if user := list(); user.QuotaUsage == nil || user.QuotaUsage.InFlight != nil {
		t.Fatalf("usage view = %+v, want nothing in flight", user.QuotaUsage)
	}
}

func TestPatchEndUserRejectsInvalidPolicyWithFieldDetails(t *testing.T) {
	f := newAccessPolicyFixture(t)
	rec := f.call(t, f.h.PatchEndUser, http.MethodPatch, "/v0/management/end-users/"+f.userID,
		`{"model-rules":[{"models":["*"],"limits":{"cost":{"day":1}}}]}`, gin.Params{{Key: "id", Value: f.userID}})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body)
	}
	var body struct {
		Error struct {
			Code    string         `json:"code"`
			Details map[string]any `json:"details"`
		} `json:"error"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if body.Error.Code != "access_policy_invalid" || body.Error.Details["field"] != "model-rules[0].models[0]" {
		t.Fatalf("error = %+v", body.Error)
	}

	rec = f.call(t, f.h.PatchEndUser, http.MethodPatch, "/v0/management/end-users/"+f.userID,
		`{"quota-limits":{"tokens":{"week":100000}},"model-routes":[{"from":["gpt-5"],"to":"gpt-5-mini"}]}`, gin.Params{{Key: "id", Value: f.userID}})
	if rec.Code != http.StatusOK {
		t.Fatalf("valid patch status = %d body = %s", rec.Code, rec.Body)
	}
	var user enduser.User
	_ = json.Unmarshal(rec.Body.Bytes(), &user)
	if user.QuotaLimits.Tokens.Week != 100000 || len(user.ModelRoutes) != 1 || user.QuotaUsage == nil {
		t.Fatalf("patched user = %+v", user)
	}
}

func TestPermissionProfilesRoundTripPolicyAndKeepItForOldPanels(t *testing.T) {
	f := newAccessPolicyFixture(t)
	f.principal = identity.Principal{PlatformAdmin: true, EffectiveTenant: identity.Tenant{ID: identity.SystemTenantID}}
	rec := f.call(t, f.h.PutAPIKeyPermissionProfiles, http.MethodPut, "/v0/management/api-key-permission-profiles", `{"items":[{
		"id":"team","name":"Team","allowed-models":["claude-sonnet-4-5","claude-opus-4-5"],
		"quota-limits":{"cost":{"week":200,"lifetime":999},"requests":{"day":300,"5h":60},"tokens":{"month":9000000}},
		"model-rules":[{"id":"opus","name":"Opus","models":["claude-opus-*"],"limits":{"cost":{"week":50}},"fallback-model":"claude-sonnet-4-5"}],
		"model-routes":[{"id":"r","from":["claude-opus-4-1"],"to":"claude-opus-4-5"}]
	}]}`, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("put status = %d body = %s", rec.Code, rec.Body)
	}
	get := func() permissionProfileDTO {
		rec := f.call(t, f.h.GetAPIKeyPermissionProfiles, http.MethodGet, "/v0/management/api-key-permission-profiles", "", nil)
		var body struct {
			Items        []permissionProfileDTO `json:"items"`
			Capabilities map[string]bool        `json:"capabilities"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || len(body.Items) != 1 || !body.Capabilities["access-policy"] {
			t.Fatalf("get = %s (%v)", rec.Body, err)
		}
		return body.Items[0]
	}
	profile := get()
	if profile.QuotaLimits == nil || profile.QuotaLimits.Cost.Week != 200 || profile.QuotaLimits.Requests.FiveHour != 60 ||
		profile.QuotaLimits.Tokens.Month != 9000000 || profile.DailyLimit != 300 || profile.PeriodSpendingLimits.Week != 200 {
		t.Fatalf("profile limits = %+v legacy daily=%d", profile.QuotaLimits, profile.DailyLimit)
	}
	if profile.QuotaLimits.Cost.Lifetime != 0 {
		t.Fatalf("a profile never owns the lifetime allowance: %+v", profile.QuotaLimits)
	}
	if len(*profile.ModelRules) != 1 || (*profile.ModelRules)[0].FallbackModel != "claude-sonnet-4-5" || len(*profile.ModelRoutes) != 1 {
		t.Fatalf("profile rules = %+v routes = %+v", profile.ModelRules, profile.ModelRoutes)
	}

	// 旧版面板只认识扁平字段：整表替换时不带新字段，规则与路由不能因此丢失。
	rec = f.call(t, f.h.PutAPIKeyPermissionProfiles, http.MethodPut, "/v0/management/api-key-permission-profiles",
		`{"items":[{"id":"team","name":"Team renamed","daily-limit":300,"allowed-models":["claude-sonnet-4-5","claude-opus-4-5"]}]}`, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("legacy put status = %d body = %s", rec.Code, rec.Body)
	}
	profile = get()
	if profile.Name != "Team renamed" || len(*profile.ModelRules) != 1 || len(*profile.ModelRoutes) != 1 || profile.QuotaLimits.Tokens.Month != 9000000 {
		t.Fatalf("legacy save dropped the policy: %+v rules=%v", profile.QuotaLimits, profile.ModelRules)
	}

	// 降级模型不在模板的可用模型里：保存时就拒绝。
	rec = f.call(t, f.h.PutAPIKeyPermissionProfiles, http.MethodPut, "/v0/management/api-key-permission-profiles", `{"items":[{
		"id":"team","name":"Team","allowed-models":["claude-opus-4-5"],
		"model-rules":[{"models":["claude-opus-*"],"limits":{"cost":{"week":50}},"fallback-model":"claude-sonnet-4-5"}]
	}]}`, nil)
	if rec.Code != http.StatusBadRequest || !bytes.Contains(rec.Body.Bytes(), []byte("policy_target_not_allowed")) {
		t.Fatalf("disallowed fallback status = %d body = %s", rec.Code, rec.Body)
	}
}

func TestBatchBindingAndOwnedKeyAdminEndpoints(t *testing.T) {
	f := newAccessPolicyFixture(t)
	if err := sqlapikey.NewTenantStore(f.db, identity.SystemTenantID).ReplaceAllPermissionProfiles([]sqlapikey.PermissionProfileRow{{ID: "basic", Name: "Basic", RPMLimit: 30}}); err != nil {
		t.Fatal(err)
	}
	rec := f.call(t, f.h.PostEndUsersBatchPermissionProfile, http.MethodPost, "/v0/management/end-users/batch/permission-profile",
		`{"ids":["`+f.userID+`"],"permission-profile-id":"basic"}`, nil)
	if rec.Code != http.StatusOK || !bytes.Contains(rec.Body.Bytes(), []byte(`"updated":1`)) {
		t.Fatalf("batch status = %d body = %s", rec.Code, rec.Body)
	}

	first, err := f.svc.CreateKey(context.Background(), identity.SystemTenantID, f.userID, "first")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.CreateKey(context.Background(), identity.SystemTenantID, f.userID, "second"); err != nil {
		t.Fatal(err)
	}
	keyParams := gin.Params{{Key: "id", Value: f.userID}, {Key: "key_id", Value: first.APIKey.ID}}
	rec = f.call(t, f.h.GetEndUserAPIKeySecret, http.MethodGet, "/secret", "", keyParams)
	if rec.Code != http.StatusOK || !bytes.Contains(rec.Body.Bytes(), []byte(first.PlaintextKey)) || rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("secret status = %d body = %s", rec.Code, rec.Body)
	}
	rec = f.call(t, f.h.PatchEndUserAPIKey, http.MethodPatch, "/key", `{"disabled":true}`, keyParams)
	if rec.Code != http.StatusOK {
		t.Fatalf("pause status = %d body = %s", rec.Code, rec.Body)
	}
	var paused enduser.APIKey
	_ = json.Unmarshal(rec.Body.Bytes(), &paused)
	if !paused.Disabled {
		t.Fatalf("paused key = %+v", paused)
	}
	rec = f.call(t, f.h.GetEndUserAPIKeys, http.MethodGet, "/keys", "", gin.Params{{Key: "id", Value: f.userID}})
	var list struct {
		Items []enduser.APIKey `json:"items"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &list)
	if len(list.Items) != 2 {
		t.Fatalf("admin list must keep the paused key: %s", rec.Body)
	}
	if got := activePortalKeys(list.Items); len(got) != 1 || got[0].Disabled {
		t.Fatalf("portal list = %+v", got)
	}
}
