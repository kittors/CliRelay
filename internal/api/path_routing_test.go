package api

import (
	"path/filepath"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/usage"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

func TestResolvePathRouteContextUsesCcSwitchEndpointPath(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "usage.db")
	if err := usage.InitDB(dbPath, config.RequestLogStorageConfig{}, nil); err != nil {
		t.Fatalf("usage.InitDB() error = %v", err)
	}
	defer usage.CloseDB()

	if err := usage.ReplaceAllCcSwitchImportConfigs([]usage.CcSwitchImportConfigRow{
		{
			ID:                   "cfg-kimi-opus",
			ClientType:           "claude",
			ProviderName:         "Kimi Opus",
			DefaultModel:         "kimi-k2.6",
			AllowedChannelGroups: []string{"kimicode"},
			RoutePath:            "/kimicode/cs_opus",
			EndpointPath:         "",
			UsageAutoInterval:    30,
			ModelMappings: []usage.CcSwitchModelMappingRow{
				{Role: "opus", RequestModel: "claude-opus-4-7", TargetModel: "kimi-k2.6"},
			},
		},
	}); err != nil {
		t.Fatalf("ReplaceAllCcSwitchImportConfigs() error = %v", err)
	}

	authManager := cliproxyauth.NewManager(nil, nil, nil)
	authManager.SetConfig(&config.Config{
		Routing: config.RoutingConfig{
			ChannelGroups: []config.RoutingChannelGroup{{Name: "kimicode"}},
		},
	})

	route, ok := resolvePathRouteContext(&config.Config{}, authManager, "/kimicode/cs_opus")
	if !ok || route == nil {
		t.Fatal("resolvePathRouteContext() did not resolve CC Switch route")
	}
	if route.Group != "kimicode" {
		t.Fatalf("route.Group = %q, want kimicode", route.Group)
	}
	if route.RoutePath != "/kimicode/cs_opus" {
		t.Fatalf("route.RoutePath = %q, want /kimicode/cs_opus", route.RoutePath)
	}
	if route.CcSwitch == nil {
		t.Fatal("route.CcSwitch = nil, want config metadata")
	}
	if route.CcSwitch.ConfigID != "cfg-kimi-opus" {
		t.Fatalf("route.CcSwitch.ConfigID = %q, want cfg-kimi-opus", route.CcSwitch.ConfigID)
	}
	if len(route.CcSwitch.ModelMappings) != 1 ||
		route.CcSwitch.ModelMappings[0].RequestModel != "claude-opus-4-7" ||
		route.CcSwitch.ModelMappings[0].TargetModel != "kimi-k2.6" {
		t.Fatalf("route.CcSwitch.ModelMappings = %#v", route.CcSwitch.ModelMappings)
	}

	legacyRoute, ok := resolvePathRouteContext(&config.Config{}, authManager, "/kimicode")
	if !ok || legacyRoute == nil {
		t.Fatal("resolvePathRouteContext() did not resolve legacy channel group route")
	}
	if legacyRoute.CcSwitch != nil {
		t.Fatalf("legacy route CcSwitch = %#v, want nil", legacyRoute.CcSwitch)
	}
}
