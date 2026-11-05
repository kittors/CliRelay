package management

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/contentmoderation"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/enduser"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/identity"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/testutil/postgrestest"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/usage"
	"golang.org/x/crypto/bcrypt"
)

// privacyMaskHandler is a handler whose privacy masking settings live in a
// registry of the test's own.
func privacyMaskHandler(t *testing.T) *Handler {
	t.Helper()
	return withMaskSettings(setupWordPolicyTest(t))
}

func withMaskSettings(h *Handler) *Handler {
	registry := contentmoderation.NewSecretMaskingRegistry()
	registry.SetStore(memoryMaskSettings{rows: map[string]contentmoderation.SecretMaskingSettings{}, versions: map[string]int64{}})
	h.secretMasking = registry
	return h
}

type memoryMaskSettings struct {
	rows     map[string]contentmoderation.SecretMaskingSettings
	versions map[string]int64
}

func (m memoryMaskSettings) Load(_ context.Context, tenantID string) (contentmoderation.SecretMaskingSettings, int64, error) {
	return m.rows[tenantID], m.versions[tenantID], nil
}

func (m memoryMaskSettings) Save(_ context.Context, tenantID string, settings contentmoderation.SecretMaskingSettings, expected int64) error {
	if m.versions[tenantID] != expected {
		return contentmoderation.ErrSecretMaskingConflict
	}
	m.rows[tenantID] = settings
	m.versions[tenantID]++
	return nil
}

func TestPrivacyMaskingSettingsRoundTripAndTheOldSwitch(t *testing.T) {
	h := privacyMaskHandler(t)
	rec, body := wordTenantAUser.call(h.GetPrivacyMaskingSettings, http.MethodGet, "", "")
	wordExpectStatus(t, rec, http.StatusOK)
	if policy, _ := body["policy"].(map[string]any); policy["secrets"] != false || body["portal_default_allowed"] != true || body["persistent"] != true {
		t.Fatalf("a tenant that set nothing: %v", body)
	}
	rec, body = wordTenantAUser.call(h.PutPrivacyMaskingSettings, http.MethodPut,
		`{"policy":{"secrets":true,"personal":true,"words":["Nightjar"," Nightjar "],"actions":{"EMAIL":"warn"}},"enforced":["personal"],"portal_default_allowed":false}`, "")
	wordExpectStatus(t, rec, http.StatusOK)
	policy, _ := body["policy"].(map[string]any)
	if words, _ := policy["words"].([]any); len(words) != 1 || body["portal_default_allowed"] != false || body["enforced"].([]any)[0] != "personal" {
		t.Fatalf("saved = %v", body)
	}
	if strings.Contains(rec.Body.String(), `"key"`) {
		t.Fatalf("the placeholder key was sent: %s", rec.Body.String())
	}
	// the switch that predates policies reads and writes the secrets
	rec, body = wordTenantAUser.call(h.GetSecretMasking, http.MethodGet, "", "")
	wordExpectStatus(t, rec, http.StatusOK)
	if body["enabled"] != true {
		t.Fatalf("old switch = %v", body)
	}
	rec, _ = wordTenantAUser.call(h.PutSecretMasking, http.MethodPut, `{"enabled":false}`, "")
	wordExpectStatus(t, rec, http.StatusOK)
	_, body = wordTenantAUser.call(h.GetPrivacyMaskingSettings, http.MethodGet, "", "")
	if policy, _ := body["policy"].(map[string]any); policy["secrets"] != false || policy["personal"] != true {
		t.Fatalf("after the old switch: %v", body)
	}
	// tenant B sees none of it
	_, body = wordTenantBUser.call(h.GetPrivacyMaskingSettings, http.MethodGet, "", "")
	if policy, _ := body["policy"].(map[string]any); policy["personal"] != false {
		t.Fatalf("tenant B: %v", body)
	}

	for _, bad := range []string{
		`{"policy":{"actions":{"EMAIL":"shred"}},"portal_default_allowed":true}`,
		`{"policy":{},"enforced":["words"],"portal_default_allowed":true}`,
		`{"policy":{}}`,
	} {
		if rec, _ := wordTenantAUser.call(h.PutPrivacyMaskingSettings, http.MethodPut, bad, ""); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: %d", bad, rec.Code)
		}
	}
}

