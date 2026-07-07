package synthesizer

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

func TestNewConfigSynthesizer(t *testing.T) {
	synth := NewConfigSynthesizer()
	if synth == nil {
		t.Fatal("expected non-nil synthesizer")
	}
}

func TestConfigSynthesizer_Synthesize_NilContext(t *testing.T) {
	synth := NewConfigSynthesizer()
	auths, err := synth.Synthesize(nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(auths) != 0 {
		t.Fatalf("expected empty auths, got %d", len(auths))
	}
}

func TestConfigSynthesizer_Synthesize_NilConfig(t *testing.T) {
	synth := NewConfigSynthesizer()
	ctx := &SynthesisContext{
		Config:      nil,
		Now:         time.Now(),
		IDGenerator: NewStableIDGenerator(),
	}
	auths, err := synth.Synthesize(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(auths) != 0 {
		t.Fatalf("expected empty auths, got %d", len(auths))
	}
}

func TestConfigSynthesizer_GeminiKeys(t *testing.T) {
	tests := []struct {
		name       string
		geminiKeys []config.GeminiKey
		wantLen    int
		validate   func(*testing.T, []*coreauth.Auth)
	}{
		{
			name: "single gemini key",
			geminiKeys: []config.GeminiKey{
				{APIKey: "test-key-123", Prefix: "team-a"},
			},
			wantLen: 1,
			validate: func(t *testing.T, auths []*coreauth.Auth) {
				if auths[0].Provider != "gemini" {
					t.Errorf("expected provider gemini, got %s", auths[0].Provider)
				}
				if auths[0].Prefix != "team-a" {
					t.Errorf("expected prefix team-a, got %s", auths[0].Prefix)
				}
				if auths[0].Label != "gemini-apikey" {
					t.Errorf("expected label gemini-apikey, got %s", auths[0].Label)
				}
				if auths[0].Attributes["api_key"] != "test-key-123" {
					t.Errorf("expected api_key test-key-123, got %s", auths[0].Attributes["api_key"])
				}
				if auths[0].Status != coreauth.StatusActive {
					t.Errorf("expected status active, got %s", auths[0].Status)
				}
			},
		},
		{
			name: "gemini key with base url and proxy",
			geminiKeys: []config.GeminiKey{
				{
					APIKey:   "api-key",
					BaseURL:  "https://custom.api.com",
					ProxyURL: "http://proxy.local:8080",
					ProxyID:  "hk",
					Prefix:   "custom",
				},
			},
			wantLen: 1,
			validate: func(t *testing.T, auths []*coreauth.Auth) {
				if auths[0].Attributes["base_url"] != "https://custom.api.com" {
					t.Errorf("expected base_url https://custom.api.com, got %s", auths[0].Attributes["base_url"])
				}
				if auths[0].ProxyURL != "http://proxy.local:8080" {
					t.Errorf("expected proxy_url http://proxy.local:8080, got %s", auths[0].ProxyURL)
				}
				if auths[0].ProxyID != "hk" {
					t.Errorf("expected proxy_id hk, got %s", auths[0].ProxyID)
				}
			},
		},
		{
			name: "gemini key with headers",
			geminiKeys: []config.GeminiKey{
				{
					APIKey:  "api-key",
					Headers: map[string]string{"X-Custom": "value"},
				},
			},
			wantLen: 1,
			validate: func(t *testing.T, auths []*coreauth.Auth) {
				if auths[0].Attributes["header:X-Custom"] != "value" {
					t.Errorf("expected header:X-Custom=value, got %s", auths[0].Attributes["header:X-Custom"])
				}
			},
		},
		{
			name: "empty api key skipped",
			geminiKeys: []config.GeminiKey{
				{APIKey: ""},
				{APIKey: "  "},
				{APIKey: "valid-key"},
			},
			wantLen: 1,
		},
		{
			name: "multiple gemini keys",
			geminiKeys: []config.GeminiKey{
				{APIKey: "key-1", Prefix: "a"},
				{APIKey: "key-2", Prefix: "b"},
				{APIKey: "key-3", Prefix: "c"},
			},
			wantLen: 3,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			synth := NewConfigSynthesizer()
			ctx := &SynthesisContext{
				Config: &config.Config{
					GeminiKey: tt.geminiKeys,
				},
				Now:         time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC),
				IDGenerator: NewStableIDGenerator(),
			}

			auths, err := synth.Synthesize(ctx)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(auths) != tt.wantLen {
				t.Fatalf("expected %d auths, got %d", tt.wantLen, len(auths))
			}

			if tt.validate != nil && len(auths) > 0 {
				tt.validate(t, auths)
			}
		})
	}
}

