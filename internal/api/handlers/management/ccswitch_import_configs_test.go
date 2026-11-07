package management

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sort"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/usage"
)

func TestCcSwitchImportConfigsManagementHandlersUseDatabase(t *testing.T) {
	gin.SetMode(gin.TestMode)
	setupPermissionProfilesTestDB(t)

	h := NewHandler(&config.Config{}, "", nil)

	getEmptyRec := httptest.NewRecorder()
	getEmptyCtx, _ := gin.CreateTestContext(getEmptyRec)
	getEmptyCtx.Request = httptest.NewRequest(http.MethodGet, "/ccswitch-import-configs", nil)

	h.GetCcSwitchImportConfigs(getEmptyCtx)
	if getEmptyRec.Code != http.StatusOK {
		t.Fatalf("empty GET status = %d, want %d; body=%s", getEmptyRec.Code, http.StatusOK, getEmptyRec.Body.String())
	}

	var emptyPayload struct {
		Items []map[string]any `json:"items"`
	}
	if err := json.Unmarshal(getEmptyRec.Body.Bytes(), &emptyPayload); err != nil {
		t.Fatalf("unmarshal empty GET response: %v", err)
	}
	if len(emptyPayload.Items) != 0 {
		t.Fatalf("empty GET returned %d item(s), want 0", len(emptyPayload.Items))
	}

	body := []byte(`[
  {
    "id": "cfg-claude",
    "client-type": "claude",
    "provider-name": "Relay Claude",
    "note": "Team preset",
    "default-model": "claude-sonnet-4-5",
    "allowed-channel-groups": ["team-a", "team-b"],
    "endpoint-path": "/anthropic",
    "usage-auto-interval": 45,
    "api-key-field": "ANTHROPIC_AUTH_TOKEN",
    "model-mappings": [
      {"role": "main", "request-model": "kimi-k2.5", "target-model": "kimi-k2.5"},
      {"role": "haiku", "request-model": "claude-3-5-haiku", "target-model": "kimi-k2.5"}
    ]
  }
]`)

	putRec := httptest.NewRecorder()
	putCtx, _ := gin.CreateTestContext(putRec)
	putCtx.Request = httptest.NewRequest(http.MethodPut, "/ccswitch-import-configs", bytes.NewReader(body))

	h.PutCcSwitchImportConfigs(putCtx)
	if putRec.Code != http.StatusOK {
		t.Fatalf("PUT status = %d, want %d; body=%s", putRec.Code, http.StatusOK, putRec.Body.String())
	}

	getRec := httptest.NewRecorder()
	getCtx, _ := gin.CreateTestContext(getRec)
	getCtx.Request = httptest.NewRequest(http.MethodGet, "/ccswitch-import-configs", nil)

	h.GetCcSwitchImportConfigs(getCtx)
	if getRec.Code != http.StatusOK {
		t.Fatalf("GET status = %d, want %d; body=%s", getRec.Code, http.StatusOK, getRec.Body.String())
	}

	var got struct {
		Items []map[string]any `json:"items"`
	}
	if err := json.Unmarshal(getRec.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal GET response: %v", err)
	}
	if len(got.Items) != 1 {
		t.Fatalf("items len = %d, want 1", len(got.Items))
	}
	if got.Items[0]["client-type"] != "claude" {
		t.Fatalf("client-type = %#v, want claude", got.Items[0]["client-type"])
	}
	if got.Items[0]["provider-name"] != "Relay Claude" {
		t.Fatalf("provider-name = %#v, want Relay Claude", got.Items[0]["provider-name"])
	}
	if got.Items[0]["api-key-field"] != "ANTHROPIC_AUTH_TOKEN" {
		t.Fatalf("api-key-field = %#v, want ANTHROPIC_AUTH_TOKEN", got.Items[0]["api-key-field"])
	}
	modelMappings, ok := got.Items[0]["model-mappings"].([]any)
	if !ok || len(modelMappings) != 2 {
		t.Fatalf("model-mappings = %#v, want 2 mappings", got.Items[0]["model-mappings"])
	}
	secondMapping, ok := modelMappings[1].(map[string]any)
	if !ok {
		t.Fatalf("second model mapping = %#v, want object", modelMappings[1])
	}
	if secondMapping["role"] != "haiku" ||
		secondMapping["request-model"] != "claude-3-5-haiku" ||
		secondMapping["target-model"] != "kimi-k2.5" {
		t.Fatalf("second model mapping = %#v, want haiku mapping", secondMapping)
	}
}

