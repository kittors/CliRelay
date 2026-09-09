package executor

import (
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/registry"
	sdkmodelcatalog "github.com/router-for-me/CLIProxyAPI/v6/sdk/modelcatalog"
)

// withCodexImageModels merges the Codex image models into a discovered model list.
//
// A Codex credential's routable models come entirely from the ChatGPT manifest,
// which lists chat models only — image generation is served through a separate
// tool/endpoint and never appears there. Registering an image model in the static
// catalog therefore makes it visible in the panel while the request router still
// has no credential that serves it, and the request fails with
//
//	auth_not_found: no auth available
//
// before any upstream call is made. gpt-image-2 escaped this because it predates
// the manifest becoming the sole source; gpt-image-2.5-flare and -sunburst did not,
// and were selectable in the console but unusable.
//
// This is the same merge xAI already does in withXAIMediaModels, for the same
// reason. Doing it per provider is what let Codex miss it.
//
// Merging is safe because image generation is reachable with the same token the
// chat surface uses; a credential that cannot serve one fails at request time with
// a real upstream error rather than looking like a missing model.
func withCodexImageModels(models []*sdkmodelcatalog.ModelInfo) []*sdkmodelcatalog.ModelInfo {
	imageModels := codexImageModelInfos()
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

	merged := make([]*sdkmodelcatalog.ModelInfo, 0, len(models)+len(imageModels))
	merged = append(merged, models...)
	for _, model := range imageModels {
		// A live listing that already advertises the model wins, so upstream stays
		// authoritative about anything it does report.
		if _, exists := seen[strings.ToLower(strings.TrimSpace(model.ID))]; exists {
			continue
		}
		merged = append(merged, model)
	}
	return merged
}

// codexImageModelInfos converts the static Codex image definitions into catalog
// entries. Sourcing them from the registry keeps one definition of the model set,
// so adding a model there makes it routable without a second edit here.
func codexImageModelInfos() []*sdkmodelcatalog.ModelInfo {
	definitions := registry.GetOpenAIModels()
	models := make([]*sdkmodelcatalog.ModelInfo, 0, 4)
	for _, definition := range definitions {
		if definition == nil || !registry.IsImageGenerationModel(definition.ID) {
			continue
		}
		// The provider serving the model has to be codex, or a Codex credential
		// would advertise another pool's models and fail at request time.
		if !strings.EqualFold(registry.ImageGenerationProvider(definition.ID), registry.ImageProviderCodex) {
			continue
		}
		models = append(models, &sdkmodelcatalog.ModelInfo{
			ID:                  definition.ID,
			Object:              definition.Object,
			Created:             definition.Created,
			OwnedBy:             definition.OwnedBy,
			Type:                definition.Type,
			DisplayName:         definition.DisplayName,
			Description:         definition.Description,
			Version:             definition.Version,
			SupportedParameters: append([]string(nil), definition.SupportedParameters...),
		})
	}
	return models
}
