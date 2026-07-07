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

func TestClineKeyManagementPutGetPatchDelete(t *testing.T) {
	gin.SetMode(gin.TestMode)
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(configPath, []byte("{}\n"), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	h := &Handler{cfg: &config.Config{}, configFilePath: configPath}

	putBody := []byte(`[{"api-key":" cline-key ","name":" primary ","prefix":" team ","base-url":" https://api.cline.bot/api/v1/ ","headers":{"X-Test":" yes "},"models":[{"name":" cline-pass/glm-5.2 "}],"excluded-models":[" cline-pass/minimax-m3 "],"vision-fallback-model":" cline-pass/mimo-v2.5-pro "}]`)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPut, "/v0/management/cline-api-key", bytes.NewReader(putBody))
	h.ProviderKeys().PutClineKeys(c)
	if w.Code != http.StatusOK {
		t.Fatalf("PUT status = %d body=%s", w.Code, w.Body.String())
	}
	if len(h.cfg.ClineKey) != 1 || h.cfg.ClineKey[0].APIKey != "cline-key" || h.cfg.ClineKey[0].Prefix != "team" || h.cfg.ClineKey[0].BaseURL != config.DefaultClineBaseURL {
		t.Fatalf("ClineKey after PUT = %+v", h.cfg.ClineKey)
	}
	if len(h.cfg.ClineKey[0].Models) != 1 || h.cfg.ClineKey[0].Models[0].Name != "cline-pass/glm-5.2" {
		t.Fatalf("ClineKey models after PUT = %+v, want sanitized model", h.cfg.ClineKey[0].Models)
	}
	if len(h.cfg.ClineKey[0].ExcludedModels) != 0 {
		t.Fatalf("ClineKey excluded models after PUT = %+v", h.cfg.ClineKey[0].ExcludedModels)
	}
	if h.cfg.ClineKey[0].VisionFallbackModel != "cline-pass/mimo-v2.5-pro" {
		t.Fatalf("ClineKey vision fallback after PUT = %+v", h.cfg.ClineKey[0])
	}

	patchBody := []byte(`{"index":0,"value":{"name":"secondary","base-url":"","models":[{"name":"cline-pass/qwen3.7-max"}],"excluded-models":[" cline-pass/deepseek-v4-flash ","*"],"vision-fallback-model":" cline-pass/qwen3.7-vl "}}`)
	w = httptest.NewRecorder()
	c, _ = gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPatch, "/v0/management/cline-api-key", bytes.NewReader(patchBody))
	h.ProviderKeys().PatchClineKey(c)
	if w.Code != http.StatusOK {
		t.Fatalf("PATCH status = %d body=%s", w.Code, w.Body.String())
	}
	if h.cfg.ClineKey[0].Name != "secondary" || h.cfg.ClineKey[0].BaseURL != config.DefaultClineBaseURL || len(h.cfg.ClineKey[0].Models) != 1 || h.cfg.ClineKey[0].Models[0].Name != "cline-pass/qwen3.7-max" || len(h.cfg.ClineKey[0].ExcludedModels) != 1 || h.cfg.ClineKey[0].ExcludedModels[0] != "*" || h.cfg.ClineKey[0].VisionFallbackModel != "cline-pass/qwen3.7-vl" {
		t.Fatalf("ClineKey after PATCH = %+v", h.cfg.ClineKey[0])
	}

	w = httptest.NewRecorder()
	c, _ = gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/v0/management/cline-api-key", nil)
	h.ProviderKeys().GetClineKeys(c)
	if w.Code != http.StatusOK {
		t.Fatalf("GET status = %d body=%s", w.Code, w.Body.String())
	}
	var getBody struct {
		Items []config.ClineKey `json:"cline-api-key"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &getBody); err != nil {
		t.Fatalf("decode GET body: %v", err)
	}
	if len(getBody.Items) != 1 || getBody.Items[0].Name != "secondary" || getBody.Items[0].BaseURL != config.DefaultClineBaseURL || len(getBody.Items[0].Models) != 1 || getBody.Items[0].Models[0].Name != "cline-pass/qwen3.7-max" || len(getBody.Items[0].ExcludedModels) != 1 || getBody.Items[0].ExcludedModels[0] != "*" || getBody.Items[0].VisionFallbackModel != "cline-pass/qwen3.7-vl" {
		t.Fatalf("GET body = %+v", getBody)
	}

	w = httptest.NewRecorder()
	c, _ = gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodDelete, "/v0/management/cline-api-key?name=secondary", nil)
	h.ProviderKeys().DeleteClineKey(c)
	if w.Code != http.StatusOK {
		t.Fatalf("DELETE status = %d body=%s", w.Code, w.Body.String())
	}
	if len(h.cfg.ClineKey) != 0 {
		t.Fatalf("ClineKey after DELETE = %+v", h.cfg.ClineKey)
	}
}

func TestClineKeyManagementRejectsNonClinePassModels(t *testing.T) {
	gin.SetMode(gin.TestMode)
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(configPath, []byte("{}\n"), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	h := &Handler{
		cfg:            &config.Config{ClineKey: []config.ClineKey{{APIKey: "existing"}}},
		configFilePath: configPath,
	}

	putBody := []byte(`[{"api-key":"cline-key","models":[{"name":"glm-5.2"}],"excluded-models":["cline-pass/minimax-m3","*"],"vision-fallback-model":"cline-pass/mimo-v2.5-pro"}]`)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPut, "/v0/management/cline-api-key", bytes.NewReader(putBody))
	h.ProviderKeys().PutClineKeys(c)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("PUT status = %d body=%s, want 400", w.Code, w.Body.String())
	}
	if got := h.cfg.ClineKey; len(got) != 1 || got[0].APIKey != "existing" {
		t.Fatalf("ClineKey after rejected PUT = %+v, want unchanged existing entry", got)
	}
}
