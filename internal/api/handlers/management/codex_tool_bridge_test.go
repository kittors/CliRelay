package management

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	settingsstore "github.com/router-for-me/CLIProxyAPI/v6/internal/management/settings/store"
)

// codex-tool-bridge must be writable through PUT and PATCH, survive a reload
// from the settings store, and read back through GET, for both provider kinds
// that expose it. PATCHes that do not mention it must leave it alone.
func TestCodexToolBridgeManagementPersistence(t *testing.T) {
	type providerCase struct {
		name    string
		path    string
		listKey string
		putBody string
		put     func(*ProviderKeysHandler) gin.HandlerFunc
		patch   func(*ProviderKeysHandler) gin.HandlerFunc
		get     func(*ProviderKeysHandler) gin.HandlerFunc
		stored  func(*config.Config) (any, int)
	}
	cases := []providerCase{
		{
			name:    "openai-compatibility",
			path:    "/openai-compatibility",
			listKey: "openai-compatibility",
			putBody: `[{"name":"bridged","base-url":"https://bridge.example/v1","api-key-entries":[{"api-key":"test-key"}],"codex-tool-bridge":true}]`,
			put:     func(h *ProviderKeysHandler) gin.HandlerFunc { return h.PutOpenAICompat },
			patch:   func(h *ProviderKeysHandler) gin.HandlerFunc { return h.PatchOpenAICompat },
			get:     func(h *ProviderKeysHandler) gin.HandlerFunc { return h.GetOpenAICompat },
			stored: func(cfg *config.Config) (any, int) {
				if len(cfg.OpenAICompatibility) == 0 {
					return nil, 0
				}
				return cfg.OpenAICompatibility[0], len(cfg.OpenAICompatibility)
			},
		},
		{
			name:    "opencode-go",
			path:    "/opencode-go-api-key",
			listKey: "opencode-go-api-key",
			putBody: `[{"api-key":"test-go","codex-tool-bridge":true}]`,
			put:     func(h *ProviderKeysHandler) gin.HandlerFunc { return h.PutOpenCodeGoKeys },
			patch:   func(h *ProviderKeysHandler) gin.HandlerFunc { return h.PatchOpenCodeGoKey },
			get:     func(h *ProviderKeysHandler) gin.HandlerFunc { return h.GetOpenCodeGoKeys },
			stored: func(cfg *config.Config) (any, int) {
				if len(cfg.OpenCodeGoKey) == 0 {
					return nil, 0
				}
				return cfg.OpenCodeGoKey[0], len(cfg.OpenCodeGoKey)
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			initManagementModelsTestDB(t)
			path := filepath.Join(t.TempDir(), "config.yaml")
			if err := os.WriteFile(path, []byte("logging-to-file: true\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			h := NewHandler(&config.Config{}, path, nil)
			put := performModelsRequest(http.MethodPut, tc.path, []byte(tc.putBody), tc.put(h.ProviderKeys()))
			if put.Code != http.StatusOK {
				t.Fatalf("PUT = %d: %s", put.Code, put.Body.String())
			}
			assertStored := func(step string, want bool) {
				t.Helper()
				var stored config.Config
				if !settingsstore.ApplyStoredRuntimeSettings(&stored) {
					t.Fatalf("%s: nothing reloaded from the settings store", step)
				}
				entry, count := tc.stored(&stored)
				if count != 1 {
					t.Fatalf("%s: reloaded %d entries, want 1", step, count)
				}
				encoded, err := json.Marshal(entry)
				if err != nil {
					t.Fatal(err)
				}
				var fields map[string]any
				if err := json.Unmarshal(encoded, &fields); err != nil {
					t.Fatal(err)
				}
				if got, _ := fields["codex-tool-bridge"].(bool); got != want {
					t.Fatalf("%s: persisted codex-tool-bridge != %t: %s", step, want, encoded)
				}
				// A fresh handler must serve the persisted value through GET as well.
				restored := NewHandler(&stored, path, nil)
				get := performModelsRequest(http.MethodGet, tc.path, nil, tc.get(restored.ProviderKeys()))
				var payload map[string][]map[string]any
				if err := json.Unmarshal(get.Body.Bytes(), &payload); err != nil || len(payload[tc.listKey]) != 1 {
					t.Fatalf("%s: GET = %s", step, get.Body.String())
				}
				if got, _ := payload[tc.listKey][0]["codex-tool-bridge"].(bool); got != want {
					t.Fatalf("%s: GET codex-tool-bridge != %t: %s", step, want, get.Body.String())
				}
			}
			assertStored("PUT", true)
			for _, step := range []struct {
				value string
				want  bool
			}{
				{`{"prefix":"team"}`, true},
				{`{"codex-tool-bridge":false}`, false},
				{`{"codex-tool-bridge":true}`, true},
			} {
				patch := performModelsRequest(http.MethodPatch, tc.path, []byte(`{"index":0,"value":`+step.value+`}`), tc.patch(h.ProviderKeys()))
				if patch.Code != http.StatusOK {
					t.Fatalf("PATCH %s = %d: %s", step.value, patch.Code, patch.Body.String())
				}
				assertStored("PATCH "+step.value, step.want)
			}
		})
	}
}
