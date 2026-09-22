package registry

import "testing"

func TestGPT56CodexCapabilities(t *testing.T) {
	for _, modelID := range []string{"gpt-5.6", "gpt-5.6-sol", "gpt-5.6-terra", "gpt-5.6-luna"} {
		capability, ok := GetCodexModelCapability(modelID)
		if !ok {
			t.Fatalf("GetCodexModelCapability(%q) not found", modelID)
		}
		if capability.ContextWindow != 1050000 || capability.MaxContextWindow != 1050000 {
			t.Fatalf("%s context = (%d, %d), want (1050000, 1050000)", modelID, capability.ContextWindow, capability.MaxContextWindow)
		}
		if capability.MaxCompletionTokens != 128000 {
			t.Fatalf("%s max completion = %d, want 128000", modelID, capability.MaxCompletionTokens)
		}
		assertStringSlice(t, modelID+" catalog levels", capability.CatalogReasoningLevels, []string{"low", "medium", "high", "xhigh", "max", "ultra"})
		assertStringSlice(t, modelID+" wire levels", capability.RuntimeWireReasoningLevels, []string{"low", "medium", "high", "xhigh", "max"})
	}

	alias, ok := GetCodexModelCapability("gpt-5.6-ultra")
	if !ok || !alias.CompatibilityAlias || alias.CanonicalTarget != "gpt-5.6-sol" || alias.DefaultReasoningLevel != "ultra" {
		t.Fatalf("ultra alias = %#v, found=%v", alias, ok)
	}
}

func TestGPT6AstraCodexCapabilities(t *testing.T) {
	for _, modelID := range []string{"gpt-6-astra", "gpt-6-astra-pro"} {
		capability, ok := GetCodexModelCapability(modelID)
		if !ok {
			t.Fatalf("GetCodexModelCapability(%q) not found", modelID)
		}
		if capability.ContextWindow != 1050000 || capability.MaxContextWindow != 1050000 {
			t.Fatalf("%s context = (%d, %d), want (1050000, 1050000)", modelID, capability.ContextWindow, capability.MaxContextWindow)
		}
		if capability.MaxCompletionTokens != 128000 {
			t.Fatalf("%s max completion = %d, want 128000", modelID, capability.MaxCompletionTokens)
		}
		assertStringSlice(t, modelID+" catalog levels", capability.CatalogReasoningLevels, []string{"low", "medium", "high", "xhigh", "max", "ultra"})
		assertStringSlice(t, modelID+" wire levels", capability.RuntimeWireReasoningLevels, []string{"low", "medium", "high", "xhigh", "max"})
	}
}

func TestGPT6AstraStaticRegistryUsesWireLevels(t *testing.T) {
	info := LookupStaticModelInfo("gpt-6-astra")
	if info == nil || info.Thinking == nil {
		t.Fatal("gpt-6-astra registry metadata missing")
	}
	if info.ContextLength != 1050000 || info.MaxCompletionTokens != 128000 {
		t.Fatalf("registry limits = (%d, %d), want (1050000, 128000)", info.ContextLength, info.MaxCompletionTokens)
	}
	assertStringSlice(t, "registry wire levels", info.Thinking.Levels, []string{"low", "medium", "high", "xhigh", "max"})
}

func TestGPT6SolLunaCodexCapabilities(t *testing.T) {
	for _, modelID := range []string{"gpt-6-sol", "gpt-6-luna"} {
		capability, ok := GetCodexModelCapability(modelID)
		if !ok {
			t.Fatalf("GetCodexModelCapability(%q) not found", modelID)
		}
		if capability.ContextWindow != 272000 || capability.MaxContextWindow != 272000 {
			t.Fatalf("%s context = (%d, %d), want (272000, 272000)", modelID, capability.ContextWindow, capability.MaxContextWindow)
		}
		if capability.DefaultReasoningLevel != "medium" {
			t.Fatalf("%s default reasoning = %q, want medium", modelID, capability.DefaultReasoningLevel)
		}
		wantCatalog := []string{"low", "medium", "high", "xhigh", "max"}
		if modelID == "gpt-6-sol" {
			wantCatalog = append(wantCatalog, "ultra")
		}
		assertStringSlice(t, modelID+" catalog levels", capability.CatalogReasoningLevels, wantCatalog)
		assertStringSlice(t, modelID+" wire levels", capability.RuntimeWireReasoningLevels, []string{"low", "medium", "high", "xhigh", "max"})
		info := LookupStaticModelInfo(modelID)
		if info == nil || info.ContextLength != 272000 || info.Thinking == nil {
			t.Fatalf("%s static registry metadata missing or incorrect: %#v", modelID, info)
		}
	}
}

func TestGPT56StaticRegistryUsesWireLevels(t *testing.T) {
	info := LookupStaticModelInfo("gpt-5.6-sol")
	if info == nil || info.Thinking == nil {
		t.Fatal("gpt-5.6-sol registry metadata missing")
	}
	if info.ContextLength != 1050000 || info.MaxCompletionTokens != 128000 {
		t.Fatalf("registry limits = (%d, %d), want (1050000, 128000)", info.ContextLength, info.MaxCompletionTokens)
	}
	assertStringSlice(t, "registry wire levels", info.Thinking.Levels, []string{"low", "medium", "high", "xhigh", "max"})
}

func assertStringSlice(t *testing.T, label string, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s len = %d, want %d: %#v", label, len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("%s[%d] = %q, want %q: %#v", label, i, got[i], want[i], got)
		}
	}
}
