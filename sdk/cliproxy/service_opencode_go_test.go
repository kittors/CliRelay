package cliproxy

import (
	"context"
	"testing"

	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v6/sdk/config"
)

func TestEnsureExecutorsForAuth_OpenCodeGoBindsOpenCodeGoExecutor(t *testing.T) {
	service := &Service{
		cfg:         &config.Config{},
		coreManager: coreauth.NewManager(nil, nil, nil),
	}
	auth := &coreauth.Auth{
		ID:       "opencode-go-auth",
		Provider: "opencode-go",
		Status:   coreauth.StatusActive,
	}

	service.ensureExecutorsForAuth(auth)

	exec, ok := service.coreManager.Executor("opencode-go")
	if !ok || exec == nil {
		t.Fatal("expected opencode-go executor after bind")
	}
	if exec.Identifier() != "opencode-go" {
		t.Fatalf("executor identifier = %q, want opencode-go", exec.Identifier())
	}
}

func TestRegisterModelsForAuth_OpenCodeGoRegistersAllDefaultModels(t *testing.T) {
	service := &Service{cfg: &config.Config{}}
	auth := &coreauth.Auth{
		ID:       "opencode-go-auth-models",
		Provider: "opencode-go",
		Status:   coreauth.StatusActive,
		Attributes: map[string]string{
			"auth_kind": "apikey",
			"api_key":   "go-key",
		},
	}

	registry := GlobalModelRegistry()
	registry.UnregisterClient(auth.ID)
	t.Cleanup(func() {
		registry.UnregisterClient(auth.ID)
	})

	service.registerModelsForAuth(context.Background(), auth)

	models := registry.GetAvailableModelsByProvider("opencode-go")
	if len(models) != 20 {
		t.Fatalf("expected 20 registered opencode-go models, got %d: %+v", len(models), models)
	}
	ids := make(map[string]struct{}, len(models))
	for _, model := range models {
		if model != nil {
			ids[model.ID] = struct{}{}
		}
	}
	if _, ok := ids["deepseek-v4-flash"]; !ok {
		t.Fatalf("deepseek-v4-flash not registered; got ids %#v", ids)
	}
	if _, ok := ids["minimax-m2.7"]; !ok {
		t.Fatalf("minimax-m2.7 not registered; got ids %#v", ids)
	}
	if _, ok := ids["kimi-k2.7-code"]; !ok {
		t.Fatalf("kimi-k2.7-code not registered; got ids %#v", ids)
	}
}

func TestRegisterModelsForAuth_OpenCodeGoUsesPerKeyModels(t *testing.T) {
	service := &Service{cfg: &config.Config{
		OpenCodeGoKey: []config.OpenCodeGoKey{{
			APIKey: "go-key-explicit",
			Models: []config.OpenCodeGoModel{
				{Name: "qwen3.7-max"},
				{Name: "official-new-model"},
			},
		}},
	}}
	auth := &coreauth.Auth{
		ID:       "opencode-go-auth-explicit-models",
		Provider: "opencode-go",
		Status:   coreauth.StatusActive,
		Attributes: map[string]string{
			"auth_kind": "apikey",
			"api_key":   "go-key-explicit",
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
		t.Fatalf("expected configured opencode-go models, got %d: %+v", len(models), models)
	}
	if !hasModelID(models, "official-new-model") {
		t.Fatalf("per-key OpenCode Go model should be registered; got %+v", models)
	}
	if hasModelID(models, "deepseek-v4-flash") {
		t.Fatalf("configured OpenCode Go models should replace defaults; got %+v", models)
	}
}

func TestRegisterModelsForAuth_OpenCodeGoFiltersDirtyClinePassModels(t *testing.T) {
	service := &Service{cfg: &config.Config{
		OpenCodeGoKey: []config.OpenCodeGoKey{{
			APIKey: "go-key-dirty",
			Models: []config.OpenCodeGoModel{
				{Name: "cline-pass/glm-5.2"},
				{Name: "qwen3.7-max"},
			},
		}},
	}}
	auth := &coreauth.Auth{
		ID:       "opencode-go-auth-dirty-models",
		Provider: "opencode-go",
		Status:   coreauth.StatusActive,
		Attributes: map[string]string{
			"auth_kind": "apikey",
			"api_key":   "go-key-dirty",
		},
	}

	registry := GlobalModelRegistry()
	registry.UnregisterClient(auth.ID)
	t.Cleanup(func() {
		registry.UnregisterClient(auth.ID)
	})

	service.registerModelsForAuth(context.Background(), auth)

	models := registry.GetModelsForClient(auth.ID)
	if len(models) != 1 || !hasModelID(models, "qwen3.7-max") {
		t.Fatalf("expected only valid OpenCode Go model after dirty filtering, got %+v", models)
	}
	if hasModelID(models, "cline-pass/glm-5.2") {
		t.Fatalf("dirty ClinePass model should not be registered for OpenCode Go; got %+v", models)
	}
}

func TestRegisterModelsForAuth_OpenCodeGoUnregistersWhenOnlyDirtyModelsRemain(t *testing.T) {
	service := &Service{cfg: &config.Config{
		OpenCodeGoKey: []config.OpenCodeGoKey{{
			APIKey: "go-key-only-dirty",
			Models: []config.OpenCodeGoModel{
				{Name: "cline-pass/glm-5.2"},
			},
		}},
	}}
	auth := &coreauth.Auth{
		ID:       "opencode-go-auth-only-dirty-models",
		Provider: "opencode-go",
		Status:   coreauth.StatusActive,
		Attributes: map[string]string{
			"auth_kind": "apikey",
			"api_key":   "go-key-only-dirty",
		},
	}

	registry := GlobalModelRegistry()
	registry.UnregisterClient(auth.ID)
	t.Cleanup(func() {
		registry.UnregisterClient(auth.ID)
	})

	service.registerModelsForAuth(context.Background(), auth)

	models := registry.GetModelsForClient(auth.ID)
	if len(models) != 0 {
		t.Fatalf("expected no OpenCode Go models after all dirty models were filtered, got %+v", models)
	}
}
