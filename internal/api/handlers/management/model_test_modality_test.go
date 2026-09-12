package management

import (
	"net/http"
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

// The bug this whole file exists for: the catalog probe sent a chat completion
// for every model, so testing an image model asked the upstream to have a
// conversation with it and reported whatever prose came back as success.
func TestImageModelDoesNotProbeAsChat(t *testing.T) {
	modes := modelTestModes("gpt-image-2.5-flare")
	if len(modes) == 0 {
		t.Fatal("an image model must offer at least one probe mode")
	}
	if modes[0].Mode != modeImage {
		t.Fatalf("default mode = %q, want %q so the panel opens on image generation",
			modes[0].Mode, modeImage)
	}
	for _, mode := range modes {
		if mode.Mode == modeText {
			t.Fatal("an image model must not offer a text-completion probe")
		}
		if strings.TrimSpace(mode.DefaultPrompt) == "" {
			t.Fatalf("mode %q has no default prompt", mode.Mode)
		}
	}
	if got := modes[0].DefaultPrompt; got == defaultTextTestPrompt {
		t.Fatal("image generation must not default to the chat weather prompt")
	}
}

func TestVideoModelOffersBothDirections(t *testing.T) {
	modes := modelTestModes("grok-imagine-video-1.5")
	if modes[0].Mode != modeVideo {
		t.Fatalf("default mode = %q, want %q", modes[0].Mode, modeVideo)
	}
	found := false
	for _, mode := range modes {
		if mode.Mode == modeVideoFromImage {
			found = true
			if !mode.RequiresImage {
				t.Fatal("image-to-video must be marked as needing a reference frame")
			}
		}
	}
	if !found {
		t.Fatal("a model that animates a source image must offer image-to-video")
	}
}

func TestChatModelOffersTextAndVision(t *testing.T) {
	modes := modelTestModes("gpt-5.5")
	if modes[0].Mode != modeText {
		t.Fatalf("default mode = %q, want %q", modes[0].Mode, modeText)
	}
	if modes[0].DefaultPrompt != defaultTextTestPrompt {
		t.Fatalf("text default prompt = %q, want the existing probe prompt", modes[0].DefaultPrompt)
	}
}

// A stale browser tab must not be able to run an edit against a model that has
// none; the error names what the model does support so the operator can act.
func TestResolveModelTestModeRejectsUnsupportedMode(t *testing.T) {
	if _, err := resolveModelTestMode("gpt-image-2.5-flare", "video"); err == nil {
		t.Fatal("an image model must reject a video probe")
	}
	mode, err := resolveModelTestMode("gpt-image-2.5-flare", "")
	if err != nil {
		t.Fatalf("default mode: %v", err)
	}
	if mode != modeImage {
		t.Fatalf("mode = %q, want %q", mode, modeImage)
	}
}

func TestBuildImagePayloadTargetsTheImagesEndpoint(t *testing.T) {
	payload, err := buildUpstreamPayload(modelTestRequest{
		Model:   "gpt-image-2.5-flare",
		Prompt:  "a red teapot",
		Size:    "1024x1024",
		Quality: "high",
	}, modeImage)
	if err != nil {
		t.Fatalf("buildUpstreamPayload: %v", err)
	}
	if gjson.GetBytes(payload, "messages").Exists() {
		t.Fatal("an image request must not carry chat messages")
	}
	for path, want := range map[string]string{
		"model":   "gpt-image-2.5-flare",
		"prompt":  "a red teapot",
		"size":    "1024x1024",
		"quality": "high",
	} {
		if got := gjson.GetBytes(payload, path).String(); got != want {
			t.Fatalf("%s = %q, want %q", path, got, want)
		}
	}
	if got := gjson.GetBytes(payload, "n").Int(); got != 1 {
		t.Fatalf("n = %d, want 1 by default", got)
	}
	if got := upstreamAlt(modeImage); got != imageGenerationAlt {
		t.Fatalf("alt = %q, want %q", got, imageGenerationAlt)
	}
	if got := upstreamAlt(modeImageEdit); got != imageEditsAlt {
		t.Fatalf("edit alt = %q, want %q", got, imageEditsAlt)
	}
}

func TestBuildVisionPayloadAttachesTheImage(t *testing.T) {
	payload, err := buildUpstreamPayload(modelTestRequest{
		Model:  "gpt-5.5",
		Prompt: "what is this",
		Images: []string{"https://example.com/a.png"},
	}, modeVision)
	if err != nil {
		t.Fatalf("buildUpstreamPayload: %v", err)
	}
	if got := gjson.GetBytes(payload, "messages.0.content.0.text").String(); got != "what is this" {
		t.Fatalf("prompt part = %q", got)
	}
	if got := gjson.GetBytes(payload, "messages.0.content.1.image_url.url").String(); got != "https://example.com/a.png" {
		t.Fatalf("image part = %q", got)
	}
}

func TestBuildVideoFromImagePayloadCarriesOneFrame(t *testing.T) {
	payload, err := buildUpstreamPayload(modelTestRequest{
		Model:    "grok-imagine-video-1.5",
		Prompt:   "pan across",
		Duration: 6,
		Images:   []string{"https://example.com/a.png", "https://example.com/b.png"},
	}, modeVideoFromImage)
	if err != nil {
		t.Fatalf("buildUpstreamPayload: %v", err)
	}
	if got := gjson.GetBytes(payload, "image").String(); got != "https://example.com/a.png" {
		t.Fatalf("image = %q, want the first frame only", got)
	}
	if got := gjson.GetBytes(payload, "duration").Int(); got != 6 {
		t.Fatalf("duration = %d, want 6", got)
	}
}

func TestValidateModelTestImagesEnforcesTheModeContract(t *testing.T) {
	if _, err := validateModelTestImages(nil, modeImageEdit, 5); err == nil {
		t.Fatal("an edit without a reference image must be rejected")
	}
	if _, err := validateModelTestImages([]string{"http://example.com/a.png"}, modeImageEdit, 5); err == nil {
		t.Fatal("plaintext http references must be rejected")
	}
	cleaned, err := validateModelTestImages([]string{" ", "https://example.com/a.png"}, modeImage, 5)
	if err != nil {
		t.Fatalf("validateModelTestImages: %v", err)
	}
	if len(cleaned) != 1 {
		t.Fatalf("cleaned = %v, want blanks dropped", cleaned)
	}
	if _, err = validateModelTestImages([]string{
		"https://example.com/a.png", "https://example.com/b.png",
	}, modeImage, 1); err == nil {
		t.Fatal("more references than the model accepts must be rejected")
	}
}

// The panel must not have to guess a model's form from its id.
func TestModelTestOptionsDescribeTheForm(t *testing.T) {
	options := buildModelTestOptions("gpt-image-2.5-flare", "")
	if len(options.Sizes) == 0 {
		t.Fatal("an image model must report selectable sizes")
	}
	if len(options.Qualities) == 0 {
		t.Fatal("an image model must report quality levels")
	}
	video := buildModelTestOptions("grok-imagine-video-1.5", "")
	if video.MaxDurationSeconds <= 0 {
		t.Fatal("a video model must report its maximum clip length")
	}
}

// An image model reaching the probe must be routed to the image pool, not
// rejected by the chat catalog lookup that used to guard every request.
func TestPostModelTestAcceptsMediaModels(t *testing.T) {
	h := &Handler{}
	rec := postModelTest(t, h, map[string]any{
		"model":  "gpt-image-2.5-flare",
		"prompt": "a red teapot",
	})
	if rec.Code == http.StatusNotFound {
		t.Fatalf("status = 404: an image model must not be rejected as unserved (%s)", rec.Body.String())
	}
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 for a handler with no auth manager", rec.Code)
	}
}

