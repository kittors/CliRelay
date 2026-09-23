package cliproxy

import (
	"context"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/modeldiscovery"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/registry"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v6/sdk/config"
	sdkmodelcatalog "github.com/router-for-me/CLIProxyAPI/v6/sdk/modelcatalog"
)

// manifestOnlyModel stands for a model ChatGPT ships after this build. The name is
// made up on purpose: the property under test is "listed upstream, therefore
// routable", which must hold for models nobody has heard of yet.
const manifestOnlyModel = "gpt-manifest-only-test"

func manifestOnlyInfo() *ModelInfo {
	return &ModelInfo{
		ID:            manifestOnlyModel,
		Object:        "model",
		OwnedBy:       "openai",
		Type:          "codex",
		DisplayName:   "Manifest Only",
		ContextLength: 400000,
		Thinking:      &ModelThinkingSupport{Levels: []string{"low", "medium", "high"}},
	}
}

func codexOAuthAuth(id, tenantID string) *coreauth.Auth {
	return &coreauth.Auth{
		ID:       id,
		Provider: "codex",
		TenantID: tenantID,
		Status:   coreauth.StatusActive,
		Metadata: map[string]any{"email": id + "@example.com"},
	}
}

// resetModelDiscovery also drops the change hook: a test that ran loadInitialState
// without Shutdown leaves its service registered there.
func resetModelDiscovery(t *testing.T) {
	t.Helper()
	reset := func() {
		modeldiscovery.ResetForTest()
		modeldiscovery.SetRoutingChangeHook(nil)
	}
	reset()
	t.Cleanup(reset)
}

func unregisterOnCleanup(t *testing.T, authID string) ModelRegistry {
	t.Helper()
	reg := GlobalModelRegistry()
	reg.UnregisterClient(authID)
	t.Cleanup(func() { reg.UnregisterClient(authID) })
	return reg
}

