package executor

import (
	"strings"
	"testing"

	sdkmodelcatalog "github.com/router-for-me/CLIProxyAPI/v6/sdk/modelcatalog"
)

// TestCodexImageModelsAreRoutable is the guard for "visible in the panel, unusable
// on call".
//
// A Codex credential's routable model set comes from the ChatGPT manifest, which
// lists chat models only. Without this merge, selecting gpt-image-2.5-flare in the
// console produced auth_not_found: no auth available — the router had no credential
// advertising it — while every catalog surface happily offered it.
func TestCodexImageModelsAreRoutable(t *testing.T) {
	merged := withCodexImageModels(nil)

	ids := make(map[string]struct{}, len(merged))
	for _, model := range merged {
		ids[strings.ToLower(model.ID)] = struct{}{}
	}

	for _, modelID := range []string{"gpt-image-2", "gpt-image-2.5-flare", "gpt-image-2.5-sunburst"} {
		if _, ok := ids[modelID]; !ok {
			t.Errorf("%s is not in a Codex credential's model set, so requests for it fail with auth_not_found", modelID)
		}
	}

	// Another pool's image models must not ride along, or a Codex credential would
	// advertise models it cannot serve.
	for _, modelID := range []string{"grok-imagine-image", "image-01"} {
		if _, ok := ids[modelID]; ok {
			t.Errorf("%s belongs to another provider and must not be merged into Codex", modelID)
		}
	}
}

// TestCodexImageModelsPreserveLiveDefinitions keeps upstream authoritative about
// anything the manifest does report.
func TestCodexImageModelsPreserveLiveDefinitions(t *testing.T) {
	live := []*sdkmodelcatalog.ModelInfo{
		{ID: "gpt-5.5", DisplayName: "Live Chat"},
		{ID: "gpt-image-2", DisplayName: "Live Image Definition"},
	}

	merged := withCodexImageModels(live)

	var seenImage2 int
	for _, model := range merged {
		if strings.EqualFold(model.ID, "gpt-image-2") {
			seenImage2++
			if model.DisplayName != "Live Image Definition" {
				t.Errorf("gpt-image-2 display name = %q, want the live definition to win", model.DisplayName)
			}
		}
	}
	if seenImage2 != 1 {
		t.Errorf("gpt-image-2 appears %d times, want exactly one entry", seenImage2)
	}

	// The chat model the manifest reported must survive untouched.
	found := false
	for _, model := range merged {
		if model.ID == "gpt-5.5" {
			found = true
		}
	}
	if !found {
		t.Error("gpt-5.5 was dropped from the merged model set")
	}
}
