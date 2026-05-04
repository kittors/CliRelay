package cliproxy

import (
	"context"
	"testing"

	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v6/sdk/config"
)

func TestEnsureExecutorsForAuth_BedrockBindsBedrockExecutor(t *testing.T) {
	service := &Service{
		cfg:         &config.Config{},
		coreManager: coreauth.NewManager(nil, nil, nil),
	}
	auth := &coreauth.Auth{
		ID:       "bedrock-auth",
		Provider: "bedrock",
		Status:   coreauth.StatusActive,
	}

	service.ensureExecutorsForAuth(auth)

	exec, ok := service.coreManager.Executor("bedrock")
	if !ok || exec == nil {
		t.Fatal("expected bedrock executor after bind")
	}
	if exec.Identifier() != "bedrock" {
		t.Fatalf("executor identifier = %q, want bedrock", exec.Identifier())
	}
}

func TestRegisterModelsForAuth_BedrockConfigModels(t *testing.T) {
	service := &Service{
		cfg: &config.Config{
			BedrockKey: []config.BedrockKey{
				{
					AuthMode: "api-key",
					APIKey:   "br-key",
					Models: []config.BedrockModel{
						{Name: "claude-sonnet-4-5", Alias: "aws-sonnet"},
						{Name: "claude-opus-4-5", Alias: "aws-opus"},
					},
					ExcludedModels: []string{"aws-opus"},
				},
			},
		},
	}
	auth := &coreauth.Auth{
		ID:       "bedrock-auth-models",
		Provider: "bedrock",
		Status:   coreauth.StatusActive,
		Attributes: map[string]string{
			"auth_kind": "apikey",
			"api_key":   "br-key",
		},
	}

	registry := GlobalModelRegistry()
	registry.UnregisterClient(auth.ID)
	t.Cleanup(func() {
		registry.UnregisterClient(auth.ID)
	})

	service.registerModelsForAuth(context.Background(), auth)

	models := registry.GetAvailableModelsByProvider("bedrock")
	if len(models) != 1 {
		t.Fatalf("expected 1 registered bedrock model after exclusion, got %d: %+v", len(models), models)
	}
	if models[0].ID != "aws-sonnet" {
		t.Fatalf("registered model id = %q, want aws-sonnet", models[0].ID)
	}
	if models[0].OwnedBy != "aws" || models[0].Type != "bedrock" {
		t.Fatalf("unexpected model ownership/type: %+v", models[0])
	}
}