func TestPutCcSwitchImportConfigsRejectsInvalidClientType(t *testing.T) {
	gin.SetMode(gin.TestMode)
	setupPermissionProfilesTestDB(t)

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(
		http.MethodPut,
		"/ccswitch-import-configs",
		bytes.NewReader([]byte(`[{"id":"cfg-1","client-type":"unknown","provider-name":"Relay","default-model":"gpt-5.5"}]`)),
	)

	h := NewHandler(&config.Config{}, "", nil)
	h.PutCcSwitchImportConfigs(c)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d; body=%s", rec.Code, http.StatusBadRequest, rec.Body.String())
	}
}

func TestCcSwitchImportConfigsPreservesCodexMappingContextWindow(t *testing.T) {
	gin.SetMode(gin.TestMode)
	setupPermissionProfilesTestDB(t)

	body := []byte(`[
  {
    "id": "cfg-codex",
    "client-type": "codex",
    "provider-name": "Relay Codex",
    "default-model": "gpt-5.5",
    "allowed-channel-groups": ["codex"],
    "endpoint-path": "/v1",
    "usage-auto-interval": 30,
    "model-mappings": [
      {"request-model": "gpt-5.5", "target-model": "gpt-5.5", "context-window": 272000},
      {"request-model": "kimi-k2", "target-model": "kimi-k2"}
    ]
  }
]`)

	h := NewHandler(&config.Config{}, "", nil)
	putRec := httptest.NewRecorder()
	putCtx, _ := gin.CreateTestContext(putRec)
	putCtx.Request = httptest.NewRequest(http.MethodPut, "/ccswitch-import-configs", bytes.NewReader(body))

	h.PutCcSwitchImportConfigs(putCtx)
	if putRec.Code != http.StatusOK {
		t.Fatalf("PUT status = %d, want %d; body=%s", putRec.Code, http.StatusOK, putRec.Body.String())
	}

	getRec := httptest.NewRecorder()
	getCtx, _ := gin.CreateTestContext(getRec)
	getCtx.Request = httptest.NewRequest(http.MethodGet, "/ccswitch-import-configs", nil)

	h.GetCcSwitchImportConfigs(getCtx)
	if getRec.Code != http.StatusOK {
		t.Fatalf("GET status = %d, want %d; body=%s", getRec.Code, http.StatusOK, getRec.Body.String())
	}

	var got struct {
		Items []map[string]any `json:"items"`
	}
	if err := json.Unmarshal(getRec.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal GET response: %v", err)
	}
	if len(got.Items) != 1 {
		t.Fatalf("items len = %d, want 1", len(got.Items))
	}
	modelMappings, ok := got.Items[0]["model-mappings"].([]any)
	if !ok || len(modelMappings) != 2 {
		t.Fatalf("model-mappings = %#v, want 2 mappings", got.Items[0]["model-mappings"])
	}
	firstMapping, ok := modelMappings[0].(map[string]any)
	if !ok {
		t.Fatalf("first model mapping = %#v, want object", modelMappings[0])
	}
	if firstMapping["context-window"] != float64(272000) {
		t.Fatalf("first mapping context-window = %#v, want 272000", firstMapping["context-window"])
	}
	catalog, ok := got.Items[0]["codex-model-catalog"].(map[string]any)
	if !ok {
		t.Fatalf("codex-model-catalog = %#v, want object", got.Items[0]["codex-model-catalog"])
	}
	models, ok := catalog["models"].([]any)
	if !ok || len(models) != 2 {
		t.Fatalf("catalog models = %#v, want 2 models", catalog["models"])
	}
	firstModel, ok := models[0].(map[string]any)
	if !ok {
		t.Fatalf("first catalog model = %#v, want object", models[0])
	}
	if firstModel["context_window"] != float64(272000) {
		t.Fatalf("first catalog context_window = %#v, want 272000", firstModel["context_window"])
	}
}

