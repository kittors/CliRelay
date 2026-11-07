package synthesizer

import (
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

// Every provider entry can override the global disable-cooling switch, and the
// override has to reach the credentials synthesized from it: that attribute is
// all the cooldown code sees of a provider entry.
func TestConfigSynthesizerCarriesDisableCoolingOverride(t *testing.T) {
	providers := map[string]func(override *bool) *config.Config{
		"gemini-api-key": func(o *bool) *config.Config {
			return &config.Config{GeminiKey: []config.GeminiKey{{APIKey: "gemini-key", DisableCooling: o}}}
		},
		"claude-api-key": func(o *bool) *config.Config {
			return &config.Config{ClaudeKey: []config.ClaudeKey{{APIKey: "claude-key", DisableCooling: o}}}
		},
		"codex-api-key": func(o *bool) *config.Config {
			return &config.Config{CodexKey: []config.CodexKey{{APIKey: "codex-key", DisableCooling: o}}}
		},
		"bedrock-api-key": func(o *bool) *config.Config {
			return &config.Config{BedrockKey: []config.BedrockKey{{AuthMode: "api-key", APIKey: "bedrock-key", DisableCooling: o}}}
		},
		"opencode-go-api-key": func(o *bool) *config.Config {
			return &config.Config{OpenCodeGoKey: []config.OpenCodeGoKey{{APIKey: "opencode-go-key", DisableCooling: o}}}
		},
		"cline-api-key": func(o *bool) *config.Config {
			return &config.Config{ClineKey: []config.ClineKey{{APIKey: "cline-key", DisableCooling: o}}}
		},
		"commandcode-api-key": func(o *bool) *config.Config {
			return &config.Config{CommandCodeKey: []config.CommandCodeKey{{APIKey: "commandcode-key", DisableCooling: o}}}
		},
		"ollama-cloud-api-key": func(o *bool) *config.Config {
			return &config.Config{OllamaCloudKey: []config.OllamaCloudKey{{APIKey: "ollama-key", DisableCooling: o}}}
		},
		"vertex-api-key": func(o *bool) *config.Config {
			return &config.Config{VertexCompatAPIKey: []config.VertexCompatKey{{APIKey: "vertex-key", BaseURL: "https://vertex.example", DisableCooling: o}}}
		},
		// The override sits on the provider and applies to each of its keys.
		"openai-compatibility": func(o *bool) *config.Config {
			return &config.Config{OpenAICompatibility: []config.OpenAICompatibility{{
				Name:           "relay",
				BaseURL:        "https://relay.example/v1",
				DisableCooling: o,
				APIKeyEntries:  []config.OpenAICompatibilityAPIKey{{APIKey: "relay-key-1"}, {APIKey: "relay-key-2"}},
			}}}
		},
		"openai-compatibility without keys": func(o *bool) *config.Config {
			return &config.Config{OpenAICompatibility: []config.OpenAICompatibility{{
				Name:           "relay",
				BaseURL:        "https://relay.example/v1",
				DisableCooling: o,
			}}}
		},
	}
	on, off := true, false
	for name, build := range providers {
		for _, tc := range []struct {
			label    string
			override *bool
			want     string
			present  bool
		}{
			{label: "unset", override: nil},
			{label: "true", override: &on, want: "true", present: true},
			{label: "false", override: &off, want: "false", present: true},
		} {
			t.Run(name+"/"+tc.label, func(t *testing.T) {
				auths, err := NewConfigSynthesizer().Synthesize(&SynthesisContext{
					Config:      build(tc.override),
					Now:         time.Now(),
					IDGenerator: NewStableIDGenerator(),
				})
				if err != nil {
					t.Fatalf("Synthesize: %v", err)
				}
				if len(auths) == 0 {
					t.Fatal("no credential synthesized")
				}
				for _, auth := range auths {
					got, present := auth.Attributes[coreauth.DisableCoolingAttributeKey]
					if present != tc.present || got != tc.want {
						t.Fatalf("%s: disable_cooling attribute = %q (present %t), want %q (present %t)", auth.ID, got, present, tc.want, tc.present)
					}
					override, ok := auth.DisableCoolingOverride()
					if ok != tc.present || (tc.present && override != *tc.override) {
						t.Fatalf("%s: DisableCoolingOverride = %t, %t; want the entry's override", auth.ID, override, ok)
					}
				}
			})
		}
	}
}
