package management

import (
	"encoding/base64"
	"fmt"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/registry"
	"github.com/tidwall/sjson"
)

// Modality routing for the model catalog probe.
//
// The probe used to build one shape for every model: a non-streaming chat
// completion carrying the operator's prompt. For a text model that answers the
// question an operator is asking ("can this be reached"). For gpt-image-2.5-flare
// it does not — the request either comes back as prose, because the upstream
// treated an image model as a chat model, or fails for a reason that says nothing
// about the model's actual health. Either way the catalog reported on a code path
// no real image client uses.
//
// Modality is derived from the model registry rather than trusted from the
// caller. The registry is already the source /v1/images/* and /v1/videos/* use to
// decide which models they serve, so deriving from it is what keeps "testable in
// the catalog" and "servable in production" from drifting apart.

type modelTestMode string

const (
	modeText           modelTestMode = "text"
	modeVision         modelTestMode = "vision"
	modeImage          modelTestMode = "image"
	modeImageEdit      modelTestMode = "image_edit"
	modeVideo          modelTestMode = "video"
	modeVideoFromImage modelTestMode = "video_from_image"
)

// Default prompts per modality.
//
// These are kept in English on purpose: they are sent to the model, not shown as
// UI copy, and every provider here is measurably better at following an English
// prompt. Each one is written so a wrong result is obvious at a glance — a
// specific subject and setting beats "draw something nice", which any model can
// satisfy with anything.
const (
	defaultTextTestPrompt      = "How is the weather in Los Angeles today?"
	defaultVisionTestPrompt    = "Describe this image in one sentence, and name any text you can read in it."
	defaultImageTestPrompt     = "A red ceramic teapot on a white marble table, soft window light, photorealistic"
	defaultImageEditTestPrompt = "Replace the background with a snowy mountain range. Keep the main subject unchanged."
	defaultVideoTestPrompt     = "A paper boat drifting across a rain puddle, close-up, gentle ripples, natural daylight"
	defaultVideoImagePrompt    = "Slowly push the camera in while the scene comes to life with subtle motion."
)

// modelTestModeInfo describes one runnable probe shape for a model.
type modelTestModeInfo struct {
	Mode          modelTestMode `json:"mode"`
	DefaultPrompt string        `json:"default_prompt"`
	// RequiresImage marks the modes whose whole point is a reference image, so the
	// panel can require an upload instead of letting the operator submit a request
	// the upstream will reject.
	RequiresImage bool `json:"requires_image"`
}

// modelTestOptions is everything the panel needs to render a correct form for one
// model. It is served by the API rather than inferred in the browser because the
// browser only has the model id and its OpenRouter modality tags, which say
// nothing about whether this deployment can run an edit or an image-to-video call
// for that model.
type modelTestOptions struct {
	Model              string              `json:"model"`
	Modes              []modelTestModeInfo `json:"modes"`
	Sizes              []string            `json:"sizes,omitempty"`
	Qualities          []string            `json:"qualities,omitempty"`
	MaxImages          int                 `json:"max_images,omitempty"`
	MaxDurationSeconds int                 `json:"max_duration_seconds,omitempty"`
}

// imageTestQualities mirrors the levels the image console offers. Kept as a plain
// list because the upstream validates the value and an unknown one is a 400, so
// the panel should only ever offer levels that exist.
var imageTestQualities = []string{"low", "medium", "high"}

// modelTestModes reports the probe shapes a model supports, most representative
// first. The first entry is what the panel selects by default, so an image model
// opens on "generate an image" rather than on a chat box.
func modelTestModes(model string) []modelTestModeInfo {
	switch {
	case registry.IsVideoGenerationModel(model):
		modes := []modelTestModeInfo{{Mode: modeVideo, DefaultPrompt: defaultVideoTestPrompt}}
		if registry.SupportsImageToVideo(model) {
			modes = append(modes, modelTestModeInfo{
				Mode:          modeVideoFromImage,
				DefaultPrompt: defaultVideoImagePrompt,
				RequiresImage: true,
			})
		}
		return modes
	case registry.IsImageGenerationModel(model):
		modes := []modelTestModeInfo{{Mode: modeImage, DefaultPrompt: defaultImageTestPrompt}}
		if registry.SupportsImageEditing(model) {
			modes = append(modes, modelTestModeInfo{
				Mode:          modeImageEdit,
				DefaultPrompt: defaultImageEditTestPrompt,
				RequiresImage: true,
			})
		}
		return modes
	default:
		// Vision is offered for every chat model rather than gated on a capability
		// flag: the catalog's modality metadata is missing or wrong for a good part
		// of the library, and a model that cannot see returns a clear upstream
		// error, which is a better answer than hiding the button that would have
		// proved it.
		return []modelTestModeInfo{
			{Mode: modeText, DefaultPrompt: defaultTextTestPrompt},
			{Mode: modeVision, DefaultPrompt: defaultVisionTestPrompt, RequiresImage: true},
		}
	}
}

