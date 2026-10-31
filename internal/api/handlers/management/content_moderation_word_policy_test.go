package management

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/contentmoderation"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/identity"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/usage"
)

const (
	wordTenantA = "00000000-0000-0000-0000-0000000000a1"
	wordTenantB = "00000000-0000-0000-0000-0000000000b2"
)

func setupWordPolicyTest(t *testing.T) *Handler {
	t.Helper()
	gin.SetMode(gin.TestMode)
	usage.CloseDB()
	if err := usage.InitDB(filepath.Join(t.TempDir(), "usage.db"), config.RequestLogStorageConfig{}, time.UTC); err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	t.Cleanup(usage.CloseDB)
	return &Handler{cfg: &config.Config{}}
}

// wordCaller is who makes a request: the tenant it acts in and whether it is a
// platform administrator.
type wordCaller struct {
	tenant string
	admin  bool
}

var (
	wordPlatformAdmin  = wordCaller{tenant: identity.SystemTenantID, admin: true}
	wordAdminInTenantA = wordCaller{tenant: wordTenantA, admin: true}
	wordPlainSystem    = wordCaller{tenant: identity.SystemTenantID}
	wordTenantAUser    = wordCaller{tenant: wordTenantA}
	wordTenantBUser    = wordCaller{tenant: wordTenantB}
)

func (w wordCaller) call(handler gin.HandlerFunc, method, body, id string) (*httptest.ResponseRecorder, map[string]any) {
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Set(managementPrincipalKey, identity.Principal{PlatformAdmin: w.admin, EffectiveTenant: identity.Tenant{ID: w.tenant}})
	c.Request = httptest.NewRequest(method, "/v0/management/content-moderation/word-policies", bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")
	if id != "" {
		c.Params = gin.Params{{Key: "id", Value: id}}
	}
	handler(c)
	// gin writes a status-only reply (204) when the handler chain ends.
	c.Writer.WriteHeaderNow()
	var decoded map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &decoded)
	return rec, decoded
}

func wordExpectStatus(t *testing.T, rec *httptest.ResponseRecorder, want int) {
	t.Helper()
	if rec.Code != want {
		t.Fatalf("status = %d, want %d; body = %s", rec.Code, want, rec.Body.String())
	}
}

func wordErrorCode(body map[string]any) string {
	if e, ok := body["error"].(map[string]any); ok {
		code, _ := e["code"].(string)
		return code
	}
	return ""
}

func wordRuleList(t *testing.T, body map[string]any) []map[string]any {
	t.Helper()
	raw, _ := body["rules"].([]any)
	out := make([]map[string]any, 0, len(raw))
	for _, item := range raw {
		out = append(out, item.(map[string]any))
	}
	return out
}

func createWordPolicyKey(t *testing.T, tenantID, id, name string) {
	t.Helper()
	if err := usage.UpsertAPIKeyForTenant(tenantID, usage.APIKeyRow{ID: id, Key: "sk-" + id, Name: name}); err != nil {
		t.Fatalf("create key: %v", err)
	}
}

func TestWordGuardIsAdvertisedAsACapability(t *testing.T) {
	h := setupWordPolicyTest(t)
	rec, body := wordTenantAUser.call(h.GetContentModerationProfiles, http.MethodGet, "", "")
	wordExpectStatus(t, rec, http.StatusOK)
	capabilities, _ := body["capabilities"].(map[string]any)
	if capabilities["word_guard"] != true {
		t.Fatalf("capabilities = %v", capabilities)
	}
}

