package providers

import (
	"encoding/json"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
)

// PATCH on every provider list accepts disable-cooling as a tri-state: a
// missing key keeps the channel's override, null clears it back to the global
// switch, and true or false sets it.
func TestPatchProviderEntryDisableCooling(t *testing.T) {
	zero := 0
	type provider struct {
		seed  *config.Config
		patch func(s *Service, body []byte) error
		read  func(cfg *config.Config) *bool
	}
	decode := func(body []byte, into any) error { return json.Unmarshal(body, into) }
	providers := map[string]provider{
		"gemini-api-key": {
			seed: &config.Config{GeminiKey: []config.GeminiKey{{APIKey: "gemini-key"}}},
			patch: func(s *Service, body []byte) error {
				var p GeminiKeyPatch
				if err := decode(body, &p); err != nil {
					return err
				}
				return s.PatchGeminiKey(&zero, nil, p)
			},
			read: func(cfg *config.Config) *bool { return cfg.GeminiKey[0].DisableCooling },
		},
		"claude-api-key": {
			seed: &config.Config{ClaudeKey: []config.ClaudeKey{{APIKey: "claude-key"}}},
			patch: func(s *Service, body []byte) error {
				var p ClaudeKeyPatch
				if err := decode(body, &p); err != nil {
					return err
				}
				return s.PatchClaudeKey(&zero, nil, p)
			},
			read: func(cfg *config.Config) *bool { return cfg.ClaudeKey[0].DisableCooling },
		},
		"codex-api-key": {
			seed: &config.Config{CodexKey: []config.CodexKey{{APIKey: "codex-key", BaseURL: "https://codex.example"}}},
			patch: func(s *Service, body []byte) error {
				var p CodexKeyPatch
				if err := decode(body, &p); err != nil {
					return err
				}
				return s.PatchCodexKey(&zero, nil, p)
			},
			read: func(cfg *config.Config) *bool { return cfg.CodexKey[0].DisableCooling },
		},
		"bedrock-api-key": {
			seed: &config.Config{BedrockKey: []config.BedrockKey{{AuthMode: "api-key", APIKey: "bedrock-key", Region: "us-east-1"}}},
			patch: func(s *Service, body []byte) error {
				var p BedrockKeyPatch
				if err := decode(body, &p); err != nil {
					return err
				}
				return s.PatchBedrockKey(&zero, nil, p)
			},
			read: func(cfg *config.Config) *bool { return cfg.BedrockKey[0].DisableCooling },
		},
		"opencode-go-api-key": {
			seed: &config.Config{OpenCodeGoKey: []config.OpenCodeGoKey{{APIKey: "opencode-go-key"}}},
			patch: func(s *Service, body []byte) error {
				var p OpenCodeGoPatch
				if err := decode(body, &p); err != nil {
					return err
				}
				return s.PatchOpenCodeGoKey(&zero, nil, nil, p)
			},
			read: func(cfg *config.Config) *bool { return cfg.OpenCodeGoKey[0].DisableCooling },
		},
		"cline-api-key": {
			seed: &config.Config{ClineKey: []config.ClineKey{{APIKey: "cline-key"}}},
			patch: func(s *Service, body []byte) error {
				var p ClinePatch
				if err := decode(body, &p); err != nil {
					return err
				}
				return s.PatchClineKey(&zero, nil, nil, p)
			},
			read: func(cfg *config.Config) *bool { return cfg.ClineKey[0].DisableCooling },
		},
		"ollama-cloud-api-key": {
			seed: &config.Config{OllamaCloudKey: []config.OllamaCloudKey{{APIKey: "ollama-key"}}},
			patch: func(s *Service, body []byte) error {
				var p OllamaCloudPatch
				if err := decode(body, &p); err != nil {
					return err
				}
				return s.PatchOllamaCloudKey(&zero, nil, nil, p)
			},
			read: func(cfg *config.Config) *bool { return cfg.OllamaCloudKey[0].DisableCooling },
		},
		"commandcode-api-key": {
			seed: &config.Config{CommandCodeKey: []config.CommandCodeKey{{APIKey: "commandcode-key"}}},
			patch: func(s *Service, body []byte) error {
				var p CommandCodePatch
				if err := decode(body, &p); err != nil {
					return err
				}
				return s.PatchCommandCodeKey(&zero, nil, nil, p)
			},
			read: func(cfg *config.Config) *bool { return cfg.CommandCodeKey[0].DisableCooling },
		},
		"openai-compatibility": {
			seed: &config.Config{OpenAICompatibility: []config.OpenAICompatibility{{
				Name:          "relay",
				BaseURL:       "https://relay.example/v1",
				APIKeyEntries: []config.OpenAICompatibilityAPIKey{{APIKey: "relay-key"}},
			}}},
			patch: func(s *Service, body []byte) error {
				var p OpenAICompatibilityPatch
				if err := decode(body, &p); err != nil {
					return err
				}
				return s.PatchOpenAICompatibility(&zero, nil, p)
			},
			read: func(cfg *config.Config) *bool { return cfg.OpenAICompatibility[0].DisableCooling },
		},
		"vertex-api-key": {
			seed: &config.Config{VertexCompatAPIKey: []config.VertexCompatKey{{APIKey: "vertex-key", BaseURL: "https://vertex.example"}}},
			patch: func(s *Service, body []byte) error {
				var p VertexCompatPatch
				if err := decode(body, &p); err != nil {
					return err
				}
				return s.PatchVertexCompatKey(&zero, nil, p)
			},
			read: func(cfg *config.Config) *bool { return cfg.VertexCompatAPIKey[0].DisableCooling },
		},
	}
	steps := []struct {
		body string
		want string
	}{
		{body: `{"disable-cooling": true}`, want: "true"},
		{body: `{"prefix": "team"}`, want: "true"}, // another field leaves the override alone
		{body: `{"disable-cooling": false}`, want: "false"},
		{body: `{"disable-cooling": null}`, want: "inherit"},
	}
	for name, p := range providers {
		t.Run(name, func(t *testing.T) {
			s := NewService(p.seed, nil)
			for _, step := range steps {
				if err := p.patch(s, []byte(step.body)); err != nil {
					t.Fatalf("PATCH %s: %v", step.body, err)
				}
				got := "inherit"
				if v := p.read(p.seed); v != nil {
					got = map[bool]string{true: "true", false: "false"}[*v]
				}
				if got != step.want {
					t.Fatalf("after PATCH %s: disable-cooling = %s, want %s", step.body, got, step.want)
				}
			}
		})
	}
}

