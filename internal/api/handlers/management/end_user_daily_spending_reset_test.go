package management

import (
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
	"github.com/router-for-me/CLIProxyAPI/v6/internal/usage"
)

func setupEndUserDailySpendingHandlerTest(t *testing.T) (*Handler, identity.Principal, string, string) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	if err := usage.InitDB(filepath.Join(t.TempDir(), "usage.db"), config.RequestLogStorageConfig{}, time.UTC); err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	t.Cleanup(func() {
		enduser.SetDefault(nil)
		usage.CloseDB()
	})
	db := usage.RuntimeDB()
	if _, err := db.Exec(`
		CREATE TABLE end_users (
			id TEXT PRIMARY KEY,
			tenant_id TEXT NOT NULL,
			username TEXT NOT NULL,
			username_normalized TEXT NOT NULL UNIQUE,
			display_name TEXT NOT NULL,
			password_hash TEXT NOT NULL,
			status TEXT NOT NULL DEFAULT 'active',
			must_change_password INTEGER NOT NULL DEFAULT 0,
			password_changed_at TIMESTAMP,
			last_login_at TIMESTAMP,
			failed_login_count INTEGER NOT NULL DEFAULT 0,
			lock_stage INTEGER NOT NULL DEFAULT 0,
			locked_until TIMESTAMP,
			created_at TIMESTAMP NOT NULL,
			updated_at TIMESTAMP NOT NULL,
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
	tenantID := uuid.NewString()
	endUserID := uuid.NewString()
	now := time.Now().UTC()
	if _, err := db.Exec(`
		INSERT INTO end_users (
			id, tenant_id, username, username_normalized, display_name, password_hash, created_at, updated_at
		) VALUES (?, ?, 'alice', 'alice', 'Alice', 'unused', ?, ?)
	`, endUserID, tenantID, now, now); err != nil {
		t.Fatalf("insert end user: %v", err)
	}
	if _, err := db.Exec(`
		INSERT INTO usage_rollup_buckets (
			tenant_id, bucket_kind, bucket_start, end_user_id, model, source, cost_total, updated_at
		) VALUES (?, 'day', ?, ?, 'test-model', 'test', 12.5, ?)
	`, tenantID, usage.LocalDayKeyAt(now), endUserID, now); err != nil {
		t.Fatalf("insert end-user rollup: %v", err)
	}
	enduser.SetDefault(enduser.NewService(db))
	principal := identity.Principal{
		Kind: "user",
		User: identity.User{
			ID:          uuid.NewString(),
			Username:    "admin",
			DisplayName: "Admin",
		},
		EffectiveTenant: identity.Tenant{ID: tenantID},
		Permissions: map[string]bool{
			"end_users.read":  true,
			"end_users.write": true,
		},
	}
	return &Handler{}, principal, tenantID, endUserID
}

func TestEndUserDailySpendingResetWritesAndListsHistory(t *testing.T) {
	h, principal, tenantID, endUserID := setupEndUserDailySpendingHandlerTest(t)

	resetRecorder := httptest.NewRecorder()
	resetContext, _ := gin.CreateTestContext(resetRecorder)
	resetContext.Set(managementPrincipalKey, principal)
	resetContext.Params = gin.Params{{Key: "id", Value: endUserID}}
	resetContext.Request = httptest.NewRequest(http.MethodPost, "/v0/management/end-users/"+endUserID+"/daily-spending/reset", nil)
	h.PostEndUserDailySpendingReset(resetContext)
	if resetRecorder.Code != http.StatusOK {
		t.Fatalf("reset status = %d, want %d; body=%s", resetRecorder.Code, http.StatusOK, resetRecorder.Body.String())
	}
	var resetBody struct {
		ResetCount int     `json:"daily-spending-reset-count"`
		UsedBefore float64 `json:"effective-used-before"`
		RawToday   float64 `json:"raw-today-cost"`
	}
	if err := json.Unmarshal(resetRecorder.Body.Bytes(), &resetBody); err != nil {
		t.Fatalf("decode reset response: %v", err)
	}
	if resetBody.ResetCount != 1 || resetBody.UsedBefore != 12.5 || resetBody.RawToday != 12.5 {
		t.Fatalf("reset response = %#v, want count=1 used/raw=12.5", resetBody)
	}
	events, err := usage.ListEndUserDailySpendingResetEvents(tenantID, endUserID, 10)
	if err != nil {
		t.Fatalf("list persisted reset events: %v", err)
	}
	if len(events) != 1 || events[0].ActorUserID != principal.User.ID || events[0].ActorUsername != "admin" || events[0].ActorKind != "user" {
		t.Fatalf("persisted events = %#v, want actor from principal", events)
	}

	writeOnly := principal
	writeOnly.Permissions = map[string]bool{"end_users.write": true}
	historyRecorder := httptest.NewRecorder()
	historyContext, _ := gin.CreateTestContext(historyRecorder)
	historyContext.Set(managementPrincipalKey, writeOnly)
	historyContext.Params = gin.Params{{Key: "id", Value: endUserID}}
	historyContext.Request = httptest.NewRequest(http.MethodGet, "/v0/management/end-users/"+endUserID+"/daily-spending/reset-history?limit=1", nil)
	h.GetEndUserDailySpendingResetHistory(historyContext)
	if historyRecorder.Code != http.StatusOK {
		t.Fatalf("history status = %d, want %d; body=%s", historyRecorder.Code, http.StatusOK, historyRecorder.Body.String())
	}
	var historyBody struct {
		Items             []usage.EndUserDailySpendingResetEvent `json:"items"`
		Total             int                                    `json:"total"`
		RawTodayCost      float64                                `json:"raw-today-cost"`
		DailySpendingUsed float64                                `json:"daily-spending-used"`
	}
	if err := json.Unmarshal(historyRecorder.Body.Bytes(), &historyBody); err != nil {
		t.Fatalf("decode history response: %v", err)
	}
	if historyBody.Total != 1 || len(historyBody.Items) != 1 || historyBody.RawTodayCost != 12.5 || historyBody.DailySpendingUsed != 0 {
		t.Fatalf("history response = %#v, want total/items=1 raw=12.5 used=0", historyBody)
	}
	if historyBody.Items[0].EffectiveUsedBefore != 12.5 || historyBody.Items[0].RawTodayCost != 12.5 {
		t.Fatalf("history item = %#v, want reset costs 12.5", historyBody.Items[0])
	}

	listRecorder := httptest.NewRecorder()
	listContext, _ := gin.CreateTestContext(listRecorder)
	listContext.Set(managementPrincipalKey, principal)
	listContext.Request = httptest.NewRequest(http.MethodGet, "/v0/management/end-users", nil)
	h.GetEndUsers(listContext)
	if listRecorder.Code != http.StatusOK {
		t.Fatalf("list status = %d, want %d; body=%s", listRecorder.Code, http.StatusOK, listRecorder.Body.String())
	}
	var listBody struct {
		Items []enduser.User `json:"items"`
	}
	if err := json.Unmarshal(listRecorder.Body.Bytes(), &listBody); err != nil {
		t.Fatalf("decode list response: %v", err)
	}
	if len(listBody.Items) != 1 || listBody.Items[0].DailySpendingResetCount != 1 || listBody.Items[0].DailySpendingUsed != 0 {
		t.Fatalf("end-user list = %#v, want reset count=1 used=0", listBody.Items)
	}
}