func TestConfigSynthesizer_ClaudeKeys(t *testing.T) {
	synth := NewConfigSynthesizer()
	ctx := &SynthesisContext{
		Config: &config.Config{
			ClaudeKey: []config.ClaudeKey{
				{
					APIKey:  "sk-ant-api-xxx",
					Prefix:  "main",
					BaseURL: "https://api.anthropic.com",
					Models: []config.ClaudeModel{
						{Name: "claude-3-opus"},
						{Name: "claude-3-sonnet"},
					},
				},
			},
		},
		Now:         time.Now(),
		IDGenerator: NewStableIDGenerator(),
	}

	auths, err := synth.Synthesize(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(auths) != 1 {
		t.Fatalf("expected 1 auth, got %d", len(auths))
	}

	if auths[0].Provider != "claude" {
		t.Errorf("expected provider claude, got %s", auths[0].Provider)
	}
	if auths[0].Label != "claude-apikey" {
		t.Errorf("expected label claude-apikey, got %s", auths[0].Label)
	}
	if auths[0].Prefix != "main" {
		t.Errorf("expected prefix main, got %s", auths[0].Prefix)
	}
	if auths[0].Attributes["api_key"] != "sk-ant-api-xxx" {
		t.Errorf("expected api_key sk-ant-api-xxx, got %s", auths[0].Attributes["api_key"])
	}
	if _, ok := auths[0].Attributes["models_hash"]; !ok {
		t.Error("expected models_hash in attributes")
	}
}

func TestConfigSynthesizer_ClaudeKeys_SkipsEmptyAndHeaders(t *testing.T) {
	synth := NewConfigSynthesizer()
	ctx := &SynthesisContext{
		Config: &config.Config{
			ClaudeKey: []config.ClaudeKey{
				{APIKey: ""},    // empty, should be skipped
				{APIKey: "   "}, // whitespace, should be skipped
				{APIKey: "valid-key", Headers: map[string]string{"X-Custom": "value"}},
			},
		},
		Now:         time.Now(),
		IDGenerator: NewStableIDGenerator(),
	}

	auths, err := synth.Synthesize(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(auths) != 1 {
		t.Fatalf("expected 1 auth (empty keys skipped), got %d", len(auths))
	}
	if auths[0].Attributes["header:X-Custom"] != "value" {
		t.Errorf("expected header:X-Custom=value, got %s", auths[0].Attributes["header:X-Custom"])
	}
}

func TestConfigSynthesizer_ClaudeKeyDisableAllModelsMarksAuthDisabled(t *testing.T) {
	synth := NewConfigSynthesizer()
	ctx := &SynthesisContext{
		Config: &config.Config{
			ClaudeKey: []config.ClaudeKey{
				{
					APIKey:         "sk-claude-disabled",
					Name:           "Kimi渠道",
					BaseURL:        "https://api.kimi.com/coding/",
					ExcludedModels: []string{"*"},
					Models: []config.ClaudeModel{
						{Name: "K2.6", Alias: "claude-sonnet-4-6"},
					},
				},
			},
		},
		Now:         time.Now(),
		IDGenerator: NewStableIDGenerator(),
	}

	auths, err := synth.Synthesize(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(auths) != 1 {
		t.Fatalf("expected 1 auth, got %d", len(auths))
	}
	auth := auths[0]
	if !auth.Disabled || auth.Status != coreauth.StatusDisabled {
		t.Fatalf("expected disabled auth for excluded-models wildcard, got disabled=%t status=%s", auth.Disabled, auth.Status)
	}
	if auth.Attributes["excluded_models"] != "*" || auth.Attributes["auth_kind"] != "apikey" {
		t.Fatalf("expected exclusion metadata to be preserved, got %#v", auth.Attributes)
	}
}

func TestConfigSynthesizer_ModelAccessWildcardDoesNotDisableAuth(t *testing.T) {
	synth := NewConfigSynthesizer()
	ctx := &SynthesisContext{
		Config: &config.Config{
			OpenCodeGoKey: []config.OpenCodeGoKey{{
				APIKey:         "sk-opencode",
				ExcludedModels: []string{"*"},
				Models:         []config.OpenCodeGoModel{{Name: "qwen3.5-plus"}},
			}},
			ClineKey: []config.ClineKey{{
				APIKey:         "sk-cline",
				ExcludedModels: []string{"*"},
				Models:         []config.ClineModel{{Name: "cline-pass/glm-5.2"}},
			}},
			OllamaCloudKey: []config.OllamaCloudKey{{
				APIKey:         "sk-ollama",
				ExcludedModels: []string{"*"},
				Models:         []config.OllamaCloudModel{{Name: "gpt-oss:120b"}},
			}},
		},
		Now:         time.Now(),
		IDGenerator: NewStableIDGenerator(),
	}

	auths, err := synth.Synthesize(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(auths) != 3 {
		t.Fatalf("expected 3 auths, got %d", len(auths))
	}
	for _, auth := range auths {
		if auth.Disabled || auth.Status != coreauth.StatusActive {
			t.Fatalf("%s disabled=%t status=%s, want active auth", auth.Provider, auth.Disabled, auth.Status)
		}
		if auth.Attributes["excluded_models"] != "*" {
			t.Fatalf("%s excluded_models = %q, want wildcard metadata", auth.Provider, auth.Attributes["excluded_models"])
		}
	}
}

func TestConfigSynthesizer_ModelAccessDisabledFieldDisablesAuth(t *testing.T) {
	synth := NewConfigSynthesizer()
	ctx := &SynthesisContext{
		Config: &config.Config{
			OpenCodeGoKey:  []config.OpenCodeGoKey{{APIKey: "sk-opencode", Disabled: true}},
			ClineKey:       []config.ClineKey{{APIKey: "sk-cline", Disabled: true}},
			OllamaCloudKey: []config.OllamaCloudKey{{APIKey: "sk-ollama", Disabled: true}},
		},
		Now:         time.Now(),
		IDGenerator: NewStableIDGenerator(),
	}

	auths, err := synth.Synthesize(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(auths) != 3 {
		t.Fatalf("expected 3 auths, got %d", len(auths))
	}
	for _, auth := range auths {
		if !auth.Disabled || auth.Status != coreauth.StatusDisabled || auth.StatusMessage != "disabled via config" {
			t.Fatalf("%s disabled=%t status=%s message=%q, want config-disabled auth", auth.Provider, auth.Disabled, auth.Status, auth.StatusMessage)
		}
	}
}

func TestConfigSynthesizer_CodexKeys(t *testing.T) {
	synth := NewConfigSynthesizer()
	ctx := &SynthesisContext{
		Config: &config.Config{
			CodexKey: []config.CodexKey{
				{
					APIKey:     "codex-key-123",
					Prefix:     "dev",
					BaseURL:    "https://api.openai.com",
					ProxyURL:   "http://proxy.local",
					Websockets: true,
				},
			},
		},
		Now:         time.Now(),
		IDGenerator: NewStableIDGenerator(),
	}

	auths, err := synth.Synthesize(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(auths) != 1 {
		t.Fatalf("expected 1 auth, got %d", len(auths))
	}

	if auths[0].Provider != "codex" {
		t.Errorf("expected provider codex, got %s", auths[0].Provider)
	}
	if auths[0].Label != "codex-apikey" {
		t.Errorf("expected label codex-apikey, got %s", auths[0].Label)
	}
	if auths[0].ProxyURL != "http://proxy.local" {
		t.Errorf("expected proxy_url http://proxy.local, got %s", auths[0].ProxyURL)
	}
	if auths[0].Attributes["websockets"] != "true" {
		t.Errorf("expected websockets=true, got %s", auths[0].Attributes["websockets"])
	}
}

func TestConfigSynthesizer_CodexKeys_SkipsEmptyAndHeaders(t *testing.T) {
	synth := NewConfigSynthesizer()
	ctx := &SynthesisContext{
		Config: &config.Config{
			CodexKey: []config.CodexKey{
				{APIKey: ""},   // empty, should be skipped
				{APIKey: "  "}, // whitespace, should be skipped
				{APIKey: "valid-key", Headers: map[string]string{"Authorization": "Bearer xyz"}},
			},
		},
		Now:         time.Now(),
		IDGenerator: NewStableIDGenerator(),
	}

	auths, err := synth.Synthesize(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(auths) != 1 {
		t.Fatalf("expected 1 auth (empty keys skipped), got %d", len(auths))
	}
	if auths[0].Attributes["header:Authorization"] != "Bearer xyz" {
		t.Errorf("expected header:Authorization=Bearer xyz, got %s", auths[0].Attributes["header:Authorization"])
	}
}

func TestConfigSynthesizer_OpenCodeGoKeys(t *testing.T) {
	synth := NewConfigSynthesizer()
	ctx := &SynthesisContext{
		Config: &config.Config{
			OpenCodeGoKey: []config.OpenCodeGoKey{
				{
					APIKey:              "go-key",
					Name:                "go",
					Priority:            9,
					Prefix:              "team",
					ProxyURL:            "http://proxy",
					ProxyID:             "hk",
					Headers:             map[string]string{"X-Test": "yes"},
					Models:              []config.OpenCodeGoModel{{Name: "glm-5.2"}},
					ExcludedModels:      []string{"minimax-m2.5"},
					VisionFallbackModel: "qwen3.5-plus",
				},
			},
		},
		Now:         time.Now(),
		IDGenerator: NewStableIDGenerator(),
	}

	auths, err := synth.Synthesize(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(auths) != 1 {
		t.Fatalf("expected 1 auth, got %d", len(auths))
	}
	auth := auths[0]
	if auth.Provider != "opencode-go" || auth.Label != "go" || auth.Prefix != "team" {
		t.Fatalf("unexpected auth identity: %+v", auth)
	}
	if auth.Attributes["api_key"] != "go-key" || auth.Attributes["priority"] != "9" || auth.Attributes["header:X-Test"] != "yes" {
		t.Fatalf("unexpected attrs: %#v", auth.Attributes)
	}
	if auth.ProxyURL != "http://proxy" || auth.ProxyID != "hk" {
		t.Fatalf("unexpected proxy settings: %+v", auth)
	}
	if auth.Attributes["auth_kind"] != "apikey" || auth.Attributes["excluded_models"] != "minimax-m2.5" {
		t.Fatalf("expected api key metadata, got %#v", auth.Attributes)
	}
	if auth.Attributes["models_hash"] == "" || auth.Attributes["vision_fallback_model"] != "qwen3.5-plus" {
		t.Fatalf("expected OpenCode Go per-key model metadata, got %#v", auth.Attributes)
	}
}

func TestConfigSynthesizer_ClineKeys(t *testing.T) {
	synth := NewConfigSynthesizer()
	ctx := &SynthesisContext{
		Config: &config.Config{
			ClineKey: []config.ClineKey{
				{
					APIKey:              "cline-key",
					Name:                "cline",
					Priority:            7,
					Prefix:              "team",
					BaseURL:             "https://api.cline.bot/api/v1/",
					ProxyURL:            "http://proxy",
					ProxyID:             "hk",
					Headers:             map[string]string{"X-Test": "yes"},
					Models:              []config.ClineModel{{Name: "cline-pass/mimo-v2.5-pro", Alias: "mimo"}},
					ExcludedModels:      []string{"cline-pass/minimax-m3"},
					VisionFallbackModel: "cline-pass/mimo-v2.5-pro",
				},
			},
		},
		Now:         time.Now(),
		IDGenerator: NewStableIDGenerator(),
	}

	auths, err := synth.Synthesize(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(auths) != 1 {
		t.Fatalf("expected 1 auth, got %d", len(auths))
	}
	auth := auths[0]
	if auth.Provider != "cline" || auth.Label != "cline" || auth.Prefix != "team" {
		t.Fatalf("unexpected auth identity: %+v", auth)
	}
	if auth.Attributes["api_key"] != "cline-key" || auth.Attributes["base_url"] != config.DefaultClineBaseURL || auth.Attributes["provider_key"] != "cline" || auth.Attributes["compat_name"] != "ClinePass" {
		t.Fatalf("unexpected attrs: %#v", auth.Attributes)
	}
	if auth.Attributes["priority"] != "7" || auth.Attributes["header:X-Test"] != "yes" {
		t.Fatalf("unexpected attrs: %#v", auth.Attributes)
	}
	if auth.ProxyURL != "http://proxy" || auth.ProxyID != "hk" {
		t.Fatalf("unexpected proxy settings: %+v", auth)
	}
	if auth.Attributes["auth_kind"] != "apikey" || auth.Attributes["excluded_models"] != "cline-pass/minimax-m3" {
		t.Fatalf("expected api key metadata, got %#v", auth.Attributes)
	}
	if auth.Attributes["models_hash"] == "" || auth.Attributes["vision_fallback_model"] != "cline-pass/mimo-v2.5-pro" {
		t.Fatalf("expected ClinePass per-key model metadata, got %#v", auth.Attributes)
	}
}

func TestConfigSynthesizer_OpenAICompat(t *testing.T) {
	tests := []struct {
		name    string
		compat  []config.OpenAICompatibility
		wantLen int
	}{
		{
			name: "with APIKeyEntries",
			compat: []config.OpenAICompatibility{
				{
					Name:    "CustomProvider",
					BaseURL: "https://custom.api.com",
					APIKeyEntries: []config.OpenAICompatibilityAPIKey{
						{APIKey: "key-1"},
						{APIKey: "key-2"},
					},
				},
			},
			wantLen: 2,
		},
		{
			name: "skips disabled APIKeyEntries without fallback",
			compat: []config.OpenAICompatibility{
				{
					Name:    "CustomProvider",
					BaseURL: "https://custom.api.com",
					APIKeyEntries: []config.OpenAICompatibilityAPIKey{
						{APIKey: "key-enabled"},
						{APIKey: "key-disabled", Disabled: true},
					},
				},
			},
			wantLen: 1,
		},
		{
			name: "all disabled APIKeyEntries do not create fallback auth",
			compat: []config.OpenAICompatibility{
				{
					Name:    "CustomProvider",
					BaseURL: "https://custom.api.com",
					APIKeyEntries: []config.OpenAICompatibilityAPIKey{
						{APIKey: "key-disabled", Disabled: true},
					},
				},
			},
			wantLen: 0,
		},
		{
			name: "provider disabled skips all APIKeyEntries",
			compat: mustDecodeOpenAICompatibility(t, `[
				{
					"name": "DisabledProvider",
					"disabled": true,
					"base-url": "https://disabled.api.com",
					"api-key-entries": [
						{"api-key": "key-enabled"}
					]
				}
			]`),
			wantLen: 0,
		},
		{
			name: "empty APIKeyEntries included (legacy)",
			compat: []config.OpenAICompatibility{
				{
					Name:    "EmptyKeys",
					BaseURL: "https://empty.api.com",
					APIKeyEntries: []config.OpenAICompatibilityAPIKey{
						{APIKey: ""},
						{APIKey: "   "},
					},
				},
			},
			wantLen: 2,
		},
		{
			name: "without APIKeyEntries (fallback)",
			compat: []config.OpenAICompatibility{
				{
					Name:    "NoKeyProvider",
					BaseURL: "https://no-key.api.com",
				},
			},
			wantLen: 1,
		},
		{
			name: "empty name defaults",
			compat: []config.OpenAICompatibility{
				{
					Name:    "",
					BaseURL: "https://default.api.com",
				},
			},
			wantLen: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			synth := NewConfigSynthesizer()
			ctx := &SynthesisContext{
				Config: &config.Config{
					OpenAICompatibility: tt.compat,
				},
				Now:         time.Now(),
				IDGenerator: NewStableIDGenerator(),
			}

			auths, err := synth.Synthesize(ctx)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(auths) != tt.wantLen {
				t.Fatalf("expected %d auths, got %d", tt.wantLen, len(auths))
			}
		})
	}
}

func mustDecodeOpenAICompatibility(t *testing.T, raw string) []config.OpenAICompatibility {
	t.Helper()
	var out []config.OpenAICompatibility
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		t.Fatalf("decode openai compatibility: %v", err)
	}
	return out
}

func TestConfigSynthesizer_VertexCompat(t *testing.T) {
	synth := NewConfigSynthesizer()
	ctx := &SynthesisContext{
		Config: &config.Config{
			VertexCompatAPIKey: []config.VertexCompatKey{
				{
					APIKey:  "vertex-key-123",
					BaseURL: "https://vertex.googleapis.com",
					Prefix:  "vertex-prod",
				},
			},
		},
		Now:         time.Now(),
		IDGenerator: NewStableIDGenerator(),
	}

	auths, err := synth.Synthesize(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(auths) != 1 {
		t.Fatalf("expected 1 auth, got %d", len(auths))
	}

	if auths[0].Provider != "vertex" {
		t.Errorf("expected provider vertex, got %s", auths[0].Provider)
	}
	if auths[0].Label != "vertex-apikey" {
		t.Errorf("expected label vertex-apikey, got %s", auths[0].Label)
	}
	if auths[0].Prefix != "vertex-prod" {
		t.Errorf("expected prefix vertex-prod, got %s", auths[0].Prefix)
	}
}

func TestConfigSynthesizer_VertexCompat_SkipsEmptyAndHeaders(t *testing.T) {
	synth := NewConfigSynthesizer()
	ctx := &SynthesisContext{
		Config: &config.Config{
			VertexCompatAPIKey: []config.VertexCompatKey{
				{APIKey: "", BaseURL: "https://vertex.api"},   // empty key creates auth without api_key attr
				{APIKey: "  ", BaseURL: "https://vertex.api"}, // whitespace key creates auth without api_key attr
				{APIKey: "valid-key", BaseURL: "https://vertex.api", Headers: map[string]string{"X-Vertex": "test"}},
			},
		},
		Now:         time.Now(),
		IDGenerator: NewStableIDGenerator(),
	}

	auths, err := synth.Synthesize(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Vertex compat doesn't skip empty keys - it creates auths without api_key attribute
	if len(auths) != 3 {
		t.Fatalf("expected 3 auths, got %d", len(auths))
	}
	// First two should not have api_key attribute
	if _, ok := auths[0].Attributes["api_key"]; ok {
		t.Error("expected first auth to not have api_key attribute")
	}
	if _, ok := auths[1].Attributes["api_key"]; ok {
		t.Error("expected second auth to not have api_key attribute")
	}
	// Third should have headers
	if auths[2].Attributes["header:X-Vertex"] != "test" {
		t.Errorf("expected header:X-Vertex=test, got %s", auths[2].Attributes["header:X-Vertex"])
	}
}

func TestConfigSynthesizer_OpenAICompat_WithModelsHash(t *testing.T) {
	synth := NewConfigSynthesizer()
	ctx := &SynthesisContext{
		Config: &config.Config{
			OpenAICompatibility: []config.OpenAICompatibility{
				{
					Name:    "TestProvider",
					BaseURL: "https://test.api.com",
					Models: []config.OpenAICompatibilityModel{
						{Name: "model-a"},
						{Name: "model-b"},
					},
					APIKeyEntries: []config.OpenAICompatibilityAPIKey{
						{APIKey: "key-with-models"},
					},
				},
			},
		},
		Now:         time.Now(),
		IDGenerator: NewStableIDGenerator(),
	}

	auths, err := synth.Synthesize(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(auths) != 1 {
		t.Fatalf("expected 1 auth, got %d", len(auths))
	}
	if _, ok := auths[0].Attributes["models_hash"]; !ok {
		t.Error("expected models_hash in attributes")
	}
	if auths[0].Attributes["api_key"] != "key-with-models" {
		t.Errorf("expected api_key key-with-models, got %s", auths[0].Attributes["api_key"])
	}
}

func TestConfigSynthesizer_OpenAICompat_FallbackWithModels(t *testing.T) {
	synth := NewConfigSynthesizer()
	ctx := &SynthesisContext{
		Config: &config.Config{
			OpenAICompatibility: []config.OpenAICompatibility{
				{
					Name:    "NoKeyWithModels",
					BaseURL: "https://nokey.api.com",
					Models: []config.OpenAICompatibilityModel{
						{Name: "model-x"},
					},
					Headers: map[string]string{"X-API": "header-value"},
					// No APIKeyEntries - should use fallback path
				},
			},
		},
		Now:         time.Now(),
		IDGenerator: NewStableIDGenerator(),
	}

	auths, err := synth.Synthesize(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(auths) != 1 {
		t.Fatalf("expected 1 auth, got %d", len(auths))
	}
	if _, ok := auths[0].Attributes["models_hash"]; !ok {
		t.Error("expected models_hash in fallback path")
	}
	if auths[0].Attributes["header:X-API"] != "header-value" {
		t.Errorf("expected header:X-API=header-value, got %s", auths[0].Attributes["header:X-API"])
	}
}

func TestConfigSynthesizer_VertexCompat_WithModels(t *testing.T) {
	synth := NewConfigSynthesizer()
	ctx := &SynthesisContext{
		Config: &config.Config{
			VertexCompatAPIKey: []config.VertexCompatKey{
				{
					APIKey:  "vertex-key",
					BaseURL: "https://vertex.api",
					Models: []config.VertexCompatModel{
						{Name: "gemini-pro", Alias: "pro"},
						{Name: "gemini-ultra", Alias: "ultra"},
					},
				},
			},
		},
		Now:         time.Now(),
		IDGenerator: NewStableIDGenerator(),
	}

	auths, err := synth.Synthesize(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(auths) != 1 {
		t.Fatalf("expected 1 auth, got %d", len(auths))
	}
	if _, ok := auths[0].Attributes["models_hash"]; !ok {
		t.Error("expected models_hash in vertex auth with models")
	}
}

func TestConfigSynthesizer_BedrockKeys(t *testing.T) {
	synth := NewConfigSynthesizer()
	ctx := &SynthesisContext{
		Config: &config.Config{
			BedrockKey: []config.BedrockKey{
				{
					Name:        "aws api",
					AuthMode:    "api-key",
					APIKey:      "br-key",
					Region:      "eu-west-1",
					ForceGlobal: true,
					BaseURL:     "https://bedrock.local",
					Prefix:      "aws",
					ProxyURL:    "http://proxy.local",
					ProxyID:     "hk",
					Headers:     map[string]string{"X-Bedrock": "test"},
					Models: []config.BedrockModel{
						{Name: "claude-sonnet-4-5", Alias: "aws-sonnet"},
					},
					ExcludedModels: []string{"claude-opus-*"},
				},
				{
					Name:            "aws sigv4",
					AuthMode:        "sigv4",
					AccessKeyID:     "AKIATEST",
					SecretAccessKey: "SECRET",
					SessionToken:    "SESSION",
					Region:          "us-east-1",
				},
			},
		},
		Now:         time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC),
		IDGenerator: NewStableIDGenerator(),
	}

	auths, err := synth.Synthesize(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(auths) != 2 {
		t.Fatalf("expected 2 bedrock auths, got %d", len(auths))
	}

	apiAuth := auths[0]
	if apiAuth.Provider != "bedrock" || apiAuth.Label != "aws api" || apiAuth.Prefix != "aws" {
		t.Fatalf("unexpected api auth identity: %+v", apiAuth)
	}
	if apiAuth.Attributes["auth_mode"] != "api-key" || apiAuth.Attributes["api_key"] != "br-key" {
		t.Fatalf("unexpected api auth attributes: %+v", apiAuth.Attributes)
	}
	if apiAuth.Attributes["region"] != "eu-west-1" || apiAuth.Attributes["force_global"] != "true" {
		t.Fatalf("unexpected region attrs: %+v", apiAuth.Attributes)
	}
	if apiAuth.Attributes["base_url"] != "https://bedrock.local" || apiAuth.Attributes["header:X-Bedrock"] != "test" {
		t.Fatalf("unexpected base/header attrs: %+v", apiAuth.Attributes)
	}
	if _, ok := apiAuth.Attributes["models_hash"]; !ok {
		t.Fatal("expected models_hash for bedrock models")
	}
	if apiAuth.ProxyURL != "http://proxy.local" || apiAuth.ProxyID != "hk" {
		t.Fatalf("unexpected proxy settings: proxy=%q proxyID=%q", apiAuth.ProxyURL, apiAuth.ProxyID)
	}
	if apiAuth.Attributes["excluded_models_hash"] == "" || apiAuth.Attributes["auth_kind"] != "apikey" {
		t.Fatalf("expected API key exclusion metadata, got %+v", apiAuth.Attributes)
	}

	sigAuth := auths[1]
	if sigAuth.Provider != "bedrock" || sigAuth.Label != "aws sigv4" {
		t.Fatalf("unexpected sigv4 auth identity: %+v", sigAuth)
	}
	if sigAuth.Attributes["auth_mode"] != "sigv4" {
		t.Fatalf("auth_mode = %q, want sigv4", sigAuth.Attributes["auth_mode"])
	}
	if sigAuth.Attributes["access_key_id"] != "AKIATEST" || sigAuth.Attributes["secret_access_key"] != "SECRET" || sigAuth.Attributes["session_token"] != "SESSION" {
		t.Fatalf("unexpected sigv4 attrs: %+v", sigAuth.Attributes)
	}
	if sigAuth.Attributes["api_key"] != "AKIATEST" {
		t.Fatalf("expected api_key identifier to use access key id for alias/account handling, got %+v", sigAuth.Attributes)
	}
}

func TestConfigSynthesizer_IDStability(t *testing.T) {
	cfg := &config.Config{
		GeminiKey: []config.GeminiKey{
			{APIKey: "stable-key", Prefix: "test"},
		},
	}

	// Generate IDs twice with fresh generators
	synth1 := NewConfigSynthesizer()
	ctx1 := &SynthesisContext{
		Config:      cfg,
		Now:         time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC),
		IDGenerator: NewStableIDGenerator(),
	}
	auths1, _ := synth1.Synthesize(ctx1)

	synth2 := NewConfigSynthesizer()
	ctx2 := &SynthesisContext{
		Config:      cfg,
		Now:         time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC),
		IDGenerator: NewStableIDGenerator(),
	}
	auths2, _ := synth2.Synthesize(ctx2)

	if auths1[0].ID != auths2[0].ID {
		t.Errorf("same config should produce same ID: got %q and %q", auths1[0].ID, auths2[0].ID)
	}
}

func TestConfigSynthesizer_AllProviders(t *testing.T) {
	synth := NewConfigSynthesizer()
	ctx := &SynthesisContext{
		Config: &config.Config{
			GeminiKey: []config.GeminiKey{
				{APIKey: "gemini-key"},
			},
			ClaudeKey: []config.ClaudeKey{
				{APIKey: "claude-key"},
			},
			CodexKey: []config.CodexKey{
				{APIKey: "codex-key"},
			},
			OpenCodeGoKey: []config.OpenCodeGoKey{
				{APIKey: "opencode-go-key"},
			},
			ClineKey: []config.ClineKey{
				{APIKey: "cline-key"},
			},
			OpenAICompatibility: []config.OpenAICompatibility{
				{Name: "compat", BaseURL: "https://compat.api"},
			},
			VertexCompatAPIKey: []config.VertexCompatKey{
				{APIKey: "vertex-key", BaseURL: "https://vertex.api"},
			},
			BedrockKey: []config.BedrockKey{
				{AuthMode: "api-key", APIKey: "bedrock-key"},
			},
		},
		Now:         time.Now(),
		IDGenerator: NewStableIDGenerator(),
	}

	auths, err := synth.Synthesize(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(auths) != 8 {
		t.Fatalf("expected 8 auths, got %d", len(auths))
	}

	providers := make(map[string]bool)
	for _, a := range auths {
		providers[a.Provider] = true
	}

	expected := []string{"gemini", "claude", "codex", "opencode-go", "cline", "compat", "vertex", "bedrock"}
	for _, p := range expected {
		if !providers[p] {
			t.Errorf("expected provider %s not found", p)
		}
	}
}
