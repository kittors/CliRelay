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

func TestRegisterModelsForAuth_ClineUsesPerKeyModels(t *testing.T) {
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
	if len(models) != 2 {
		t.Fatalf("expected configured cline models, got %d: %+v", len(models), models)
	}
	if !hasModelID(models, "cline-pass/new-model") {
		t.Fatalf("per-key ClinePass model should be registered; got %+v", models)
	}
	if !hasModelID(models, "cline-pass/glm-5.2") {
		t.Fatalf("specific ClinePass exclusion should be ignored; got %+v", models)
	}
}

func TestRegisterModelsForAuth_ClineFiltersDirtyNonClinePassModels(t *testing.T) {
	service := &Service{cfg: &config.Config{
		ClineKey: []config.ClineKey{{
			APIKey: "cline-key-dirty",
			Models: []config.ClineModel{
				{Name: "glm-5.2"},
				{Name: "cline-pass/qwen3.7-max"},
			},
		}},
	}}
	auth := &coreauth.Auth{
		ID:       "cline-auth-dirty-models",
		Provider: "cline",
		Status:   coreauth.StatusActive,
		Attributes: map[string]string{
			"auth_kind": "apikey",
			"api_key":   "cline-key-dirty",
		},
	}

	registry := GlobalModelRegistry()
	registry.UnregisterClient(auth.ID)
	t.Cleanup(func() {
		registry.UnregisterClient(auth.ID)
	})

	service.registerModelsForAuth(context.Background(), auth)

	models := registry.GetModelsForClient(auth.ID)
	if len(models) != 1 || !hasModelID(models, "cline-pass/qwen3.7-max") {
		t.Fatalf("expected only valid ClinePass model after dirty filtering, got %+v", models)
	}
	if hasModelID(models, "glm-5.2") {
		t.Fatalf("dirty non-ClinePass model should not be registered for Cline; got %+v", models)
	}
}

func TestRegisterModelsForAuth_ClineUsesPerKeyAlias(t *testing.T) {
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
	if !hasModelID(models, "mimo-v2.5-pro") {
		t.Fatalf("per-key ClinePass alias should be registered; got %+v", models)
	}
	if hasModelID(models, "cline-pass/mimo-v2.5-pro") {
		t.Fatalf("aliased ClinePass upstream id should not be registered separately; got %+v", models)
	}
}
