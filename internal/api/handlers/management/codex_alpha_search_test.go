package management

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	settingsstore "github.com/router-for-me/CLIProxyAPI/v8/internal/management/settings/store"
)

func TestCodexAlphaSearchManagementPersistence(t *testing.T) {
	initManagementModelsTestDB(t)
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("logging-to-file: true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	h := NewHandler(&config.Config{}, path, nil)
	put := performModelsRequest(http.MethodPut, "/codex-api-key", []byte(`[{"api-key":"test-codex","alpha-search":true}]`), h.ProviderKeys().PutCodexKeys)
	if put.Code != http.StatusOK {
		t.Fatalf("PUT = %d: %s", put.Code, put.Body.String())
	}
	assertStored := func(want bool) {
		t.Helper()
		var stored config.Config
		if !settingsstore.ApplyStoredRuntimeSettings(&stored) || len(stored.CodexKey) != 1 {
			t.Fatal("failed to reload Codex key from SQLite")
		}
		encoded, err := json.Marshal(stored.CodexKey[0])
		if err != nil {
			t.Fatal(err)
		}
		var fields map[string]interface{}
		if err := json.Unmarshal(encoded, &fields); err != nil {
			t.Fatal(err)
		}
		if got, _ := fields["alpha-search"].(bool); got != want {
			t.Fatalf("persisted alpha-search != %t: %s", want, encoded)
		}
		// A fresh handler must expose the persisted capability through GET as well.
		restored := NewHandler(&stored, path, nil)
		get := performModelsRequest(http.MethodGet, "/codex-api-key", nil, restored.ProviderKeys().GetCodexKeys)
		var payload struct {
			Keys []struct {
				AlphaSearch bool `json:"alpha-search"`
			} `json:"codex-api-key"`
		}
		if err := json.Unmarshal(get.Body.Bytes(), &payload); err != nil || len(payload.Keys) != 1 || payload.Keys[0].AlphaSearch != want {
			t.Fatalf("GET did not restore capability: %s", get.Body.String())
		}
	}
	assertStored(true)
	for _, tc := range []struct {
		value string
		want  bool
	}{
		{`{"prefix":"team"}`, true},
		{`{"alpha-search":false}`, false},
		{`{"alpha-search":true}`, true},
	} {
		patch := performModelsRequest(http.MethodPatch, "/codex-api-key", []byte(`{"index":0,"value":`+tc.value+`}`), h.ProviderKeys().PatchCodexKey)
		if patch.Code != http.StatusOK {
			t.Fatalf("PATCH = %d: %s", patch.Code, patch.Body.String())
		}
		assertStored(tc.want)
	}
}
