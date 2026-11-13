package management

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/accessfreeze"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/identity"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/storage/postgres"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/storage/postgres/compatdriver"
)

func callAccessFreeze(t *testing.T, principal identity.Principal, handler gin.HandlerFunc, method, target, body string, params gin.Params) *httptest.ResponseRecorder {
	t.Helper()
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Set(managementPrincipalKey, principal)
	c.Params = params
	c.Request = httptest.NewRequest(method, target, bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")
	handler(c)
	return rec
}

func errorCodeOf(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var body struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	return body.Error.Code
}

func installAccessFreezes(t *testing.T, service *accessfreeze.Service) {
	t.Helper()
	accessfreeze.SetRuntime(service)
	t.Cleanup(func() { accessfreeze.SetRuntime(nil) })
}

func TestAccessFreezeHandlersValidateBeforeTheStore(t *testing.T) {
	h := &Handler{}
	admin := identity.Principal{PlatformAdmin: true, EffectiveTenant: identity.Tenant{ID: identity.SystemTenantID}}

	accessfreeze.SetRuntime(nil)
	rec := callAccessFreeze(t, admin, h.GetAccessFreezes, http.MethodGet, "/v0/management/end-users/access-freezes", "", nil)
	if rec.Code != http.StatusServiceUnavailable || errorCodeOf(t, rec) != "access_freezes_unavailable" {
		t.Fatalf("without a service: %d %s", rec.Code, rec.Body)
	}

	// A service without a database: every case below must be refused before
	// the store is reached, or it would answer 503 instead.
	installAccessFreezes(t, accessfreeze.NewService(accessfreeze.NewStore(nil), time.UTC))
	readOnly := identity.Principal{EffectiveTenant: identity.Tenant{ID: identity.SystemTenantID}, Permissions: map[string]bool{"end_users.read": true}}
	cases := []struct {
		name      string
		principal identity.Principal
		handler   gin.HandlerFunc
		method    string
		target    string
		body      string
		params    gin.Params
		status    int
		code      string
	}{
		{"unknown status", admin, h.GetAccessFreezes, http.MethodGet, "/x?status=frozen", "", nil, http.StatusBadRequest, "invalid_status"},
		{"unknown subject kind", admin, h.GetAccessFreezes, http.MethodGet, "/x?subject_kind=user", "", nil, http.StatusBadRequest, "invalid_subject_kind"},
		{"strikes need a subject", admin, h.GetAccessFreezeStrikes, http.MethodGet, "/x?subject_kind=end_user", "", nil, http.StatusBadRequest, "invalid_subject"},
		{"release of a malformed id", admin, h.PostAccessFreezeRelease, http.MethodPost, "/x", "", gin.Params{{Key: "freeze_id", Value: "abc"}}, http.StatusNotFound, "access_freeze_not_found"},
		{"release note too long", admin, h.PostAccessFreezeRelease, http.MethodPost, "/x", `{"note":"` + strings.Repeat("长", 501) + `"}`, gin.Params{{Key: "freeze_id", Value: "7"}}, http.StatusBadRequest, "note_too_long"},
		{"release needs write", readOnly, h.PostAccessFreezeRelease, http.MethodPost, "/x", "", gin.Params{{Key: "freeze_id", Value: "7"}}, http.StatusForbidden, ""},
		{"threshold missing", admin, h.PutAccessFreezeSettings, http.MethodPut, "/x", `{}`, nil, http.StatusBadRequest, "invalid_threshold"},
		{"threshold too high", admin, h.PutAccessFreezeSettings, http.MethodPut, "/x", `{"threshold":101}`, nil, http.StatusBadRequest, "invalid_threshold"},
		{"threshold negative", admin, h.PutAccessFreezeSettings, http.MethodPut, "/x", `{"threshold":-1}`, nil, http.StatusBadRequest, "invalid_threshold"},
		{"settings need write", readOnly, h.PutAccessFreezeSettings, http.MethodPut, "/x", `{"threshold":2}`, nil, http.StatusForbidden, ""},
		{"listing reaches the store", readOnly, h.GetAccessFreezes, http.MethodGet, "/x?status=active", "", nil, http.StatusServiceUnavailable, "access_freezes_unavailable"},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			rec := callAccessFreeze(t, tt.principal, tt.handler, tt.method, tt.target, tt.body, tt.params)
			if rec.Code != tt.status {
				t.Fatalf("status = %d, want %d: %s", rec.Code, tt.status, rec.Body)
			}
			if tt.code != "" && errorCodeOf(t, rec) != tt.code {
				t.Fatalf("code = %q, want %q: %s", errorCodeOf(t, rec), tt.code, rec.Body)
			}
		})
	}
}

