package modelconfig

import (
	"database/sql"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/registry"
	log "github.com/sirupsen/logrus"
)

// seedModelChannels lists the provider channels whose static models are seeded
// into the model library.
//
// xai was missing from this list, which is why no Grok model ever reached the
// library: the catalog knew them, but the library — what the channel-group editor
// and the pricing table read — did not, so a request for one was rejected with
// "not in the allowed models of channel group" and the operator had no way to add
// it from the console.
func seedModelChannels() []string {
	return []string{
		"claude",
		"gemini",
		"vertex",
		"gemini-cli",
		"aistudio",
		"codex",
		"qwen",
		"iflow",
		"kimi",
		"cline",
		"opencode-go",
		"antigravity",
		"xai",
	}
}

// applyMediaGenerationSeedDefaults stamps the billing and modality shape a media
// model needs onto a seed row.
//
// Image and video models are billed per invocation rather than per token, so a row
// seeded with the default token pricing would under-report spend. The
// classification lives in internal/registry alongside the model definitions, so
// adding a provider does not mean hunting for every branch that special-cased a
// model ID.
func applyMediaGenerationSeedDefaults(row *ModelConfigRow, modelID string) {
	applyImageGenerationSeedDefaults(row, modelID)
	applyVideoGenerationSeedDefaults(row, modelID)
}

// applyVideoGenerationSeedDefaults is the video half. It is separate from the
// image one because the two classifications must not bleed into each other: a
// video model that classified as an image model would be offered on the
// /images/* endpoints it cannot serve.
func applyVideoGenerationSeedDefaults(row *ModelConfigRow, modelID string) {
	if row == nil || !registry.IsVideoGenerationModel(modelID) {
		return
	}
	row.InputModalities = registry.VideoGenerationInputModalities(modelID)
	row.OutputModalities = registry.VideoGenerationOutputModalities()
	row.PricingMode = "call"
	if price, description, ok := registry.VideoGenerationModelDefaults(modelID); ok {
		row.PricePerCall = price
		row.Description = description
	}
}

// isMediaGenerationModel reports whether a model is billed per call because it
// produces images or video.
func isMediaGenerationModel(modelID string) bool {
	return registry.IsImageGenerationModel(modelID) || registry.IsVideoGenerationModel(modelID)
}

// repairMediaGenerationModelConfigRows promotes pre-existing media rows into the
// model library.
//
// Rows for these models can predate their catalog entry — legacy pricing import
// creates them with source 'legacy-pricing', which the library scope filters out.
// The effect is that the channel-group editor cannot offer the model, so a request
// for it is rejected with "not in the allowed models of channel group" and the
// operator has no way to fix it from the console.
//
// Only 'legacy-pricing' rows are touched: 'user' rows carry deliberate operator
// pricing, and 'openrouter' rows already surface in the library. Every tenant is
// covered, because the seed itself only ever wrote the system tenant.
func repairMediaGenerationModelConfigRows(db *sql.DB) {
	if db == nil {
		return
	}
	now := nowRFC3339()
	for _, row := range defaultModelConfigRows() {
		if !isMediaGenerationModel(row.ModelID) {
			continue
		}
		_, err := db.Exec(
			`UPDATE model_configs
			 SET source = 'seed',
			     pricing_mode = 'call',
			     input_modalities = ?,
			     output_modalities = ?,
			     owned_by = CASE WHEN owned_by = '' THEN ? ELSE owned_by END,
			     display_name = CASE WHEN display_name = '' THEN ? ELSE display_name END,
			     updated_at = ?
			 WHERE model_id = ? AND source = 'legacy-pricing'`,
			encodeModelModalities(row.InputModalities),
			encodeModelModalities(row.OutputModalities),
			row.OwnedBy,
			row.DisplayName,
			now,
			row.ModelID,
		)
		if err != nil {
			log.Warnf("sqlite/modelconfig: repair media model config %s: %v", row.ModelID, err)
		}
	}
}
