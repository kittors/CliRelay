package cliproxy

import (
	"context"
	"testing"

	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v6/sdk/config"
)

func TestEnsureExecutorsForAuth_ClineBindsOpenAICompatExecutor(t *testing.T) {
	service := &Service{
		cfg:         &config.Config{},
		coreManager: coreauth.NewManager(nil, nil, nil),
	}
	auth := &coreauth.Auth{
		ID:       "cline-auth",
		Provider: "cline",
		Status:   coreauth.StatusActive,
		Attributes: map[string]string{
			"provider_key": "cline",
			"compat_name":  "Cline",
		},
	}

	service.ensureExecutorsForAuth(auth)

	exec, ok := service.coreManager.Executor("cline")
	if !ok || exec == nil {
		t.Fatal("expected cline executor after bind")
	}
	if exec.Identifier() != "cline" {
		t.Fatalf("executor identifier = %q, want cline", exec.Identifier())
	}
}

func TestRegisterModelsForAuth_ClineRegistersDefaultModels(t *testing.T) {
	service := &Service{cfg: &config.Config{}}
	auth := &coreauth.Auth{
		ID:       "cline-auth-models",
		Provider: "cline",
		Status:   coreauth.StatusActive,
		Attributes: map[string]string{
			"auth_kind": "apikey",
			"api_key":   "cline-key",
		},
	}

	registry := GlobalModelRegistry()
	registry.UnregisterClient(auth.ID)
	t.Cleanup(func() {
		registry.UnregisterClient(auth.ID)
	})

	service.registerModelsForAuth(context.Background(), auth)

	models := registry.GetModelsForClient(auth.ID)
	if len(models) != 10 {
		t.Fatalf("expected 10 registered cline models, got %d: %+v", len(models), models)
	}
	if !hasModelID(models, "cline-pass/glm-5.2") {
		t.Fatalf("cline-pass/glm-5.2 not registered; got %+v", models)
	}
	if !hasModelID(models, "cline-pass/minimax-m3") {
		t.Fatalf("cline-pass/minimax-m3 not registered; got %+v", models)
	}
}

func TestRegisterModelsForAuth_ClineUsesExplicitAndExcludedModels(t *testing.T) {
	service := &Service{cfg: &config.Config{
		ClineKey: []config.ClineKey{{
			APIKey: "cline-key-explicit",
			Models: []config.ClineModel{
				{Name: "cline-pass/glm-5.2"},
				{Name: "cline-pass/new-model"},
			},
			ExcludedModels: []string{"cline-pass/glm-5.2"},
		}},
	}}
	auth := &coreauth.Auth{
		ID:       "cline-auth-explicit-models",
		Provider: "cline",
		Status:   coreauth.StatusActive,
		Attributes: map[string]string{
			"auth_kind": "apikey",
			"api_key":   "cline-key-explicit",
		},
	}

	registry := GlobalModelRegistry()
	registry.UnregisterClient(auth.ID)
	t.Cleanup(func() {
		registry.UnregisterClient(auth.ID)
	})

	service.registerModelsForAuth(context.Background(), auth)

	models := registry.GetModelsForClient(auth.ID)
	if len(models) != 1 {
		t.Fatalf("expected 1 explicit cline model after exclusion, got %d: %+v", len(models), models)
	}
	if hasModelID(models, "cline-pass/glm-5.2") {
		t.Fatalf("cline-pass/glm-5.2 should be excluded; got %+v", models)
	}
	if !hasModelID(models, "cline-pass/new-model") {
		t.Fatalf("cline-pass/new-model not registered; got %+v", models)
	}
}

func TestRegisterModelsForAuth_ClineRegistersAliasWithUpstreamModelID(t *testing.T) {
	service := &Service{cfg: &config.Config{
		ClineKey: []config.ClineKey{{
			APIKey: "cline-key-alias",
			Models: []config.ClineModel{
				{Name: "cline-pass/mimo-v2.5-pro", Alias: "mimo-v2.5-pro"},
			},
		}},
	}}
	auth := &coreauth.Auth{
		ID:       "cline-auth-alias-models",
		Provider: "cline",
		Status:   coreauth.StatusActive,
		Attributes: map[string]string{
			"auth_kind": "apikey",
			"api_key":   "cline-key-alias",
		},
	}

	registry := GlobalModelRegistry()
	registry.UnregisterClient(auth.ID)
	t.Cleanup(func() {
		registry.UnregisterClient(auth.ID)
	})

	service.registerModelsForAuth(context.Background(), auth)

	models := registry.GetModelsForClient(auth.ID)
	for _, model := range models {
		if model.ID == "mimo-v2.5-pro" {
			if model.UpstreamModelID != "cline-pass/mimo-v2.5-pro" {
				t.Fatalf("UpstreamModelID = %q, want cline-pass/mimo-v2.5-pro", model.UpstreamModelID)
			}
			return
		}
	}
	t.Fatalf("mimo-v2.5-pro alias not registered; got %+v", models)
}
