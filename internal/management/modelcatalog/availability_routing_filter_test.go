package modelcatalog

import (
	"reflect"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
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