func TestWordPolicyTenantLifecycle(t *testing.T) {
	h := setupWordPolicyTest(t)

	rec, body := wordTenantAUser.call(h.GetWordPolicyTenant, http.MethodGet, "", "")
	wordExpectStatus(t, rec, http.StatusOK)
	if body["exists"] != false || body["enabled"] != true || body["version"] != float64(0) || len(wordRuleList(t, body)) != 0 || body["can_edit"] != true {
		t.Fatalf("a policy never saved: %v", body)
	}
	if effective, _ := body["effective"].(map[string]any); effective["action"] != "block" || effective["replacement"] != "***" || effective["scan_scope"] != "all_user" {
		t.Fatalf("effective settings of an empty policy = %v", body["effective"])
	}

	// The rule that does not say whether it is enabled is, and one without a kind
	// is a keyword.
	save := `{"action":"replace","replacement":"[x]","scan_scope":"with_system","reply_filter":true,
		"rules":[{"pattern":" Badword "},{"kind":"regex","pattern":"\\d{4}-\\d{4}","enabled":false,"note":"card"}]}`
	rec, body = wordTenantAUser.call(h.PutWordPolicyTenant, http.MethodPut, save, "")
	wordExpectStatus(t, rec, http.StatusOK)
	if body["exists"] != true || body["version"] != float64(1) || body["action"] != "replace" || body["reply_filter"] != true {
		t.Fatalf("saved policy = %v", body)
	}
	rules := wordRuleList(t, body)
	if len(rules) != 2 || rules[0]["pattern"] != "Badword" || rules[0]["kind"] != "keyword" || rules[0]["enabled"] != true ||
		rules[1]["kind"] != "regex" || rules[1]["enabled"] != false || rules[1]["note"] != "card" || rules[0]["id"] == "" || rules[0]["id"] == rules[1]["id"] {
		t.Fatalf("saved rules = %v", rules)
	}
	if effective, _ := body["effective"].(map[string]any); effective["action"] != "replace" || effective["reply_filter"] != true {
		t.Fatalf("effective = %v", body["effective"])
	}

	rec, again := wordTenantAUser.call(h.GetWordPolicyTenant, http.MethodGet, "", "")
	wordExpectStatus(t, rec, http.StatusOK)
	if again["version"] != float64(1) || len(wordRuleList(t, again)) != 2 {
		t.Fatalf("reloaded policy = %v", again)
	}

	// A save that names the version it read goes through once and is then stale.
	rec, body = wordTenantAUser.call(h.PutWordPolicyTenant, http.MethodPut, `{"version":1,"rules":[{"pattern":"only"}]}`, "")
	wordExpectStatus(t, rec, http.StatusOK)
	if body["version"] != float64(2) || body["enabled"] != true {
		t.Fatalf("second save = %v", body)
	}
	rec, body = wordTenantAUser.call(h.PutWordPolicyTenant, http.MethodPut, `{"version":1,"rules":[{"pattern":"stale"}]}`, "")
	wordExpectStatus(t, rec, http.StatusConflict)
	if wordErrorCode(body) != "word_policy_version_conflict" {
		t.Fatalf("conflict body = %v", body)
	}
	// Without a version the save is unconditional.
	rec, _ = wordTenantAUser.call(h.PutWordPolicyTenant, http.MethodPut, `{"enabled":false,"rules":[{"pattern":"last"}]}`, "")
	wordExpectStatus(t, rec, http.StatusOK)

	rec, _ = wordTenantAUser.call(h.DeleteWordPolicyTenant, http.MethodDelete, "", "")
	wordExpectStatus(t, rec, http.StatusNoContent)
	rec, _ = wordTenantAUser.call(h.DeleteWordPolicyTenant, http.MethodDelete, "", "")
	wordExpectStatus(t, rec, http.StatusNoContent)
	_, body = wordTenantAUser.call(h.GetWordPolicyTenant, http.MethodGet, "", "")
	if body["exists"] != false {
		t.Fatalf("policy survived its deletion: %v", body)
	}
}

