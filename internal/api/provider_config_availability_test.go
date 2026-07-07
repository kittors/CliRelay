package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"golang.org/x/crypto/bcrypt"

	proxyconfig "github.com/router-for-me/CLIProxyAPI/v6/internal/config"
)

func TestManagementProviderConfigSaveUpdatesConfiguredAvailability(t *testing.T) {
	const managementKey = "management-test-key"
	hashed, err := bcrypt.GenerateFromPassword([]byte(managementKey), bcrypt.MinCost)
	if err != nil {
		t.Fatalf("hash management key: %v", err)
	}

	server := newTestServerWithConfig(t, func(cfg *proxyconfig.Config) {
		cfg.RemoteManagement.SecretKey = string(hashed)
		cfg.RemoteManagement.AllowRemote = true
	})
	if err := os.WriteFile(server.configFilePath, []byte("{}\n"), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	putProviderConfig(t, server, managementKey, "/v0/management/opencode-go-api-key", `[{
		"api-key":"go-key",
		"name":"OpenCode Go",
		"models":[{"name":"glm-5.2"}]
	}]`)
	assertAvailabilityHasModel(t, server, managementKey, "glm-5.2", "opencode-go", "opencode-go · OpenCode Go")

	putProviderConfig(t, server, managementKey, "/v0/management/cline-api-key", `[{
		"api-key":"cline-key",
		"name":"ClinePass",
		"models":[{"name":"cline-pass/mimo-v2.5-pro","alias":"mimo-v2.5-pro"}]
	}]`)
	assertAvailabilityHasModel(t, server, managementKey, "mimo-v2.5-pro", "cline", "cline · ClinePass")
	assertAvailabilityMissingModel(t, server, managementKey, "cline-pass/mimo-v2.5-pro")

	putProviderConfig(t, server, managementKey, "/v0/management/ollama-cloud-api-key", `[{
		"api-key":"ollama-key",
		"name":"Ollama Cloud",
		"models":[{"name":"gpt-oss:120b"}]
	}]`)
	assertAvailabilityHasModel(t, server, managementKey, "gpt-oss:120b", "ollama-cloud", "ollama-cloud · Ollama Cloud")

	patchProviderConfig(t, server, managementKey, "/v0/management/ollama-cloud-api-key", `{
		"index":0,
		"value":{"excluded-models":["gpt-oss:120b"]}
	}`)
	assertAvailabilityMissingModel(t, server, managementKey, "gpt-oss:120b")
}

func putProviderConfig(t *testing.T, server *Server, managementKey, path, body string) {
	t.Helper()
	managementRequest(t, server, managementKey, http.MethodPut, path, body, http.StatusOK)
}

func patchProviderConfig(t *testing.T, server *Server, managementKey, path, body string) {
	t.Helper()
	managementRequest(t, server, managementKey, http.MethodPatch, path, body, http.StatusOK)
}

func managementRequest(t *testing.T, server *Server, managementKey, method, path, body string, wantStatus int) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, bytes.NewReader([]byte(body)))
	req.Header.Set("Authorization", "Bearer "+managementKey)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	server.engine.ServeHTTP(rec, req)
	if rec.Code != wantStatus {
		t.Fatalf("%s %s status = %d body=%s, want %d", method, path, rec.Code, rec.Body.String(), wantStatus)
	}
	return rec
}

func configuredAvailability(t *testing.T, server *Server, managementKey string) configuredAvailabilityPayload {
	t.Helper()
	rec := managementRequest(t, server, managementKey, http.MethodGet, "/v0/management/models/configured-availability", "", http.StatusOK)
	var payload configuredAvailabilityPayload
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode configured availability: %v; body=%s", err, rec.Body.String())
	}
	return payload
}

type configuredAvailabilityPayload struct {
	Data []configuredAvailabilityModel `json:"data"`
}

type configuredAvailabilityModel struct {
	ID      string `json:"id"`
	OwnedBy string `json:"owned_by"`
	Sources []struct {
		Label    string `json:"label"`
		Provider string `json:"provider"`
	} `json:"sources"`
}

func assertAvailabilityHasModel(t *testing.T, server *Server, managementKey, modelID, provider, sourceLabel string) {
	t.Helper()
	payload := configuredAvailability(t, server, managementKey)
	for _, model := range payload.Data {
		if !strings.EqualFold(model.ID, modelID) {
			continue
		}
		for _, source := range model.Sources {
			if source.Provider == provider && source.Label == sourceLabel {
				return
			}
		}
		t.Fatalf("model %q sources = %+v, want provider %q label %q", modelID, model.Sources, provider, sourceLabel)
	}
	t.Fatalf("missing configured model %q; ids=%v", modelID, availabilityIDs(payload))
}

func assertAvailabilityMissingModel(t *testing.T, server *Server, managementKey, modelID string) {
	t.Helper()
	payload := configuredAvailability(t, server, managementKey)
	for _, model := range payload.Data {
		if strings.EqualFold(model.ID, modelID) {
			t.Fatalf("unexpected configured model %q in availability; model=%+v", modelID, model)
		}
	}
}

func availabilityIDs(payload configuredAvailabilityPayload) []string {
	ids := make([]string, 0, len(payload.Data))
	for _, model := range payload.Data {
		ids = append(ids, model.ID)
	}
	return ids
}