func waitForCondition(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestCodexOAuthRegistersModelsTheManifestLists(t *testing.T) {
	resetModelDiscovery(t)
	const tenantID = "tenant-live-discovery"
	modeldiscovery.Store(tenantID, "codex", []*ModelInfo{manifestOnlyInfo()})

	service := &Service{cfg: &config.Config{}}
	auth := codexOAuthAuth("codex-live-discovery", tenantID)
	reg := unregisterOnCleanup(t, auth.ID)

	service.registerModelsForAuth(context.Background(), auth)

	if !reg.ClientSupportsModel(auth.ID, manifestOnlyModel) {
		t.Fatalf("a model the manifest lists is not routable: %v", modelIDs(reg.GetModelsForClient(auth.ID)))
	}
	// The model probe and request routing both start from this lookup; an empty
	// answer is the "no provider serves model" failure.
	if providers := sdkmodelcatalog.GetProviderName(manifestOnlyModel); !slices.Contains(providers, "codex") {
		t.Fatalf("providers for %s = %v, want codex", manifestOnlyModel, providers)
	}
	// The catalog is the floor. Replacing it with the manifest was #673.
	registered := reg.GetModelsForClient(auth.ID)
	for _, static := range sdkmodelcatalog.StaticModelDefinitionsByChannel("codex") {
		if !hasModelID(registered, static.ID) {
			t.Fatalf("catalog model %s dropped when the manifest was merged", static.ID)
		}
	}
	// Request handling reads reasoning support from here. Without the manifest's
	// levels the model would have none, and its reasoning settings would be stripped.
	info := registry.LookupModelInfo(manifestOnlyModel, "codex")
	if info == nil || info.Thinking == nil || strings.Join(info.Thinking.Levels, ",") != "low,medium,high" {
		t.Fatalf("reasoning support = %+v, want the manifest's levels", info)
	}
}

func TestManifestModelStaysRoutableAfterTheListGoesStale(t *testing.T) {
	resetModelDiscovery(t)
	clock := time.Date(2026, 9, 23, 0, 0, 0, 0, time.UTC)
	t.Cleanup(modeldiscovery.SetClockForTest(func() time.Time { return clock }))
	const tenantID = "tenant-live-discovery-stale"
	modeldiscovery.Store(tenantID, "codex", []*ModelInfo{manifestOnlyInfo()})

	// A day later the list is due for a refresh that has not succeeded — the
	// manifest endpoint is down, say. Any credential re-registering now (a token
	// refresh does) must keep the model; only a newer answer may drop it.
	clock = clock.Add(modeldiscovery.FreshFor + time.Hour)

	service := &Service{cfg: &config.Config{}}
	auth := codexOAuthAuth("codex-live-discovery-stale", tenantID)
	reg := unregisterOnCleanup(t, auth.ID)
	service.registerModelsForAuth(context.Background(), auth)

	if !reg.ClientSupportsModel(auth.ID, manifestOnlyModel) {
		t.Fatal("a stale list deregistered its models")
	}
}

func TestCodexAPIKeyCredentialIgnoresTheManifest(t *testing.T) {
	resetModelDiscovery(t)
	const tenantID = "tenant-live-discovery-apikey"
	modeldiscovery.Store(tenantID, "codex", []*ModelInfo{manifestOnlyInfo()})

	service := &Service{cfg: &config.Config{}}
	auth := &coreauth.Auth{
		ID:       "codex-live-discovery-apikey",
		Provider: "codex",
		TenantID: tenantID,
		Status:   coreauth.StatusActive,
		Attributes: map[string]string{
			"api_key":  "sk-test",
			"base_url": "https://relay.example.com/v1",
		},
	}
	reg := unregisterOnCleanup(t, auth.ID)

	service.registerModelsForAuth(context.Background(), auth)

	// An API key points at its own relay; what ChatGPT lists for the tenant's
	// subscriptions says nothing about what that relay serves.
	if reg.ClientSupportsModel(auth.ID, manifestOnlyModel) {
		t.Fatalf("API key credential registered a manifest model: %v", modelIDs(reg.GetModelsForClient(auth.ID)))
	}
}

func TestManifestDoesNotRedefineCatalogModels(t *testing.T) {
	resetModelDiscovery(t)
	var catalog *ModelInfo
	for _, model := range sdkmodelcatalog.StaticModelDefinitionsByChannel("codex") {
		if model != nil && model.Thinking != nil && len(model.Thinking.Levels) > 1 {
			catalog = model
			break
		}
	}
	if catalog == nil {
		t.Skip("the codex catalog has no model with several reasoning levels")
	}
	const tenantID = "tenant-live-discovery-catalog"
	narrowed := *catalog
	narrowed.DisplayName = "as the manifest describes it"
	narrowed.Thinking = &ModelThinkingSupport{Levels: []string{"low"}}
	modeldiscovery.Store(tenantID, "codex", []*ModelInfo{&narrowed, manifestOnlyInfo()})

	service := &Service{cfg: &config.Config{}}
	auth := codexOAuthAuth("codex-live-discovery-catalog", tenantID)
	reg := unregisterOnCleanup(t, auth.ID)

	service.registerModelsForAuth(context.Background(), auth)

	for _, model := range reg.GetModelsForClient(auth.ID) {
		if model == nil || model.ID != catalog.ID {
			continue
		}
		if model.Thinking == nil || !slices.Equal(model.Thinking.Levels, catalog.Thinking.Levels) {
			t.Fatalf("%s reasoning levels = %+v, want the catalog's %v", catalog.ID, model.Thinking, catalog.Thinking.Levels)
		}
		return
	}
	t.Fatalf("catalog model %s missing", catalog.ID)
}

func TestManifestArrivalReRegistersAfterStartupPass(t *testing.T) {
	resetModelDiscovery(t)
	const tenantID = "tenant-live-discovery-startup"
	release := make(chan struct{})
	restore := modeldiscovery.SetFetcherForTest("codex", func(ctx context.Context, _ *coreauth.Auth, _ *config.Config) ([]*ModelInfo, error) {
		select {
		case <-release:
			return []*ModelInfo{manifestOnlyInfo()}, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	})
	t.Cleanup(restore)

	manager := coreauth.NewManager(nil, nil, nil)
	auth := codexOAuthAuth("codex-live-discovery-startup", tenantID)
	if _, err := manager.Register(context.Background(), auth); err != nil {
		t.Fatalf("Register: %v", err)
	}
	service := &Service{cfg: &config.Config{}, coreManager: manager}
	reg := unregisterOnCleanup(t, auth.ID)

	service.startProviderDiscovery(context.Background())
	t.Cleanup(service.stopProviderDiscovery)

	// The startup pass registers before the manifest has answered.
	service.registerModelsForAuth(context.Background(), auth)
	if reg.ClientSupportsModel(auth.ID, manifestOnlyModel) {
		t.Fatal("manifest model registered before the manifest answered")
	}

	close(release)
	waitForCondition(t, "the manifest to be stored", func() bool {
		return len(modeldiscovery.Snapshot(tenantID, "codex")) > 0
	})
	// Held until the startup pass is over, which could otherwise still overwrite
	// the merged registration with its catalog-only one. Watched for a while: an
	// unheld re-registration runs on another goroutine and takes a moment.
	for deadline := time.Now().Add(200 * time.Millisecond); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
		if reg.ClientSupportsModel(auth.ID, manifestOnlyModel) {
			t.Fatal("re-registration ran before the startup pass finished")
		}
	}

	service.markProviderDiscoveryRegistrationReady()
	waitForCondition(t, "the manifest model to become routable", func() bool {
		return reg.ClientSupportsModel(auth.ID, manifestOnlyModel)
	})
}

func TestFirstCredentialOfATenantRequestsItsManifest(t *testing.T) {
	resetModelDiscovery(t)
	const tenantID = "tenant-live-discovery-new"
	var calls atomic.Int32
	restore := modeldiscovery.SetFetcherForTest("codex", func(context.Context, *coreauth.Auth, *config.Config) ([]*ModelInfo, error) {
		calls.Add(1)
		return []*ModelInfo{manifestOnlyInfo()}, nil
	})
	t.Cleanup(restore)

	manager := coreauth.NewManager(nil, nil, nil)
	service := &Service{cfg: &config.Config{}, coreManager: manager}
	service.startProviderDiscovery(context.Background())
	service.markProviderDiscoveryRegistrationReady()
	t.Cleanup(service.stopProviderDiscovery)

	// The tenant's first Codex account arrives after startup: nothing is stored for
	// it, and the next periodic run is half an hour away.
	auth := codexOAuthAuth("codex-live-discovery-new", tenantID)
	if _, err := manager.Register(context.Background(), auth); err != nil {
		t.Fatalf("Register: %v", err)
	}
	reg := unregisterOnCleanup(t, auth.ID)
	service.registerModelsForAuth(context.Background(), auth)

	waitForCondition(t, "the new tenant's manifest model to become routable", func() bool {
		return reg.ClientSupportsModel(auth.ID, manifestOnlyModel)
	})
	if calls.Load() == 0 {
		t.Fatal("the manifest was never fetched")
	}
}

func TestProviderDiscoveryTargetsOnlyEligibleCredentials(t *testing.T) {
	manager := coreauth.NewManager(nil, nil, nil)
	for _, auth := range []*coreauth.Auth{
		codexOAuthAuth("codex-target-oauth-a", "tenant-target-a"),
		codexOAuthAuth("codex-target-oauth-a2", "tenant-target-a"),
		{ID: "codex-target-apikey", Provider: "codex", TenantID: "tenant-target-b", Attributes: map[string]string{"api_key": "sk-test"}},
		{ID: "codex-target-disabled", Provider: "codex", TenantID: "tenant-target-c", Disabled: true},
		{ID: "xai-target", Provider: "xai", TenantID: "tenant-target-d"},
	} {
		if _, err := manager.Register(context.Background(), auth); err != nil {
			t.Fatalf("Register %s: %v", auth.ID, err)
		}
	}
	service := &Service{cfg: &config.Config{}, coreManager: manager}

	targets := service.providerDiscoveryTargets()

	want := providerDiscoveryTarget{tenantID: "tenant-target-a", provider: "codex"}
	if len(targets) != 1 || targets[0] != want {
		t.Fatalf("targets = %+v, want only %+v", targets, want)
	}
}

func TestStoppedDiscoveryStartsNoFetch(t *testing.T) {
	resetModelDiscovery(t)
	var calls atomic.Int32
	restore := modeldiscovery.SetFetcherForTest("codex", func(context.Context, *coreauth.Auth, *config.Config) ([]*ModelInfo, error) {
		calls.Add(1)
		return []*ModelInfo{manifestOnlyInfo()}, nil
	})
	t.Cleanup(restore)

	manager := coreauth.NewManager(nil, nil, nil)
	service := &Service{cfg: &config.Config{}, coreManager: manager}
	service.startProviderDiscovery(context.Background())
	service.stopProviderDiscovery()

	auth := codexOAuthAuth("codex-live-discovery-stopped", "tenant-live-discovery-stopped")
	if _, err := manager.Register(context.Background(), auth); err != nil {
		t.Fatalf("Register: %v", err)
	}
	unregisterOnCleanup(t, auth.ID)
	service.registerModelsForAuth(context.Background(), auth)

	if got := calls.Load(); got != 0 {
		t.Fatalf("fetches after stop = %d, want 0", got)
	}
}

func TestMergeLiveDiscoveredModelsKeepsBaseDefinitions(t *testing.T) {
	base := []*ModelInfo{{ID: "shared-model", DisplayName: "catalog"}}
	live := []*ModelInfo{{ID: "Shared-Model", DisplayName: "manifest"}, {ID: "new-model"}, nil, {ID: " "}}

	got := mergeLiveDiscoveredModels(base, live)

	if len(got) != 2 || got[0].DisplayName != "catalog" || got[1].ID != "new-model" {
		t.Fatalf("merged = %+v", got)
	}
}