func TestWordPolicyRefusesBadInputAndNamesTheRule(t *testing.T) {
	h := setupWordPolicyTest(t)

	rec, body := wordTenantAUser.call(h.PutWordPolicyTenant, http.MethodPut,
		`{"rules":[{"pattern":"fine"},{"kind":"regex","pattern":"(unclosed"},{"pattern":"   "},{"kind":"regex","pattern":"a*"},{"kind":"glob","pattern":"x"}]}`, "")
	wordExpectStatus(t, rec, http.StatusUnprocessableEntity)
	if wordErrorCode(body) != "word_policy_invalid" {
		t.Fatalf("body = %v", body)
	}
	errBody := body["error"].(map[string]any)
	problems := errBody["rules"].([]any)
	if len(problems) != 4 || errBody["rules_total"] != float64(4) {
		t.Fatalf("problems = %v", problems)
	}
	first := problems[0].(map[string]any)
	if first["index"] != float64(1) || first["pattern"] != "(unclosed" || first["code"] != "invalid_regex" || first["message"] == "" {
		t.Fatalf("the first problem must name the broken rule: %v", first)
	}
	if message, _ := errBody["message"].(string); !strings.Contains(message, "(unclosed") {
		t.Fatalf("the message does not name the rule: %q", message)
	}
	codes := []string{}
	for _, problem := range problems {
		codes = append(codes, problem.(map[string]any)["code"].(string))
	}
	if strings.Join(codes, ",") != "invalid_regex,empty,matches_empty,unknown_kind" {
		t.Fatalf("codes = %v", codes)
	}
	// Nothing was stored.
	if _, got := wordTenantAUser.call(h.GetWordPolicyTenant, http.MethodGet, "", ""); got["exists"] != false {
		t.Fatalf("a refused save left a policy behind: %v", got)
	}

	for field, payload := range map[string]string{
		"action":        `{"action":"purge"}`,
		"scan_scope":    `{"scan_scope":"everything"}`,
		"replacement":   `{"replacement":"a\u0007b"}`,
		"block_message": `{"block_message":"` + strings.Repeat("x", contentmoderation.MaxWordBlockMessageRunes+1) + `"}`,
	} {
		rec, body = wordTenantAUser.call(h.PutWordPolicyTenant, http.MethodPut, payload, "")
		wordExpectStatus(t, rec, http.StatusUnprocessableEntity)
		if got := body["error"].(map[string]any)["field"]; got != field {
			t.Errorf("%s: refused for field %v", field, got)
		}
	}

	manyRegex := []string{}
	for i := 0; i <= contentmoderation.MaxWordRegexPerPolicy; i++ {
		manyRegex = append(manyRegex, `{"kind":"regex","pattern":"word`+strings.Repeat("x", i)+`"}`)
	}
	rec, body = wordTenantAUser.call(h.PutWordPolicyTenant, http.MethodPut, `{"rules":[`+strings.Join(manyRegex, ",")+`]}`, "")
	wordExpectStatus(t, rec, http.StatusUnprocessableEntity)
	if problems := body["error"].(map[string]any)["rules"].([]any); len(problems) != 1 || problems[0].(map[string]any)["code"] != "too_many_regex" {
		t.Fatalf("regex limit: %v", body)
	}

	for _, payload := range []string{``, `not json`, `{"rules":`} {
		rec, body = wordTenantAUser.call(h.PutWordPolicyTenant, http.MethodPut, payload, "")
		wordExpectStatus(t, rec, http.StatusBadRequest)
		if wordErrorCode(body) != "content_moderation_invalid_request" {
			t.Errorf("%q: body = %v", payload, body)
		}
	}
}

func TestWordPolicyBodyIsBounded(t *testing.T) {
	h := setupWordPolicyTest(t)
	huge := `{"rules":[{"pattern":"x","note":"` + strings.Repeat("n", maxWordPolicyBodyBytes+1024) + `"}]}`
	rec, body := wordTenantAUser.call(h.PutWordPolicyTenant, http.MethodPut, huge, "")
	wordExpectStatus(t, rec, http.StatusRequestEntityTooLarge)
	if wordErrorCode(body) != "word_policy_too_large" {
		t.Fatalf("body = %v", body)
	}
}

