package executor

import (
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

// TestParseCodexImageRequestAcceptsImageFamily is the gate that used to reject
// anything but one model id. A new gpt-image release was turned away here, before
// upstream ever saw the request, which is not this layer's call to make.
func TestParseCodexImageRequestAcceptsImageFamily(t *testing.T) {
	accepted := []string{
		"gpt-image-2",
		"gpt-image-2.5-flare",
		"gpt-image-2.5-sunburst",
		// Not in the catalog: the prefix classifier still routes it, so a future
		// release is servable without a code change.
		"gpt-image-3",
		"GPT-Image-2.5-Flare",
	}
	for _, model := range accepted {
		parsed, err := parseCodexImageRequest([]byte(`{"model":"` + model + `","prompt":"draw a fox"}`))
		if err != nil {
			t.Fatalf("parseCodexImageRequest(%q) error = %v, want it accepted", model, err)
		}
		if parsed.Model != model {
			t.Fatalf("parsed model = %q, want %q preserved verbatim", parsed.Model, model)
		}
	}

	rejected := []string{
		"gpt-5.5",                // chat model, wrong endpoint
		"grok-imagine-image",     // image model, but served by the xAI pool
		"definitely-not-a-model", // nonsense
		"gpt-image",              // family root, not a model id upstream serves
	}
	for _, model := range rejected {
		if _, err := parseCodexImageRequest([]byte(`{"model":"` + model + `","prompt":"x"}`)); err == nil {
			t.Fatalf("parseCodexImageRequest(%q) = nil error, want rejection", model)
		}
	}

	// An omitted model still defaults, so existing clients are unaffected.
	parsed, err := parseCodexImageRequest([]byte(`{"prompt":"draw a fox"}`))
	if err != nil {
		t.Fatalf("parseCodexImageRequest() error = %v", err)
	}
	if parsed.Model != codexImageModel {
		t.Fatalf("default model = %q, want %q", parsed.Model, codexImageModel)
	}
}

// TestCodexImageRequestAcceptsExtendedQuality covers the levels 2.5 added. Refusing
// them locally would fail a request the proxy has no basis to refuse.
func TestCodexImageRequestAcceptsExtendedQuality(t *testing.T) {
	for _, quality := range []string{"low", "medium", "high", "xhigh", "max", "auto", "AUTO"} {
		parsed, err := parseCodexImageRequest([]byte(`{"model":"gpt-image-2.5-flare","prompt":"x","quality":"` + quality + `"}`))
		if err != nil {
			t.Fatalf("quality %q rejected: %v", quality, err)
		}
		if parsed.Quality != strings.ToLower(quality) {
			t.Fatalf("parsed quality = %q, want %q", parsed.Quality, strings.ToLower(quality))
		}
	}
	if _, err := parseCodexImageRequest([]byte(`{"model":"gpt-image-2","prompt":"x","quality":"ultra"}`)); err == nil {
		t.Fatal("quality \"ultra\" accepted, want rejection")
	}
}

// TestBuildCodexImageResponsesRequestForwardsSelectedModel proves the caller's
// choice reaches the tool payload instead of being overwritten by the default.
// Without this the 2.5 ids would be selectable but never actually sent.
func TestBuildCodexImageResponsesRequestForwardsSelectedModel(t *testing.T) {
	for _, model := range []string{"gpt-image-2", "gpt-image-2.5-flare", "gpt-image-2.5-sunburst"} {
		parsed := &codexImageRequest{Model: model, Prompt: "draw a fox", N: 1}
		body, err := buildCodexImageResponsesRequest(parsed, codexImageToolModel(parsed), "gpt-5.5")
		if err != nil {
			t.Fatalf("buildCodexImageResponsesRequest(%q) error = %v", model, err)
		}
		if got := gjson.GetBytes(body, "tools.0.model").String(); got != model {
			t.Fatalf("tools.0.model = %q, want the selected model %q", got, model)
		}
		if got := gjson.GetBytes(body, "model").String(); got != "gpt-5.5" {
			t.Fatalf("carrier model = %q, want it untouched by the image model", got)
		}
	}

	// A request with no model still sends the default rather than an empty string,
	// which upstream rejects as an invalid model.
	parsed := &codexImageRequest{Prompt: "draw a fox", N: 1}
	body, err := buildCodexImageResponsesRequest(parsed, codexImageToolModel(parsed), "gpt-5.5")
	if err != nil {
		t.Fatalf("buildCodexImageResponsesRequest() error = %v", err)
	}
	if got := gjson.GetBytes(body, "tools.0.model").String(); got != codexImageModel {
		t.Fatalf("tools.0.model = %q, want the default %q", got, codexImageModel)
	}
}
