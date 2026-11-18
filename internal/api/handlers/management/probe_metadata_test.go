package management

import (
	"context"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

const probeTestTenant = "00000000-0000-0000-0000-0000000000d1"

// probeTestManager is the tenant whose panel probes were turned away: its
// default group excludes the given models, which keeps API keys off them.
func probeTestManager(t *testing.T, excluded ...string) *coreauth.Manager {
	t.Helper()
	manager := coreauth.NewManager(nil, nil, nil)
	manager.SetConfigForTenant(probeTestTenant, &config.Config{
		Routing: config.RoutingConfig{
			IncludeDefaultGroup: true,
			ChannelGroups:       []config.RoutingChannelGroup{{Name: "default", ExcludedModels: excluded}},
		},
	})
	return manager
}

func registerProbeTestAuth(t *testing.T, manager *coreauth.Manager, id, provider string, models ...string) {
	t.Helper()
	if _, err := manager.Register(context.Background(), &coreauth.Auth{
		ID:       id,
		TenantID: probeTestTenant,
		Provider: provider,
		Status:   coreauth.StatusActive,
		Metadata: map[string]any{"access_token": "token"},
	}); err != nil {
		t.Fatalf("Register(%s): %v", id, err)
	}
	infos := make([]*registry.ModelInfo, 0, len(models))
	for _, model := range models {
		infos = append(infos, &registry.ModelInfo{ID: model})
	}
	registry.GetGlobalRegistry().RegisterClient(id, provider, infos)
	t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(id) })
}

func TestProbeMetadataSelectsAcrossTheTenant(t *testing.T) {
	meta := probeMetadata("")
	if meta[coreexecutor.SinglePickMetadataKey] != true || meta[coreexecutor.TenantWideSelectionMetadataKey] != true {
		t.Fatalf("probe metadata = %#v, want a single tenant-wide pick", meta)
	}
	if meta[coreexecutor.TenantMetadataKey] != coreauth.NormalizedTenantID("") {
		t.Fatalf("tenant = %v, want the normalised system tenant", meta[coreexecutor.TenantMetadataKey])
	}
}

func TestImageConsoleProbeReachesModelTheDefaultGroupExcludes(t *testing.T) {
	executor := &managementImageProviderExecutor{identifier: "antigravity"}
	manager := probeTestManager(t, geminiImageTestModel)
	manager.RegisterExecutorForTenant(probeTestTenant, executor)
	registerProbeTestAuth(t, manager, "probe-antigravity-image", "antigravity", geminiImageTestModel)

	h := &Handler{authManager: manager}
	if _, err := h.executeImageGenerationTestForTenant(context.Background(), probeTestTenant,
		[]byte(`{"model":"gemini-3.1-flash-image","prompt":"a cat"}`), imageGenerationAlt); err != nil {
		t.Fatalf("image console probe error = %v", err)
	}
	if executor.authID != "probe-antigravity-image" {
		t.Fatalf("served by %q, want probe-antigravity-image", executor.authID)
	}
}

func TestModelTestProbeReachesModelTheDefaultGroupExcludes(t *testing.T) {
	executor := &managementImageProviderExecutor{identifier: "antigravity"}
	manager := probeTestManager(t, geminiImageTestModel)
	manager.RegisterExecutorForTenant(probeTestTenant, executor)
	registerProbeTestAuth(t, manager, "probe-antigravity-model-test", "antigravity", geminiImageTestModel)

	h := &Handler{authManager: manager}
	if _, err := h.executeModelTestCall(context.Background(), probeTestTenant, "antigravity",
		modelTestEnvelope{Model: geminiImageTestModel},
		[]byte(`{"model":"gemini-3.1-flash-image","prompt":"a cat"}`), imageGenerationAlt); err != nil {
		t.Fatalf("model test probe error = %v", err)
	}
	if executor.authID != "probe-antigravity-model-test" {
		t.Fatalf("served by %q, want probe-antigravity-model-test", executor.authID)
	}
}

func TestVideoConsoleProbeReachesModelTheDefaultGroupExcludes(t *testing.T) {
	const model = "grok-imagine-video-1.5"
	executor := &consoleVideoExecutor{owners: make(map[string]string)}
	manager := probeTestManager(t, model)
	manager.RegisterExecutorForTenant(probeTestTenant, executor)
	registerProbeTestAuth(t, manager, "probe-xai-video", "xai", model)

	h := &Handler{cfg: &config.Config{}, authManager: manager}
	_, selected, err := h.executeVideoCall(context.Background(), probeTestTenant, "xai", model,
		[]byte(`{"model":"grok-imagine-video-1.5","prompt":"a cat"}`), videoGenerationAlt, "")
	if err != nil {
		t.Fatalf("video console probe error = %v", err)
	}
	if selected != "probe-xai-video" {
		t.Fatalf("served by %q, want probe-xai-video", selected)
	}
}
