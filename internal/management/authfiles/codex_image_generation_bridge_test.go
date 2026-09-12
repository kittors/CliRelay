package authfiles

import (
	"strings"
	"testing"

	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

func codexAuth(metadata map[string]any) *coreauth.Auth {
	if metadata == nil {
		metadata = map[string]any{}
	}
	return &coreauth.Auth{Provider: "codex", Metadata: metadata}
}

// The panel must not hold its own list of Codex image models: they are added to
// the catalog over time, and a browser-side list goes stale the first time one
// ships.
func TestBridgePayloadReportsSelectableModels(t *testing.T) {
	payload := CodexImageGenerationBridgePayload(codexAuth(nil))
	if payload == nil {
		t.Fatal("a codex OAuth auth must expose the bridge payload")
	}
	models, _ := payload["available_models"].([]map[string]any)
	if len(models) < 2 {
		t.Fatalf("available_models = %v, want the Codex image catalog", models)
	}
	ids := make([]string, 0, len(models))
	for _, model := range models {
		id, _ := model["id"].(string)
		ids = append(ids, id)
	}
	for _, want := range []string{"gpt-image-2", "gpt-image-2.5-flare"} {
		found := false
		for _, id := range ids {
			if id == want {
				found = true
			}
		}
		if !found {
			t.Fatalf("available_models %v is missing %q", ids, want)
		}
	}
	// No pin means "follow the build default", which the panel renders as the
	// default option rather than pre-selecting a release.
	if got := payload["model"]; got != "" {
		t.Fatalf("model = %v, want empty for an account with no pin", got)
	}
}

func TestBridgePayloadReportsThePin(t *testing.T) {
	payload := CodexImageGenerationBridgePayload(codexAuth(map[string]any{
		metadataKeyCodexImageGenerationModel: "gpt-image-2.5-flare",
	}))
	if got := payload["model"]; got != "gpt-image-2.5-flare" {
		t.Fatalf("model = %v, want the pinned model", got)
	}
}

// A typo must fail on save rather than surfacing as an upstream 400 on the
// account's next image turn, long after the operator left the form.
func TestNormalizeCodexImageGenerationModel(t *testing.T) {
	got, err := normalizeCodexImageGenerationModel("  GPT-Image-2.5-Flare  ")
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if got != "gpt-image-2.5-flare" {
		t.Fatalf("normalized = %q, want the catalog casing", got)
	}
	if got, err = normalizeCodexImageGenerationModel(""); err != nil || got != "" {
		t.Fatalf("clearing the pin must be allowed, got %q %v", got, err)
	}
	_, err = normalizeCodexImageGenerationModel("gpt-image-9-not-real")
	if err == nil {
		t.Fatal("an unknown model must be rejected")
	}
	if !strings.Contains(err.Error(), "gpt-image-2") {
		t.Fatalf("error %q should name the supported models", err)
	}
	// Another pool's image model cannot be served by a Codex token.
	if _, err = normalizeCodexImageGenerationModel("grok-imagine-image"); err == nil {
		t.Fatal("a non-Codex image model must be rejected")
	}
}

func TestBridgePayloadIsCodexOnly(t *testing.T) {
	if payload := CodexImageGenerationBridgePayload(&coreauth.Auth{Provider: "gemini"}); payload != nil {
		t.Fatal("the bridge payload must not be reported for other providers")
	}
}
