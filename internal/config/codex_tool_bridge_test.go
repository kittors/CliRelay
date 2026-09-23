package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// codex-tool-bridge is opt-in on openai-compatibility and opencode-go entries.
// The value has to survive the management API (JSON) and config.yaml in both
// directions, including being switched back off.

func TestCodexToolBridgeConfigRoundTrip(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		value := "false"
		if enabled {
			value = "true"
		}
		var compat OpenAICompatibility
		var goKey OpenCodeGoKey
		for _, tc := range []struct {
			name   string
			input  string
			target any
		}{
			{"openai-compatibility", `{"name":"bridged","base-url":"https://bridge.example/v1","codex-tool-bridge":` + value + `}`, &compat},
			{"opencode-go", `{"api-key":"test-go","codex-tool-bridge":` + value + `}`, &goKey},
		} {
			if err := json.Unmarshal([]byte(tc.input), tc.target); err != nil {
				t.Fatalf("%s: %v", tc.name, err)
			}
			encoded, err := yaml.Marshal(tc.target)
			if err != nil {
				t.Fatalf("%s: %v", tc.name, err)
			}
			if strings.Contains(string(encoded), "codex-tool-bridge: true") != enabled {
				t.Fatalf("%s: YAML lost the setting: %s", tc.name, encoded)
			}
			if err := yaml.Unmarshal(encoded, tc.target); err != nil {
				t.Fatalf("%s: %v", tc.name, err)
			}
			encoded, err = json.Marshal(tc.target)
			if err != nil {
				t.Fatalf("%s: %v", tc.name, err)
			}
			var fields map[string]any
			if err := json.Unmarshal(encoded, &fields); err != nil {
				t.Fatalf("%s: %v", tc.name, err)
			}
			if got, _ := fields["codex-tool-bridge"].(bool); got != enabled {
				t.Fatalf("%s: JSON lost the setting: %s", tc.name, encoded)
			}
		}
	}
}

func TestCodexToolBridgeYAMLSaveCanDisable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	original := "# operator config\n" +
		"openai-compatibility:\n" +
		"  - name: bridged\n" +
		"    base-url: https://bridge.example/v1\n" +
		"    codex-tool-bridge: true\n" +
		"opencode-go-api-key:\n" +
		"  - api-key: test-go\n" +
		"    codex-tool-bridge: true\n"
	if err := os.WriteFile(path, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.OpenAICompatibility) != 1 || !cfg.OpenAICompatibility[0].CodexToolBridge {
		t.Fatalf("LoadConfig lost the openai-compatibility opt-in: %+v", cfg.OpenAICompatibility)
	}
	if len(cfg.OpenCodeGoKey) != 1 || !cfg.OpenCodeGoKey[0].CodexToolBridge {
		t.Fatalf("LoadConfig lost the opencode-go opt-in: %+v", cfg.OpenCodeGoKey)
	}
	cfg.OpenAICompatibility[0].CodexToolBridge = false
	cfg.OpenCodeGoKey[0].CodexToolBridge = false
	if err := SaveConfigPreserveComments(path, cfg); err != nil {
		t.Fatal(err)
	}
	restored, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(restored.OpenAICompatibility) != 1 || restored.OpenAICompatibility[0].CodexToolBridge {
		t.Fatal("YAML save kept a stale openai-compatibility codex-tool-bridge opt-in")
	}
	if len(restored.OpenCodeGoKey) != 1 || restored.OpenCodeGoKey[0].CodexToolBridge {
		t.Fatal("YAML save kept a stale opencode-go codex-tool-bridge opt-in")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "# operator config") {
		t.Fatal("YAML save lost the operator comment")
	}
}
