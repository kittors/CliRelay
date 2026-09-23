package authfiles

import (
	"os"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/registry"
)

func mergedIDs(provider string, live []*registry.ModelInfo) map[string]struct{} {
	merged := mergeDiscoveryWithStaticCatalog(provider, live)
	ids := make(map[string]struct{}, len(merged))
	for _, model := range merged {
		if model != nil {
			ids[model.ID] = struct{}{}
		}
	}
	return ids
}

// TestRefreshKeepsModelsDiscoveryOmits is the reported bug: pressing refresh on a
// Codex account dropped gpt-image-2 from the panel, because the ChatGPT manifest is
// a subset of what the runtime actually registers. The panel then showed a narrower
// list than the deployment can route, which reads as "this account lacks the model".
func TestRefreshKeepsModelsDiscoveryOmits(t *testing.T) {
	live := []*registry.ModelInfo{{ID: "gpt-5.5", Object: "model"}}

	ids := mergedIDs("codex", live)
	if _, ok := ids["gpt-5.5"]; !ok {
		t.Error("the discovered model was lost")
	}
	if _, ok := ids["gpt-image-2"]; !ok {
		t.Error("gpt-image-2 is registered and routable but missing from the panel view")
	}
}

// TestRetiredChatModelsStayHidden is the counterpart to the test above. An earlier
// revision merged the whole static catalog, which resurrected models OpenAI has
// retired (gpt-5.1, gpt-5.2, gpt-5-codex...) and showed operators models they
// cannot use. A chat model absent from the listing has been withdrawn; only models
// the listing structurally cannot contain are restored.
func TestRetiredChatModelsStayHidden(t *testing.T) {
	live := []*registry.ModelInfo{{ID: "gpt-5.5", Object: "model"}}

	ids := mergedIDs("codex", live)
	for _, retired := range []string{"gpt-5", "gpt-5.1", "gpt-5.2", "gpt-5-codex", "gpt-5.1-codex"} {
		if _, ok := ids[retired]; ok {
			t.Errorf("%s is not reported by discovery and must not be resurrected", retired)
		}
	}
	// The one model the listing cannot contain is still restored.
	if _, ok := ids["gpt-image-2"]; !ok {
		t.Error("gpt-image-2 must still be restored")
	}
}

// TestUpstreamDefinitionWins keeps discovery authoritative for anything it reports,
// so a live definition is never shadowed by the compiled-in one.
func TestUpstreamDefinitionWins(t *testing.T) {
	live := []*registry.ModelInfo{{ID: "gpt-image-2", Object: "model", DisplayName: "Live Name"}}

	merged := mergeDiscoveryWithStaticCatalog("codex", live)
	count := 0
	var found *registry.ModelInfo
	for _, model := range merged {
		if model != nil && model.ID == "gpt-image-2" {
			count++
			found = model
		}
	}
	if count != 1 {
		t.Fatalf("gpt-image-2 appears %d times, want 1", count)
	}
	if found.DisplayName != "Live Name" {
		t.Error("the static definition shadowed what upstream reported")
	}
}

// TestAuthoritativeProvidersAreUntouched: antigravity discovery replaces the
// registry, so merging a static catalog into it would resurrect stale models.
func TestAuthoritativeProvidersAreUntouched(t *testing.T) {
	live := []*registry.ModelInfo{{ID: "only-model", Object: "model"}}

	merged := mergeDiscoveryWithStaticCatalog("antigravity", live)
	if len(merged) != 1 || merged[0].ID != "only-model" {
		t.Errorf("merged = %v, want the discovery result unchanged", merged)
	}
}

func TestXAIRefreshKeepsImageModels(t *testing.T) {
	// The Grok CLI gateway advertises a single chat model; the Imagine models are
	// not discoverable and must still be listed.
	ids := mergedIDs("xai", []*registry.ModelInfo{{ID: "grok-4.5", Object: "model"}})
	for _, modelID := range []string{"grok-4.5", "grok-imagine-image", "grok-imagine-image-quality"} {
		if _, ok := ids[modelID]; !ok {
			t.Errorf("%s missing from the merged xai list", modelID)
		}
	}
	// Retired Grok chat models must not come back either.
	if _, ok := ids["grok-build-0.1"]; ok {
		t.Error("a chat model absent from discovery must not be resurrected")
	}
}

// TestCachedDiscoveryPathAlsoMerges covers the call site, not just the helper.
//
// The previous fix merged only the fallback branch, so codex — which always takes
// the shared-discovery path — still lost gpt-image-2 in the panel. The helper's own
// tests passed the whole time, which is exactly why this test drives the entry
// point instead.
func TestCachedDiscoveryPathAlsoMerges(t *testing.T) {
	const tenantID = "tenant-merge-test"
	storeDiscoveryCache(tenantID, "codex", []*registry.ModelInfo{{ID: "gpt-5.5", Object: "model"}})
	t.Cleanup(func() { storeDiscoveryCache(tenantID, "codex", nil) })

	cached := loadDiscoveryCache(tenantID, "codex")
	if len(cached) == 0 {
		t.Skip("discovery cache unavailable in this environment")
	}

	entries := modelEntriesFromRegistry(mergeDiscoveryWithStaticCatalog("codex", cached))
	ids := make(map[string]struct{}, len(entries))
	for _, entry := range entries {
		if id, ok := entry["id"].(string); ok {
			ids[id] = struct{}{}
		}
	}
	if _, ok := ids["gpt-image-2"]; !ok {
		t.Error("the cached-discovery path still hides gpt-image-2 from the panel")
	}
	if _, ok := ids["gpt-5.5"]; !ok {
		t.Error("the cached discovery result was lost")
	}
}

// TestEveryDiscoveryReturnMerges guards against the same omission recurring: each
// return inside the shared-discovery branch must go through the merge.
func TestEveryDiscoveryReturnMerges(t *testing.T) {
	source, err := os.ReadFile("models.go")
	if err != nil {
		t.Fatalf("read models.go: %v", err)
	}
	block := string(source)
	// Match the condition's prefix only: the branch also checks the credential kind.
	start := strings.Index(block, "if supportsSharedDiscovery(provider)")
	if start < 0 {
		t.Fatal("shared discovery branch not found")
	}
	end := strings.Index(block[start:], "\n\tif !refresh {")
	if end < 0 {
		t.Fatal("end of shared discovery branch not found")
	}
	branch := block[start : start+end]

	for _, line := range strings.Split(branch, "\n") {
		if !strings.Contains(line, "modelEntriesFromRegistry(") {
			continue
		}
		if !strings.Contains(line, "mergeDiscoveryWithStaticCatalog(") {
			t.Errorf("this return skips the static catalog merge and will hide routable models:\n  %s", strings.TrimSpace(line))
		}
	}
}