// The panel saves a provider list with PUT, so the field has to survive the
// JSON round trip of a whole entry, false included.
func TestReplaceProviderListKeepsDisableCooling(t *testing.T) {
	cfg := &config.Config{}
	s := NewService(cfg, nil)
	var entries []config.OpenAICompatibility
	body := `[
		{"name": "relay-a", "base-url": "https://a.example/v1", "disable-cooling": true, "api-key-entries": [{"api-key": "a"}]},
		{"name": "relay-b", "base-url": "https://b.example/v1", "disable-cooling": false, "api-key-entries": [{"api-key": "b"}]},
		{"name": "relay-c", "base-url": "https://c.example/v1", "api-key-entries": [{"api-key": "c"}]}
	]`
	if err := json.Unmarshal([]byte(body), &entries); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if err := s.ReplaceOpenAICompatibility(entries); err != nil {
		t.Fatalf("replace: %v", err)
	}
	got := map[string]string{}
	for _, entry := range cfg.OpenAICompatibility {
		value := "inherit"
		if entry.DisableCooling != nil {
			value = map[bool]string{true: "true", false: "false"}[*entry.DisableCooling]
		}
		got[entry.Name] = value
	}
	want := map[string]string{"relay-a": "true", "relay-b": "false", "relay-c": "inherit"}
	for name, value := range want {
		if got[name] != value {
			t.Fatalf("%s disable-cooling = %s, want %s (all: %v)", name, got[name], value, got)
		}
	}
	encoded, err := json.Marshal(cfg.OpenAICompatibility)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	var echoed []map[string]any
	if err := json.Unmarshal(encoded, &echoed); err != nil {
		t.Fatalf("decode echo: %v", err)
	}
	for _, entry := range echoed {
		_, present := entry["disable-cooling"]
		if wantPresent := entry["name"] != "relay-c"; present != wantPresent {
			t.Fatalf("%v: disable-cooling present = %t, want %t", entry["name"], present, wantPresent)
		}
	}
}
