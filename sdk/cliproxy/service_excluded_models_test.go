package cliproxy

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	internalregistry "github.com/router-for-me/CLIProxyAPI/v6/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/usage"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/util"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v6/sdk/config"
)

func TestApplyConfigReloadRefreshesModelRegistryForConfigAuths(t *testing.T) {
	cfg := &config.Config{
		GeminiKey: []config.GeminiKey{{
			APIKey: "gemini-hot-reload-key",
			Models: []internalconfig.GeminiModel{{Name: "models/gemini-hot-reload-model", Alias: "gemini-hot-reload-model"}},
		}},
	}
	manager := coreauth.NewManager(nil, nil, nil)
	service := &Service{cfg: cfg, coreManager: manager}
	auth := &coreauth.Auth{
		ID:       "gemini-hot-reload-auth",
		Provider: "gemini",
		Status:   coreauth.StatusActive,
		Attributes: map[string]string{
			"api_key":   "gemini-hot-reload-key",
			"auth_kind": "apikey",
			"source":    "config:gemini[test]",
		},
	}
	ctx := context.Background()
	if _, err := manager.Register(ctx, auth); err != nil {
		t.Fatalf("register auth: %v", err)
	}

	registry := GlobalModelRegistry()
	registry.UnregisterClient(auth.ID)
	t.Cleanup(func() {
		registry.UnregisterClient(auth.ID)
	})

	service.registerModelsForAuth(ctx, auth)
	if !hasModelID(registry.GetAvailableModelsByProvider("gemini"), "gemini-hot-reload-model") {
		t.Fatal("expected model to be registered before config reload")
	}

	next := *cfg
	next.GeminiKey = []config.GeminiKey{{
		APIKey:         "gemini-hot-reload-key",
		Models:         []internalconfig.GeminiModel{{Name: "models/gemini-hot-reload-model", Alias: "gemini-hot-reload-model"}},
		ExcludedModels: []string{"*"},
	}}
	service.applyConfigReload(&next, true)

	if hasModelID(registry.GetAvailableModelsByProvider("gemini"), "gemini-hot-reload-model") {
		t.Fatal("expected model registry to drop disabled provider models after config reload")
	}
}

func hasModelID(models []*ModelInfo, id string) bool {
	for _, model := range models {
		if model != nil && strings.EqualFold(strings.TrimSpace(model.ID), id) {
			return true
		}
	}
	return false
}

func TestRegisterModelsForAuth_UsesPreMergedExcludedModelsAttribute(t *testing.T) {
	service := &Service{
		cfg: &config.Config{
			OAuthExcludedModels: map[string][]string{
				"gemini-cli": {"gemini-2.5-pro"},
			},
		},
	}
	auth := &coreauth.Auth{
		ID:       "auth-gemini-cli",
		Provider: "gemini-cli",
		Status:   coreauth.StatusActive,
		Attributes: map[string]string{
			"auth_kind":       "oauth",
			"excluded_models": "gemini-2.5-flash",
		},
	}

	registry := GlobalModelRegistry()
	registry.UnregisterClient(auth.ID)
	t.Cleanup(func() {
		registry.UnregisterClient(auth.ID)
	})

	service.registerModelsForAuth(context.Background(), auth)

	models := registry.GetAvailableModelsByProvider("gemini-cli")
	if len(models) == 0 {
		t.Fatal("expected gemini-cli models to be registered")
	}

	for _, model := range models {
		if model == nil {
			continue
		}
		modelID := strings.TrimSpace(model.ID)
		if strings.EqualFold(modelID, "gemini-2.5-flash") {
			t.Fatalf("expected model %q to be excluded by auth attribute", modelID)
		}
	}

	seenGlobalExcluded := false
	for _, model := range models {
		if model == nil {
			continue
		}
		if strings.EqualFold(strings.TrimSpace(model.ID), "gemini-2.5-pro") {
			seenGlobalExcluded = true
			break
		}
	}
	if !seenGlobalExcluded {
		t.Fatal("expected global excluded model to be present when attribute override is set")
	}
}

func TestRegisterModelsForAuth_OpenAICompatibilityRegistersConfigModels(t *testing.T) {
	service := &Service{
		cfg: &config.Config{
			OpenAICompatibility: []config.OpenAICompatibility{
				{
					Name: "qwen tokenplan",
					Models: []config.OpenAICompatibilityModel{
						{Name: "qwen3.6-plus"},
						{Name: "qwen-image-2.0"},
					},
				},
			},
		},
	}
	auth := &coreauth.Auth{
		ID:       "compat-qwen-tokenplan",
		Provider: "qwen tokenplan",
		Status:   coreauth.StatusActive,
		Attributes: map[string]string{
			"compat_name":  "qwen tokenplan",
			"provider_key": "qwen tokenplan",
		},
	}

	registry := GlobalModelRegistry()
	registry.UnregisterClient(auth.ID)
	t.Cleanup(func() {
		registry.UnregisterClient(auth.ID)
	})

	service.registerModelsForAuth(context.Background(), auth)

	models := registry.GetAvailableModelsByProvider("qwen tokenplan")
	ids := map[string]bool{}
	for _, model := range models {
		if model != nil {
			ids[strings.TrimSpace(model.ID)] = true
		}
	}
	for _, want := range []string{"qwen3.6-plus", "qwen-image-2.0"} {
		if !ids[want] {
			t.Fatalf("expected model %q to be registered, got %+v", want, ids)
		}
	}
}