func TestCcSwitchImportConfigsReturnsGPT56CapabilitiesAndClampsContext(t *testing.T) {
	gin.SetMode(gin.TestMode)
	setupPermissionProfilesTestDB(t)

	body := []byte(`[
  {
    "id": "cfg-gpt56",
    "client-type": "codex",
    "provider-name": "Relay GPT-5.6",
    "default-model": "gpt-5.6-sol",
    "allowed-channel-groups": ["codex"],
    "endpoint-path": "/v1",
    "usage-auto-interval": 30,
    "model-mappings": [
      {"request-model": "gpt-5.6-sol", "target-model": "gpt-5.6-sol", "context-window": 2000000},
      {"request-model": "deepseek-v4-flash", "target-model": "deepseek-chat"}
    ]
  }
]`)

	h := NewHandler(&config.Config{}, "", nil)
	putRec := httptest.NewRecorder()
	putCtx, _ := gin.CreateTestContext(putRec)
	putCtx.Request = httptest.NewRequest(http.MethodPut, "/ccswitch-import-configs", bytes.NewReader(body))
	h.PutCcSwitchImportConfigs(putCtx)
	if putRec.Code != http.StatusOK {
		t.Fatalf("PUT status = %d, want %d; body=%s", putRec.Code, http.StatusOK, putRec.Body.String())
	}

	getRec := httptest.NewRecorder()
	getCtx, _ := gin.CreateTestContext(getRec)
	getCtx.Request = httptest.NewRequest(http.MethodGet, "/ccswitch-import-configs", nil)
	h.GetCcSwitchImportConfigs(getCtx)
	if getRec.Code != http.StatusOK {
		t.Fatalf("GET status = %d, want %d; body=%s", getRec.Code, http.StatusOK, getRec.Body.String())
	}

	var got struct {
		Items []usage.CcSwitchImportConfigRow `json:"items"`
	}
	if err := json.Unmarshal(getRec.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal GET response: %v", err)
	}
	if len(got.Items) != 1 || got.Items[0].CodexModelCatalog == nil {
		t.Fatalf("items = %#v, want one generated catalog", got.Items)
	}
	if got.Items[0].ModelMappings[0].ContextWindow != 1050000 {
		t.Fatalf("persisted context override = %d, want clamped 1050000", got.Items[0].ModelMappings[0].ContextWindow)
	}
	models := got.Items[0].CodexModelCatalog.Models
	if models[0].ContextWindow != 1050000 || models[0].MaxContextWindow != 1050000 {
		t.Fatalf("GPT-5.6 catalog context = (%d, %d), want (1050000, 1050000)", models[0].ContextWindow, models[0].MaxContextWindow)
	}
	gotEfforts := make([]string, 0, len(models[0].SupportedReasoningLevels))
	for _, level := range models[0].SupportedReasoningLevels {
		gotEfforts = append(gotEfforts, level.Effort)
	}
	wantEfforts := []string{"low", "medium", "high", "xhigh", "max", "ultra"}
	if !reflect.DeepEqual(gotEfforts, wantEfforts) {
		t.Fatalf("GPT-5.6 reasoning levels = %#v, want %#v", gotEfforts, wantEfforts)
	}
	if len(models[1].SupportedReasoningLevels) != 4 {
		t.Fatalf("unknown model reasoning levels = %#v, want conservative four-level fallback", models[1].SupportedReasoningLevels)
	}
}

