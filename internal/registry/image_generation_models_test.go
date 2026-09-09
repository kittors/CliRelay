package registry

import (
	"strings"
	"testing"
)

func TestIsImageGenerationModel(t *testing.T) {
	imageModels := []string{
		"gpt-image-2",
		"GPT-Image-2",
		"gpt-image-3",
		"grok-imagine-image",
		"grok-imagine-image-quality",
	}
	for _, modelID := range imageModels {
		if !IsImageGenerationModel(modelID) {
			t.Errorf("IsImageGenerationModel(%q) = false, want true", modelID)
		}
	}

	// grok-2-image-1212 and grok-imagine-image-pro came from a third-party model
	// list and are not in the live xAI catalog, so they must not classify.
	textModels := []string{
		"", "grok-4.5", "grok-build-0.1", "gpt-5.5", "grok-imagine-video", "grok-2-image-1212",
	}
	for _, modelID := range textModels {
		if IsImageGenerationModel(modelID) {
			t.Errorf("IsImageGenerationModel(%q) = true, want false", modelID)
		}
	}
}

// TestUnknownImageModelVersionsStillClassify is why the prefix list exists: a new
// release in a known family must be billed per call on day one, not per token.
func TestUnknownImageModelVersionsStillClassify(t *testing.T) {
	if !IsImageGenerationModel("gpt-image-9-preview") {
		t.Error("an unreleased gpt-image version should still classify as image generation")
	}
	if price, _, ok := ImageGenerationModelDefaults("gpt-image-9-preview"); ok || price != 0 {
		t.Error("a prefix-only match must not invent a price")
	}
}

// TestVideoModelsAreNotImageModels guards the boundary while video support is being
// built out: grok-imagine-video shares the family prefix but is not an image model.
func TestVideoModelsAreNotImageModels(t *testing.T) {
	if IsImageGenerationModel("grok-imagine-video") {
		t.Error("grok-imagine-video must not classify as an image generation model")
	}
}

func TestImageGenerationModelDefaults(t *testing.T) {
	price, description, ok := ImageGenerationModelDefaults("gpt-image-2")
	if !ok {
		t.Fatal("gpt-image-2 should have defaults")
	}
	if price != 0.04 {
		t.Errorf("price = %v, want 0.04 to preserve existing billing", price)
	}
	if description == "" {
		t.Error("description should be set")
	}

	// Grok Imagine is billed against the subscription, so no per-call price is
	// published. Inventing one would show up as real spend in usage reporting.
	if price, _, ok := ImageGenerationModelDefaults("grok-imagine-image"); !ok || price != 0 {
		t.Errorf("grok-imagine-image price = %v, want 0", price)
	}

	// Models absent from the live catalog must not classify at all.
	if IsImageGenerationModel("grok-2-image-1212") {
		t.Error("grok-2-image-1212 is not in the xAI catalog and must not classify")
	}

	if _, _, ok := ImageGenerationModelDefaults("grok-4.5"); ok {
		t.Error("a text model should have no image defaults")
	}
}

// TestImageInputModalityStaysTextOnly pins a deliberate decision: edit support is
// not the same as an image input modality, and widening it would reclassify
// existing models as image-to-image in the catalog.
func TestImageInputModalityStaysTextOnly(t *testing.T) {
	for _, modelID := range []string{"gpt-image-2", "grok-imagine-image", "grok-imagine-image-quality"} {
		got := ImageGenerationInputModalities(modelID)
		if len(got) != 1 || got[0] != "text" {
			t.Errorf("ImageGenerationInputModalities(%q) = %v, want [text]", modelID, got)
		}
	}
}

func TestSupportsImageEditing(t *testing.T) {
	for _, modelID := range []string{"gpt-image-2", "grok-imagine-image", "grok-imagine-image-quality"} {
		if !SupportsImageEditing(modelID) {
			t.Errorf("SupportsImageEditing(%q) = false, want true", modelID)
		}
	}
}

func TestImageGenerationOutputModalitiesIsNotShared(t *testing.T) {
	first := ImageGenerationOutputModalities()
	first[0] = "mutated"
	if second := ImageGenerationOutputModalities(); second[0] != "image" {
		t.Error("callers must not be able to mutate the shared modality set")
	}
}

// TestGrokImageModelsAreRegistered ties the classifier to the static catalog: a
// model that classifies as image generation but is absent from the catalog would
// never be selectable.
func TestGrokImageModelsAreRegistered(t *testing.T) {
	registered := make(map[string]struct{})
	for _, model := range GetXAIModels() {
		registered[model.ID] = struct{}{}
	}
	for _, modelID := range []string{"grok-imagine-image", "grok-imagine-image-quality"} {
		if _, ok := registered[modelID]; !ok {
			t.Errorf("%s is classified as an image model but is not in the xAI catalog", modelID)
		}
	}
}

func TestMiniMaxImageModelsClassify(t *testing.T) {
	for _, modelID := range []string{"image-01", "IMAGE-01", " image-01 "} {
		if !IsImageGenerationModel(modelID) {
			t.Errorf("IsImageGenerationModel(%q) = false, want true", modelID)
		}
	}

	// The official image-01 rate is charged once per single-image invocation.
	for _, modelID := range []string{"image-01"} {
		price, description, ok := ImageGenerationModelDefaults(modelID)
		if !ok {
			t.Errorf("%s should have defaults", modelID)
			continue
		}
		if price != 0.0035 {
			t.Errorf("%s price = %v, want 0.0035", modelID, price)
		}
		if description == "" {
			t.Errorf("%s description should be set", modelID)
		}
	}
}