func TestPlatformWordPolicyIsOnlyForPlatformAdministrators(t *testing.T) {
	h := setupWordPolicyTest(t)
	save := `{"action":"block","rules":[{"pattern":"platformword"}]}`

	for name, caller := range map[string]wordCaller{
		"a tenant":                          wordTenantAUser,
		"a plain user of the system tenant": wordPlainSystem,
		"an administrator inside a tenant":  wordAdminInTenantA,
	} {
		for _, call := range []struct {
			handler gin.HandlerFunc
			method  string
		}{{h.PutWordPolicyPlatform, http.MethodPut}, {h.DeleteWordPolicyPlatform, http.MethodDelete}} {
			rec, body := caller.call(call.handler, call.method, save, "")
			wordExpectStatus(t, rec, http.StatusForbidden)
			if wordErrorCode(body) != "word_policy_platform_forbidden" {
				t.Errorf("%s %s: body = %v", name, call.method, body)
			}
		}
	}

	rec, body := wordPlatformAdmin.call(h.PutWordPolicyPlatform, http.MethodPut, save, "")
	wordExpectStatus(t, rec, http.StatusOK)
	if body["scope_type"] != "platform" || body["can_edit"] != true || body["version"] != float64(1) {
		t.Fatalf("platform save = %v", body)
	}

	// Everyone can read it, and sees it cannot be edited from where they are.
	for name, caller := range map[string]wordCaller{"tenant": wordTenantAUser, "other tenant": wordTenantBUser, "admin inside a tenant": wordAdminInTenantA, "plain system user": wordPlainSystem} {
		rec, body = caller.call(h.GetWordPolicyPlatform, http.MethodGet, "", "")
		wordExpectStatus(t, rec, http.StatusOK)
		rules := wordRuleList(t, body)
		if body["exists"] != true || len(rules) != 1 || rules[0]["pattern"] != "platformword" || body["can_edit"] != false && caller == wordPlatformAdmin {
			t.Errorf("%s reads %v", name, body)
		}
		if body["can_edit"] != false {
			t.Errorf("%s may not edit the platform policy but is told it may", name)
		}
	}
	if _, body = wordPlatformAdmin.call(h.GetWordPolicyPlatform, http.MethodGet, "", ""); body["can_edit"] != true {
		t.Fatalf("the platform administrator cannot edit: %v", body)
	}

	// The refused writes left it as it was.
	_, body = wordTenantBUser.call(h.GetWordPolicyPlatform, http.MethodGet, "", "")
	if body["version"] != float64(1) || len(wordRuleList(t, body)) != 1 {
		t.Fatalf("platform policy changed under a refused write: %v", body)
	}

	// The platform policy and the system tenant's own policy are separate.
	if _, body = wordPlatformAdmin.call(h.GetWordPolicyTenant, http.MethodGet, "", ""); body["exists"] != false {
		t.Fatalf("the system tenant's own policy picked up the platform policy: %v", body)
	}

	rec, _ = wordPlatformAdmin.call(h.DeleteWordPolicyPlatform, http.MethodDelete, "", "")
	wordExpectStatus(t, rec, http.StatusNoContent)
	if _, body = wordTenantAUser.call(h.GetWordPolicyPlatform, http.MethodGet, "", ""); body["exists"] != false {
		t.Fatalf("platform policy survived its deletion: %v", body)
	}
}

