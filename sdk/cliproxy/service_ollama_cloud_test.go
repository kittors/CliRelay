package cliproxy

import (
	"context"
	"testing"

	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v6/sdk/config"
)

func TestRegisterModelsForAuth_OllamaCloudFiltersDirtyClinePassModels(t *testing.T) {
	service := &Service{cfg: &config.Config{
		OllamaCloudKey: []config.OllamaCloudKey{{
			APIKey:  "ollama-key-dirty",
			BaseURL: config.DefaultOllamaCloudBaseURL,
			Models: []config.OllamaCloudModel{
				{Name: "cline-pass/glm-5.2"},
				{Name: "glm-5.2"},
			},
		}},
	}}
	auth := &coreauth.Auth{
		ID:       "ollama-cloud-auth-dirty-models",
		Provider: "ollama-cloud",
		Status:   coreauth.StatusActive,
		Attributes: map[string]string{
			"auth_kind":    "apikey",
			"api_key":      "ollama-key-dirty",
			"base_url":     config.DefaultOllamaCloudBaseURL,
			"compat_name":  "Ollama Cloud",
			"provider_key": "ollama-cloud",
		},
	}

	registry := GlobalModelRegistry()
	registry.UnregisterClient(auth.ID)
	t.Cleanup(func() {
		registry.UnregisterClient(auth.ID)
	})

	service.registerModelsForAuth(context.Background(), auth)

	models := registry.GetModelsForClient(auth.ID)
	if len(models) != 1 || !hasModelID(models, "glm-5.2") {
		t.Fatalf("expected only valid Ollama Cloud model after dirty filtering, got %+v", models)
	}
	if hasModelID(models, "cline-pass/glm-5.2") {
		t.Fatalf("dirty ClinePass model should not be registered for Ollama Cloud; got %+v", models)
	}
	if !hasProvider(registry.GetModelProviders("glm-5.2"), "ollama-cloud") {
		t.Fatalf("expected Ollama Cloud provider for glm-5.2, got %+v", registry.GetModelProviders("glm-5.2"))
	}
}

func hasProvider(providers []string, want string) bool {
	for _, provider := range providers {
		if provider == want {
			return true
		}
	}
	return false
}