func TestPrivacyMaskingPreviewAndRecordLookups(t *testing.T) {
	h := privacyMaskHandler(t)
	rec, body := wordTenantAUser.call(h.PostPrivacyMaskingPreview, http.MethodPost,
		`{"text":"use sk-proj-`+strings.Repeat("Ab3", 16)+`","policy":{"secrets":true}}`, "")
	wordExpectStatus(t, rec, http.StatusOK)
	if text, _ := body["text"].(string); !strings.Contains(text, "{{API_KEY_") || strings.Contains(text, "sk-proj-") {
		t.Fatalf("preview = %v", body)
	}
	if rec, _ := wordTenantAUser.call(h.PostPrivacyMaskingPreview, http.MethodPost, `{"text":"`+strings.Repeat("x", contentmoderation.MaxMaskPreviewBytes+1)+`"}`, ""); rec.Code != http.StatusBadRequest {
		t.Fatalf("an oversized preview: %d", rec.Code)
	}

	// a key that is not one, and one that is not the tenant's, are both not found
	c, rec := privacyMaskContext(t, wordTenantA, http.MethodGet, "/v0/management/privacy-masking/records/x")
	c.Params = gin.Params{{Key: "key", Value: "not-a-key"}}
	h.GetPrivacyMaskingRecord(c)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("record lookup = %d %s", rec.Code, rec.Body.String())
	}
	c, rec = privacyMaskContext(t, wordTenantA, http.MethodGet, "/v0/management/privacy-masking/records?facet=nope")
	h.GetPrivacyMaskingRecords(c)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("a bad facet = %d", rec.Code)
	}
	c, rec = privacyMaskContext(t, wordTenantA, http.MethodGet, "/v0/management/privacy-masking/summary?days=30")
	h.GetPrivacyMaskingSummary(c)
	var summary contentmoderation.MaskSummary
	if err := json.Unmarshal(rec.Body.Bytes(), &summary); rec.Code != http.StatusOK || err != nil || len(summary.Days) != 30 {
		t.Fatalf("summary = %d %s", rec.Code, rec.Body.String())
	}
}

func privacyMaskContext(t *testing.T, tenantID, method, target string) (*gin.Context, *httptest.ResponseRecorder) {
	t.Helper()
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Set(managementPrincipalKey, identity.Principal{EffectiveTenant: identity.Tenant{ID: tenantID}})
	c.Request = httptest.NewRequest(method, target, nil)
	return c, rec
}

// portalMaskCall drives a portal route with the user's session token.
func portalMaskCall(t *testing.T, h *Handler, method, path, token, body string) (int, map[string]any) {
	t.Helper()
	router := gin.New()
	router.GET("/v0/portal/auth/me", h.GetPortalMe)
	router.GET("/v0/portal/privacy-masking/settings", h.GetPortalPrivacyMaskingSettings)
	router.PUT("/v0/portal/privacy-masking/settings", h.PutPortalPrivacyMaskingSettings)
	router.DELETE("/v0/portal/privacy-masking/settings", h.DeletePortalPrivacyMaskingSettings)
	router.POST("/v0/portal/privacy-masking/preview", h.PostPortalPrivacyMaskingPreview)
	router.GET("/v0/portal/privacy-masking/records", h.GetPortalPrivacyMaskingRecords)
	router.GET("/v0/portal/privacy-masking/records/:key", h.GetPortalPrivacyMaskingRecord)
	req := httptest.NewRequest(method, path, bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	var decoded map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &decoded)
	return rec.Code, decoded
}