func TestWordPoliciesAreIsolatedBetweenTenants(t *testing.T) {
	h := setupWordPolicyTest(t)
	createWordPolicyKey(t, wordTenantA, "key-a", "Key of A")
	createWordPolicyKey(t, wordTenantB, "key-b", "Key of B")

	rec, _ := wordTenantAUser.call(h.PutWordPolicyTenant, http.MethodPut, `{"rules":[{"pattern":"secret-of-a"}]}`, "")
	wordExpectStatus(t, rec, http.StatusOK)
	rec, _ = wordTenantAUser.call(h.PutWordPolicyKey, http.MethodPut, `{"rules":[{"pattern":"key-secret-of-a"}]}`, "key-a")
	wordExpectStatus(t, rec, http.StatusOK)

	// B sees none of it.
	_, body := wordTenantBUser.call(h.GetWordPolicyTenant, http.MethodGet, "", "")
	if body["exists"] != false || len(wordRuleList(t, body)) != 0 {
		t.Fatalf("tenant B reads tenant A's policy: %v", body)
	}
	_, overview := wordTenantBUser.call(h.GetWordPolicies, http.MethodGet, "", "")
	if tenant := overview["tenant"].(map[string]any); tenant["exists"] != false {
		t.Fatalf("tenant B's overview shows tenant A's policy: %v", tenant)
	}
	if keys := overview["keys"].([]any); len(keys) != 0 {
		t.Fatalf("tenant B's overview lists tenant A's key policies: %v", keys)
	}

	// B cannot reach A's key policy by guessing its id, nor create one for it.
	rec, body = wordTenantBUser.call(h.GetWordPolicyKey, http.MethodGet, "", "key-a")
	wordExpectStatus(t, rec, http.StatusNotFound)
	rec, _ = wordTenantBUser.call(h.PutWordPolicyKey, http.MethodPut, `{"rules":[{"pattern":"planted"}]}`, "key-a")
	wordExpectStatus(t, rec, http.StatusNotFound)
	// Deleting is allowed for any id (it also clears leftovers) but only ever
	// reaches B's own policies.
	rec, _ = wordTenantBUser.call(h.DeleteWordPolicyKey, http.MethodDelete, "", "key-a")
	wordExpectStatus(t, rec, http.StatusNoContent)
	_, body = wordTenantAUser.call(h.GetWordPolicyKey, http.MethodGet, "", "key-a")
	if body["exists"] != true || len(wordRuleList(t, body)) != 1 || wordRuleList(t, body)[0]["pattern"] != "key-secret-of-a" {
		t.Fatalf("tenant A's key policy was touched by tenant B: %v", body)
	}
	rec, body = wordTenantAUser.call(h.GetWordPolicyKey, http.MethodGet, "", "key-b")
	wordExpectStatus(t, rec, http.StatusNotFound)

	// Validation and the test dialog resolve keys in the caller's tenant only.
	rec, _ = wordTenantBUser.call(h.PostWordPolicyValidate, http.MethodPost, `{"scope":"key","key_id":"key-a","rules":[{"pattern":"x"}]}`, "")
	wordExpectStatus(t, rec, http.StatusNotFound)
	rec, _ = wordTenantBUser.call(h.PostWordPolicyTest, http.MethodPost, `{"input":"secret-of-a key-secret-of-a","key_id":"key-a"}`, "")
	wordExpectStatus(t, rec, http.StatusNotFound)
	rec, body = wordTenantBUser.call(h.PostWordPolicyTest, http.MethodPost, `{"input":"secret-of-a key-secret-of-a"}`, "")
	wordExpectStatus(t, rec, http.StatusOK)
	if body["action"] != "allow" || body["match_count"] != float64(0) {
		t.Fatalf("tenant B's test hit tenant A's words: %v", body)
	}
}

