package executor

import (
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	sdkmodelcatalog "github.com/router-for-me/CLIProxyAPI/v8/sdk/modelcatalog"
)

// withXAIMediaModels merges the Grok Imagine models into a discovered model list.
//
// An xAI credential's routable models come entirely from the upstream /models
// endpoint. For an OAuth subscription that endpoint is the CLI chat proxy, which
// lists chat models only and never reports the Imagine models — they live on the
// media host and are not discoverable at all.
//
// Registering them in the static catalog is therefore not enough: it makes them
// visible in the panel while the request router still has no credential that
// serves them, which surfaces as "auth_not_found: no auth available" before any
// upstream call is made. They have to be merged into every credential's model set.
//
// Merging is safe because the media host is reachable with the same token the chat
// surface uses; a credential that cannot reach it fails at request time with a real
// upstream error rather than looking like a missing model.
func withXAIMediaModels(models []*sdkmodelcatalog.ModelInfo) []*sdkmodelcatalog.ModelInfo {
	imageModels := xaiMediaModelInfos()
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

// xaiMediaModelInfos converts the static Grok media definitions into catalog
// entries. Sourcing them from the registry keeps one definition of the model set,
// so adding a model there makes it routable without a second edit here.
//
// Both image and video belong here: neither is discoverable upstream, and a video
// model that is only in the static catalog reproduces exactly the failure this
// file exists to prevent — visible in the panel, "auth_not_found: no auth
// available" on use.
func xaiMediaModelInfos() []*sdkmodelcatalog.ModelInfo {
	definitions := registry.GetXAIModels()
	models := make([]*sdkmodelcatalog.ModelInfo, 0, len(definitions))
	for _, definition := range definitions {
		if definition == nil {
			continue
		}
		if !registry.IsImageGenerationModel(definition.ID) && !registry.IsVideoGenerationModel(definition.ID) {
			continue
		}
		models = append(models, &sdkmodelcatalog.ModelInfo{
			ID:                  definition.ID,
			Object:              "model",
			Type:                "xai",
			OwnedBy:             definition.OwnedBy,
			DisplayName:         definition.DisplayName,
			Name:                definition.Name,
			Version:             definition.Version,
			Description:         definition.Description,
			SupportedParameters: append([]string(nil), definition.SupportedParameters...),
		})
	}
	return models
}