func TestRegisterModelsForAuth_OpenAICompatibilityRegistersAliasesAndUpstreamNames(t *testing.T) {
	service := &Service{
		cfg: &config.Config{
			OpenAICompatibility: []config.OpenAICompatibility{
				{
					Name: "bigmodel-coding",
					Models: []config.OpenAICompatibilityModel{
						{Name: "glm-5.1", Alias: "gpt-5.3-codex"},
						{Name: "vendor/custom-model", Alias: "custom-public-model"},
						{Name: "direct-only-model"},
					},
				},
			},
		},
	}
	auth := &coreauth.Auth{
		ID:       "compat-bigmodel-coding",
		Provider: "bigmodel-coding",
		Status:   coreauth.StatusActive,
		Attributes: map[string]string{
			"auth_kind":    "apikey",
			"compat_name":  "bigmodel-coding",
			"provider_key": "bigmodel-coding",
		},
	}

	modelRegistry := internalregistry.GetGlobalRegistry()
	modelRegistry.UnregisterClient(auth.ID)
	t.Cleanup(func() {
		modelRegistry.UnregisterClient(auth.ID)
	})

	service.registerModelsForAuth(context.Background(), auth)

	for _, modelID := range []string{
		"gpt-5.3-codex",
		"glm-5.1",
		"custom-public-model",
		"vendor/custom-model",
		"direct-only-model",
	} {
		providers := modelRegistry.GetModelProviders(modelID)
		if len(providers) != 1 || providers[0] != "bigmodel-coding" {
			t.Fatalf("providers for %q = %#v, want [bigmodel-coding]", modelID, providers)
		}
	}
}

func TestRegisterModelsForAuth_AddsUserModelConfigsForOAuthProvider(t *testing.T) {
	usage.CloseDB()
	dbPath := filepath.Join(t.TempDir(), "usage.db")
	if err := usage.InitDB(dbPath, internalconfig.RequestLogStorageConfig{}, time.UTC); err != nil {
		t.Fatalf("InitDB() error = %v", err)
	}
	t.Cleanup(usage.CloseDB)

	if err := usage.UpsertModelConfig(usage.ModelConfigRow{
		ModelID:     "claude-opus-4-7",
		OwnedBy:     "claude",
		Description: "User-defined Claude OAuth model",
		Enabled:     true,
		Source:      "user",
	}); err != nil {
		t.Fatalf("UpsertModelConfig() error = %v", err)
	}

	service := &Service{cfg: &config.Config{}}
	auth := &coreauth.Auth{
		ID:       "claude-oauth-user-models",
		Provider: "claude",
		Status:   coreauth.StatusActive,
		Attributes: map[string]string{
			"auth_kind": "oauth",
		},
		Metadata: map[string]any{
			"email": "claude@example.com",
		},
	}

	registry := GlobalModelRegistry()
	registry.UnregisterClient(auth.ID)
	t.Cleanup(func() {
		registry.UnregisterClient(auth.ID)
	})

	service.registerModelsForAuth(context.Background(), auth)

	if !hasModelID(registry.GetAvailableModelsByProvider("claude"), "claude-opus-4-7") {
		t.Fatal("expected user-defined Claude model config to be registered for Claude OAuth auth")
	}
}

func TestRegisterModelsForAuth_ClaudeOAuthStaticMaxModelIsRouteableWithoutModelConfig(t *testing.T) {
	usage.CloseDB()
	dbPath := filepath.Join(t.TempDir(), "usage.db")
	if err := usage.InitDB(dbPath, internalconfig.RequestLogStorageConfig{}, time.UTC); err != nil {
		t.Fatalf("InitDB() error = %v", err)
	}
	t.Cleanup(usage.CloseDB)

	service := &Service{cfg: &config.Config{}}
	auth := &coreauth.Auth{
		ID:       "claude-oauth-static-max-models",
		Provider: "claude",
		Status:   coreauth.StatusActive,
		Metadata: map[string]any{
			"email": "claude@example.com",
		},
	}

	registry := GlobalModelRegistry()
	registry.UnregisterClient(auth.ID)
	t.Cleanup(func() {
		registry.UnregisterClient(auth.ID)
	})

	service.registerModelsForAuth(context.Background(), auth)

	providers := util.GetProviderName("claude-opus-4-7")
	for _, provider := range providers {
		if provider == "claude" {
			return
		}
	}
	t.Fatalf("expected claude-opus-4-7 to route to claude after Claude OAuth registration, got providers %v", providers)
}

