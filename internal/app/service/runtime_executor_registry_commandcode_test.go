package serviceapp

import (
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
	sdkmodelcatalog "github.com/router-for-me/CLIProxyAPI/v8/sdk/modelcatalog"
)

func commandCodeAuth() *coreauth.Auth {
	return &coreauth.Auth{
		ID:       "commandcode-auth",
		Provider: "commandcode",
		Status:   coreauth.StatusActive,
		Attributes: map[string]string{
			"api_key":      "cc-key",
			"base_url":     config.DefaultCommandCodeBaseURL,
			"compat_name":  "Command Code",
			"provider_key": "commandcode",
		},
	}
}

// Command Code speaks plain OpenAI, so it must land on the shared compat
// executor rather than falling through to a provider-specific one.
func TestRegisterExecutorForAuthCommandCodeUsesOpenAICompatExecutor(t *testing.T) {
	manager := coreauth.NewManager(nil, nil, nil)

	RegisterExecutorForAuth(manager, &config.Config{}, commandCodeAuth(), false, nil)

	got, ok := manager.Executor("commandcode")
	if !ok || got == nil {
		t.Fatal("expected commandcode executor after bind")
	}
	compat, ok := got.(*executor.OpenAICompatExecutor)
	if !ok {
		t.Fatalf("executor = %T, want *executor.OpenAICompatExecutor", got)
	}
	if compat.Identifier() != "commandcode" {
		t.Fatalf("identifier = %q, want commandcode", compat.Identifier())
	}
}

// Without configured models the channel serves the static catalog snapshot.
func TestSyncDynamicConfigAuthModelsCommandCodeFallsBackToCatalog(t *testing.T) {
	reg := &testModelRegistry{}
	cfg := &config.Config{CommandCodeKey: []config.CommandCodeKey{{
		APIKey:  "cc-key",
		BaseURL: config.DefaultCommandCodeBaseURL,
	}}}

	syncDynamicConfigAuthModels(reg, cfg, commandCodeAuth())

	if len(reg.models) == 0 {
		t.Fatal("expected the static Command Code catalog to be registered")
	}
	for _, model := range reg.models {
		if model.OwnedBy != "command-code" {
			t.Fatalf("model %q owned_by = %q, want command-code", model.ID, model.OwnedBy)
		}
	}
}

// An explicit model list wins over the snapshot, which is how an operator routes
// a model that shipped upstream after this build.
func TestSyncDynamicConfigAuthModelsCommandCodeHonoursConfiguredModels(t *testing.T) {
	reg := &testModelRegistry{}
	cfg := &config.Config{CommandCodeKey: []config.CommandCodeKey{{
		APIKey:  "cc-key",
		BaseURL: config.DefaultCommandCodeBaseURL,
		Models: []config.CommandCodeModel{
			{Name: "model-that-shipped-yesterday"},
			{Name: "cline-pass/glm-5.2"},
		},
	}}}

	syncDynamicConfigAuthModels(reg, cfg, commandCodeAuth())

	ids := make(map[string]struct{}, len(reg.models))
	for _, model := range reg.models {
		ids[model.ID] = struct{}{}
	}
	if _, ok := ids["model-that-shipped-yesterday"]; !ok {
		t.Fatalf("configured model missing from %v", modelIDs(reg.models))
	}
	// cline-pass ids belong to the Cline channel and must not route here.
	if _, ok := ids["cline-pass/glm-5.2"]; ok {
		t.Fatalf("cline-pass row leaked into Command Code: %v", modelIDs(reg.models))
	}
}

func modelIDs(models []*sdkmodelcatalog.ModelInfo) []string {
	out := make([]string, 0, len(models))
	for _, model := range models {
		out = append(out, model.ID)
	}
	return out
}