// buildModelTestOptions assembles the form description for one model.
func buildModelTestOptions(model, tenantID string) modelTestOptions {
	options := modelTestOptions{Model: model, Modes: modelTestModes(model)}
	for _, mode := range options.Modes {
		switch mode.Mode {
		case modeImage, modeImageEdit:
			options.Sizes = imageGenerationSizePresetsForTenant(tenantID)
			options.Qualities = imageTestQualities
			options.MaxImages = imageMaxUploads
		case modeVideo, modeVideoFromImage:
			options.MaxImages = 1
			if _, _, ok := registry.VideoGenerationModelDefaults(model); ok {
				for _, item := range registry.ListVideoGenerationModels() {
					if strings.EqualFold(item.ID, model) {
						options.MaxDurationSeconds = item.MaxDurationSeconds
						break
					}
				}
			}
		}
	}
	return options
}

// resolveModelTestMode picks the probe shape to run. An empty request mode takes
// the model's default; an explicit one is validated against the same list the
// panel was given, so a stale tab cannot ask for an edit on a model that has none.
func resolveModelTestMode(model, requested string) (modelTestMode, error) {
	modes := modelTestModes(model)
	requested = strings.TrimSpace(strings.ToLower(requested))
	if requested == "" {
		return modes[0].Mode, nil
	}
	for _, mode := range modes {
		if string(mode.Mode) == requested {
			return mode.Mode, nil
		}
	}
	names := make([]string, 0, len(modes))
	for _, mode := range modes {
		names = append(names, string(mode.Mode))
	}
	return "", fmt.Errorf("model %q does not support test mode %q (supported: %s)",
		model, requested, strings.Join(names, ", "))
}

// modeRequiresImage reports whether a mode is meaningless without a reference
// image.
func modeRequiresImage(mode modelTestMode) bool {
	return mode == modeImageEdit || mode == modeVision || mode == modeVideoFromImage
}

// upstreamAlt maps a mode to the executor alt that selects the upstream endpoint.
// Text modes have none: they go through the normal chat completion path.
func upstreamAlt(mode modelTestMode) string {
	switch mode {
	case modeImage:
		return imageGenerationAlt
	case modeImageEdit:
		return imageEditsAlt
	case modeVideo, modeVideoFromImage:
		return videoGenerationAlt
	default:
		return ""
	}
}

// buildUpstreamPayload turns a validated probe request into the body its upstream
// endpoint expects. Each branch produces exactly the shape a real client of that
// endpoint would send, which is the point: a probe that tests a bespoke shape
// proves nothing about the path production traffic takes.
func buildUpstreamPayload(req modelTestRequest, mode modelTestMode) ([]byte, error) {
	switch mode {
	case modeImage, modeImageEdit:
		return buildImageTestPayload(req, mode)
	case modeVideo, modeVideoFromImage:
		return buildVideoTestPayload(req, mode)
	case modeVision:
		return buildVisionTestPayload(req)
	default:
		return buildChatTestPayload(req)
	}
}

func buildChatTestPayload(req modelTestRequest) ([]byte, error) {
	payload := []byte(`{"stream":false,"messages":[]}`)
	var err error
	if payload, err = sjson.SetBytes(payload, "model", req.Model); err != nil {
		return nil, fmt.Errorf("build model test payload: %w", err)
	}
	if payload, err = sjson.SetBytes(payload, "messages.0.role", "user"); err != nil {
		return nil, fmt.Errorf("build model test payload: %w", err)
	}
	if payload, err = sjson.SetBytes(payload, "messages.0.content", req.Prompt); err != nil {
		return nil, fmt.Errorf("build model test payload: %w", err)
	}
	return payload, nil
}

// buildVisionTestPayload sends the prompt alongside the reference images using
// the multi-part content form, which is what a vision client sends.
func buildVisionTestPayload(req modelTestRequest) ([]byte, error) {
	payload := []byte(`{"stream":false,"messages":[{"role":"user","content":[]}]}`)
	var err error
	if payload, err = sjson.SetBytes(payload, "model", req.Model); err != nil {
		return nil, fmt.Errorf("build model test payload: %w", err)
	}
	if payload, err = sjson.SetBytes(payload, "messages.0.content.0", map[string]any{
		"type": "text",
		"text": req.Prompt,
	}); err != nil {
		return nil, fmt.Errorf("build model test payload: %w", err)
	}
	for index, image := range req.Images {
		part := map[string]any{
			"type":      "image_url",
			"image_url": map[string]any{"url": image},
		}
		if payload, err = sjson.SetBytes(payload, fmt.Sprintf("messages.0.content.%d", index+1), part); err != nil {
			return nil, fmt.Errorf("build model test payload: %w", err)
		}
	}
	return payload, nil
}

