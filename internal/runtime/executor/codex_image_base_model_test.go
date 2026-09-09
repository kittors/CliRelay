package executor

import (
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/codexcarrier"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	sdkmodelcatalog "github.com/router-for-me/CLIProxyAPI/v6/sdk/modelcatalog"
	"github.com/tidwall/gjson"
)

func resetCodexCarrierState(t *testing.T) {
	t.Helper()
	previous := loadCodexModels()
	codexcarrier.Publish(nil)
	codexcarrier.SetOverride("")
	t.Cleanup(func() {
		codexModelsCache.mu.Lock()
		codexModelsCache.models = previous
		codexModelsCache.mu.Unlock()
		codexcarrier.Publish(nil)
		codexcarrier.SetOverride("")
	})
}

func codexTestModels(ids ...string) []*sdkmodelcatalog.ModelInfo {
	models := make([]*sdkmodelcatalog.ModelInfo, 0, len(ids))
	for _, id := range ids {
		models = append(models, &sdkmodelcatalog.ModelInfo{ID: id})
	}
	return models
}

// TestStoreCodexModelsPublishesCarrierCandidates covers the wiring that keeps the
// translator path working. That path cannot read the manifest cache itself, so if
// a manifest refresh does not publish, an image-only /v1/responses request keeps
// naming a carrier the account may no longer call.
func TestStoreCodexModelsPublishesCarrierCandidates(t *testing.T) {
	resetCodexCarrierState(t)

	if !storeCodexModels(codexTestModels("gpt-9.9-future", "gpt-5.3-codex-spark")) {
		t.Fatal("storeCodexModels() = false, want the list to be stored")
	}
	if got := codexcarrier.Resolve(); got != "gpt-9.9-future" {
		t.Fatalf("Resolve() = %q, want the surviving manifest entry", got)
	}

	if !storeCodexModels(codexTestModels("gpt-9.9-future", codexcarrier.Default)) {
		t.Fatal("storeCodexModels() = false, want the list to be stored")
	}
	if got := codexcarrier.Resolve(); got != codexcarrier.Default {
		t.Fatalf("Resolve() = %q, want %q once the manifest lists it", got, codexcarrier.Default)
	}
}

func TestResolveCodexImageBaseModelFollowsManifest(t *testing.T) {
	resetCodexCarrierState(t)

	executor := &CodexExecutor{cfg: &config.Config{}}

	// Cold manifest: the default is the best available guess.
	if got := executor.resolveCodexImageBaseModel(); got != codexcarrier.Default {
		t.Fatalf("resolveCodexImageBaseModel() = %q, want %q", got, codexcarrier.Default)
	}

	storeCodexModels(codexTestModels("gpt-9.9-future", "gpt-8.1"))
	if got := executor.resolveCodexImageBaseModel(); got != "gpt-9.9-future" {
		t.Fatalf("resolveCodexImageBaseModel() = %q, want the manifest entry", got)
	}
}

func TestResolveCodexImageBaseModelHonoursConfigOverride(t *testing.T) {
	resetCodexCarrierState(t)

	storeCodexModels(codexTestModels(codexcarrier.Default, "gpt-8.1"))
	executor := &CodexExecutor{cfg: &config.Config{CodexImageBaseModel: " gpt-operator-choice "}}

	if got := executor.resolveCodexImageBaseModel(); got != "gpt-operator-choice" {
		t.Fatalf("resolveCodexImageBaseModel() = %q, want the trimmed operator override", got)
	}
}

// TestBuildCodexImageResponsesRequestUsesResolvedCarrier is the assertion the old
// test suite was missing: it checks the request carries whatever resolution chose,
// rather than pinning the model name the way the constant-era test did.
func TestBuildCodexImageResponsesRequestUsesResolvedCarrier(t *testing.T) {
	resetCodexCarrierState(t)

	parsed := &codexImageRequest{Prompt: "draw a fox", N: 1}
	body, err := buildCodexImageResponsesRequest(parsed, codexImageModel, "gpt-carrier-under-test")
	if err != nil {
		t.Fatalf("buildCodexImageResponsesRequest() error = %v", err)
	}
	if got := gjson.GetBytes(body, "model").String(); got != "gpt-carrier-under-test" {
		t.Fatalf("model = %q, want the resolved carrier", got)
	}
	if got := gjson.GetBytes(body, "tools.0.model").String(); got != codexImageModel {
		t.Fatalf("tools.0.model = %q, want %q", got, codexImageModel)
	}

	// An empty carrier must fall back to resolution rather than being sent blank,
	// which upstream rejects as an invalid model.
	body, err = buildCodexImageResponsesRequest(parsed, codexImageModel, "  ")
	if err != nil {
		t.Fatalf("buildCodexImageResponsesRequest() error = %v", err)
	}
	if got := gjson.GetBytes(body, "model").String(); got != codexcarrier.Default {
		t.Fatalf("model = %q, want %q for a blank carrier", got, codexcarrier.Default)
	}
}
