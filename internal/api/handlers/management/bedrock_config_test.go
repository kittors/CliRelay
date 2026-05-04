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

func performBedrockConfigRequest(method string, path string, body []byte, handler gin.HandlerFunc) *httptest.ResponseRecorder {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	if body == nil {
		c.Request = httptest.NewRequest(method, path, nil)
	} else {
		c.Request = httptest.NewRequest(method, path, bytes.NewReader(body))
	}
	handler(c)
	return rec
}

func TestBedrockKeyManagementHandlers(t *testing.T) {
	cfg := &config.Config{}
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(configPath, []byte("port: 8317\n"), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	h := NewHandler(cfg, configPath, nil)
	t.Cleanup(h.Close)

	payload := []byte(`[
		{
			"name":"aws api",
			"auth-mode":"api-key",
			"api-key":"br-key",
			"region":"eu-west-1",
			"force-global":true,
			"models":[{"name":"claude-sonnet-4-5","alias":"aws-sonnet"}]
		},
		{
			"name":"aws sigv4",
			"auth-mode":"sigv4",
			"access-key-id":"AKIA",
			"secret-access-key":"SECRET",
			"region":"us-east-1"
		}
	]`)

	putRec := performBedrockConfigRequest(http.MethodPut, "/bedrock-api-key", payload, h.PutBedrockKeys)
	if putRec.Code != http.StatusOK {
		t.Fatalf("PUT status = %d body=%s", putRec.Code, putRec.Body.String())
	}
	if len(cfg.BedrockKey) != 2 {
		t.Fatalf("expected 2 bedrock keys, got %d", len(cfg.BedrockKey))
	}

	patchPayload := []byte(`{"index":1,"value":{"name":"renamed sigv4","region":"ap-southeast-2","session-token":"SESSION"}}`)
	patchRec := performBedrockConfigRequest(http.MethodPatch, "/bedrock-api-key", patchPayload, h.PatchBedrockKey)
	if patchRec.Code != http.StatusOK {
		t.Fatalf("PATCH status = %d body=%s", patchRec.Code, patchRec.Body.String())
	}
	if cfg.BedrockKey[1].Name != "renamed sigv4" || cfg.BedrockKey[1].Region != "ap-southeast-2" || cfg.BedrockKey[1].SessionToken != "SESSION" {
		t.Fatalf("unexpected patched key: %+v", cfg.BedrockKey[1])
	}

	getRec := performBedrockConfigRequest(http.MethodGet, "/bedrock-api-key", nil, h.GetBedrockKeys)
	if getRec.Code != http.StatusOK {
		t.Fatalf("GET status = %d body=%s", getRec.Code, getRec.Body.String())
	}
	var got map[string][]config.BedrockKey
	if err := json.Unmarshal(getRec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode GET body: %v", err)
	}
	if len(got["bedrock-api-key"]) != 2 {
		t.Fatalf("expected 2 keys from GET, got %+v", got)
	}

	deleteRec := performBedrockConfigRequest(http.MethodDelete, "/bedrock-api-key?index=0", nil, h.DeleteBedrockKey)
	if deleteRec.Code != http.StatusOK {
		t.Fatalf("DELETE status = %d body=%s", deleteRec.Code, deleteRec.Body.String())
	}
	if len(cfg.BedrockKey) != 1 || cfg.BedrockKey[0].Name != "renamed sigv4" {
		t.Fatalf("unexpected keys after delete: %+v", cfg.BedrockKey)
	}
}