func buildImageTestPayload(req modelTestRequest, mode modelTestMode) ([]byte, error) {
	payload := []byte(`{}`)
	var err error
	if payload, err = sjson.SetBytes(payload, "model", req.Model); err != nil {
		return nil, fmt.Errorf("build image test payload: %w", err)
	}
	if payload, err = sjson.SetBytes(payload, "prompt", req.Prompt); err != nil {
		return nil, fmt.Errorf("build image test payload: %w", err)
	}
	count := req.N
	if count <= 0 {
		count = 1
	}
	if payload, err = sjson.SetBytes(payload, "n", count); err != nil {
		return nil, fmt.Errorf("build image test payload: %w", err)
	}
	for path, value := range map[string]string{"size": req.Size, "quality": req.Quality} {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			if payload, err = sjson.SetBytes(payload, path, trimmed); err != nil {
				return nil, fmt.Errorf("build image test payload: %w", err)
			}
		}
	}
	if mode == modeImageEdit {
		// The edits endpoint reads its reference frames from "image". Data URIs are
		// accepted here because the panel uploads through JSON rather than the
		// multipart form a raw HTTP client would use.
		if payload, err = sjson.SetBytes(payload, "image", req.Images); err != nil {
			return nil, fmt.Errorf("build image test payload: %w", err)
		}
	}
	return payload, nil
}

func buildVideoTestPayload(req modelTestRequest, mode modelTestMode) ([]byte, error) {
	payload := []byte(`{}`)
	var err error
	if payload, err = sjson.SetBytes(payload, "model", req.Model); err != nil {
		return nil, fmt.Errorf("build video test payload: %w", err)
	}
	if payload, err = sjson.SetBytes(payload, "prompt", req.Prompt); err != nil {
		return nil, fmt.Errorf("build video test payload: %w", err)
	}
	if req.Duration > 0 {
		if payload, err = sjson.SetBytes(payload, "duration", req.Duration); err != nil {
			return nil, fmt.Errorf("build video test payload: %w", err)
		}
	}
	for path, value := range map[string]string{
		"aspect_ratio": req.AspectRatio,
		"resolution":   req.Resolution,
	} {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			if payload, err = sjson.SetBytes(payload, path, trimmed); err != nil {
				return nil, fmt.Errorf("build video test payload: %w", err)
			}
		}
	}
	if mode == modeVideoFromImage && len(req.Images) > 0 {
		// xAI's media host animates a single source frame, so only the first upload
		// is sent rather than silently dropping the rest inside the executor.
		if payload, err = sjson.SetBytes(payload, "image", req.Images[0]); err != nil {
			return nil, fmt.Errorf("build video test payload: %w", err)
		}
	}
	return payload, nil
}

// maxModelTestImageBytes bounds one decoded reference image. The limit exists so a
// mistaken upload fails in the panel with a clear message instead of after a
// minute of uploading to the upstream.
const maxModelTestImageBytes = 8 * 1024 * 1024

// validateModelTestImages checks the reference images a mode needs. Both data
// URIs and https URLs are accepted: the panel sends data URIs from a file picker,
// while an operator reproducing a report usually has a URL.
func validateModelTestImages(images []string, mode modelTestMode, maxImages int) ([]string, error) {
	cleaned := make([]string, 0, len(images))
	for _, image := range images {
		image = strings.TrimSpace(image)
		if image == "" {
			continue
		}
		switch {
		case strings.HasPrefix(image, "data:"):
			decoded, err := dataURIBytes(image)
			if err != nil {
				return nil, err
			}
			if len(decoded) > maxModelTestImageBytes {
				return nil, fmt.Errorf("reference image exceeds %d MB", maxModelTestImageBytes/(1024*1024))
			}
		case strings.HasPrefix(image, "https://"):
		default:
			return nil, fmt.Errorf("reference image must be a data URI or an https URL")
		}
		cleaned = append(cleaned, image)
	}
	if modeRequiresImage(mode) && len(cleaned) == 0 {
		return nil, fmt.Errorf("test mode %q requires at least one reference image", mode)
	}
	if maxImages > 0 && len(cleaned) > maxImages {
		return nil, fmt.Errorf("at most %d reference images are accepted", maxImages)
	}
	return cleaned, nil
}

// dataURIBytes decodes the base64 payload of a data URI so its real size can be
// checked before it is forwarded.
func dataURIBytes(uri string) ([]byte, error) {
	comma := strings.IndexByte(uri, ',')
	if comma < 0 {
		return nil, fmt.Errorf("malformed data URI")
	}
	header := uri[:comma]
	if !strings.Contains(header, ";base64") {
		return nil, fmt.Errorf("data URI must be base64 encoded")
	}
	decoded, err := base64.StdEncoding.DecodeString(uri[comma+1:])
	if err != nil {
		return nil, fmt.Errorf("malformed data URI")
	}
	return decoded, nil
}
