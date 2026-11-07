package management

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/identity"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/usage"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

func initCoolingTestDB(t *testing.T) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	usage.CloseDB()
	if err := usage.InitDB(filepath.Join(t.TempDir(), "usage.db"), config.RequestLogStorageConfig{}, time.UTC); err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	t.Cleanup(usage.CloseDB)
}

func callProviderHandler(tenantID, method, body string, handler gin.HandlerFunc) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Set(managementPrincipalKey, identity.Principal{EffectiveTenant: identity.Tenant{ID: tenantID}})
	c.Request = httptest.NewRequest(method, "/v0/management/provider", bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")
	handler(c)
	return rec
}

// The panel updates itself and may run against an older backend, which drops
// unknown fields silently. It offers the per-channel switch only when the list
// it loaded says this backend stores it, so every provider list must say so.
func TestProviderListsDeclareDisableCooling(t *testing.T) {
	initCoolingTestDB(t)
	keys := (&Handler{cfg: &config.Config{}}).ProviderKeys()
	lists := map[string]gin.HandlerFunc{
		"gemini-api-key":       keys.GetGeminiKeys,
		"claude-api-key":       keys.GetClaudeKeys,
		"codex-api-key":        keys.GetCodexKeys,
		"bedrock-api-key":      keys.GetBedrockKeys,
		"opencode-go-api-key":  keys.GetOpenCodeGoKeys,
		"cline-api-key":        keys.GetClineKeys,
		"ollama-cloud-api-key": keys.GetOllamaCloudKeys,
		"commandcode-api-key":  keys.GetCommandCodeKeys,
		"openai-compatibility": keys.GetOpenAICompat,
		"vertex-api-key":       keys.GetVertexCompatKeys,
	}
	for key, get := range lists {
		rec := callProviderHandler(identity.SystemTenantID, http.MethodGet, "", get)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s status=%d body=%s", key, rec.Code, rec.Body.String())
		}
		var body map[string]json.RawMessage
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("GET %s: %v", key, err)
		}
		if _, ok := body[key]; !ok {
			t.Fatalf("GET %s lost its list: %s", key, rec.Body.String())
		}
		var capabilities map[string]bool
		if err := json.Unmarshal(body["capabilities"], &capabilities); err != nil || !capabilities["disable-cooling"] {
			t.Fatalf("GET %s capabilities = %s, want disable-cooling", key, body["capabilities"])
		}
	}
}

// What the panel does: save the whole list with PUT, read it back, and the API
// clients that PATCH one entry, including clearing the override with null.
func TestProviderSettingsStoreDisableCoolingPerChannel(t *testing.T) {
	initCoolingTestDB(t)
	keys := (&Handler{cfg: &config.Config{}}).ProviderKeys()
	const tenant = "00000000-0000-0000-0000-0000000000c1"

	put := `[{"name": "relay", "base-url": "https://relay.example/v1", "disable-cooling": true, "api-key-entries": [{"api-key": "relay-key"}]},
		{"name": "backup", "base-url": "https://backup.example/v1", "api-key-entries": [{"api-key": "backup-key"}]}]`
	if rec := callProviderHandler(tenant, http.MethodPut, put, keys.PutOpenAICompat); rec.Code != http.StatusOK {
		t.Fatalf("PUT status=%d body=%s", rec.Code, rec.Body.String())
	}
	read := func() map[string]*bool {
		rec := callProviderHandler(tenant, http.MethodGet, "", keys.GetOpenAICompat)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET status=%d body=%s", rec.Code, rec.Body.String())
		}
		var body struct {
			Entries []config.OpenAICompatibility `json:"openai-compatibility"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("GET decode: %v", err)
		}
		out := map[string]*bool{}
		for _, entry := range body.Entries {
			out[entry.Name] = entry.DisableCooling
		}
		return out
	}
	got := read()
	if got["relay"] == nil || !*got["relay"] || got["backup"] != nil {
		t.Fatalf("after PUT: relay=%v backup=%v; want relay true, backup unset", got["relay"], got["backup"])
	}

	if rec := callProviderHandler(tenant, http.MethodPatch, `{"name": "relay", "value": {"disable-cooling": null}}`, keys.PatchOpenAICompat); rec.Code != http.StatusOK {
		t.Fatalf("PATCH null status=%d body=%s", rec.Code, rec.Body.String())
	}
	if got = read(); got["relay"] != nil {
		t.Fatalf("after PATCH null: relay=%v, want unset", *got["relay"])
	}
	if rec := callProviderHandler(tenant, http.MethodPatch, `{"name": "backup", "value": {"disable-cooling": false}}`, keys.PatchOpenAICompat); rec.Code != http.StatusOK {
		t.Fatalf("PATCH false status=%d body=%s", rec.Code, rec.Body.String())
	}
	if got = read(); got["backup"] == nil || *got["backup"] {
		t.Fatalf("after PATCH false: backup=%v, want false", got["backup"])
	}
}

// The account detail reads an auth file's override from the list and only
// offers the switch when the list says PATCH /auth-files/fields stores it.
func TestListAuthFilesReportsDisableCooling(t *testing.T) {
	initCoolingTestDB(t)
	manager := coreauth.NewManager(&memoryAuthStore{}, nil, nil)
	for id, metadata := range map[string]map[string]any{
		"cooling-off.json": {"email": "off@example.com", "disable_cooling": true},
		"inherits.json":    {"email": "inherit@example.com"},
	} {
		if _, err := manager.Register(context.Background(), &coreauth.Auth{
			ID:         id,
			FileName:   id,
			Provider:   "codex",
			Status:     coreauth.StatusActive,
			Attributes: map[string]string{"runtime_only": "true"},
			Metadata:   metadata,
		}); err != nil {
			t.Fatalf("register %s: %v", id, err)
		}
	}
	h := &Handler{cfg: &config.Config{}, authManager: manager}
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodGet, "/auth-files", nil)
	h.ListAuthFiles(c)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var body struct {
		Files        []map[string]any `json:"files"`
		Capabilities map[string]bool  `json:"capabilities"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !body.Capabilities["disable-cooling"] {
		t.Fatalf("capabilities = %v, want disable-cooling", body.Capabilities)
	}
	seen := 0
	for _, file := range body.Files {
		value, present := file["disable_cooling"]
		switch file["id"] {
		case "cooling-off.json":
			seen++
			if value != true {
				t.Fatalf("cooling-off.json disable_cooling = %#v, want true", value)
			}
		case "inherits.json":
			seen++
			if present {
				t.Fatalf("inherits.json reports disable_cooling = %#v, want it absent", value)
			}
		}
	}
	if seen != 2 {
		t.Fatalf("listed %d of the 2 accounts: %s", seen, rec.Body.String())
	}
}
