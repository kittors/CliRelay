package modelcatalog

import (
	"reflect"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/identity"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/modeldiscovery"
	sdkmodelcatalog "github.com/router-for-me/CLIProxyAPI/v6/sdk/modelcatalog"
)

func filteredModelIDs(models []map[string]any) []string {
	ids := make([]string, 0, len(models))
	for _, model := range models {
		id, _ := model["id"].(string)
		ids = append(ids, id)
	}
	return ids
}

// Plaza and the catalog must list exactly what a request could reach, so they
// read exclusions through the runtime gate's matcher: either side may carry a
// route prefix, and entries take wildcards.
func TestFilterModelsByRoutingAllowedModelsExclusionMatching(t *testing.T) {
	svc := NewForTenant("", &config.Config{
		Routing: config.RoutingConfig{
			ChannelGroups: []config.RoutingChannelGroup{
				{Name: "team", ExcludedModels: []string{"xai/grok-imagine-video-1.5", "gemini-*-preview"}},
			},
		},
	}, nil)
	models := []map[string]any{
		{"id": "grok-imagine-video-1.5"},
		{"id": "xai/grok-imagine-video-1.5"},
		{"id": "gemini-3-flash-preview"},
		{"id": "grok-4.7"},
		{"id": "gemini-3-flash"},
	}

	got := filteredModelIDs(svc.filterModelsByRoutingAllowedModels(models, "team"))
	if want := []string{"grok-4.7", "gemini-3-flash"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("filtered = %v, want %v", got, want)
	}
}

// "*" in an allow list never meant "everything": before exclusions existed it
// matched no model, and reading it as a wildcard would open the group.
func TestFilterModelsByRoutingAllowedModelsAllowListStarIsNotAWildcard(t *testing.T) {
	svc := NewForTenant("", &config.Config{
		Routing: config.RoutingConfig{
			ChannelGroups: []config.RoutingChannelGroup{
				{Name: "team", AllowedModels: []string{"*"}},
			},
		},
	}, nil)

	if got := svc.filterModelsByRoutingAllowedModels([]map[string]any{{"id": "gpt-5"}}, "team"); len(got) != 0 {
		t.Fatalf("filtered = %v, want nothing listed", filteredModelIDs(got))
	}
}

// Plaza and the path list must follow the same setting the request gate reads:
// a group that follows upstream lists what discovery added to it, a frozen one
// does not, whatever its name.
func TestFilterModelsByRoutingAllowedModelsFollowsTheGroupSetting(t *testing.T) {
	modeldiscovery.ResetForTest()
	t.Cleanup(modeldiscovery.ResetForTest)
	modeldiscovery.Store(identity.SystemTenantID, "codex", []*sdkmodelcatalog.ModelInfo{{ID: "gpt-6.1-terra"}})

	on, off := true, false
	svc := NewForTenant(identity.SystemTenantID, &config.Config{
		Routing: config.RoutingConfig{
			IncludeDefaultGroup: true,
			ChannelGroups: []config.RoutingChannelGroup{
				{Name: "default", AllowedModels: []string{"gpt-6.1-sol"}},
				{Name: "frozen", AllowedModels: []string{"gpt-6.1-sol"}},
				{Name: "following", AllowedModels: []string{"gpt-6.1-sol"}, FollowUpstreamModels: &on},
				{Name: "root-off", AllowedModels: []string{"gpt-6.1-sol"}, FollowUpstreamModels: &off},
			},
		},
	}, nil)
	models := []map[string]any{{"id": "gpt-6.1-sol"}, {"id": "gpt-6.1-terra"}}

	for group, want := range map[string][]string{
		"":          {"gpt-6.1-sol", "gpt-6.1-terra"},
		"frozen":    {"gpt-6.1-sol"},
		"following": {"gpt-6.1-sol", "gpt-6.1-terra"},
		"root-off":  {"gpt-6.1-sol"},
	} {
		if got := filteredModelIDs(svc.filterModelsByRoutingAllowedModels(models, group)); !reflect.DeepEqual(got, want) {
			t.Errorf("group %q lists %v, want %v", group, got, want)
		}
	}
}