func openAccessFreezeHandlerPostgres(t *testing.T) *sql.DB {
	t.Helper()
	dsn := strings.TrimSpace(os.Getenv("CLIRELAY_POSTGRES_TEST_DSN"))
	if dsn == "" {
		t.Skip("CLIRELAY_POSTGRES_TEST_DSN is not set")
	}
	admin, err := sql.Open(compatdriver.DriverName, dsn)
	if err != nil {
		t.Fatalf("open postgres: %v", err)
	}
	t.Cleanup(func() { _ = admin.Close() })
	schema := fmt.Sprintf("access_freeze_handlers_%d", time.Now().UnixNano())
	if _, err = admin.Exec(`CREATE SCHEMA ` + schema); err != nil {
		t.Fatalf("create schema: %v", err)
	}
	t.Cleanup(func() { _, _ = admin.Exec(`DROP SCHEMA ` + schema + ` CASCADE`) })
	scoped, _ := url.Parse(dsn)
	query := scoped.Query()
	query.Set("search_path", schema)
	scoped.RawQuery = query.Encode()
	db, err := sql.Open(compatdriver.DriverName, scoped.String())
	if err != nil {
		t.Fatalf("open scoped postgres: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	for _, migration := range postgres.RuntimeMigrations() {
		if migration.Version == "202611130001_access_freezes" {
			if _, err = db.Exec(migration.SQL); err != nil {
				t.Fatalf("apply access freeze migration: %v", err)
			}
		}
	}
	return db
}

func TestAccessFreezeHandlersPostgres(t *testing.T) {
	db := openAccessFreezeHandlerPostgres(t)
	store := accessfreeze.NewStore(db)
	ctx := context.Background()
	tenant := identity.SystemTenantID
	carol := accessfreeze.Subject{TenantID: tenant, Kind: accessfreeze.SubjectEndUser, ID: "carol"}
	at := time.Now().Add(-time.Minute)
	strike := accessfreeze.Strike{Subject: carol, Provider: "codex", ReasonCode: "cyber_policy", UpstreamMessage: "flagged",
		Model: "gpt-6.1-sol", AuthLabel: "pool@example.com", RequestID: "req-9", CreatedAt: at}
	count, err := store.RecordStrike(ctx, strike)
	if err != nil {
		t.Fatalf("RecordStrike: %v", err)
	}
	frozen, _, err := store.Freeze(ctx, strike, count, at)
	if err != nil {
		t.Fatalf("Freeze: %v", err)
	}
	service := accessfreeze.NewService(store, time.UTC)
	service.Start(ctx)
	t.Cleanup(service.Close)
	installAccessFreezes(t, service)
	h := &Handler{}
	admin := identity.Principal{PlatformAdmin: true, EffectiveTenant: identity.Tenant{ID: tenant}, User: identity.User{ID: "u-admin", Username: "root"}}

	rec := callAccessFreeze(t, admin, h.GetAccessFreezes, http.MethodGet, "/x?status=active&subject_kind=end_user&subject_id=carol", "", nil)
	var list struct {
		Items []struct {
			ID           int64  `json:"id"`
			Provider     string `json:"provider"`
			ProviderName string `json:"provider_name"`
			AuthLabel    string `json:"auth_label"`
			Notice       string `json:"notice"`
		} `json:"items"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &list); rec.Code != http.StatusOK || err != nil || len(list.Items) != 1 {
		t.Fatalf("list: %d %s", rec.Code, rec.Body)
	}
	if item := list.Items[0]; item.ID != frozen.ID || item.ProviderName != "Codex" || item.AuthLabel != "pool@example.com" || !strings.Contains(item.Notice, "Codex") {
		t.Fatalf("listed freeze = %#v", item)
	}

	rec = callAccessFreeze(t, admin, h.GetAccessFreezeStrikes, http.MethodGet, "/x?subject_kind=end_user&subject_id=carol&provider=codex", "", nil)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"request_id":"req-9"`) {
		t.Fatalf("strikes: %d %s", rec.Code, rec.Body)
	}

	rec = callAccessFreeze(t, admin, h.PutAccessFreezeSettings, http.MethodPut, "/x", `{"threshold":3}`, nil)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"threshold":3`) {
		t.Fatalf("put settings: %d %s", rec.Code, rec.Body)
	}
	rec = callAccessFreeze(t, admin, h.GetAccessFreezeSettings, http.MethodGet, "/x", "", nil)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"threshold":3`) || !strings.Contains(rec.Body.String(), `"default_threshold":1`) {
		t.Fatalf("get settings: %d %s", rec.Code, rec.Body)
	}

	if service.FrozenProviders(tenant, "carol", "k") == "" {
		t.Fatal("carol not frozen before the release")
	}
	params := gin.Params{{Key: "freeze_id", Value: fmt.Sprint(frozen.ID)}}
	rec = callAccessFreeze(t, admin, h.PostAccessFreezeRelease, http.MethodPost, "/x", `{"note":"reviewed"}`, params)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"released_by":"root"`) || !strings.Contains(rec.Body.String(), `"release_note":"reviewed"`) {
		t.Fatalf("release: %d %s", rec.Code, rec.Body)
	}
	if got := service.FrozenProviders(tenant, "carol", "k"); got != "" {
		t.Fatalf("carol still frozen after the release: %q", got)
	}
	rec = callAccessFreeze(t, admin, h.PostAccessFreezeRelease, http.MethodPost, "/x", "", params)
	if rec.Code != http.StatusConflict || errorCodeOf(t, rec) != "access_freeze_already_released" {
		t.Fatalf("second release: %d %s", rec.Code, rec.Body)
	}
	other := identity.Principal{PlatformAdmin: true, EffectiveTenant: identity.Tenant{ID: "00000000-0000-0000-0000-000000000002"}}
	rec = callAccessFreeze(t, other, h.GetAccessFreezes, http.MethodGet, "/x", "", nil)
	if rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), `"carol"`) {
		t.Fatalf("another tenant sees carol's freezes: %d %s", rec.Code, rec.Body)
	}
}

func TestAccessFreezeSubjectNamesNameAccountsAndKeys(t *testing.T) {
	f := newAccessPolicyFixture(t)
	created, err := f.svc.CreateKey(context.Background(), identity.SystemTenantID, f.userID, "ci-runner")
	if err != nil {
		t.Fatalf("CreateKey: %v", err)
	}
	account := accessfreeze.Subject{TenantID: identity.SystemTenantID, Kind: accessfreeze.SubjectEndUser, ID: f.userID}
	key := accessfreeze.Subject{TenantID: identity.SystemTenantID, Kind: accessfreeze.SubjectAPIKey, ID: created.APIKey.ID}
	gone := accessfreeze.Subject{TenantID: identity.SystemTenantID, Kind: accessfreeze.SubjectAPIKey, ID: "missing-key"}

	names := f.h.accessFreezeSubjectNames(context.Background(), identity.SystemTenantID, []accessfreeze.Subject{account, key, account, gone})
	if names[account] != "Policy User" || names[key] != "ci-runner" || names[gone] != "" || len(names) != 3 {
		t.Fatalf("names = %#v", names)
	}
}
