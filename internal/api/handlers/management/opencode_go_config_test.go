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

	putBody := []byte(`[{"api-key":" go-key ","name":" primary ","prefix":" team ","headers":{"X-Test":" yes "}}]`)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPut, "/v0/management/opencode-go-api-key", bytes.NewReader(putBody))
	h.PutOpenCodeGoKeys(c)
	if w.Code != http.StatusOK {
		t.Fatalf("PUT status = %d body=%s", w.Code, w.Body.String())
	}
	if len(h.cfg.OpenCodeGoKey) != 1 || h.cfg.OpenCodeGoKey[0].APIKey != "go-key" || h.cfg.OpenCodeGoKey[0].Prefix != "team" {
		t.Fatalf("OpenCodeGoKey after PUT = %+v", h.cfg.OpenCodeGoKey)
	}

	patchBody := []byte(`{"index":0,"value":{"name":"secondary","excluded-models":[" minimax-m2.5 "]}}`)
	w = httptest.NewRecorder()
	c, _ = gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPatch, "/v0/management/opencode-go-api-key", bytes.NewReader(patchBody))
	h.PatchOpenCodeGoKey(c)
	if w.Code != http.StatusOK {
		t.Fatalf("PATCH status = %d body=%s", w.Code, w.Body.String())
	}
	if h.cfg.OpenCodeGoKey[0].Name != "secondary" || h.cfg.OpenCodeGoKey[0].ExcludedModels[0] != "minimax-m2.5" {
		t.Fatalf("OpenCodeGoKey after PATCH = %+v", h.cfg.OpenCodeGoKey[0])
	}

	w = httptest.NewRecorder()
	c, _ = gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/v0/management/opencode-go-api-key", nil)
	h.GetOpenCodeGoKeys(c)
	if w.Code != http.StatusOK {
		t.Fatalf("GET status = %d body=%s", w.Code, w.Body.String())
	}
	var getBody struct {
		Items []config.OpenCodeGoKey `json:"opencode-go-api-key"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &getBody); err != nil {
		t.Fatalf("decode GET body: %v", err)
	}
	if len(getBody.Items) != 1 || getBody.Items[0].Name != "secondary" {
		t.Fatalf("GET body = %+v", getBody)
	}

	w = httptest.NewRecorder()
	c, _ = gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodDelete, "/v0/management/opencode-go-api-key?name=secondary", nil)
	h.DeleteOpenCodeGoKey(c)
	if w.Code != http.StatusOK {
		t.Fatalf("DELETE status = %d body=%s", w.Code, w.Body.String())
	}
	if len(h.cfg.OpenCodeGoKey) != 0 {
		t.Fatalf("OpenCodeGoKey after DELETE = %+v", h.cfg.OpenCodeGoKey)
	}
}
