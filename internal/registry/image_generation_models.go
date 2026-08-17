package registry

import "strings"

// Image-generation model classification.
//
// This used to be spelled out at each call site — a `modelID == "gpt-image-2"`
// branch when seeding model configs, a `HasPrefix(modelID, "gpt-image-")` branch
// when syncing pricing. Adding a provider meant finding every one of those
// branches, and missing one produced a model that generates images but is billed
// per token, or that never surfaces an image capability in the panel.
//
// The classification lives here instead, next to the static model definitions it
// describes, so a new image model is one entry rather than a hunt.

// imageGenerationModelDefaults describes an image-generation model's billing and
// modality shape.
type imageGenerationModelDefaults struct {
	// PricePerCall is the per-invocation price. Image models are billed per call
	// rather than per token, which is why they need an explicit pricing mode.
	PricePerCall float64
	Description  string
}

// imageGenerationModels maps a model ID to its defaults. IDs are lower-cased.
var imageGenerationModels = map[string]imageGenerationModelDefaults{
	"gpt-image-2": {
		PricePerCall: 0.04,
		Description:  "Image generation model billed per invocation",
	},
	// Grok Imagine. These two ids are what api.x.ai actually serves; earlier
	// revisions also listed grok-imagine-image-pro and grok-2-image-1212, taken
	// from a third-party model list and never present in the live catalog, so
	// selecting them could only ever fail.
	//
	// Pricing is left at zero because xAI bills these against the subscription
	// rather than at a published per-image rate; an invented number would show up
	// as real spend in usage reporting.
	"grok-imagine-image": {
		Description: "Grok Imagine image generation, billed per invocation",
	},
	"grok-imagine-image-quality": {
		Description: "Grok Imagine Quality image generation, billed per invocation",
	},
	// MiniMax image generation. Pricing stays at zero for the same reason as
	// above: the published reference documents no per-image rate, so any number
	// here would be invented spend in usage reporting.
	"image-01": {
		Description: "MiniMax image generation, billed per invocation",
	},
	"image-01-live": {
		Description: "MiniMax Live image generation, billed per invocation",
	},
}

// imageGenerationModelPrefixes covers families that version faster than this list
// can be updated. A new gpt-image-* release is still billed per call on day one.
var imageGenerationModelPrefixes = []string{
	"gpt-image-",
	"grok-imagine-image",
	// Covers image-01 revisions, including image-01-live.
	"image-01",
}

// IsImageGenerationModel reports whether a model produces images rather than text.
func IsImageGenerationModel(modelID string) bool {
	normalized := strings.ToLower(strings.TrimSpace(modelID))
	if normalized == "" {
		return false
	}
	if _, ok := imageGenerationModels[normalized]; ok {
		return true
	}
	for _, prefix := range imageGenerationModelPrefixes {
		if strings.HasPrefix(normalized, prefix) {
			return true
		}
	}
	return false
}

// ImageGenerationModelDefaults returns the per-call price and description for an
// image model. The second result is false for models that are not image models, and
// for image models matched only by prefix, which have no published price here.
func ImageGenerationModelDefaults(modelID string) (float64, string, bool) {
	normalized := strings.ToLower(strings.TrimSpace(modelID))
	defaults, ok := imageGenerationModels[normalized]
	if !ok {
		return 0, "", false
	}
	return defaults.PricePerCall, defaults.Description, true
}

// ImageGenerationOutputModalities is the output modality set every image model
// carries. Returned as a fresh slice so callers can mutate it safely.
func ImageGenerationOutputModalities() []string {
	return []string{"image"}
}

// ImageGenerationInputModalities lists what an image model accepts.
//
// This stays text-only even for models whose edit endpoint takes reference images.
// The modality set describes the primary generation call and drives catalog
// filtering; widening it to "image" would reclassify existing models as
// image-to-image in the panel. Edit capability is a separate question, answered by
// SupportsImageEditing, which is what the UI uses to decide whether to offer a
// reference-image control.
func ImageGenerationInputModalities(string) []string {
	return []string{"text"}
}

// Provider identifiers for image-generation models. The management console needs
// to know which credential pool can serve a given image model; without this it
// pinned every request to codex, which is what kept Grok models unreachable.
const (
	ImageProviderCodex   = "codex"
	ImageProviderXAI     = "xai"
	ImageProviderMiniMax = "minimax"
)

// ImageGenerationProvider returns the credential provider that serves a model, or
// "" when the model is not an image-generation model.
func ImageGenerationProvider(modelID string) string {
	normalized := strings.ToLower(strings.TrimSpace(modelID))
	switch {
	case strings.HasPrefix(normalized, "gpt-image-"):
		return ImageProviderCodex
	case strings.HasPrefix(normalized, "grok-"):
		if IsImageGenerationModel(normalized) {
			return ImageProviderXAI
		}
		return ""
	case strings.HasPrefix(normalized, "image-01"):
		return ImageProviderMiniMax
	default:
		return ""
	}
}

// ImageGenerationModel describes a selectable image model for the console.
type ImageGenerationModel struct {
	ID           string  `json:"id"`
	Provider     string  `json:"provider"`
	DisplayName  string  `json:"display_name,omitempty"`
	Description  string  `json:"description,omitempty"`
	SupportsEdit bool    `json:"supports_edit"`
	PricePerCall float64 `json:"price_per_call,omitempty"`
}

// ListImageGenerationModels enumerates the image models this build knows about,
// derived from the static catalog so a newly registered model needs no second edit
// here to become selectable.
func ListImageGenerationModels() []ImageGenerationModel {
	seen := make(map[string]struct{})
	models := make([]ImageGenerationModel, 0, 4)

	appendModel := func(info *ModelInfo) {
		if info == nil || !IsImageGenerationModel(info.ID) {
			return
		}
		if _, ok := seen[info.ID]; ok {
			return
		}
		provider := ImageGenerationProvider(info.ID)
		if provider == "" {
			return
		}
		seen[info.ID] = struct{}{}
		price, description, _ := ImageGenerationModelDefaults(info.ID)
		if strings.TrimSpace(info.Description) != "" {
			description = info.Description
		}
		models = append(models, ImageGenerationModel{
			ID:           info.ID,
			Provider:     provider,
			DisplayName:  info.DisplayName,
			Description:  description,
			SupportsEdit: SupportsImageEditing(info.ID),
			PricePerCall: price,
		})
	}

	for _, info := range GetOpenAIModels() {
		appendModel(info)
	}
	for _, info := range GetXAIModels() {
		appendModel(info)
	}
	for _, info := range GetMiniMaxModels() {
		appendModel(info)
	}
	return models
}

// SupportsImageEditing reports whether a model accepts reference images through the
// /images/edits endpoint, as opposed to text-to-image only.
func SupportsImageEditing(modelID string) bool {
	normalized := strings.ToLower(strings.TrimSpace(modelID))
	switch {
	case strings.HasPrefix(normalized, "gpt-image-"):
		return true
	case strings.HasPrefix(normalized, "grok-imagine-image"):
		return true
	// image-01 is text-to-image here. The reference-image form of these models is
	// a separate upstream request shape rather than a variant of the generation
	// call, so claiming edit support would offer the console a control this build
	// cannot serve yet.
	default:
		return false
	}
}
