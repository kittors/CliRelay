package modelcatalog

import (
	"context"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	managementauthfiles "github.com/router-for-me/CLIProxyAPI/v6/internal/management/authfiles"
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

func TestDefaultMappedOwnerRowsIncludeConfigRowWithoutRuntimeSource(t *testing.T) {
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
	if len(got) != 1 || got[0]["id"] != modelID {
		t.Fatalf("models = %#v, want owner-mapped config row kept without runtime source", got)
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

func TestConfiguredAvailabilityDoesNotLeakSystemRegistryModelsToOtherTenant(t *testing.T) {
	const (
		systemModelID = "tenant-isolation-system-model"
		tenantModelID = "tenant-isolation-tenant-model"
		systemAuthID  = "tenant-isolation-system-auth"
		tenantAuthID  = "tenant-isolation-tenant-auth"
		tenantID      = "14b1ee9a-6177-4f5f-b5d4-4fba60ad24fa"
	)

	modelRegistry := registry.GetGlobalRegistry()
	modelRegistry.UnregisterClient(systemAuthID)
	modelRegistry.UnregisterClient(tenantAuthID)
	t.Cleanup(func() {
		modelRegistry.UnregisterClient(systemAuthID)
		modelRegistry.UnregisterClient(tenantAuthID)
	})

	// System tenant clients remain visible in the process-global registry.
	modelRegistry.RegisterClient(systemAuthID, "codex", []*registry.ModelInfo{
		{ID: systemModelID, Object: "model", OwnedBy: "openai"},
	})
	modelRegistry.RegisterClient(tenantAuthID, "codex", []*registry.ModelInfo{
		{ID: tenantModelID, Object: "model", OwnedBy: "openai"},
	})

	manager := coreauth.NewManager(nil, nil, nil)
	if _, err := manager.Register(context.Background(), &coreauth.Auth{
		ID:       systemAuthID,
		TenantID: "",
		Provider: "codex",
		Status:   coreauth.StatusActive,
	}); err != nil {
		t.Fatalf("register system auth: %v", err)
	}
	if _, err := manager.Register(context.Background(), &coreauth.Auth{
		ID:       tenantAuthID,
		TenantID: tenantID,
		Provider: "codex",
		Status:   coreauth.StatusActive,
	}); err != nil {
		t.Fatalf("register tenant auth: %v", err)
	}

	// Default models page path: no channel/group filters.
	result := NewForTenant(tenantID, &config.Config{}, manager).ConfiguredAvailability("", "")
	data, ok := result["data"].([]map[string]any)
	if !ok {
		t.Fatalf("data = %#v, want []map[string]any", result["data"])
	}
	ids := make(map[string]struct{}, len(data))
	for _, item := range data {
		if id, _ := item["id"].(string); id != "" {
			ids[id] = struct{}{}
		}
	}
	if _, ok := ids[tenantModelID]; !ok {
		t.Fatalf("missing tenant-owned model %q; ids=%v", tenantModelID, ids)
	}
	if _, ok := ids[systemModelID]; ok {
		t.Fatalf("system registry model %q leaked into tenant availability; ids=%v", systemModelID, ids)
	}
}

func TestFilterModelsByScopesAlwaysScopesToTenantWithoutChannelFilters(t *testing.T) {
	const (
		systemModelID = "filter-scope-system-model"
		tenantModelID = "filter-scope-tenant-model"
		systemAuthID  = "filter-scope-system-auth"
		tenantAuthID  = "filter-scope-tenant-auth"
		tenantID      = "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
	)

	modelRegistry := registry.GetGlobalRegistry()
	modelRegistry.UnregisterClient(systemAuthID)
	modelRegistry.UnregisterClient(tenantAuthID)
	t.Cleanup(func() {
		modelRegistry.UnregisterClient(systemAuthID)
		modelRegistry.UnregisterClient(tenantAuthID)
	})
	modelRegistry.RegisterClient(systemAuthID, "codex", []*registry.ModelInfo{{ID: systemModelID}})
	modelRegistry.RegisterClient(tenantAuthID, "codex", []*registry.ModelInfo{{ID: tenantModelID}})

	manager := coreauth.NewManager(nil, nil, nil)
	if _, err := manager.Register(context.Background(), &coreauth.Auth{
		ID: systemAuthID, Provider: "codex", Status: coreauth.StatusActive,
	}); err != nil {
		t.Fatalf("register system auth: %v", err)
	}
	if _, err := manager.Register(context.Background(), &coreauth.Auth{
		ID: tenantAuthID, TenantID: tenantID, Provider: "codex", Status: coreauth.StatusActive,
	}); err != nil {
		t.Fatalf("register tenant auth: %v", err)
	}

	svc := NewForTenant(tenantID, &config.Config{}, manager)
	models := []map[string]any{
		{"id": systemModelID},
		{"id": tenantModelID},
	}
	filtered := svc.filterModelsByScopes(models, "", "", AvailabilityFilterOptions{})
	if len(filtered) != 1 || filtered[0]["id"] != tenantModelID {
		t.Fatalf("filtered = %#v, want only tenant model", filtered)
	}
}

func TestConfiguredAvailabilityReplacesStaticCodexWithDiscovery(t *testing.T) {
	const (
		staticModelID    = "gpt-5.1-static-only"
		liveModelID      = "gpt-5.6-sol"
		bothModelID      = "gpt-5.4" // on static registry AND discovery — must remain
		codexClientID    = "plaza-discovery-codex"
		openCodeClientID = "plaza-discovery-opencode"
		openCodeModelID  = "opencode-keep-model"
	)

	managementauthfiles.ResetDiscoveryCacheForTest()
	t.Cleanup(managementauthfiles.ResetDiscoveryCacheForTest)

	modelRegistry := registry.GetGlobalRegistry()
	modelRegistry.UnregisterClient(codexClientID)
	modelRegistry.UnregisterClient(openCodeClientID)
	t.Cleanup(func() {
		modelRegistry.UnregisterClient(codexClientID)
		modelRegistry.UnregisterClient(openCodeClientID)
	})

	// Static codex catalog rows (what RegisterClient does at startup).
	modelRegistry.RegisterClient(codexClientID, "codex", []*registry.ModelInfo{
		{ID: staticModelID, Object: "model", OwnedBy: "openai"},
		{ID: bothModelID, Object: "model", OwnedBy: "openai"},
	})
	// Non-codex provider must not be stripped.
	modelRegistry.RegisterClient(openCodeClientID, "opencode-go", []*registry.ModelInfo{
		{ID: openCodeModelID, Object: "model", OwnedBy: "opencode"},
	})

	// Seed live discovery with modern models (not registered into runtime registry).
	managementauthfiles.StoreDiscoveryCacheForTest("", "codex", []*registry.ModelInfo{
		{ID: liveModelID, Object: "model", OwnedBy: "openai", DisplayName: "GPT-5.6 Sol"},
		{ID: bothModelID, Object: "model", OwnedBy: "openai", DisplayName: "GPT 5.4"},
	})

	manager := coreauth.NewManager(nil, nil, nil)
	if _, err := manager.Register(context.Background(), &coreauth.Auth{
		ID: codexClientID, Provider: "codex", Label: "Codex Pro", Status: coreauth.StatusActive,
	}); err != nil {
		t.Fatalf("register codex: %v", err)
	}
	if _, err := manager.Register(context.Background(), &coreauth.Auth{
		ID: openCodeClientID, Provider: "opencode-go", Label: "OpenCode", Status: coreauth.StatusActive,
	}); err != nil {
		t.Fatalf("register opencode: %v", err)
	}

	result := New(&config.Config{}, manager).ConfiguredAvailability("", "")
	data, ok := result["data"].([]map[string]any)
	if !ok {
		t.Fatalf("data = %#v", result["data"])
	}
	ids := make(map[string]struct{}, len(data))
	var liveSources []map[string]any
	for _, item := range data {
		id, _ := item["id"].(string)
		ids[id] = struct{}{}
		if id == liveModelID {
			liveSources, _ = item["sources"].([]map[string]any)
		}
	}

	if _, ok := ids[liveModelID]; !ok {
		t.Fatalf("missing live discovery model %q; ids=%v", liveModelID, ids)
	}
	if _, ok := ids[bothModelID]; !ok {
		t.Fatalf("missing overlap model %q; ids=%v", bothModelID, ids)
	}
	if _, ok := ids[openCodeModelID]; !ok {
		t.Fatalf("opencode model %q was stripped; ids=%v", openCodeModelID, ids)
	}
	if _, ok := ids[staticModelID]; ok {
		t.Fatalf("static-only codex model %q should be replaced by discovery; ids=%v", staticModelID, ids)
	}
	if len(liveSources) == 0 {
		t.Fatalf("live discovery model should have synthesized sources")
	}
}

func TestDropStaticDiscoveryProviderModelsDropsMappedOwnerLibrary(t *testing.T) {
	const (
		staleLibraryID = "gpt-5-stale-library"
		liveModelID    = "gpt-5.6-sol"
		codexClientID  = "mapped-owner-drop-codex"
	)
	managementauthfiles.ResetDiscoveryCacheForTest()
	t.Cleanup(managementauthfiles.ResetDiscoveryCacheForTest)

	modelRegistry := registry.GetGlobalRegistry()
	modelRegistry.UnregisterClient(codexClientID)
	t.Cleanup(func() { modelRegistry.UnregisterClient(codexClientID) })

	modelRegistry.RegisterClient(codexClientID, "codex", []*registry.ModelInfo{
		{ID: staleLibraryID, Object: "model", OwnedBy: "openai"},
	})
	managementauthfiles.StoreDiscoveryCacheForTest("", "codex", []*registry.ModelInfo{
		{ID: liveModelID, Object: "model", OwnedBy: "openai"},
	})

	models := []map[string]any{
		{"id": staleLibraryID, "object": "model", "owned_by": "openai"},
		{"id": liveModelID, "object": "model", "owned_by": "openai"},
		{"id": "unrelated-model", "object": "model", "owned_by": "deepseek"},
	}
	got := dropStaticDiscoveryProviderModels(
		models,
		modelRegistry,
		map[string][]*registry.ModelInfo{
			"codex": {{ID: liveModelID, Object: "model", OwnedBy: "openai"}},
		},
		map[string]*coreauth.Auth{
			codexClientID: {ID: codexClientID, Provider: "codex", Status: coreauth.StatusActive},
		},
		map[string]string{"codex": "openai"},
	)
	ids := make(map[string]struct{}, len(got))
	for _, m := range got {
		ids[m["id"].(string)] = struct{}{}
	}
	if _, ok := ids[staleLibraryID]; ok {
		t.Fatalf("stale mapped-owner library row should be dropped; got=%v", ids)
	}
	if _, ok := ids[liveModelID]; !ok {
		t.Fatalf("live model should remain; got=%v", ids)
	}
	if _, ok := ids["unrelated-model"]; !ok {
		t.Fatalf("unrelated owner model should remain; got=%v", ids)
	}
}

func TestFilterModelsByRoutingAllowedModelsHonorsDefaultGroupList(t *testing.T) {
	// Live discovery models are merged after CanServe and must still be filtered
	// by the channel-group allowed-models list (default group when unscoped).
	svc := NewForTenant("", &config.Config{
		Routing: config.RoutingConfig{
			IncludeDefaultGroup: true,
			ChannelGroups: []config.RoutingChannelGroup{
				{
					Name:          "default",
					AllowedModels: []string{"grok-4.5"},
				},
			},
		},
	}, nil)

	models := []map[string]any{
		{"id": "grok-4.5", "object": "model", "owned_by": "xAI"},
		{"id": "grok-composer-2.5-fast", "object": "model", "owned_by": "xAI"},
		{"id": "gpt-5.4", "object": "model", "owned_by": "openai"},
	}
	filtered := svc.filterModelsByRoutingAllowedModels(models, "")
	if len(filtered) != 1 || filtered[0]["id"] != "grok-4.5" {
		t.Fatalf("filtered = %#v, want only grok-4.5", filtered)
	}
}

func TestPathAvailabilityFiltersLiveDiscoveryByDefaultAllowedModels(t *testing.T) {
	// Path availability used to append xAI/codex/claude live discovery after
	// CanServe without AllowedModels, so plaza/catalog re-introduced blocked
	// models via path merge. Discovery rows must be filtered the same way.
	// Use empty tenant so tenantRoutingConfig falls back to cfg.Routing (no DB).
	const (
		allowedModelID = "grok-4.5"
		blockedModelID = "grok-composer-2.5-fast"
		authID         = "path-allowed-xai-auth"
	)
	managementauthfiles.ResetDiscoveryCacheForTest()
	t.Cleanup(managementauthfiles.ResetDiscoveryCacheForTest)
	managementauthfiles.StoreDiscoveryCacheForTest("", "xai", []*registry.ModelInfo{
		{ID: allowedModelID, Object: "model", OwnedBy: "xAI", Type: "xai"},
		{ID: blockedModelID, Object: "model", OwnedBy: "xAI", Type: "xai"},
	})

	cfg := &config.Config{
		Routing: config.RoutingConfig{
			IncludeDefaultGroup: true,
			ChannelGroups: []config.RoutingChannelGroup{
				{
					Name:          "default",
					AllowedModels: []string{allowedModelID},
				},
			},
		},
	}
	manager := coreauth.NewManager(nil, nil, nil)
	manager.SetConfig(cfg)
	if _, err := manager.Register(context.Background(), &coreauth.Auth{
		ID: authID, Provider: "xai", Status: coreauth.StatusActive,
	}); err != nil {
		t.Fatalf("register xai auth: %v", err)
	}

	result := New(cfg, manager).PathAvailability()
	// PathAvailability returns typed rows, not map[string]any.
	rows, ok := result["data"].([]modelPathAvailabilityResponse)
	if !ok {
		t.Fatalf("data type = %T, want []modelPathAvailabilityResponse", result["data"])
	}
	ids := make(map[string]struct{}, len(rows))
	for _, item := range rows {
		if item.ID != "" {
			ids[item.ID] = struct{}{}
		}
	}
	if _, ok := ids[allowedModelID]; !ok {
		t.Fatalf("path availability missing allowed model %q; ids=%v", allowedModelID, ids)
	}
	if _, ok := ids[blockedModelID]; ok {
		t.Fatalf("path availability leaked blocked model %q; ids=%v", blockedModelID, ids)
	}
}

func TestFilterModelsByRoutingAllowedModelsEmptyMeansUnrestricted(t *testing.T) {
	svc := NewForTenant("", &config.Config{
		Routing: config.RoutingConfig{
			IncludeDefaultGroup: true,
			ChannelGroups: []config.RoutingChannelGroup{
				{Name: "default", AllowedModels: nil},
			},
		},
	}, nil)
	models := []map[string]any{{"id": "a"}, {"id": "b"}}
	filtered := svc.filterModelsByRoutingAllowedModels(models, "")
	if len(filtered) != 2 {
		t.Fatalf("filtered = %#v, want unrestricted", filtered)
	}
}

func TestFilterModelsByRoutingAllowedModelsHonorsNamedGroup(t *testing.T) {
	svc := NewForTenant("", &config.Config{
		Routing: config.RoutingConfig{
			ChannelGroups: []config.RoutingChannelGroup{
				{
					Name:          "team",
					AllowedModels: []string{"gpt-5"},
				},
			},
		},
	}, nil)
	models := []map[string]any{{"id": "gpt-5"}, {"id": "claude-opus"}}
	filtered := svc.filterModelsByRoutingAllowedModels(models, "team")
	if len(filtered) != 1 || filtered[0]["id"] != "gpt-5" {
		t.Fatalf("filtered = %#v, want only gpt-5", filtered)
	}
}

func TestConfiguredAvailabilityIgnoreGroupAllowedModelsKeepsFullChannelSet(t *testing.T) {
	// Channel-group editor must list every channel-servable model for checkbox
	// selection, even when AllowedModels already restricts plaza/catalog.
	// Use a non-discovery provider so registry rows are not stripped as static
	// claude/codex/xai discovery placeholders.
	const (
		authID   = "editor-picker-auth"
		tenantID = "bbbbbbbb-cccc-dddd-eeee-ffffffffffff"
		modelA   = "editor-picker-model-a"
		modelB   = "editor-picker-model-b"
	)
	modelRegistry := registry.GetGlobalRegistry()
	modelRegistry.UnregisterClient(authID)
	t.Cleanup(func() { modelRegistry.UnregisterClient(authID) })
	modelRegistry.RegisterClient(authID, "opencode-go", []*registry.ModelInfo{
		{ID: modelA, Object: "model", OwnedBy: "opencode"},
		{ID: modelB, Object: "model", OwnedBy: "opencode"},
	})

	cfg := &config.Config{
		Routing: config.RoutingConfig{
			IncludeDefaultGroup: true,
			ChannelGroups: []config.RoutingChannelGroup{
				{
					Name:          "default",
					AllowedModels: []string{modelA},
				},
			},
		},
	}
	manager := coreauth.NewManager(nil, nil, nil)
	manager.SetConfigForTenant(tenantID, cfg)
	if _, err := manager.Register(context.Background(), &coreauth.Auth{
		ID: authID, TenantID: tenantID, Provider: "opencode-go", Status: coreauth.StatusActive,
	}); err != nil {
		t.Fatalf("register auth: %v", err)
	}

	svc := NewForTenant(tenantID, cfg, manager)

	restricted := svc.ConfiguredAvailability("", "default")
	restrictedIDs := configuredAvailabilityIDs(restricted)
	if len(restrictedIDs) != 1 || restrictedIDs[0] != modelA {
		t.Fatalf("restricted availability = %v, want only %s", restrictedIDs, modelA)
	}

	editor := svc.ConfiguredAvailability("", "default", AvailabilityFilterOptions{IgnoreGroupAllowedModels: true})
	editorIDs := configuredAvailabilityIDs(editor)
	if len(editorIDs) != 2 {
		t.Fatalf("editor availability = %v, want both channel-servable models", editorIDs)
	}
	want := map[string]bool{modelA: true, modelB: true}
	for _, id := range editorIDs {
		if !want[id] {
			t.Fatalf("editor availability unexpected id %q in %v", id, editorIDs)
		}
	}
}

func configuredAvailabilityIDs(result map[string]any) []string {
	data, _ := result["data"].([]map[string]any)
	if data == nil {
		if raw, ok := result["data"].([]any); ok {
			out := make([]string, 0, len(raw))
			for _, item := range raw {
				entry, _ := item.(map[string]any)
				if id, _ := entry["id"].(string); id != "" {
					out = append(out, id)
				}
			}
			return out
		}
		return nil
	}
	out := make([]string, 0, len(data))
	for _, entry := range data {
		if id, _ := entry["id"].(string); id != "" {
			out = append(out, id)
		}
	}
	return out
}
