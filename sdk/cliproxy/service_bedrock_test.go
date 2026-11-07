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

// A Bedrock key may list a model by its Bedrock id. Its thinking capabilities
// are those of the catalog entry for the Anthropic id behind it; without them
// a budget is passed to a model that only takes adaptive thinking.
func TestLookupBedrockStaticModelThinkingResolvesBedrockIDs(t *testing.T) {
	for _, name := range []string{
		"claude-opus-4-7",
		"us.anthropic.claude-opus-4-7-v1",
		"global.anthropic.claude-opus-4-7-v1:0",
		"arn:aws:bedrock:us-east-1:123456789012:inference-profile/us.anthropic.claude-opus-4-7-v1",
	} {
		if got := lookupBedrockStaticModelThinking(name); got == nil || got.Adaptive == nil {
			t.Errorf("lookupBedrockStaticModelThinking(%q) = %+v, want claude-opus-4-7's adaptive-only thinking", name, got)
		}
	}
	if got := lookupBedrockStaticModelThinking("us.anthropic.claude-sonnet-4-5-20250929-v1:0"); got == nil || got.Adaptive != nil {
		t.Errorf("claude-sonnet-4-5 thinking = %+v, want budget thinking", got)
	}
	for _, name := range []string{"meta.llama3-70b-instruct-v1:0", "arn:aws:bedrock:us-east-1:123456789012:application-inference-profile/a1b2c3"} {
		if got := lookupBedrockStaticModelThinking(name); got != nil {
			t.Errorf("lookupBedrockStaticModelThinking(%q) = %+v, want nil", name, got)
		}
	}
}

func TestRegisterModelsForAuth_BedrockIDsGetCatalogThinking(t *testing.T) {
	service := &Service{
		cfg: &config.Config{
			BedrockKey: []config.BedrockKey{{
				AuthMode: "api-key",
				APIKey:   "br-ids-key",
				Models:   []config.BedrockModel{{Name: "us.anthropic.claude-opus-4-7-v1", Alias: "aws-opus-47"}},
			}},
		},
	}
	auth := &coreauth.Auth{
		ID:         "bedrock-auth-bedrock-ids",
		Provider:   "bedrock",
		Status:     coreauth.StatusActive,
		Attributes: map[string]string{"auth_kind": "apikey", "api_key": "br-ids-key"},
	}
	registry := GlobalModelRegistry()
	registry.UnregisterClient(auth.ID)
	t.Cleanup(func() { registry.UnregisterClient(auth.ID) })

	service.registerModelsForAuth(context.Background(), auth)

	for _, model := range registry.GetAvailableModelsByProvider("bedrock") {
		if model.ID != "aws-opus-47" {
			continue
		}
		if model.Thinking == nil || model.Thinking.Adaptive == nil {
			t.Fatalf("aws-opus-47 thinking = %+v, want claude-opus-4-7's adaptive-only thinking", model.Thinking)
		}
		return
	}
	t.Fatal("aws-opus-47 not registered")
}