// Provenance comparison is the field that catches an upstream serving a
// different release than the one requested, so its edge cases matter more than
// most: a rule that flags everything, or nothing, is equally useless.
func TestProvenanceMatchesRequest(t *testing.T) {
	cases := []struct {
		requested string
		generator string
		version   string
		want      bool
		why       string
	}{
		{"gpt-image-2.5-flare", "gpt-image", "2.0", false,
			"the reported bug: a 2.5 request answered by a 2.0 manifest"},
		{"gpt-image-2.5-flare", "gpt-image", "2.5", true, "2.5 served by 2.5"},
		{"gpt-image-2.5-sunburst", "gpt-image", "2.5", true, "the other 2.5 release"},
		{"gpt-image-2", "gpt-image", "2.0", true, "a model id writes 2 where a manifest writes 2.0"},
		{"gpt-image-2", "gpt-image", "3.0", false, "a major version jump is a real mismatch"},
		{"grok-imagine-image", "gpt-image", "2.0", true,
			"a different generator family cannot be compared, so nothing is asserted"},
		{"gpt-image-flare", "gpt-image", "2.0", true, "an id with no version segment asserts nothing"},
		{"gpt-image-2.5-flare", "gpt-image", "", true, "an unsigned image asserts nothing"},
	}
	for _, testCase := range cases {
		got := provenanceMatchesRequest(testCase.requested, testCase.generator, testCase.version)
		if got != testCase.want {
			t.Errorf("provenanceMatchesRequest(%q, %q, %q) = %v, want %v — %s",
				testCase.requested, testCase.generator, testCase.version, got, testCase.want, testCase.why)
		}
	}
}
