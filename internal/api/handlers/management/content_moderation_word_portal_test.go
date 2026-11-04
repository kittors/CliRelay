package management

import (
	"bytes"
	"context"
	"encoding/json"
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

func TestWordPortalIsAdvertisedAndSummed(t *testing.T) {
	h := setupWordPolicyTest(t)
	rec, body := wordTenantAUser.call(h.GetContentModerationProfiles, http.MethodGet, "", "")
	wordExpectStatus(t, rec, http.StatusOK)
	if capabilities, _ := body["capabilities"].(map[string]any); capabilities["word_portal"] != true {
		t.Fatalf("capabilities = %v, want word_portal declared", capabilities)
	}

	rec, body = wordTenantAUser.call(h.GetWordPolicies, http.MethodGet, "", "")
	wordExpectStatus(t, rec, http.StatusOK)
	if portal, _ := body["portal"].(map[string]any); portal["default_allowed"] != false || portal["policies"] != float64(0) {
		t.Fatalf("a tenant that decided nothing: portal = %v", body["portal"])
	}

	rec, _ = wordTenantAUser.call(h.PutWordPortal, http.MethodPut, `{}`, "")
	wordExpectStatus(t, rec, http.StatusBadRequest)
	rec, body = wordTenantAUser.call(h.PutWordPortal, http.MethodPut, `{"default_allowed":true}`, "")
	wordExpectStatus(t, rec, http.StatusOK)
	if body["default_allowed"] != true {
		t.Fatalf("put default = %v", body)
	}

	// A portal user's policy, as stored by the portal, is summed up for the
	// tenant, and only for that tenant.
	store := h.contentModerationStore()
	policy := contentmoderation.WordPolicy{TenantID: wordTenantA, ScopeType: contentmoderation.WordScopeEndUser, ScopeID: "u-1", Enabled: true,
		Rules: []contentmoderation.WordRule{{Kind: contentmoderation.WordRuleKeyword, Pattern: "secret", Enabled: true}}}
	if err := policy.Normalize(); err != nil {
		t.Fatal(err)
	}
	if _, err := store.PutWordPolicy(context.Background(), policy, nil); err != nil {
		t.Fatal(err)
	}
	_, body = wordTenantAUser.call(h.GetWordPolicies, http.MethodGet, "", "")
	if portal, _ := body["portal"].(map[string]any); portal["default_allowed"] != true || portal["policies"] != float64(1) || portal["active_policies"] != float64(1) {
		t.Fatalf("tenant A: portal = %v", body["portal"])
	}
	_, body = wordTenantBUser.call(h.GetWordPolicies, http.MethodGet, "", "")
	if portal, _ := body["portal"].(map[string]any); portal["default_allowed"] != false || portal["policies"] != float64(0) {
		t.Fatalf("tenant B sees tenant A's portal users: %v", body["portal"])
	}
}

func TestWordPolicyEndUserNeedsAUserOfTheTenant(t *testing.T) {
	h := setupWordPolicyTest(t)
	for _, call := range []struct {
		name    string
		handler gin.HandlerFunc
		method  string
		body    string
	}{
		{"get", h.GetWordPolicyEndUser, http.MethodGet, ""},
		{"put", h.PutWordPolicyEndUser, http.MethodPut, `{"rules":[]}`},
		{"permission", h.PutWordPortalUser, http.MethodPut, `{"allowed":true}`},
	} {
		rec, body := wordTenantAUser.call(call.handler, call.method, call.body, "not-a-user")
		if rec.Code != http.StatusNotFound || wordErrorCode(body) != "content_moderation_not_found" {
			t.Fatalf("%s: status = %d, body = %v", call.name, rec.Code, body)
		}
	}
	rec, body := wordTenantAUser.call(h.PostWordPolicyTest, http.MethodPost, `{"input":"x","end_user_id":"not-a-user"}`, "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("test with an unknown user: %d %v", rec.Code, body)
	}
}

// portalWordCall drives a portal route with the user's session token.
func portalWordCall(t *testing.T, h *Handler, method, path, token, body string) (int, map[string]any) {
	t.Helper()
	router := gin.New()
	router.GET("/v0/portal/auth/me", h.GetPortalMe)
	router.GET("/v0/portal/word-policy", h.GetPortalWordPolicy)
	router.PUT("/v0/portal/word-policy", h.PutPortalWordPolicy)
	router.DELETE("/v0/portal/word-policy", h.DeletePortalWordPolicy)
	router.POST("/v0/portal/word-policy/validate", h.PostPortalWordPolicyValidate)
	router.POST("/v0/portal/word-policy/test", h.PostPortalWordPolicyTest)
	req := httptest.NewRequest(method, path, bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	var decoded map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &decoded)
	return rec.Code, decoded
}

// TestPostgresPortalUserKeepsWordsWhileAllowed walks the whole feature on the
// database production runs: a portal user may not keep words until the tenant
// allows it, then keeps and tests them in the portal, the tenant sees and
// revokes them, and requests stop applying them at once.
func TestPostgresPortalUserKeepsWordsWhileAllowed(t *testing.T) {
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

	const password = "Portal-Words-123!"
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	tenantID, userID := identity.SystemTenantID, uuid.NewString()
	username := "words_" + strings.ReplaceAll(userID[:8], "-", "")
	if _, err := db.ExecContext(ctx, `INSERT INTO end_users (id, tenant_id, username, username_normalized, display_name, password_hash, must_change_password)
		VALUES (?, ?, ?, ?, '词表用户', ?, false)`, userID, tenantID, username, username, string(hash)); err != nil {
		t.Fatalf("insert portal account: %v", err)
	}
	store := contentmoderation.NewStore(db)
	t.Cleanup(func() {
		_ = store.DeleteEndUserWordData(context.Background(), tenantID, userID)
		_ = store.SetWordPortalOverride(context.Background(), tenantID, userID, nil)
		_, _ = db.ExecContext(context.Background(), `DELETE FROM content_moderation_word_portal_access WHERE tenant_id = ? AND end_user_id = ''`, tenantID)
		_, _ = db.ExecContext(context.Background(), `DELETE FROM end_user_sessions WHERE end_user_id = ?`, userID)
		_, _ = db.ExecContext(context.Background(), `DELETE FROM end_users WHERE id = ?`, userID)
	})
	login, err := svc.Login(ctx, username, password, "test")
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	token := login.AccessToken
	h := &Handler{cfg: &config.Config{}}
	admin := wordCaller{tenant: tenantID, admin: true}

	// Not allowed: the portal does not offer it and refuses it.
	status, body := portalWordCall(t, h, http.MethodGet, "/v0/portal/auth/me", token, "")
	if status != http.StatusOK || body["word_policy_allowed"] != false {
		t.Fatalf("me before = %d %v", status, body)
	}
	status, body = portalWordCall(t, h, http.MethodGet, "/v0/portal/word-policy", token, "")
	if status != http.StatusForbidden || wordErrorCode(body) != "word_policy_portal_forbidden" {
		t.Fatalf("get before = %d %v", status, body)
	}

	// The tenant allows its portal users.
	rec, _ := admin.call(h.PutWordPortal, http.MethodPut, `{"default_allowed":true}`, "")
	wordExpectStatus(t, rec, http.StatusOK)
	status, body = portalWordCall(t, h, http.MethodGet, "/v0/portal/auth/me", token, "")
	if body["word_policy_allowed"] != true {
		t.Fatalf("me after = %d %v", status, body)
	}
	status, body = portalWordCall(t, h, http.MethodGet, "/v0/portal/word-policy", token, "")
	limits, _ := body["limits"].(map[string]any)
	if status != http.StatusOK || body["exists"] != false || body["can_edit"] != true || limits["max_rules"] != float64(contentmoderation.MaxWordRulesPerEndUserPolicy) {
		t.Fatalf("get after = %d %v", status, body)
	}

	// The user keeps words, checks an import against them and tries them out.
	status, body = portalWordCall(t, h, http.MethodPut, "/v0/portal/word-policy", token, `{"action":"block","block_message":"blocked by me","rules":[{"pattern":"秘密"}],"version":0}`)
	if status != http.StatusOK || body["version"] != float64(1) || len(wordRuleList(t, body)) != 1 {
		t.Fatalf("put = %d %v", status, body)
	}
	status, body = portalWordCall(t, h, http.MethodPost, "/v0/portal/word-policy/validate", token, `{"rules":[{"pattern":"秘密"},{"pattern":"新词"}]}`)
	summary, _ := body["summary"].(map[string]any)
	if status != http.StatusOK || summary["existing"] != float64(1) || summary["new"] != float64(1) || summary["fits"] != true {
		t.Fatalf("validate = %d %v", status, body)
	}
	status, body = portalWordCall(t, h, http.MethodPost, "/v0/portal/word-policy/test", token, `{"input":"这是秘密"}`)
	if status != http.StatusOK || body["blocked"] != true || body["block_message"] != "blocked by me" {
		t.Fatalf("test = %d %v", status, body)
	}

	// Requests of the user's keys have them applied.
	policies, err := store.LoadWordPolicies(ctx, tenantID, "some-key", userID)
	if err != nil || policies.EndUser == nil {
		t.Fatalf("load while allowed = %+v, %v", policies, err)
	}

	// The tenant sees who keeps what, and the words themselves.
	rec, body = admin.call(h.GetWordPortal, http.MethodGet, "", "")
	wordExpectStatus(t, rec, http.StatusOK)
	var listed map[string]any
	users, _ := body["users"].([]any)
	for _, item := range users {
		if user, _ := item.(map[string]any); user["id"] == userID {
			listed = user
		}
	}
	if policy, _ := listed["policy"].(map[string]any); listed["allowed"] != true || listed["override"] != nil || policy["rule_count"] != float64(1) {
		t.Fatalf("portal list entry = %v", listed)
	}
	rec, body = admin.call(h.GetWordPolicyEndUser, http.MethodGet, "", userID)
	wordExpectStatus(t, rec, http.StatusOK)
	if body["end_user_allowed"] != true || body["end_user_name"] != "词表用户" || len(wordRuleList(t, body)) != 1 {
		t.Fatalf("admin view = %v", body)
	}

	// Revoked for this user: the portal refuses it and requests stop applying it.
	rec, body = admin.call(h.PutWordPortalUser, http.MethodPut, `{"allowed":false}`, userID)
	wordExpectStatus(t, rec, http.StatusOK)
	if body["allowed"] != false || body["override"] != false {
		t.Fatalf("override = %v", body)
	}
	status, _ = portalWordCall(t, h, http.MethodGet, "/v0/portal/word-policy", token, "")
	if status != http.StatusForbidden {
		t.Fatalf("get after revoke = %d", status)
	}
	if policies, _ := store.LoadWordPolicies(ctx, tenantID, "some-key", userID); policies.EndUser != nil {
		t.Fatal("a revoked user's words still apply")
	}
	rec, body = admin.call(h.GetWordPolicyEndUser, http.MethodGet, "", userID)
	if body["end_user_allowed"] != false || body["exists"] != true {
		t.Fatalf("admin view after revoke = %d %v", rec.Code, body)
	}
}
