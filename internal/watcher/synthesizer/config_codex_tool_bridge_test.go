package synthesizer

import (
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
)

// The executor decides whether to add the Codex tool bridge from the
// codex_tool_bridge auth attribute, so the attribute has to appear exactly on
// the credentials of entries that opted in, and on nothing else.
func TestConfigSynthesizerCarriesCodexToolBridgeOptIn(t *testing.T) {
	auths, err := NewConfigSynthesizer().Synthesize(&SynthesisContext{
		Config: &config.Config{
			OpenAICompatibility: []config.OpenAICompatibility{
				{
					Name:            "Bridged",
					BaseURL:         "https://bridged.example/v1",
					CodexToolBridge: true,
					APIKeyEntries:   []config.OpenAICompatibilityAPIKey{{APIKey: "bridged-1"}, {APIKey: "bridged-2"}},
				},
				{
					Name:          "Plain",
					BaseURL:       "https://plain.example/v1",
					APIKeyEntries: []config.OpenAICompatibilityAPIKey{{APIKey: "plain-1"}},
				},
				// An entry without API keys is synthesized through a separate branch.
				{Name: "Keyless", BaseURL: "https://keyless.example/v1", CodexToolBridge: true},
			},
			OpenCodeGoKey: []config.OpenCodeGoKey{
				{APIKey: "go-bridged", CodexToolBridge: true},
				{APIKey: "go-plain"},
			},
			ClineKey:       []config.ClineKey{{APIKey: "cline-key"}},
			CommandCodeKey: []config.CommandCodeKey{{APIKey: "cc-key"}},
			OllamaCloudKey: []config.OllamaCloudKey{{APIKey: "ollama-key"}},
		},
		Now:         time.Now(),
		IDGenerator: NewStableIDGenerator(),
	})
	if err != nil {
		t.Fatalf("synthesize: %v", err)
	}

	wantBridged := map[string]bool{
		"bridged/bridged-1":       true,
		"bridged/bridged-2":       true,
		"plain/plain-1":           false,
		"keyless/":                true,
		"opencode-go/go-bridged":  true,
		"opencode-go/go-plain":    false,
		"cline/cline-key":         false,
		"commandcode/cc-key":      false,
		"ollama-cloud/ollama-key": false,
	}
	seen := map[string]bool{}
	for _, auth := range auths {
		key := auth.Provider + "/" + auth.Attributes["api_key"]
		want, tracked := wantBridged[key]
		if !tracked {
			t.Fatalf("unexpected synthesized auth %s", key)
		}
		seen[key] = true
		got, present := auth.Attributes["codex_tool_bridge"]
		if want && got != "true" {
			t.Errorf("%s: codex_tool_bridge = %q, want \"true\"", key, got)
		}
		if !want && present {
			t.Errorf("%s: codex_tool_bridge = %q, want the attribute absent", key, got)
		}
	}
	for key := range wantBridged {
		if !seen[key] {
			t.Errorf("no auth synthesized for %s", key)
		}
	}
}
