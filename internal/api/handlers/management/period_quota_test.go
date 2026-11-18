package management

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/quota"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/usage"
)

func TestPatchEndUserRejectsLegacyPeriodDayConflict(t *testing.T) {
	gin.SetMode(gin.TestMode)
	h, principal, _, endUserID := setupEndUserDailySpendingHandlerTest(t)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Set(managementPrincipalKey, principal)
	ctx.Params = gin.Params{{Key: "id", Value: endUserID}}
	ctx.Request = httptest.NewRequest(http.MethodPatch, "/v0/management/end-users/"+endUserID, bytes.NewBufferString(`{
		"daily-spending-limit":10,
		"period-spending-limits":{"day":20}
	}`))
	ctx.Request.Header.Set("Content-Type", "application/json")
	h.PatchEndUser(ctx)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", recorder.Code, recorder.Body.String())
	}
	var body struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Error.Code != "period_day_legacy_conflict" {
		t.Fatalf("code = %q, want period_day_legacy_conflict", body.Error.Code)
	}
}

func TestPostEndUserAPIKeyRejectsLimitAboveAccount(t *testing.T) {
	gin.SetMode(gin.TestMode)
	h, principal, _, endUserID := setupEndUserDailySpendingHandlerTest(t)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Set(managementPrincipalKey, principal)
	ctx.Params = gin.Params{{Key: "id", Value: endUserID}}
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v0/management/end-users/"+endUserID+"/api-keys", bytes.NewBufferString(`{
		"name":"too-large",
		"period-spending-limits":{"day":200}
	}`))
	ctx.Request.Header.Set("Content-Type", "application/json")
	h.PostEndUserAPIKey(ctx)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", recorder.Code, recorder.Body.String())
	}
	var body struct {
		Error struct {
			Code    string `json:"code"`
			Details struct {
				Period       string  `json:"period"`
				KeyLimit     float64 `json:"key_limit"`
				AccountLimit float64 `json:"account_limit"`
			} `json:"details"`
		} `json:"error"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Error.Code != "key_period_limit_exceeds_account" || body.Error.Details.Period != "day" || body.Error.Details.KeyLimit != 200 || body.Error.Details.AccountLimit != 100 {
		t.Fatalf("error = %+v", body.Error)
	}
}

func TestPostEndUserPeriodSpendingResetAndMissingLimit(t *testing.T) {
	gin.SetMode(gin.TestMode)
	h, principal, tenantID, endUserID := setupEndUserDailySpendingHandlerTest(t)
	db := usage.RuntimeDB()
	if _, err := db.Exec(`UPDATE end_users SET weekly_spending_limit = 100 WHERE id = ?`, endUserID); err != nil {
		t.Fatalf("set weekly limit: %v", err)
	}

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Set(managementPrincipalKey, principal)
	ctx.Params = gin.Params{{Key: "id", Value: endUserID}}
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v0/management/end-users/"+endUserID+"/period-spending/reset",
		bytes.NewBufferString(`{"periods":["week","week"]}`))
	ctx.Request.Header.Set("Content-Type", "application/json")
	h.PostEndUserPeriodSpendingReset(ctx)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", recorder.Code, recorder.Body.String())
	}
	if !bytes.Contains(recorder.Body.Bytes(), []byte(`"periods":["week"]`)) ||
		!bytes.Contains(recorder.Body.Bytes(), []byte(`"reset-count":1`)) {
		t.Fatalf("body = %s", recorder.Body.String())
	}
	used, err := usage.QueryPeriodSpendingByEndUserForTenant(tenantID, endUserID)
	if err != nil || used.Week != 0 || used.Day != 12.5 {
		t.Fatalf("used = %+v, err=%v; want week=0 day=12.5", used, err)
	}

	recorder = httptest.NewRecorder()
	ctx, _ = gin.CreateTestContext(recorder)
	ctx.Set(managementPrincipalKey, principal)
	ctx.Params = gin.Params{{Key: "id", Value: endUserID}}
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v0/management/end-users/"+endUserID+"/period-spending/reset",
		bytes.NewBufferString(`{"periods":["month"]}`))
	ctx.Request.Header.Set("Content-Type", "application/json")
	h.PostEndUserPeriodSpendingReset(ctx)
	if recorder.Code != http.StatusBadRequest || !bytes.Contains(recorder.Body.Bytes(), []byte(`"code":"period_limit_missing"`)) {
		t.Fatalf("missing-limit status/body = %d %s", recorder.Code, recorder.Body.String())
	}
}

func TestPostEndUserAPIKeyPeriodSpendingResetValidatesOwnership(t *testing.T) {
	gin.SetMode(gin.TestMode)
	h, principal, tenantID, endUserID := setupEndUserDailySpendingHandlerTest(t)
	keyID := uuid.NewString()
	if err := usage.UpsertAPIKeyForTenant(tenantID, usage.APIKeyRow{
		ID: keyID, Key: "sk-owned-period-reset", EndUserID: endUserID,
		PeriodSpendingLimits: quota.PeriodSpendingLimits{Week: 50},
		CreatedAt:            time.Now().UTC().Format(time.RFC3339), UpdatedAt: time.Now().UTC().Format(time.RFC3339),
	}); err != nil {
		t.Fatalf("upsert owned key: %v", err)
	}
	if _, err := usage.RuntimeDB().Exec(`INSERT INTO usage_rollup_buckets
		(tenant_id,bucket_kind,bucket_start,api_key_id,end_user_id,model,cost_total,updated_at)
		VALUES (?,?,?,?,?,?,?,?)`, tenantID, "day", usage.LocalDayKeyAt(time.Now()), keyID, endUserID, "owned", 8.0, time.Now().UTC()); err != nil {
		t.Fatalf("insert owned rollup: %v", err)
	}

	call := func(ownerID string) *httptest.ResponseRecorder {
		recorder := httptest.NewRecorder()
		ctx, _ := gin.CreateTestContext(recorder)
		ctx.Set(managementPrincipalKey, principal)
		ctx.Params = gin.Params{{Key: "id", Value: ownerID}, {Key: "key_id", Value: keyID}}
		ctx.Request = httptest.NewRequest(http.MethodPost, "/v0/management/end-users/"+ownerID+"/api-keys/"+keyID+"/period-spending/reset",
			bytes.NewBufferString(`{"periods":["week"]}`))
		ctx.Request.Header.Set("Content-Type", "application/json")
		h.PostEndUserAPIKeyPeriodSpendingReset(ctx)
		return recorder
	}
	wrong := call(uuid.NewString())
	if wrong.Code != http.StatusNotFound {
		t.Fatalf("wrong owner status = %d body=%s", wrong.Code, wrong.Body.String())
	}
	ok := call(endUserID)
	if ok.Code != http.StatusOK {
		t.Fatalf("correct owner status = %d body=%s", ok.Code, ok.Body.String())
	}
}
