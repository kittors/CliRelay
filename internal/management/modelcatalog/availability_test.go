package modelcatalog

import (
	"context"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/usage"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

func TestConfiguredAvailabilityIncludesModelSources(t *testing.T) {
	const modelID = "source-test-model"
	const codexClientID = "source-test-codex"
	const openCodeClientID = "source-test-opencode"

	modelRegistry := registry.GetGlobalRegistry()
	modelRegistry.UnregisterClient(codexClientID)
	modelRegistry.UnregisterClient(openCodeClientID)
	t.Cleanup(func() {
		modelRegistry.UnregisterClient(codexClientID)
		modelRegistry.UnregisterClient(openCodeClientID)
	})

	modelRegistry.RegisterClient(codexClientID, "codex", []*registry.ModelInfo{{ID: modelID, Object: "model", OwnedBy: "openai"}})
	modelRegistry.RegisterClient(openCodeClientID, "opencode-go", []*registry.ModelInfo{{ID: modelID, Object: "model", OwnedBy: "opencode"}})

	manager := coreauth.NewManager(nil, nil, nil)
	if _, err := manager.Register(context.Background(), &coreauth.Auth{ID: codexClientID, Provider: "codex", Label: "Codex Pro", Status: coreauth.StatusActive}); err != nil {
		t.Fatalf("register codex auth: %v", err)
	}
	if _, err := manager.Register(context.Background(), &coreauth.Auth{ID: openCodeClientID, Provider: "opencode-go", Label: "OpenCode Go", Status: coreauth.StatusActive}); err != nil {
		t.Fatalf("register opencode auth: %v", err)
	}

	result := New(&config.Config{}, manager).ConfiguredAvailability("", "")
	data, ok := result["data"].([]map[string]any)
	if !ok {
		t.Fatalf("data = %#v, want []map[string]any", result["data"])
	}

	var sources []map[string]any
	for _, item := range data {
		if item["id"] == modelID {
			sources, _ = item["sources"].([]map[string]any)
			break
		}
	}
	if len(sources) != 2 {
		t.Fatalf("sources = %#v, want two sources", sources)
	}
	labels := map[string]bool{}
	for _, source := range sources {
		labels[source["label"].(string)] = true
	}
	if !labels["codex · Codex Pro"] || !labels["opencode-go · OpenCode Go"] {
		t.Fatalf("source labels = %#v", labels)
	}
}

func TestConfiguredAvailabilityIncludesClineAliasUpstreamModelID(t *testing.T) {
	const modelID = "mimo-v2.5-pro"
	const upstreamModelID = "cline-pass/mimo-v2.5-pro"
	const clientID = "source-test-cline-alias"

	modelRegistry := registry.GetGlobalRegistry()
	modelRegistry.UnregisterClient(clientID)
	t.Cleanup(func() {
		modelRegistry.UnregisterClient(clientID)
	})

	modelRegistry.RegisterClient(clientID, "cline", []*registry.ModelInfo{{
		ID:              modelID,
		Object:          "model",
		OwnedBy:         "cline",
		Type:            "cline",
		DisplayName:     upstreamModelID,
		UpstreamModelID: upstreamModelID,
		UserDefined:     true,
	}})

	manager := coreauth.NewManager(nil, nil, nil)
	if _, err := manager.Register(context.Background(), &coreauth.Auth{ID: clientID, Provider: "cline", Label: "Cline", Status: coreauth.StatusActive}); err != nil {
		t.Fatalf("register cline auth: %v", err)
	}

	result := New(&config.Config{}, manager).ConfiguredAvailability("", "")
	data, ok := result["data"].([]map[string]any)
	if !ok {
		t.Fatalf("data = %#v, want []map[string]any", result["data"])
	}

	var sources []map[string]any
	for _, item := range data {
		if item["id"] == modelID {
			sources, _ = item["sources"].([]map[string]any)
			break
		}
	}
	if len(sources) != 1 {
		t.Fatalf("sources = %#v, want one cline source", sources)
	}
	source := sources[0]
	if source["provider"] != "cline" || source["model_id"] != modelID || source["upstream_model_id"] != upstreamModelID {
		t.Fatalf("source = %#v, want cline alias with upstream model id", source)
	}
}

func TestDefaultMappedOwnerRowsKeepProviderModelWithoutConfigRow(t *testing.T) {
	const modelID = "glm-5.2"
	const clientID = "source-test-ollama-cloud"

	modelRegistry := registry.GetGlobalRegistry()
	modelRegistry.UnregisterClient(clientID)
	t.Cleanup(func() {
		modelRegistry.UnregisterClient(clientID)
	})

	modelRegistry.RegisterClient(clientID, "ollama-cloud", []*registry.ModelInfo{{
		ID:      modelID,
		Object:  "model",
		OwnedBy: "ollama",
	}})

	authByID := map[string]*coreauth.Auth{
		clientID: {ID: clientID, Provider: "ollama-cloud", Label: "Ollama Cloud", Status: coreauth.StatusActive},
	}
	ownerMappings := map[string]string{"ollama-cloud": "ollama"}
	ownerKeys := map[string]bool{"ollama": true}
	models := []map[string]any{{"id": modelID, "object": "model", "owned_by": "ollama"}}

	got := withDefaultMappedOwnerRows(modelRegistry, models, nil, ownerKeys, nil, authByID, ownerMappings)
	if len(got) != 1 || got[0]["id"] != modelID {
		t.Fatalf("models = %#v, want provider model kept when no enabled mapped-owner config row exists", got)
	}
}

func TestDefaultMappedOwnerRowsReplaceProviderModelWhenConfigRowExists(t *testing.T) {
	const modelID = "qwen3.7-max"
	const clientID = "source-test-cline-replace"

	modelRegistry := registry.GetGlobalRegistry()
	modelRegistry.UnregisterClient(clientID)
	t.Cleanup(func() {
		modelRegistry.UnregisterClient(clientID)
	})

	modelRegistry.RegisterClient(clientID, "cline", []*registry.ModelInfo{{
		ID:      modelID,
		Object:  "model",
		OwnedBy: "cline",
	}})

	authByID := map[string]*coreauth.Auth{
		clientID: {ID: clientID, Provider: "cline", Label: "ClinePass", Status: coreauth.StatusActive},
	}
	ownerMappings := map[string]string{"cline": "cline"}
	ownerKeys := map[string]bool{"cline": true}
	models := []map[string]any{{"id": modelID, "object": "model", "owned_by": "cline"}}
	rows := []usage.ModelConfigRow{{
		ModelID: modelID,
		OwnedBy: "cline",
		Enabled: true,
		Source:  "seed",
	}}

	got := withDefaultMappedOwnerRows(modelRegistry, models, rows, ownerKeys, map[string]bool{modelID: true}, authByID, ownerMappings)
	if len(got) != 1 || got[0]["id"] != modelID || got[0]["source"] != "seed" {
		t.Fatalf("models = %#v, want mapped-owner config row to replace matching provider registry model", got)
	}
}

func TestDefaultMappedOwnerRowsSkipConfigRowWithoutRuntimeSource(t *testing.T) {
	const modelID = "gpt-5.6-sol"

	ownerMappings := map[string]string{"codex": "codex"}
	ownerKeys := map[string]bool{"codex": true}
	rows := []usage.ModelConfigRow{{
		ModelID: modelID,
		OwnedBy: "codex",
		Enabled: true,
		Source:  "seed",
	}}

	got := withDefaultMappedOwnerRows(
		registry.GetGlobalRegistry(),
		nil,
		rows,
		ownerKeys,
		map[string]bool{modelID: true},
		map[string]*coreauth.Auth{},
		ownerMappings,
	)
	if len(got) != 0 {
		t.Fatalf("models = %#v, want unserviceable mapped-owner config row hidden", got)
	}
}

func TestModelSourceEntriesKeepMappedProviderSourceForRetainedRegistryModel(t *testing.T) {
	const modelID = "glm-5.2"
	const clientID = "source-test-ollama-cloud-source"

	modelRegistry := registry.GetGlobalRegistry()
	modelRegistry.UnregisterClient(clientID)
	t.Cleanup(func() {
		modelRegistry.UnregisterClient(clientID)
	})

	modelRegistry.RegisterClient(clientID, "ollama-cloud", []*registry.ModelInfo{{
		ID:      modelID,
		Object:  "model",
		OwnedBy: "ollama",
	}})

	authByID := map[string]*coreauth.Auth{
		clientID: {ID: clientID, Provider: "ollama-cloud", Label: "Ollama Cloud", Status: coreauth.StatusActive},
	}
	sources := New(&config.Config{}, nil).modelSourceEntries(
		modelRegistry,
		modelID,
		authByID,
		map[string]string{"ollama-cloud": "ollama"},
		map[string]bool{"ollama": true},
	)
	if len(sources) != 1 || sources[0]["provider"] != "ollama-cloud" || sources[0]["channel"] != "Ollama Cloud" || sources[0]["model_id"] != modelID {
		t.Fatalf("sources = %#v, want retained registry model to show mapped provider source", sources)
	}
}