func TestCcSwitchImportConfigsAcceptAdditiveDesktopApps(t *testing.T) {
	gin.SetMode(gin.TestMode)
	setupPermissionProfilesTestDB(t)

	body := []byte(`[
  {"id": "cfg-opencode", "client-type": "opencode", "provider-name": "Relay OpenCode", "default-model": "kimi-k2", "allowed-channel-groups": ["team-a"], "route-path": "/team-a/cs_opencode1", "endpoint-path": "/v1",
   "model-mappings": [{"request-model": "kimi-k2", "target-model": "kimi-k2"}]},
  {"id": "cfg-openclaw", "client-type": "OpenClaw", "provider-name": "Relay OpenClaw", "default-model": "kimi-k2", "allowed-channel-groups": ["team-a"], "route-path": "/team-a/cs_openclaw1", "endpoint-path": "/v1"},
  {"id": "cfg-hermes", "client-type": " hermes ", "provider-name": "Relay Hermes", "default-model": "kimi-k2", "allowed-channel-groups": ["team-a"], "route-path": "/team-a/cs_hermes001", "endpoint-path": "/v1"}
]`)

	h := NewHandler(&config.Config{}, "", nil)
	putRec := httptest.NewRecorder()
	putCtx, _ := gin.CreateTestContext(putRec)
	putCtx.Request = httptest.NewRequest(http.MethodPut, "/ccswitch-import-configs", bytes.NewReader(body))
	h.PutCcSwitchImportConfigs(putCtx)
	if putRec.Code != http.StatusOK {
		t.Fatalf("PUT status = %d, want %d; body=%s", putRec.Code, http.StatusOK, putRec.Body.String())
	}

	getRec := httptest.NewRecorder()
	getCtx, _ := gin.CreateTestContext(getRec)
	getCtx.Request = httptest.NewRequest(http.MethodGet, "/ccswitch-import-configs", nil)
	h.GetCcSwitchImportConfigs(getCtx)
	if getRec.Code != http.StatusOK {
		t.Fatalf("GET status = %d, want %d; body=%s", getRec.Code, http.StatusOK, getRec.Body.String())
	}

	var got struct {
		Items       []usage.CcSwitchImportConfigRow `json:"items"`
		ClientTypes []string                        `json:"client-types"`
	}
	if err := json.Unmarshal(getRec.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal GET response: %v", err)
	}
	gotTypes := make([]string, 0, len(got.Items))
	for _, item := range got.Items {
		gotTypes = append(gotTypes, item.ClientType)
		if item.CodexModelCatalog != nil {
			t.Fatalf("%s got a Codex model catalog; only codex presets carry one", item.ClientType)
		}
		if item.APIKeyField != "" {
			t.Fatalf("%s api-key-field = %q, want empty for a non-Claude preset", item.ClientType, item.APIKeyField)
		}
	}
	// Rows saved in one PUT share created-at and come back ordered by id.
	sort.Strings(gotTypes)
	if want := []string{"hermes", "openclaw", "opencode"}; !reflect.DeepEqual(gotTypes, want) {
		t.Fatalf("stored client types = %#v, want %#v", gotTypes, want)
	}
	wantCapabilities := []string{"claude", "codex", "gemini", "opencode", "openclaw", "hermes"}
	if !reflect.DeepEqual(got.ClientTypes, wantCapabilities) {
		t.Fatalf("client-types = %#v, want %#v", got.ClientTypes, wantCapabilities)
	}
}

