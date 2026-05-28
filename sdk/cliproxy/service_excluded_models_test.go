package cliproxy

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v6/internal/config"
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

func TestApplyConfigReloadDisableAllModelsPreventsClaudeAPIKeySelection(t *testing.T) {
	cfg := &config.Config{
		Routing: config.RoutingConfig{IncludeDefaultGroup: true},
		ClaudeKey: []config.ClaudeKey{{
			APIKey:  "sk-claude-hot-reload-key",
			Name:    "Kimi渠道",
			BaseURL: "https://api.kimi.com/coding/",
			Models:  []internalconfig.ClaudeModel{{Name: "K2.6", Alias: "claude-sonnet-4-6"}},
		}},
	}
	manager := coreauth.NewManager(nil, nil, nil)
	service := &Service{cfg: cfg, coreManager: manager}
	auth := &coreauth.Auth{
		ID:       "claude-hot-reload-auth",
		Provider: "claude",
		Label:    "Kimi渠道",
		Status:   coreauth.StatusActive,
		Attributes: map[string]string{
			"api_key":   "sk-claude-hot-reload-key",
			"base_url":  "https://api.kimi.com/coding/",
			"auth_kind": "apikey",
			"source":    "config:claude[test]",
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
	if !manager.CanServeModelWithScopes("claude-sonnet-4-6", nil, nil, "") {
		t.Fatal("expected model to be routeable before disable-all config")
	}

	next := *cfg
	next.ClaudeKey = []config.ClaudeKey{{
		APIKey:         "sk-claude-hot-reload-key",
		Name:           "Kimi渠道",
		BaseURL:        "https://api.kimi.com/coding/",
		Models:         []internalconfig.ClaudeModel{{Name: "K2.6", Alias: "claude-sonnet-4-6"}},
		ExcludedModels: []string{"*"},
	}}
	service.applyConfigReload(&next, true)

	updated, ok := manager.GetByID(auth.ID)
	if !ok {
		t.Fatal("expected config auth to remain registered as disabled")
	}
	if !updated.Disabled || updated.Status != coreauth.StatusDisabled {
		t.Fatalf("expected disabled config auth, got disabled=%t status=%s", updated.Disabled, updated.Status)
	}
	if manager.CanServeModelWithScopes("claude-sonnet-4-6", nil, nil, "") {
		t.Fatal("disabled config auth should not be routeable")
	}
}

func TestApplyConfigReloadDropsDisabledOpenAICompatibleProviderModels(t *testing.T) {
	cfg := &config.Config{
		OpenAICompatibility: []config.OpenAICompatibility{{
			Name:    "Compat Hot Reload",
			BaseURL: "https://compat.example.com/v1",
			Models: []config.OpenAICompatibilityModel{{
				Name:  "upstream-compat-hot-reload-model",
				Alias: "compat-hot-reload-model",
			}},
		}},
	}
	manager := coreauth.NewManager(nil, nil, nil)
	service := &Service{cfg: cfg, coreManager: manager}
	auth := &coreauth.Auth{
		ID:       "compat-hot-reload-auth",
		Provider: "compat hot reload",
		Status:   coreauth.StatusActive,
		Attributes: map[string]string{
			"auth_kind":    "apikey",
			"compat_name":  "Compat Hot Reload",
			"provider_key": "compat hot reload",
			"source":       "config:compat hot reload[test]",
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
	if !hasModelID(registry.GetAvailableModelsByProvider("compat hot reload"), "compat-hot-reload-model") {
		t.Fatal("expected compatible provider model to be registered before config reload")
	}

	next := *cfg
	next.OpenAICompatibility = []config.OpenAICompatibility{{
		Name:     "Compat Hot Reload",
		Disabled: true,
		BaseURL:  "https://compat.example.com/v1",
		Models: []config.OpenAICompatibilityModel{{
			Name:  "upstream-compat-hot-reload-model",
			Alias: "compat-hot-reload-model",
		}},
	}}
	service.applyConfigReload(&next, true)

	if hasModelID(registry.GetAvailableModelsByProvider("compat hot reload"), "compat-hot-reload-model") {
		t.Fatal("expected model registry to drop disabled OpenAI-compatible provider models after config reload")
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
