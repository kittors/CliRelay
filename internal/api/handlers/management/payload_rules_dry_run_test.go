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
	"github.com/router-for-me/CLIProxyAPI/v6/internal/api/bodyutil"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/identity"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/payloadrules"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/usage"
)

type dryRunResult struct {
	Body    string                     `json:"body"`
	Models  []string                   `json:"models"`
	Rules   []payloadrules.RuleOutcome `json:"rules"`
	Changes []payloadrules.Change      `json:"changes"`
	Dropped []droppedPayloadRule       `json:"dropped"`
	Error   struct {
		Code string `json:"code"`
	} `json:"error"`
}

func postDryRun(t *testing.T, h *Handler, principal *identity.Principal, body string) (int, dryRunResult) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	if principal != nil {
		c.Set(managementPrincipalKey, *principal)
	}
	c.Request = httptest.NewRequest(http.MethodPost, "/v0/management/payload-rules/dry-run", strings.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	h.PostPayloadRulesDryRun(c)
	var result dryRunResult
	if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
		t.Fatalf("decode %q: %v", rec.Body.String(), err)
	}
	return rec.Code, result
}

func savedOverrideConfig() *config.Config {
	return &config.Config{Payload: config.PayloadConfig{
		Override: []config.PayloadRule{{
			Models: []config.PayloadModelRule{{Name: "gpt-*", Protocol: "codex"}},
			Params: map[string]any{"service_tier": "priority"},
		}},
	}}
}

func TestPayloadRulesDryRunAppliesTheSavedRules(t *testing.T) {
	h := NewHandler(savedOverrideConfig(), "", nil)
	status, result := postDryRun(t, h, nil, `{
		"model": "gpt-5",
		"requested-model": "fast(high)",
		"protocol": "codex",
		"body": "{\"model\":\"gpt-5\",\"input\":[],\"big\":12345678901234567890}"
	}`)
	if status != http.StatusOK {
		t.Fatalf("status = %d, error = %s", status, result.Error.Code)
	}
	wantBody := "{\n  \"model\": \"gpt-5\",\n  \"input\": [],\n  \"big\": 12345678901234567890,\n  \"service_tier\": \"priority\"\n}"
	if result.Body != wantBody {
		t.Fatalf("body =\n%s\nwant\n%s", result.Body, wantBody)
	}
	if strings.Join(result.Models, ",") != "gpt-5,fast,fast(high)" {
		t.Fatalf("models = %v", result.Models)
	}
	if len(result.Rules) != 1 || !result.Rules[0].Matched || result.Rules[0].Params[0].Value != `"priority"` {
		t.Fatalf("rules = %+v", result.Rules)
	}
	if len(result.Changes) != 1 || result.Changes[0].Path != "service_tier" || result.Changes[0].Before != nil {
		t.Fatalf("changes = %+v", result.Changes)
	}

	// The same rule restricted to codex does not apply to an OpenAI body.
	status, result = postDryRun(t, h, nil, `{"model":"gpt-5","protocol":"openai","body":{"model":"gpt-5"}}`)
	if status != http.StatusOK || len(result.Changes) != 0 || result.Rules[0].Miss != payloadrules.MissProtocol {
		t.Fatalf("status = %d, rules = %+v, changes = %+v", status, result.Rules, result.Changes)
	}
}

func TestPayloadRulesDryRunDraftReplacesOnlyTheSectionsItCarries(t *testing.T) {
	h := NewHandler(savedOverrideConfig(), "", nil)

	// A draft of the defaults only: the saved override still runs.
	status, result := postDryRun(t, h, nil, `{
		"model": "gpt-5", "protocol": "codex", "body": {},
		"rules": {"default": [{"models": [{"name": "*"}], "params": {"max_output_tokens": 8000}}], "override": null}
	}`)
	if status != http.StatusOK {
		t.Fatalf("status = %d, error = %s", status, result.Error.Code)
	}
	if !strings.Contains(result.Body, `"max_output_tokens": 8000`) || !strings.Contains(result.Body, `"service_tier": "priority"`) {
		t.Fatalf("body = %s", result.Body)
	}

	// A draft filter runs like a saved one.
	status, result = postDryRun(t, h, nil, `{
		"model": "gpt-5", "protocol": "codex", "body": {"input": [], "keep": 1},
		"rules": {"filter": [{"models": [{"name": "gpt-5"}], "params": ["input"]}]}
	}`)
	if status != http.StatusOK || strings.Contains(result.Body, "input") || !strings.Contains(result.Body, `"keep": 1`) {
		t.Fatalf("status = %d, body = %s", status, result.Body)
	}

	// An empty override section in the draft removes the saved rule from the run.
	status, result = postDryRun(t, h, nil, `{"model": "gpt-5", "protocol": "codex", "body": {}, "rules": {"override": []}}`)
	if status != http.StatusOK || result.Body != "{}" || len(result.Rules) != 0 {
		t.Fatalf("status = %d, body = %s, rules = %+v", status, result.Body, result.Rules)
	}
}

