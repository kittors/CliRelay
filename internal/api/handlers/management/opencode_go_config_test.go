package management

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
)

func TestOpenCodeGoKeyManagementPutGetPatchDelete(t *testing.T) {
	gin.SetMode(gin.TestMode)
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(configPath, []byte("{}\n"), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	h := &Handler{cfg: &config.Config{}, configFilePath: configPath}

	putBody := []byte(`[{"api-key":" go-key ","name":" primary ","prefix":" team ","headers":{"X-Test":" yes "},"models":[{"name":"qwen3.5-plus"}],"excluded-models":["minimax-m2.5","*"],"vision-fallback-model":" qwen3.5-plus ","workspace-id":" wrk_123 ","auth-cookie":" auth-token "}]`)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPut, "/v0/management/opencode-go-api-key", bytes.NewReader(putBody))
	h.ProviderKeys().PutOpenCodeGoKeys(c)
	if w.Code != http.StatusOK {
		t.Fatalf("PUT status = %d body=%s", w.Code, w.Body.String())
	}
	if len(h.cfg.OpenCodeGoKey) != 1 || h.cfg.OpenCodeGoKey[0].APIKey != "go-key" || h.cfg.OpenCodeGoKey[0].Prefix != "team" || h.cfg.OpenCodeGoKey[0].VisionFallbackModel != "qwen3.5-plus" || len(h.cfg.OpenCodeGoKey[0].Models) != 1 || h.cfg.OpenCodeGoKey[0].Models[0].Name != "qwen3.5-plus" || len(h.cfg.OpenCodeGoKey[0].ExcludedModels) != 1 || h.cfg.OpenCodeGoKey[0].ExcludedModels[0] != "*" || h.cfg.OpenCodeGoKey[0].WorkspaceID != "wrk_123" || h.cfg.OpenCodeGoKey[0].AuthCookie != "auth-token" {
		t.Fatalf("OpenCodeGoKey after PUT = %+v", h.cfg.OpenCodeGoKey)
	}

	patchBody := []byte(`{"index":0,"value":{"name":"secondary","models":[{"name":"qwen3.7-max"}],"excluded-models":[" minimax-m2.5 "],"vision-fallback-model":" qwen3.6-plus ","workspace-id":" https://opencode.ai/workspace/wrk_456/go ","auth-cookie":" auth-next "}}`)
	w = httptest.NewRecorder()
	c, _ = gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPatch, "/v0/management/opencode-go-api-key", bytes.NewReader(patchBody))
	h.ProviderKeys().PatchOpenCodeGoKey(c)
	if w.Code != http.StatusOK {
		t.Fatalf("PATCH status = %d body=%s", w.Code, w.Body.String())
	}
	if h.cfg.OpenCodeGoKey[0].Name != "secondary" || len(h.cfg.OpenCodeGoKey[0].ExcludedModels) != 0 || h.cfg.OpenCodeGoKey[0].VisionFallbackModel != "qwen3.6-plus" || len(h.cfg.OpenCodeGoKey[0].Models) != 1 || h.cfg.OpenCodeGoKey[0].Models[0].Name != "qwen3.7-max" || h.cfg.OpenCodeGoKey[0].WorkspaceID != "wrk_456" || h.cfg.OpenCodeGoKey[0].AuthCookie != "auth-next" {
		t.Fatalf("OpenCodeGoKey after PATCH = %+v", h.cfg.OpenCodeGoKey[0])
	}

	w = httptest.NewRecorder()
	c, _ = gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/v0/management/opencode-go-api-key", nil)
	h.ProviderKeys().GetOpenCodeGoKeys(c)
	if w.Code != http.StatusOK {
		t.Fatalf("GET status = %d body=%s", w.Code, w.Body.String())
	}
	var getBody struct {
		Items []config.OpenCodeGoKey `json:"opencode-go-api-key"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &getBody); err != nil {
		t.Fatalf("decode GET body: %v", err)
	}
	if len(getBody.Items) != 1 || getBody.Items[0].Name != "secondary" || getBody.Items[0].VisionFallbackModel != "qwen3.6-plus" || len(getBody.Items[0].Models) != 1 || getBody.Items[0].Models[0].Name != "qwen3.7-max" || len(getBody.Items[0].ExcludedModels) != 0 || getBody.Items[0].WorkspaceID != "wrk_456" || getBody.Items[0].AuthCookie != "auth-next" {
		t.Fatalf("GET body = %+v", getBody)
	}

	w = httptest.NewRecorder()
	c, _ = gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodDelete, "/v0/management/opencode-go-api-key?name=secondary", nil)
	h.ProviderKeys().DeleteOpenCodeGoKey(c)
	if w.Code != http.StatusOK {
		t.Fatalf("DELETE status = %d body=%s", w.Code, w.Body.String())
	}
	if len(h.cfg.OpenCodeGoKey) != 0 {
		t.Fatalf("OpenCodeGoKey after DELETE = %+v", h.cfg.OpenCodeGoKey)
	}
}

func TestOpenCodeGoKeyManagementKeepsPerKeyModels(t *testing.T) {
	gin.SetMode(gin.TestMode)
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(configPath, []byte("{}\n"), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	h := &Handler{
		cfg:            &config.Config{OpenCodeGoKey: []config.OpenCodeGoKey{{APIKey: "existing"}}},
		configFilePath: configPath,
	}

	putBody := []byte(`[{"api-key":"go-key","models":[{"name":"glm-5.2"}],"excluded-models":["minimax-m2.5","*"],"vision-fallback-model":"qwen3.5-plus"}]`)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPut, "/v0/management/opencode-go-api-key", bytes.NewReader(putBody))
	h.ProviderKeys().PutOpenCodeGoKeys(c)
	if w.Code != http.StatusOK {
		t.Fatalf("PUT status = %d body=%s", w.Code, w.Body.String())
	}
	if got := h.cfg.OpenCodeGoKey; len(got) != 1 || got[0].APIKey != "go-key" || len(got[0].Models) != 1 || got[0].Models[0].Name != "glm-5.2" || got[0].VisionFallbackModel != "qwen3.5-plus" || len(got[0].ExcludedModels) != 1 || got[0].ExcludedModels[0] != "*" {
		t.Fatalf("OpenCodeGoKey after PUT = %+v, want sanitized entry", got)
	}
}