func TestRegisterModelsForAuth_AddsUserModelConfigsForOAuthProviderInferredFromAccountInfo(t *testing.T) {
	usage.CloseDB()
	dbPath := filepath.Join(t.TempDir(), "usage.db")
	if err := usage.InitDB(dbPath, internalconfig.RequestLogStorageConfig{}, time.UTC); err != nil {
		t.Fatalf("InitDB() error = %v", err)
	}
	t.Cleanup(usage.CloseDB)

	if err := usage.UpsertModelConfig(usage.ModelConfigRow{
		ModelID:     "claude-opus-4-7",
		OwnedBy:     "anthropic",
		Description: "User-defined Claude OAuth model",
		Enabled:     true,
		Source:      "user",
	}); err != nil {
		t.Fatalf("UpsertModelConfig() error = %v", err)
	}

	service := &Service{cfg: &config.Config{}}
	auth := &coreauth.Auth{
		ID:       "claude-oauth-account-info-models",
		Provider: "claude",
		Status:   coreauth.StatusActive,
		Metadata: map[string]any{
			"email": "claude@example.com",
		},
	}

	registry := GlobalModelRegistry()
	registry.UnregisterClient(auth.ID)
	t.Cleanup(func() {
		registry.UnregisterClient(auth.ID)
	})

	service.registerModelsForAuth(context.Background(), auth)

	if !hasModelID(registry.GetAvailableModelsByProvider("claude"), "claude-opus-4-7") {
		t.Fatal("expected Claude OAuth model config to be registered when oauth is inferred from account info")
	}
}

func TestRegisterModelsForAuth_AddsOpenRouterAnthropicModelConfigsForOAuthProvider(t *testing.T) {
	usage.CloseDB()
	dbPath := filepath.Join(t.TempDir(), "usage.db")
	if err := usage.InitDB(dbPath, internalconfig.RequestLogStorageConfig{}, time.UTC); err != nil {
		t.Fatalf("InitDB() error = %v", err)
	}
	t.Cleanup(usage.CloseDB)

	if err := usage.UpsertModelConfig(usage.ModelConfigRow{
		ModelID:     "claude-opus-4-7",
		OwnedBy:     "anthropic",
		Description: "OpenRouter-synced Claude model",
		Enabled:     true,
		Source:      "openrouter",
	}); err != nil {
		t.Fatalf("UpsertModelConfig() error = %v", err)
	}

	service := &Service{cfg: &config.Config{}}
	auth := &coreauth.Auth{
		ID:       "claude-oauth-openrouter-models",
		Provider: "claude",
		Status:   coreauth.StatusActive,
		Attributes: map[string]string{
			"auth_kind": "oauth",
		},
		Metadata: map[string]any{
			"email": "claude@example.com",
		},
	}

	registry := GlobalModelRegistry()
	registry.UnregisterClient(auth.ID)
	t.Cleanup(func() {
		registry.UnregisterClient(auth.ID)
	})

	service.registerModelsForAuth(context.Background(), auth)

	if !hasModelID(registry.GetAvailableModelsByProvider("claude"), "claude-opus-4-7") {
		t.Fatal("expected OpenRouter-synced Anthropic model config to be registered for Claude OAuth auth")
	}
}

func TestRegisterModelsForAuth_AddsClaudeCodeModelConfigsForOAuthProvider(t *testing.T) {
	usage.CloseDB()
	dbPath := filepath.Join(t.TempDir(), "usage.db")
	if err := usage.InitDB(dbPath, internalconfig.RequestLogStorageConfig{}, time.UTC); err != nil {
		t.Fatalf("InitDB() error = %v", err)
	}
	t.Cleanup(usage.CloseDB)

	const modelID = "claude-code-custom-model"
	if err := usage.UpsertModelConfig(usage.ModelConfigRow{
		ModelID:     modelID,
		OwnedBy:     "claude-code",
		Description: "Claude Code model library entry",
		Enabled:     true,
		Source:      "seed",
	}); err != nil {
		t.Fatalf("UpsertModelConfig() error = %v", err)
	}

	service := &Service{cfg: &config.Config{}}
	auth := &coreauth.Auth{
		ID:       "claude-oauth-claude-code-models",
		Provider: "claude",
		Status:   coreauth.StatusActive,
		Attributes: map[string]string{
			"auth_kind": "oauth",
		},
		Metadata: map[string]any{
			"email": "claude@example.com",
		},
	}

	registry := GlobalModelRegistry()
	registry.UnregisterClient(auth.ID)
	t.Cleanup(func() {
		registry.UnregisterClient(auth.ID)
	})

	service.registerModelsForAuth(context.Background(), auth)

	if !hasModelID(registry.GetAvailableModelsByProvider("claude"), modelID) {
		t.Fatal("expected Claude Code model config to be registered for Claude OAuth auth")
	}
}
