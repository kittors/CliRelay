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
	if len(models) != 14 {
		t.Fatalf("expected 14 registered opencode-go models, got %d: %+v", len(models), models)
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
}
