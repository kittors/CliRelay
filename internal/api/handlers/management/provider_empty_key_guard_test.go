package management

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
)

func TestExtendedProviderManagementRejectsEmptyAPIKeyPayloads(t *testing.T) {
	gin.SetMode(gin.TestMode)

	t.Run("opencode go", func(t *testing.T) {
		h := newProviderKeysTestHandler(t, &config.Config{
			OpenCodeGoKey: []config.OpenCodeGoKey{{APIKey: "existing", Name: "go"}},
		})

		w := performProviderKeysRequest(http.MethodPut, "/v0/management/opencode-go-api-key", `[{"name":"empty"}]`, h.ProviderKeys().PutOpenCodeGoKeys)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("PUT status = %d body=%s, want 400", w.Code, w.Body.String())
		}
		if got := h.cfg.OpenCodeGoKey; len(got) != 1 || got[0].APIKey != "existing" {
			t.Fatalf("OpenCodeGoKey after rejected PUT = %+v, want unchanged", got)
		}

		w = performProviderKeysRequest(http.MethodPatch, "/v0/management/opencode-go-api-key", `{"index":0,"value":{"api-key":" "}}`, h.ProviderKeys().PatchOpenCodeGoKey)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("PATCH status = %d body=%s, want 400", w.Code, w.Body.String())
		}
		if got := h.cfg.OpenCodeGoKey; len(got) != 1 || got[0].APIKey != "existing" {
			t.Fatalf("OpenCodeGoKey after rejected PATCH = %+v, want unchanged", got)
		}
	})

	t.Run("cline", func(t *testing.T) {
		h := newProviderKeysTestHandler(t, &config.Config{
			ClineKey: []config.ClineKey{{APIKey: "existing", Name: "cline"}},
		})

		w := performProviderKeysRequest(http.MethodPut, "/v0/management/cline-api-key", `[{"name":"empty"}]`, h.ProviderKeys().PutClineKeys)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("PUT status = %d body=%s, want 400", w.Code, w.Body.String())
		}
		if got := h.cfg.ClineKey; len(got) != 1 || got[0].APIKey != "existing" {
			t.Fatalf("ClineKey after rejected PUT = %+v, want unchanged", got)
		}

		w = performProviderKeysRequest(http.MethodPatch, "/v0/management/cline-api-key", `{"index":0,"value":{"api-key":" "}}`, h.ProviderKeys().PatchClineKey)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("PATCH status = %d body=%s, want 400", w.Code, w.Body.String())
		}
		if got := h.cfg.ClineKey; len(got) != 1 || got[0].APIKey != "existing" {
			t.Fatalf("ClineKey after rejected PATCH = %+v, want unchanged", got)
		}
	})

	t.Run("ollama cloud", func(t *testing.T) {
		h := newProviderKeysTestHandler(t, &config.Config{
			OllamaCloudKey: []config.OllamaCloudKey{{APIKey: "existing", Name: "ollama"}},
		})

		w := performProviderKeysRequest(http.MethodPut, "/v0/management/ollama-cloud-api-key", `[{"name":"empty"}]`, h.ProviderKeys().PutOllamaCloudKeys)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("PUT status = %d body=%s, want 400", w.Code, w.Body.String())
		}
		if got := h.cfg.OllamaCloudKey; len(got) != 1 || got[0].APIKey != "existing" {
			t.Fatalf("OllamaCloudKey after rejected PUT = %+v, want unchanged", got)
		}

		w = performProviderKeysRequest(http.MethodPatch, "/v0/management/ollama-cloud-api-key", `{"index":0,"value":{"api-key":" "}}`, h.ProviderKeys().PatchOllamaCloudKey)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("PATCH status = %d body=%s, want 400", w.Code, w.Body.String())
		}
		if got := h.cfg.OllamaCloudKey; len(got) != 1 || got[0].APIKey != "existing" {
			t.Fatalf("OllamaCloudKey after rejected PATCH = %+v, want unchanged", got)
		}
	})
}

func newProviderKeysTestHandler(t *testing.T, cfg *config.Config) *Handler {
	t.Helper()
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(configPath, []byte("{}\n"), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return &Handler{cfg: cfg, configFilePath: configPath}
}

func performProviderKeysRequest(method string, path string, body string, handler gin.HandlerFunc) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(method, path, bytes.NewReader([]byte(body)))
	handler(c)
	return w
}
