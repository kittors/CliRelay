package cliproxy

import (
	"context"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	serviceapp "github.com/router-for-me/CLIProxyAPI/v8/sdkbridge/service"
)

func registeredIDs(t *testing.T, auth *coreauth.Auth) map[string]struct{} {
	t.Helper()
	ids := make(map[string]struct{})
	for _, model := range GlobalModelRegistry().GetModelsForClient(auth.ID) {
		if model != nil {
			ids[model.ID] = struct{}{}
		}
	}
	return ids
}

// TestCodexKeepsImageModelWhenChatModelsAreCurated is the reported failure. A
// deployment prunes retired chat models from the registered set; gpt-image-2 was
// swept up in that pruning, so the console could not select it, could not add it to
// a channel group, and reported it unavailable — while the credential panel showed
// it.
func TestCodexKeepsImageModelWhenChatModelsAreCurated(t *testing.T) {
	auth := &coreauth.Auth{
		ID: "codex-image", Provider: "codex", Status: coreauth.StatusActive,
		Metadata:   map[string]any{"access_token": "t"},
		Attributes: map[string]string{"auth_kind": "oauth"},
	}
	// Reproduce curation by excluding everything the deployment retired, which is
	// what an operator's exclusion list ends up doing.
	auth.Attributes["excluded_models"] = "gpt-5,gpt-5.1,gpt-5.2,gpt-5-codex,gpt-5-codex-mini,gpt-image-2"

	service := &Service{cfg: &config.Config{}}
	service.registerModelsForAuth(context.Background(), auth)
	t.Cleanup(func() { GlobalModelRegistry().UnregisterClient(auth.ID) })

	ids := registeredIDs(t, auth)
	if _, ok := ids["gpt-image-2"]; !ok {
		t.Error("gpt-image-2 must stay registered: it is available to every entitled account and is never reported by chat discovery")
	}
	// Curation of chat models still applies.
	if _, ok := ids["gpt-5.1"]; ok {
		t.Error("a retired chat model must remain excluded")
	}
}

func TestImageModelsAreNotCrossRegisteredBetweenProviders(t *testing.T) {
	auth := &coreauth.Auth{
		ID: "codex-only", Provider: "codex", Status: coreauth.StatusActive,
		Metadata:   map[string]any{"access_token": "t"},
		Attributes: map[string]string{"auth_kind": "oauth"},
	}
	service := &Service{cfg: &config.Config{}}
	service.registerModelsForAuth(context.Background(), auth)
	t.Cleanup(func() { GlobalModelRegistry().UnregisterClient(auth.ID) })

	// A Codex credential advertising Grok's models would fail at request time.
	if _, ok := registeredIDs(t, auth)["grok-imagine-image"]; ok {
		t.Error("a codex credential must not advertise xai image models")
	}
}

func TestWithRegisteredImageModelsDoesNotDuplicate(t *testing.T) {
	existing := []*ModelInfo{{ID: "gpt-image-2", DisplayName: "Live Definition"}}

	merged := serviceapp.WithRegisteredImageModels("codex", existing)
	count := 0
	for _, model := range merged {
		if model != nil && model.ID == "gpt-image-2" {
			count++
			if model.DisplayName != "Live Definition" {
				t.Error("the live definition was replaced by the static one")
			}
		}
	}
	if count != 1 {
		t.Errorf("gpt-image-2 appears %d times, want 1", count)
	}
}

func TestProvidersWithoutImageModelsAreUnchanged(t *testing.T) {
	existing := []*ModelInfo{{ID: "claude-opus-4-6"}}
	if merged := serviceapp.WithRegisteredImageModels("claude", existing); len(merged) != 1 {
		t.Errorf("merged = %d models, want the input unchanged", len(merged))
	}
}