func TestPayloadRulesDryRunDropsRawRulesSavingWouldDrop(t *testing.T) {
	h := NewHandler(&config.Config{}, "", nil)
	status, result := postDryRun(t, h, nil, `{
		"model": "m", "body": {},
		"rules": {"override-raw": [
			{"models": [{"name": "*"}], "params": {"broken": "{not json"}},
			{"models": [{"name": "*"}], "params": {"thinking": "{\"type\":\"adaptive\"}"}}
		]}
	}`)
	if status != http.StatusOK {
		t.Fatalf("status = %d, error = %s", status, result.Error.Code)
	}
	if len(result.Dropped) != 1 || result.Dropped[0] != (droppedPayloadRule{Section: payloadrules.SectionOverrideRaw, Index: 0, Param: "broken"}) {
		t.Fatalf("dropped = %+v", result.Dropped)
	}
	// The rule that ran keeps its place in the draft.
	if len(result.Rules) != 1 || result.Rules[0].Index != 1 || !result.Rules[0].Matched {
		t.Fatalf("rules = %+v", result.Rules)
	}
	if !strings.Contains(result.Body, `"type": "adaptive"`) || strings.Contains(result.Body, "broken") {
		t.Fatalf("body = %s", result.Body)
	}
}

func TestPayloadRulesDryRunUsesTheEffectiveTenantsRules(t *testing.T) {
	usage.CloseDB()
	if err := usage.InitDB(filepath.Join(t.TempDir(), "usage.db"), config.RequestLogStorageConfig{}, time.UTC); err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	t.Cleanup(usage.CloseDB)
	const tenantID = "6d3c1f0e-8a52-4d8e-9a77-0f1c2b3d4e5f"
	if err := usage.UpsertRuntimeSettingForTenant(tenantID, "payload", config.PayloadConfig{
		Override: []config.PayloadRule{{Models: []config.PayloadModelRule{{Name: "*"}}, Params: map[string]any{"tenant_rule": true}}},
	}); err != nil {
		t.Fatalf("seed tenant payload: %v", err)
	}

	h := NewHandler(savedOverrideConfig(), "", nil)
	principal := identity.Principal{EffectiveTenant: identity.Tenant{ID: tenantID}}
	status, result := postDryRun(t, h, &principal, `{"model": "gpt-5", "protocol": "codex", "body": {}}`)
	if status != http.StatusOK {
		t.Fatalf("status = %d, error = %s", status, result.Error.Code)
	}
	if !strings.Contains(result.Body, `"tenant_rule": true`) || strings.Contains(result.Body, "service_tier") {
		t.Fatalf("body = %s, want the tenant's rule and not the system's", result.Body)
	}
}

func TestPayloadRulesDryRunRejectsBadInput(t *testing.T) {
	h := NewHandler(savedOverrideConfig(), "", nil)
	cases := []struct {
		name, body, code string
	}{
		{"not JSON", `{`, "payload_dry_run_invalid_request"},
		{"no model", `{"body": {}}`, "payload_dry_run_model_required"},
		{"no body", `{"model": "m"}`, "payload_dry_run_invalid_body"},
		{"array body", `{"model": "m", "body": []}`, "payload_dry_run_invalid_body"},
		{"body text not JSON", `{"model": "m", "body": "{oops"}`, "payload_dry_run_invalid_body"},
		{"body text not an object", `{"model": "m", "body": "[1]"}`, "payload_dry_run_invalid_body"},
		{"malformed rules", `{"model": "m", "body": {}, "rules": {"default": "x"}}`, "payload_dry_run_invalid_rules"},
		{"malformed filter", `{"model": "m", "body": {}, "rules": {"filter": [{"params": "x"}]}}`, "payload_dry_run_invalid_rules"},
	}
	for _, tc := range cases {
		status, result := postDryRun(t, h, nil, tc.body)
		if status != http.StatusBadRequest || result.Error.Code != tc.code {
			t.Errorf("%s: status = %d, code = %q, want 400 %q", tc.name, status, result.Error.Code, tc.code)
		}
	}

	huge := `{"model":"m","body":"` + strings.Repeat("a", int(bodyutil.ManagementBodyLimit)) + `"}`
	if status, result := postDryRun(t, h, nil, huge); status != http.StatusRequestEntityTooLarge || result.Error.Code != "payload_dry_run_too_large" {
		t.Errorf("huge body: status = %d, code = %q", status, result.Error.Code)
	}
}

func TestGetConfigAdvertisesThePayloadRulesDryRun(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodGet, "/v0/management/config", nil)
	NewHandler(&config.Config{}, "", nil).GetConfig(c)

	var response struct {
		Capabilities map[string]bool `json:"capabilities"`
	}
	if err := json.NewDecoder(bytes.NewReader(rec.Body.Bytes())).Decode(&response); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !response.Capabilities["payload-rules-dry-run"] {
		t.Fatalf("capabilities = %v in %s", response.Capabilities, rec.Body.String())
	}
}