func TestWordPolicyKeyOverride(t *testing.T) {
	h := setupWordPolicyTest(t)
	createWordPolicyKey(t, wordTenantA, "key-1", "Zeta key")
	createWordPolicyKey(t, wordTenantA, "key-2", "Alpha key")

	rec, _ := wordTenantAUser.call(h.PutWordPolicyTenant, http.MethodPut, `{"action":"replace","replacement":"[t]","reply_filter":true,"rules":[{"pattern":"tenantword"}]}`, "")
	wordExpectStatus(t, rec, http.StatusOK)

	// An id that is no key of the tenant is not found, whatever the verb.
	for _, call := range []struct {
		handler gin.HandlerFunc
		method  string
	}{{h.GetWordPolicyKey, http.MethodGet}, {h.PutWordPolicyKey, http.MethodPut}} {
		rec, body := wordTenantAUser.call(call.handler, call.method, `{}`, "00000000-0000-0000-0000-000000000099")
		wordExpectStatus(t, rec, http.StatusNotFound)
		if !strings.Contains(rec.Body.String(), "not found") || wordErrorCode(body) != "content_moderation_not_found" {
			t.Errorf("%s: body = %s", call.method, rec.Body.String())
		}
	}

	rec, body := wordTenantAUser.call(h.GetWordPolicyKey, http.MethodGet, "", "key-1")
	wordExpectStatus(t, rec, http.StatusOK)
	if body["exists"] != false || body["key_name"] != "Zeta key" || body["scope_id"] != "key-1" || body["inherit_tenant"] != true {
		t.Fatalf("a key without an override: %v", body)
	}
	if effective := body["effective"].(map[string]any); effective["action"] != "replace" || effective["replacement"] != "[t]" || effective["reply_filter"] != true {
		t.Fatalf("a key inherits the tenant's settings: %v", effective)
	}

	rec, body = wordTenantAUser.call(h.PutWordPolicyKey, http.MethodPut, `{"action":"log","inherit_tenant":false,"rules":[{"pattern":"keyword"}]}`, "key-1")
	wordExpectStatus(t, rec, http.StatusOK)
	effective := body["effective"].(map[string]any)
	if body["scope_type"] != "key" || body["inherit_tenant"] != false || effective["action"] != "log" || effective["replacement"] != "[t]" || effective["reply_filter"] != true {
		t.Fatalf("key override = %v", body)
	}

	rec, _ = wordTenantAUser.call(h.PutWordPolicyKey, http.MethodPut, `{"rules":[{"pattern":"alpha-only"},{"pattern":"two"}]}`, "key-2")
	wordExpectStatus(t, rec, http.StatusOK)

	// The overview lists the overrides by key name, with their sizes.
	_, overview := wordTenantAUser.call(h.GetWordPolicies, http.MethodGet, "", "")
	keys := overview["keys"].([]any)
	if len(keys) != 2 || keys[0].(map[string]any)["key_name"] != "Alpha key" || keys[0].(map[string]any)["rule_count"] != float64(2) ||
		keys[1].(map[string]any)["key_id"] != "key-1" || keys[1].(map[string]any)["action"] != "log" {
		t.Fatalf("keys = %v", keys)
	}
	for _, item := range keys {
		if _, leaksRules := item.(map[string]any)["rules"]; leaksRules {
			t.Fatalf("the overview carries rules: %v", item)
		}
	}

	// A key that is deleted drops out of the overview; its leftover policy can
	// still be cleared.
	if err := usage.DeleteAPIKeyByIDForTenant(wordTenantA, "key-1"); err != nil {
		t.Fatalf("delete key: %v", err)
	}
	_, overview = wordTenantAUser.call(h.GetWordPolicies, http.MethodGet, "", "")
	if keys = overview["keys"].([]any); len(keys) != 1 || keys[0].(map[string]any)["key_id"] != "key-2" {
		t.Fatalf("keys after a deletion = %v", keys)
	}
	rec, _ = wordTenantAUser.call(h.GetWordPolicyKey, http.MethodGet, "", "key-1")
	wordExpectStatus(t, rec, http.StatusNotFound)
	rec, _ = wordTenantAUser.call(h.DeleteWordPolicyKey, http.MethodDelete, "", "key-1")
	wordExpectStatus(t, rec, http.StatusNoContent)
	if _, err := h.contentModerationStore().GetWordPolicy(t.Context(), wordTenantA, contentmoderation.WordScopeKey, "key-1"); err == nil {
		t.Fatal("the leftover policy of a deleted key was not cleared")
	}
}

