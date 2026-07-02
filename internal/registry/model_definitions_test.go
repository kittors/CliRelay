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

func TestClaudeStaticModelsIncludeCurrentMaxModels(t *testing.T) {
	models := GetStaticModelDefinitionsByChannel("claude")
	modelIDs := make(map[string]bool, len(models))
	for _, model := range models {
		if model != nil {
			modelIDs[model.ID] = true
		}
	}

	for _, id := range []string{"claude-opus-4-6", "claude-opus-4-7"} {
		if !modelIDs[id] {
			t.Fatalf("expected claude static models to include %q", id)
		}
		if LookupStaticModelInfo(id) == nil {
			t.Fatalf("expected LookupStaticModelInfo to find %q", id)
		}
	}
}

func TestClineStaticModelsUseClinePassIDs(t *testing.T) {
	models := GetStaticModelDefinitionsByChannel("cline")
	modelIDs := make(map[string]bool, len(models))
	for _, model := range models {
		if model != nil {
			modelIDs[model.ID] = true
			if model.OwnedBy != "cline" || model.Type != "cline" {
				t.Fatalf("unexpected cline model metadata: %+v", model)
			}
		}
	}

	for _, id := range []string{"cline-pass/glm-5.2", "cline-pass/kimi-k2.7-code", "cline-pass/minimax-m3"} {
		if !modelIDs[id] {
			t.Fatalf("expected cline static models to include %q", id)
		}
		if LookupStaticModelInfo(id) == nil {
			t.Fatalf("expected LookupStaticModelInfo to find %q", id)
		}
	}

	if modelIDs["glm-5.2"] {
		t.Fatalf("expected cline static models to use cline-pass-prefixed IDs")
	}
	if info := LookupStaticModelInfo("glm-5.2"); info != nil && info.Type == "cline" {
		t.Fatalf("expected bare model ID glm-5.2 not to resolve as cline")
	}
}
