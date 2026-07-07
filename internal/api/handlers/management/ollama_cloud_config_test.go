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

func TestOllamaCloudKeyManagementPutGetPatchDelete(t *testing.T) {
	gin.SetMode(gin.TestMode)
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(configPath, []byte("{}\n"), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	h := &Handler{cfg: &config.Config{}, configFilePath: configPath}

	putBody := []byte(`[{"api-key":" ollama-key ","name":" primary ","prefix":" team ","base-url":" https://ollama.com/ ","headers":{"X-Test":" yes "},"models":[{"name":" gpt-oss:120b "}],"excluded-models":[" gpt-oss:20b "],"vision-fallback-model":" gpt-oss:120b "}]`)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPut, "/v0/management/ollama-cloud-api-key", bytes.NewReader(putBody))
	h.ProviderKeys().PutOllamaCloudKeys(c)
	if w.Code != http.StatusOK {
		t.Fatalf("PUT status = %d body=%s", w.Code, w.Body.String())
	}
	if len(h.cfg.OllamaCloudKey) != 1 || h.cfg.OllamaCloudKey[0].APIKey != "ollama-key" || h.cfg.OllamaCloudKey[0].Prefix != "team" || h.cfg.OllamaCloudKey[0].BaseURL != config.DefaultOllamaCloudBaseURL {
		t.Fatalf("OllamaCloudKey after PUT = %+v", h.cfg.OllamaCloudKey)
	}
	if len(h.cfg.OllamaCloudKey[0].Models) != 1 || h.cfg.OllamaCloudKey[0].Models[0].Name != "gpt-oss:120b" {
		t.Fatalf("OllamaCloudKey models after PUT = %+v, want sanitized model", h.cfg.OllamaCloudKey[0].Models)
	}
	if len(h.cfg.OllamaCloudKey[0].ExcludedModels) != 1 || h.cfg.OllamaCloudKey[0].ExcludedModels[0] != "gpt-oss:20b" {
		t.Fatalf("OllamaCloudKey excluded models after PUT = %+v", h.cfg.OllamaCloudKey[0].ExcludedModels)
	}
	if h.cfg.OllamaCloudKey[0].VisionFallbackModel != "gpt-oss:120b" {
		t.Fatalf("OllamaCloudKey vision fallback after PUT = %+v", h.cfg.OllamaCloudKey[0])
	}

	patchBody := []byte(`{"index":0,"value":{"name":"secondary","base-url":"","models":[{"name":"gpt-oss:20b"}],"excluded-models":[" gpt-oss:120b ","*"],"vision-fallback-model":" gpt-oss:20b "}}`)
	w = httptest.NewRecorder()
	c, _ = gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPatch, "/v0/management/ollama-cloud-api-key", bytes.NewReader(patchBody))
	h.ProviderKeys().PatchOllamaCloudKey(c)
	if w.Code != http.StatusOK {
		t.Fatalf("PATCH status = %d body=%s", w.Code, w.Body.String())
	}
	if h.cfg.OllamaCloudKey[0].Name != "secondary" || h.cfg.OllamaCloudKey[0].BaseURL != config.DefaultOllamaCloudBaseURL || len(h.cfg.OllamaCloudKey[0].Models) != 1 || h.cfg.OllamaCloudKey[0].Models[0].Name != "gpt-oss:20b" || len(h.cfg.OllamaCloudKey[0].ExcludedModels) != 2 || h.cfg.OllamaCloudKey[0].ExcludedModels[0] != "gpt-oss:120b" || h.cfg.OllamaCloudKey[0].ExcludedModels[1] != "*" || h.cfg.OllamaCloudKey[0].VisionFallbackModel != "gpt-oss:20b" {
		t.Fatalf("OllamaCloudKey after PATCH = %+v", h.cfg.OllamaCloudKey[0])
	}

	w = httptest.NewRecorder()
	c, _ = gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/v0/management/ollama-cloud-api-key", nil)
	h.ProviderKeys().GetOllamaCloudKeys(c)
	if w.Code != http.StatusOK {
		t.Fatalf("GET status = %d body=%s", w.Code, w.Body.String())
	}
	var getBody struct {
		Items []config.OllamaCloudKey `json:"ollama-cloud-api-key"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &getBody); err != nil {
		t.Fatalf("decode GET body: %v", err)
	}
	if len(getBody.Items) != 1 || getBody.Items[0].Name != "secondary" || getBody.Items[0].BaseURL != config.DefaultOllamaCloudBaseURL || len(getBody.Items[0].Models) != 1 || getBody.Items[0].Models[0].Name != "gpt-oss:20b" || len(getBody.Items[0].ExcludedModels) != 2 || getBody.Items[0].ExcludedModels[0] != "gpt-oss:120b" || getBody.Items[0].ExcludedModels[1] != "*" || getBody.Items[0].VisionFallbackModel != "gpt-oss:20b" {
		t.Fatalf("GET body = %+v", getBody)
	}

	w = httptest.NewRecorder()
	c, _ = gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodDelete, "/v0/management/ollama-cloud-api-key?name=secondary", nil)
	h.ProviderKeys().DeleteOllamaCloudKey(c)
	if w.Code != http.StatusOK {
		t.Fatalf("DELETE status = %d body=%s", w.Code, w.Body.String())
	}
	if len(h.cfg.OllamaCloudKey) != 0 {
		t.Fatalf("OllamaCloudKey after DELETE = %+v", h.cfg.OllamaCloudKey)
	}
}