func TestWordPolicyOverview(t *testing.T) {
	h := setupWordPolicyTest(t)
	_, overview := wordTenantAUser.call(h.GetWordPolicies, http.MethodGet, "", "")
	defaults := overview["defaults"].(map[string]any)
	limits := overview["limits"].(map[string]any)
	if defaults["action"] != "block" || defaults["replacement"] != "***" || defaults["scan_scope"] != "all_user" || defaults["block_message"] == "" {
		t.Fatalf("defaults = %v", defaults)
	}
	if limits["max_rules"] != float64(contentmoderation.MaxWordRulesPerPolicy) || limits["max_regex"] != float64(contentmoderation.MaxWordRegexPerPolicy) {
		t.Fatalf("limits = %v", limits)
	}
	if overview["can_manage_platform"] != false || overview["platform"].(map[string]any)["exists"] != false || overview["tenant"].(map[string]any)["exists"] != false {
		t.Fatalf("empty overview = %v", overview)
	}
	if keys, ok := overview["keys"].([]any); !ok || len(keys) != 0 {
		t.Fatalf("keys must be an empty list: %v", overview["keys"])
	}

	_, _ = wordPlatformAdmin.call(h.PutWordPolicyPlatform, http.MethodPut, `{"action":"log","rules":[{"pattern":"a"},{"pattern":"b"}]}`, "")
	_, _ = wordTenantAUser.call(h.PutWordPolicyTenant, http.MethodPut, `{"enabled":false,"rules":[{"pattern":"c"}]}`, "")
	_, overview = wordTenantAUser.call(h.GetWordPolicies, http.MethodGet, "", "")
	platform, tenant := overview["platform"].(map[string]any), overview["tenant"].(map[string]any)
	if platform["exists"] != true || platform["rule_count"] != float64(2) || platform["action"] != "log" ||
		tenant["exists"] != true || tenant["rule_count"] != float64(1) || tenant["enabled"] != false {
		t.Fatalf("overview = %v / %v", platform, tenant)
	}
	if _, has := platform["rules"]; has {
		t.Fatal("the overview carries the platform rules")
	}
	_, overview = wordPlatformAdmin.call(h.GetWordPolicies, http.MethodGet, "", "")
	if overview["can_manage_platform"] != true {
		t.Fatalf("a platform administrator cannot manage the platform policy: %v", overview)
	}
	// In the system tenant the platform and tenant policies are listed apart.
	if overview["tenant"].(map[string]any)["exists"] != false || overview["platform"].(map[string]any)["rule_count"] != float64(2) {
		t.Fatalf("system tenant overview = %v", overview)
	}
}

func TestWordPoliciesNeedTheDatabase(t *testing.T) {
	gin.SetMode(gin.TestMode)
	usage.CloseDB()
	h := &Handler{cfg: &config.Config{}}
	for name, call := range map[string]func() (*httptest.ResponseRecorder, map[string]any){
		"overview": func() (*httptest.ResponseRecorder, map[string]any) {
			return wordTenantAUser.call(h.GetWordPolicies, http.MethodGet, "", "")
		},
		"get": func() (*httptest.ResponseRecorder, map[string]any) {
			return wordTenantAUser.call(h.GetWordPolicyTenant, http.MethodGet, "", "")
		},
		"put": func() (*httptest.ResponseRecorder, map[string]any) {
			return wordTenantAUser.call(h.PutWordPolicyTenant, http.MethodPut, `{}`, "")
		},
		"test": func() (*httptest.ResponseRecorder, map[string]any) {
			return wordTenantAUser.call(h.PostWordPolicyTest, http.MethodPost, `{"input":"x"}`, "")
		},
	} {
		rec, body := call()
		wordExpectStatus(t, rec, http.StatusServiceUnavailable)
		if wordErrorCode(body) != "content_moderation_unavailable" {
			t.Errorf("%s: body = %v", name, body)
		}
	}
}