// TestPostgresPortalUserKeepsPrivacyMaskingWhileAllowed walks the portal side
// on the database production runs: a portal user keeps a policy of their own
// by default, previews it without the tenant's words, sees only their own
// records, and loses the policy's effect when the tenant revokes it.
func TestPostgresPortalUserKeepsPrivacyMaskingWhileAllowed(t *testing.T) {
	dsn := strings.TrimSpace(os.Getenv("CLIRELAY_POSTGRES_TEST_DSN"))
	if dsn == "" {
		t.Skip("CLIRELAY_POSTGRES_TEST_DSN is not set")
	}
	gin.SetMode(gin.TestMode)
	postgrestest.LockSharedRuntimeDB(t, dsn)
	usage.CloseDB()
	t.Cleanup(usage.CloseDB)
	if err := usage.InitPostgres(config.PostgresConfig{DSN: dsn, MaxOpenConns: 4, MaxIdleConns: 1}, config.RequestLogStorageConfig{}, time.UTC); err != nil {
		t.Fatalf("InitPostgres: %v", err)
	}
	ctx := context.Background()
	db := usage.RuntimeDB()
	if err := contentmoderation.InitTables(db); err != nil {
		t.Fatal(err)
	}
	svc := enduser.NewService(db)
	enduser.SetDefault(svc)
	t.Cleanup(func() { enduser.SetDefault(nil) })

	const password = "Portal-Mask-123!"
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	tenantID, userID, otherID := identity.SystemTenantID, uuid.NewString(), uuid.NewString()
	username := "mask_" + strings.ReplaceAll(userID[:8], "-", "")
	if _, err := db.ExecContext(ctx, `INSERT INTO end_users (id, tenant_id, username, username_normalized, display_name, password_hash, must_change_password)
		VALUES (?, ?, ?, ?, '脱敏用户', ?, false)`, userID, tenantID, username, username, string(hash)); err != nil {
		t.Fatalf("insert portal account: %v", err)
	}
	store := contentmoderation.NewStore(db)
	t.Cleanup(func() {
		_ = store.DeleteMaskUser(context.Background(), tenantID, userID)
		_ = store.DeleteMaskUser(context.Background(), tenantID, otherID)
		_, _ = db.ExecContext(context.Background(), `DELETE FROM end_user_sessions WHERE end_user_id = ?`, userID)
		_, _ = db.ExecContext(context.Background(), `DELETE FROM end_users WHERE id = ?`, userID)
	})
	login, err := svc.Login(ctx, username, password, "test")
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	token := login.AccessToken
	h := withMaskSettings(&Handler{cfg: &config.Config{}})
	admin := wordCaller{tenant: tenantID, admin: true}
	rec, _ := admin.call(h.PutPrivacyMaskingSettings, http.MethodPut,
		`{"policy":{"secrets":true,"words":["Nightjar"]},"enforced":["secrets"],"portal_default_allowed":true}`, "")
	wordExpectStatus(t, rec, http.StatusOK)

	status, body := portalMaskCall(t, h, http.MethodGet, "/v0/portal/auth/me", token, "")
	if status != http.StatusOK || body["privacy_masking_allowed"] != true {
		t.Fatalf("me = %d %v", status, body)
	}
	status, body = portalMaskCall(t, h, http.MethodGet, "/v0/portal/privacy-masking/settings", token, "")
	policy, _ := body["policy"].(map[string]any)
	if words, _ := policy["words"].([]any); status != http.StatusOK || body["own"] != false || len(words) != 0 || body["enforced"].([]any)[0] != "secrets" {
		t.Fatalf("portal settings = %d %v", status, body)
	}
	status, body = portalMaskCall(t, h, http.MethodPut, "/v0/portal/privacy-masking/settings", token, `{"policy":{"personal":true}}`)
	if status != http.StatusOK || body["own"] != true {
		t.Fatalf("put = %d %v", status, body)
	}
	// the tenant's words are not the user's to try out
	status, body = portalMaskCall(t, h, http.MethodPost, "/v0/portal/privacy-masking/preview", token, `{"text":"Nightjar to ada.lovelace@gmail.com"}`)
	if text, _ := body["text"].(string); status != http.StatusOK || !strings.Contains(text, "Nightjar") || strings.Contains(text, "ada.lovelace") {
		t.Fatalf("preview = %d %v", status, body)
	}

	// records: the user's own and nobody else's
	at := time.Now()
	mine := &contentmoderation.MaskRecord{At: at, TenantID: tenantID, EndUserID: userID, Client: "codex", Places: 1, Distinct: 1,
		Values: []contentmoderation.MaskRecordValue{{ID: "v1", Kind: "EMAIL", By: "rule", Len: 22, N: 1}}}
	theirs := &contentmoderation.MaskRecord{At: at, TenantID: tenantID, EndUserID: otherID, Client: "codex", Places: 1, Distinct: 1,
		Values: []contentmoderation.MaskRecordValue{{ID: "v2", Kind: "EMAIL", By: "rule", Len: 22, N: 1}}}
	mine.Key, theirs.Key = recordKeyAt(at, 1), recordKeyAt(at, 2)
	if err := store.WriteMaskRecords(ctx, []*contentmoderation.MaskRecord{mine, theirs}); err != nil {
		t.Fatal(err)
	}
	status, body = portalMaskCall(t, h, http.MethodGet, "/v0/portal/privacy-masking/records", token, "")
	if items, _ := body["items"].([]any); status != http.StatusOK || len(items) != 1 || items[0].(map[string]any)["end_user_name"] != "脱敏用户" {
		t.Fatalf("records = %d %v", status, body)
	}
	if status, _ := portalMaskCall(t, h, http.MethodGet, "/v0/portal/privacy-masking/records/"+theirs.Key, token, ""); status != http.StatusNotFound {
		t.Fatalf("read another user's record: %d", status)
	}

	// revoked: the user keeps the policy but may not change it, and it stops applying
	rec, body = admin.call(h.PutPrivacyMaskingPortalUser, http.MethodPut, `{"allowed":false}`, userID)
	wordExpectStatus(t, rec, http.StatusOK)
	if body["allowed"] != false {
		t.Fatalf("revoke = %v", body)
	}
	status, body = portalMaskCall(t, h, http.MethodPut, "/v0/portal/privacy-masking/settings", token, `{"policy":{"personal":false}}`)
	if status != http.StatusForbidden {
		t.Fatalf("put after revoke = %d %v", status, body)
	}
	status, body = portalMaskCall(t, h, http.MethodGet, "/v0/portal/privacy-masking/settings", token, "")
	if body["own"] != false || body["allowed"] != false {
		t.Fatalf("settings after revoke = %d %v", status, body)
	}
}

// recordKeyAt is a record key at t, told apart by n, written as the records
// write theirs.
func recordKeyAt(t time.Time, n int) string {
	return fmt.Sprintf("%016x%08x", t.UnixNano(), n)
}
