package modelcatalog

import (
	"context"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	managementauthfiles "github.com/router-for-me/CLIProxyAPI/v8/internal/management/authfiles"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/watcher/synthesizer"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func TestConfiguredAvailabilityKeepsExplicitCodexModelsOutsideOAuthDiscovery(t *testing.T) {
	const custom = "glm-5.3-flash"
	const stale = "codex-stale-discovery-test"
	cfg := &config.Config{CodexKey: []config.CodexKey{{
		Name: "zcode", APIKey: "test-config-key", BaseURL: "https://example.invalid/v1",
		Models: []config.CodexModel{{Name: custom}},
	}}}
	auths, err := synthesizer.NewConfigSynthesizer().Synthesize(&synthesizer.SynthesisContext{
		Config: cfg, Now: time.Now(), IDGenerator: synthesizer.NewStableIDGenerator(),
	})
	if err != nil || len(auths) != 1 {
		t.Fatalf("synthesize: auths=%d err=%v", len(auths), err)
	}
	oauth := &coreauth.Auth{ID: "explicit-codex-oauth", Provider: "codex", Status: coreauth.StatusActive}
	reg := registry.GetGlobalRegistry()
	managementauthfiles.ResetDiscoveryCacheForTest()
	t.Cleanup(managementauthfiles.ResetDiscoveryCacheForTest)
	t.Cleanup(func() { reg.UnregisterClient(auths[0].ID); reg.UnregisterClient(oauth.ID) })
	reg.RegisterClient(auths[0].ID, "codex", []*registry.ModelInfo{{ID: custom, Object: "model", OwnedBy: "openai"}})
	reg.RegisterClient(oauth.ID, "codex", []*registry.ModelInfo{{ID: stale, Object: "model", OwnedBy: "openai"}})
	managementauthfiles.StoreDiscoveryCacheForTest("", "codex", []*registry.ModelInfo{{ID: "gpt-5.6-luna", Object: "model", OwnedBy: "openai"}})
	manager := coreauth.NewManager(nil, nil, nil)
	for _, auth := range []*coreauth.Auth{auths[0], oauth} {
		if _, err := manager.Register(context.Background(), auth); err != nil {
			t.Fatal(err)
		}
	}
	data := New(cfg, manager).ConfiguredAvailability("", "", AvailabilityFilterOptions{IgnoreGroupAllowedModels: true})["data"].([]map[string]any)
	ids := map[string]bool{}
	for _, item := range data {
		ids[item["id"].(string)] = true
	}
	if !ids[custom] {
		t.Errorf("explicit API-key model missing after OAuth discovery: %v", ids)
	}
	if ids[stale] {
		t.Errorf("obsolete OAuth static model survived: %v", ids)
	}
	if !ids["gpt-5.6-luna"] {
		t.Errorf("live OAuth model missing: %v", ids)
	}
}
