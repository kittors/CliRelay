package modelcatalog

import (
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/registry"
)

// TestImageModelsSurviveDiscoveryPruning is the reported bug: gpt-image-2 was
// registered and routable, and the credential detail listed it, but the model
// plaza, the catalog and the channel-group editor all hid it.
//
// Those views prune static models a provider's live listing does not report, so
// operators are not offered retired models. Image generation is served by a
// separate endpoint and never appears in a chat listing, so that pruning removed
// a model that was available all along.
func TestImageModelsSurviveDiscoveryPruning(t *testing.T) {
	models := []map[string]any{
		{"id": "gpt-5.5"},
		{"id": "gpt-image-2"},
		{"id": "gpt-5.1"},
	}
	// The live listing reports only the current chat model.
	discovery := map[string][]*registry.ModelInfo{
		"codex": {{ID: "gpt-5.5"}},
	}

	kept := dropStaticDiscoveryProviderModels(models, registry.GetGlobalRegistry(), discovery, nil, nil, nil)

	ids := make(map[string]struct{}, len(kept))
	for _, model := range kept {
		if id, _ := model["id"].(string); id != "" {
			ids[id] = struct{}{}
		}
	}
	if _, ok := ids["gpt-image-2"]; !ok {
		t.Error("gpt-image-2 was pruned; it is never in a chat listing and must survive")
	}
	if _, ok := ids["gpt-5.5"]; !ok {
		t.Error("the discovered chat model was lost")
	}
}