func TestGetCcSwitchImportConfigsReportsClientTypesWhenEmpty(t *testing.T) {
	gin.SetMode(gin.TestMode)
	setupPermissionProfilesTestDB(t)

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodGet, "/ccswitch-import-configs", nil)
	NewHandler(&config.Config{}, "", nil).GetCcSwitchImportConfigs(c)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET status = %d, want %d; body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}

	var got struct {
		ClientTypes []string `json:"client-types"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal GET response: %v", err)
	}
	if !reflect.DeepEqual(got.ClientTypes, usage.CcSwitchClientTypes()) {
		t.Fatalf("client-types = %#v, want %#v", got.ClientTypes, usage.CcSwitchClientTypes())
	}
}

func TestPutCcSwitchImportConfigsRejectionNamesEveryClientType(t *testing.T) {
	gin.SetMode(gin.TestMode)
	setupPermissionProfilesTestDB(t)

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(
		http.MethodPut,
		"/ccswitch-import-configs",
		bytes.NewReader([]byte(`[{"id":"cfg-1","client-type":"grokbuild","provider-name":"Relay","default-model":"grok-4.5"}]`)),
	)
	NewHandler(&config.Config{}, "", nil).PutCcSwitchImportConfigs(c)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d; body=%s", rec.Code, http.StatusBadRequest, rec.Body.String())
	}
	var body struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal error body: %v", err)
	}
	if want := "client-type must be one of claude, codex, gemini, opencode, openclaw, hermes"; body.Error != want {
		t.Fatalf("error = %q, want %q", body.Error, want)
	}
}

func TestCcSwitchImportConfigsStoreTheExplicitCodexContextWindow(t *testing.T) {
	gin.SetMode(gin.TestMode)
	setupPermissionProfilesTestDB(t)

	// The mapping keeps the 128000 placeholder earlier panels wrote everywhere;
	// the preset's own field is the only context window that is intent.
	body := []byte(`[
  {"id": "cfg-codex", "client-type": "codex", "provider-name": "Relay Codex", "default-model": "gpt-5.5",
   "allowed-channel-groups": ["codex"], "codex-context-window": 200000,
   "model-mappings": [{"request-model": "gpt-5.5", "target-model": "gpt-5.5", "context-window": 128000}]},
  {"id": "cfg-claude", "client-type": "claude", "provider-name": "Relay Claude", "default-model": "claude-sonnet-4-5",
   "allowed-channel-groups": ["claude"], "codex-context-window": 200000}
]`)
	h := NewHandler(&config.Config{}, "", nil)
	putRec := httptest.NewRecorder()
	putCtx, _ := gin.CreateTestContext(putRec)
	putCtx.Request = httptest.NewRequest(http.MethodPut, "/ccswitch-import-configs", bytes.NewReader(body))
	h.PutCcSwitchImportConfigs(putCtx)
	if putRec.Code != http.StatusOK {
		t.Fatalf("PUT status = %d; body=%s", putRec.Code, putRec.Body.String())
	}

	getRec := httptest.NewRecorder()
	getCtx, _ := gin.CreateTestContext(getRec)
	getCtx.Request = httptest.NewRequest(http.MethodGet, "/ccswitch-import-configs", nil)
	h.GetCcSwitchImportConfigs(getCtx)
	var got struct {
		Items        []map[string]any `json:"items"`
		Capabilities map[string]bool  `json:"capabilities"`
	}
	if err := json.Unmarshal(getRec.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal GET response: %v", err)
	}
	if !got.Capabilities["codex-context-window"] {
		t.Fatalf("capabilities = %#v, want codex-context-window", got.Capabilities)
	}
	byID := map[string]map[string]any{}
	for _, item := range got.Items {
		byID[item["id"].(string)] = item
	}
	if got := byID["cfg-codex"]["codex-context-window"]; got != float64(200000) {
		t.Fatalf("codex preset codex-context-window = %#v, want 200000", got)
	}
	// Only Codex presets carry it, but every row reports the field so the panel
	// can tell this backend from one that would drop it.
	value, present := byID["cfg-claude"]["codex-context-window"]
	if !present || value != float64(0) {
		t.Fatalf("claude preset codex-context-window = %#v (present %v), want 0", value, present)
	}
}
