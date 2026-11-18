package serviceapp

import (
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	sdkmodelcatalog "github.com/router-for-me/CLIProxyAPI/v8/sdk/modelcatalog"
)

// Image-generation model registration.
//
// Chat model lists are curated: retired models are pruned so operators are not
// offered something upstream no longer serves. Image models must not be swept up
// in that pruning, and repeatedly were:
//
//   - They never appear in a provider's chat model listing, because generation is
//     served by a different endpoint. Absence there carries no signal about
//     availability, unlike a retired chat model.
//   - Every entitled account has them. gpt-image-2 is available to any Codex Plus
//     or Pro account, and the Grok Imagine models to any subscription that can
//     reach the media host.
//
// The result of getting this wrong is that a model the runtime can serve is not
// registered, so it cannot be selected, cannot be added to a channel group's
// allowed list, and reports itself as unavailable — while the credential detail
// panel happily shows it.
//
// This was previously fixed for xAI alone, inside its model fetch. Doing it per
// provider is what let Codex regress the same way, so it now applies to every
// provider that serves image models.
//
// It lives on the internal side because sdk packages may not import internal ones,
// and no sdk/cliproxy file does today. Widening that boundary for one helper would
// be the wrong trade.

// withRegisteredImageModels guarantees a provider's image-generation models are in
// the set registered for a credential.
//
// Models already present are left alone: a live definition is more current than the
// compiled-in one.
func WithRegisteredImageModels(provider string, models []*sdkmodelcatalog.ModelInfo) []*sdkmodelcatalog.ModelInfo {
	imageModels := imageModelsForProvider(provider)
	if len(imageModels) == 0 {
		return models
	}

	seen := make(map[string]struct{}, len(models))
	for _, model := range models {
		if model == nil {
			continue
		}
		seen[strings.ToLower(strings.TrimSpace(model.ID))] = struct{}{}
	}

	out := append([]*sdkmodelcatalog.ModelInfo(nil), models...)
	for _, model := range imageModels {
		if _, exists := seen[strings.ToLower(strings.TrimSpace(model.ID))]; exists {
			continue
		}
		out = append(out, model)
	}
	return out
}

// imageModelsForProvider resolves a provider's image models from the static
// catalog, so adding one there makes it routable without a second edit here.
func imageModelsForProvider(provider string) []*sdkmodelcatalog.ModelInfo {
	channel := strings.ToLower(strings.TrimSpace(provider))
	if channel == "" {
		return nil
	}

	definitions := sdkmodelcatalog.StaticModelDefinitionsByChannel(channel)
	images := make([]*sdkmodelcatalog.ModelInfo, 0, 2)
	for _, definition := range definitions {
		if definition == nil || !registry.IsImageGenerationModel(definition.ID) {
			continue
		}
		// The provider that serves the model has to match the credential's, or a
		// Codex account would advertise Grok's models and fail at request time.
		if !strings.EqualFold(registry.ImageGenerationProvider(definition.ID), channel) {
			continue
		}
		images = append(images, definition)
	}
	return images
}
