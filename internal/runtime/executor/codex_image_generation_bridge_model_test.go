package executor

import (
	"net/http"
	"testing"

	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	"github.com/tidwall/gjson"
)

func codexBridgeAuth(metadata map[string]any) *cliproxyauth.Auth {
	return &cliproxyauth.Auth{Provider: "codex", Metadata: metadata}
}

// injectedToolModel returns the model of the image_generation tool the bridge
// attached to an outbound /responses body.
func injectedToolModel(t *testing.T, auth *cliproxyauth.Auth) string {
	t.Helper()
	body := []byte(`{"model":"gpt-5.5","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"draw a cat"}]}]}`)
	out := maybeEnsureCodexImageGenerationTool(body, auth, "gpt-5.5", http.Header{})
	for _, tool := range gjson.GetBytes(out, "tools").Array() {
		if tool.Get("type").String() == "image_generation" {
			return tool.Get("model").String()
		}
	}
	t.Fatalf("no image_generation tool was injected: %s", out)
	return ""
}

// Without a per-account setting the bridge keeps using the build default, which
// is what every account did before the field existed.
func TestBridgeInjectsDefaultImageModel(t *testing.T) {
	if got := injectedToolModel(t, codexBridgeAuth(nil)); got != codexImageModel {
		t.Fatalf("model = %q, want the build default %q", got, codexImageModel)
	}
}

func TestBridgeInjectsThePinnedImageModel(t *testing.T) {
	for _, model := range []string{"gpt-image-2.5-flare", "gpt-image-2.5-sunburst"} {
		t.Run(model, func(t *testing.T) {
			auth := codexBridgeAuth(map[string]any{metadataKeyCodexImageGenerationModel: model})
			if got := injectedToolModel(t, auth); got != model {
				t.Fatalf("model = %q, want %q", got, model)
			}
		})
	}
}

// Metadata also arrives from restored credential files and hand edits, so an
// unknown model must fall back rather than turn every image turn into a 400.
func TestBridgeFallsBackForAnUnknownPinnedModel(t *testing.T) {
	auth := codexBridgeAuth(map[string]any{metadataKeyCodexImageGenerationModel: "gpt-image-9-not-real"})
	if got := injectedToolModel(t, auth); got != codexImageModel {
		t.Fatalf("model = %q, want the build default for an unknown pin", got)
	}
	// A model from another provider's pool is equally unusable on a Codex token.
	auth = codexBridgeAuth(map[string]any{metadataKeyCodexImageGenerationModel: "grok-imagine-image"})
	if got := injectedToolModel(t, auth); got != codexImageModel {
		t.Fatalf("model = %q, want the build default for another pool's model", got)
	}
}

// The edit action must carry the pinned model too; picking it up only on the
// generate path would silently downgrade every "change this image" turn.
func TestBridgeEditToolCarriesThePinnedModel(t *testing.T) {
	auth := codexBridgeAuth(map[string]any{metadataKeyCodexImageGenerationModel: "gpt-image-2.5-flare"})
	tool := codexHostedImageTool("edit", auth)
	if got := gjson.GetBytes(tool, "action").String(); got != "edit" {
		t.Fatalf("action = %q, want edit", got)
	}
	if got := gjson.GetBytes(tool, "model").String(); got != "gpt-image-2.5-flare" {
		t.Fatalf("model = %q, want the pinned model", got)
	}
}