// TestMiniMaxImageModelsRouteToMiniMax is the routing rule the shared images handler
// depends on: without it these models resolve to no provider at all and the request
// is rejected as unsupported.
func TestMiniMaxImageModelsRouteToMiniMax(t *testing.T) {
	for _, modelID := range []string{"image-01"} {
		if got := ImageGenerationProvider(modelID); got != ImageProviderMiniMax {
			t.Errorf("ImageGenerationProvider(%q) = %q, want %q", modelID, got, ImageProviderMiniMax)
		}
	}

	// The existing providers must keep their models.
	if got := ImageGenerationProvider("gpt-image-2"); got != ImageProviderCodex {
		t.Errorf("ImageGenerationProvider(gpt-image-2) = %q, want %q", got, ImageProviderCodex)
	}
	if got := ImageGenerationProvider("grok-imagine-image"); got != ImageProviderXAI {
		t.Errorf("ImageGenerationProvider(grok-imagine-image) = %q, want %q", got, ImageProviderXAI)
	}

	// Text-to-image only: the reference-image form is a different upstream request
	// shape, so claiming edit support would offer a control this build cannot serve.
	for _, modelID := range []string{"image-01"} {
		if SupportsImageEditing(modelID) {
			t.Errorf("SupportsImageEditing(%q) = true, want false", modelID)
		}
	}
}

// TestMiniMaxImageModelsAreRegistered ties the classifier to the static catalog: a
// model that routes to a provider but is absent from the catalog is selectable and
// unreachable at the same time.
func TestMiniMaxImageModelsAreRegistered(t *testing.T) {
	wanted := []string{"image-01"}

	registered := make(map[string]struct{})
	for _, model := range GetMiniMaxModels() {
		registered[model.ID] = struct{}{}
	}
	for _, modelID := range wanted {
		if _, ok := registered[modelID]; !ok {
			t.Errorf("%s is classified as an image model but is not in the MiniMax catalog", modelID)
		}
		if LookupStaticModelInfo(modelID) == nil {
			t.Errorf("%s is not reachable through the static model lookup", modelID)
		}
	}

	byChannel := make(map[string]struct{})
	for _, model := range GetStaticModelDefinitionsByChannel("minimax") {
		byChannel[model.ID] = struct{}{}
	}
	for _, modelID := range wanted {
		if _, ok := byChannel[modelID]; !ok {
			t.Errorf("%s is missing from the minimax channel catalog", modelID)
		}
	}

	listed := make(map[string]ImageGenerationModel)
	for _, model := range ListImageGenerationModels() {
		listed[model.ID] = model
	}
	for _, modelID := range wanted {
		model, ok := listed[modelID]
		if !ok {
			t.Errorf("%s is not selectable in the console", modelID)
			continue
		}
		if model.Provider != ImageProviderMiniMax {
			t.Errorf("%s listed provider = %q, want %q", modelID, model.Provider, ImageProviderMiniMax)
		}
		if model.SupportsEdit {
			t.Errorf("%s must not advertise edit support", modelID)
		}
	}
	// The models that were already selectable must stay selectable.
	for _, modelID := range []string{"gpt-image-2", "grok-imagine-image"} {
		if _, ok := listed[modelID]; !ok {
			t.Errorf("%s is no longer selectable", modelID)
		}
	}
}

func TestMiniMaxDoesNotAdvertiseUndocumentedTextToImageModels(t *testing.T) {
	for _, id := range []string{"image-01-live", "image-01-preview"} {
		if IsImageGenerationModel(id) || ImageGenerationProvider(id) != "" {
			t.Errorf("unsupported text-to-image model advertised: %s", id)
		}
		for _, model := range GetMiniMaxModels() {
			if model.ID == id {
				t.Errorf("unsupported static model: %s", id)
			}
		}
	}
}

// TestGPTImage25ModelsAreRegistered covers the 2.5 pair reaching every surface a
// selectable image model has to appear on: the codex catalog, the classifier, the
// provider mapping, edit support, and the console listing.
func TestGPTImage25ModelsAreRegistered(t *testing.T) {
	wanted := []string{"gpt-image-2.5-flare", "gpt-image-2.5-sunburst"}

	registered := make(map[string]*ModelInfo)
	for _, model := range GetOpenAIModels() {
		registered[model.ID] = model
	}
	listed := make(map[string]ImageGenerationModel)
	for _, model := range ListImageGenerationModels() {
		listed[model.ID] = model
	}

	for _, modelID := range wanted {
		info, ok := registered[modelID]
		if !ok {
			t.Errorf("%s is missing from the codex static catalog", modelID)
			continue
		}
		// The panel shows this text; it must keep saying the subscription channel
		// still renders 2.0, because upstream ignores the model field there.
		if !strings.Contains(info.Description, "GPT Image 2.0") {
			t.Errorf("%s description should disclose the 2.0 downgrade, got %q", modelID, info.Description)
		}
		if !IsImageGenerationModel(modelID) {
			t.Errorf("IsImageGenerationModel(%q) = false, want true", modelID)
		}
		if got := ImageGenerationProvider(modelID); got != ImageProviderCodex {
			t.Errorf("ImageGenerationProvider(%q) = %q, want %q", modelID, got, ImageProviderCodex)
		}
		if !SupportsImageEditing(modelID) {
			t.Errorf("SupportsImageEditing(%q) = false, want true", modelID)
		}
		if _, _, ok := ImageGenerationModelDefaults(modelID); !ok {
			t.Errorf("%s has no billing defaults, so it would be billed per token", modelID)
		}
		if _, ok := listed[modelID]; !ok {
			t.Errorf("%s is absent from ListImageGenerationModels", modelID)
		}
	}
}
