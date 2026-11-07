package authfiles

import (
	"testing"

	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

func TestApplyFieldPatchUltrafast(t *testing.T) {
	auth := &coreauth.Auth{ID: "codex-ultrafast", Provider: "codex", Metadata: map[string]any{}}
	tier := " UltraFast "
	if _, err := ApplyFieldPatch(auth, FieldPatch{CodexServiceTier: &tier}, FieldPatchOptions{}); err != nil {
		t.Fatal(err)
	}
	if got := auth.Metadata[metadataKeyCodexServiceTier]; got != "ultrafast" {
		t.Fatalf("metadata tier = %v, want ultrafast", got)
	}
	if got := auth.Attributes[metadataKeyCodexServiceTier]; got != "ultrafast" {
		t.Fatalf("runtime tier = %q, want ultrafast", got)
	}
	if got := CodexServiceTierPayload(auth); got != "ultrafast" {
		t.Fatalf("listed tier = %q, want ultrafast", got)
	}

	tier = ""
	if _, err := ApplyFieldPatch(auth, FieldPatch{CodexServiceTier: &tier}, FieldPatchOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, exists := auth.Metadata[metadataKeyCodexServiceTier]; exists {
		t.Fatal("clearing the tier must remove the persisted override")
	}
	if _, exists := auth.Attributes[metadataKeyCodexServiceTier]; exists {
		t.Fatal("clearing the tier must remove the runtime override")
	}
}

func TestApplyFieldPatchUltrafastRejectsNonCodexOAuth(t *testing.T) {
	for _, auth := range []*coreauth.Auth{
		{Provider: "claude"},
		{Provider: "codex", Attributes: map[string]string{"api_key": "test-key"}},
	} {
		tier := "ultrafast"
		if _, err := ApplyFieldPatch(auth, FieldPatch{CodexServiceTier: &tier}, FieldPatchOptions{}); err == nil {
			t.Fatalf("expected rejection for provider=%s attributes=%v", auth.Provider, auth.Attributes)
		}
	}
}
