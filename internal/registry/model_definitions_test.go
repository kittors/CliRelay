package registry

import "testing"

func TestCodexStaticModelsIncludeCurrentCodexModels(t *testing.T) {
	models := GetStaticModelDefinitionsByChannel("codex")
	modelIDs := make(map[string]bool, len(models))
	for _, model := range models {
		if model != nil {
			modelIDs[model.ID] = true
		}
	}

	for _, id := range []string{"gpt-5.5", "gpt-5.4", "gpt-5.4-mini", "gpt-5.3-codex-spark", "gpt-image-2", "codex-auto-review"} {
		if !modelIDs[id] {
			t.Fatalf("expected codex static models to include %q", id)
		}
		if LookupStaticModelInfo(id) == nil {
			t.Fatalf("expected LookupStaticModelInfo to find %q", id)
		}
	}

	if modelIDs["gptimage-2"] {
		t.Fatalf("expected codex static models to exclude removed Cherry alias gptimage-2")
	}
	if LookupStaticModelInfo("gptimage-2") != nil {
		t.Fatalf("expected LookupStaticModelInfo to exclude removed Cherry alias gptimage-2")
	}
}
