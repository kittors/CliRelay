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

// TestRepairCodexImageCarrierModel covers the executor-side correction of the
// carrier the translator pins.
//
// internal/translator is off limits to pull requests, so its constant stays; this
// hook is what stops that constant from reaching upstream. It runs on the shared
// path used by streaming, non-streaming and both websocket executors.
func TestRepairCodexImageCarrierModel(t *testing.T) {
	resetCodexCarrierState(t)
	storeCodexModels(codexTestModels("gpt-9.9-future", "gpt-8.1"))

	// A bridged image request carries the translator's retired constant at the top
	// level and the real image model on the tool. Only the former may be rewritten.
	bridged := []byte(`{"model":"gpt-5.4-mini","tool_choice":{"type":"image_generation"},"tools":[{"type":"image_generation","model":"gpt-image-2"}]}`)
	repaired := repairCodexImageCarrierModel(bridged, "gpt-image-2")
	if got := gjson.GetBytes(repaired, "model").String(); got != "gpt-9.9-future" {
		t.Fatalf("model = %q, want the resolved carrier", got)
	}
	if got := gjson.GetBytes(repaired, "tools.0.model").String(); got != "gpt-image-2" {
		t.Fatalf("tools.0.model = %q, want the image model left alone", got)
	}

	// An ordinary chat request must not be touched, however its model is spelled.
	for _, baseModel := range []string{"gpt-5.5", "gpt-6-astra", ""} {
		chat := []byte(`{"model":"gpt-5.5","input":"hi"}`)
		if got := gjson.GetBytes(repairCodexImageCarrierModel(chat, baseModel), "model").String(); got != "gpt-5.5" {
			t.Fatalf("baseModel %q rewrote a chat request to %q", baseModel, got)
		}
	}

	// Already correct: no rewrite, no corruption.
	already := []byte(`{"model":"gpt-9.9-future","input":"hi"}`)
	if got := gjson.GetBytes(repairCodexImageCarrierModel(already, "gpt-image-2"), "model").String(); got != "gpt-9.9-future" {
		t.Fatalf("model = %q, want it left as-is", got)
	}
}

// TestMaybeEnsureCodexImageGenerationToolRepairsCarrier proves the repair is wired
// into the hook every outbound Codex path shares, not just callable on its own.
func TestMaybeEnsureCodexImageGenerationToolRepairsCarrier(t *testing.T) {
	resetCodexCarrierState(t)
	storeCodexModels(codexTestModels("gpt-9.9-future", "gpt-8.1"))

	bridged := []byte(`{"model":"gpt-5.4-mini","tool_choice":{"type":"image_generation"},"tools":[{"type":"image_generation","model":"gpt-image-2"}]}`)
	out := maybeEnsureCodexImageGenerationTool(bridged, nil, "gpt-image-2", nil)
	if got := gjson.GetBytes(out, "model").String(); got != "gpt-9.9-future" {
		t.Fatalf("model = %q, want the resolved carrier through the shared hook", got)
	}
}
