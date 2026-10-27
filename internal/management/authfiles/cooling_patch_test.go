package authfiles

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

func decodeFieldPatch(t *testing.T, body string) FieldPatch {
	t.Helper()
	var patch FieldPatch
	if err := json.Unmarshal([]byte(body), &patch); err != nil {
		t.Fatalf("decode %s: %v", body, err)
	}
	return patch
}

// The account detail sets, changes and clears an auth file's disable_cooling
// through PATCH /auth-files/fields, and reads it back from the list entry.
func TestApplyFieldPatchSetsAndClearsDisableCooling(t *testing.T) {
	auth := &coreauth.Auth{
		ID:         "relay-account",
		Provider:   "codex",
		Attributes: map[string]string{"runtime_only": "true"},
		Metadata:   map[string]any{"disable-cooling": "true"},
	}

	if _, err := ApplyFieldPatch(auth, decodeFieldPatch(t, `{"name": "relay-account", "disable_cooling": false}`), FieldPatchOptions{}); err != nil {
		t.Fatalf("set: %v", err)
	}
	if got, ok := auth.Metadata["disable_cooling"].(bool); !ok || got {
		t.Fatalf("disable_cooling = %#v, want false", auth.Metadata["disable_cooling"])
	}
	if _, ok := auth.Metadata["disable-cooling"]; ok {
		t.Fatal("legacy disable-cooling key survived the patch")
	}
	if got, ok := BuildEntry(auth, EntryOptions{})["disable_cooling"].(bool); !ok || got {
		t.Fatalf("entry disable_cooling = %#v, want false", BuildEntry(auth, EntryOptions{})["disable_cooling"])
	}

	if _, err := ApplyFieldPatch(auth, decodeFieldPatch(t, `{"name": "relay-account", "disable_cooling": null}`), FieldPatchOptions{}); err != nil {
		t.Fatalf("clear: %v", err)
	}
	if _, ok := auth.DisableCoolingOverride(); ok {
		t.Fatalf("override still set after clearing: metadata %#v", auth.Metadata)
	}
	if _, ok := BuildEntry(auth, EntryOptions{})["disable_cooling"]; ok {
		t.Fatal("entry still reports disable_cooling after clearing")
	}

	// A body without the key changes nothing and is refused like any empty patch.
	if _, err := ApplyFieldPatch(auth, decodeFieldPatch(t, `{"name": "relay-account"}`), FieldPatchOptions{}); err == nil {
		t.Fatal("empty patch accepted")
	}
}

// Turning cooling off on an account frees it at once, through the same
// MutateAuth path the panel's save takes.
func TestPatchFieldsDisableCoolingReleasesRunningCooldown(t *testing.T) {
	manager := coreauth.NewManager(&managerStore{}, nil, nil)
	if _, err := manager.Register(context.Background(), &coreauth.Auth{
		ID:       "codex.json",
		FileName: "codex.json",
		Provider: "codex",
		Status:   coreauth.StatusActive,
		Metadata: map[string]any{"email": "relay@example.com"},
	}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	manager.MarkResult(context.Background(), coreauth.Result{
		AuthID: "codex.json", Provider: "codex", Model: "gpt-5",
		Error: &coreauth.Error{HTTPStatus: 429, Message: "Rate limit reached for requests"},
	})
	before, _ := manager.GetByID("codex.json")
	if state := before.ModelStates["gpt-5"]; state == nil || !state.NextRetryAfter.After(time.Now()) {
		t.Fatalf("429 did not start a cooldown: %+v", state)
	}

	if err := (PatchService{Manager: manager}).PatchFields(context.Background(), decodeFieldPatch(t, `{"name": "codex.json", "disable_cooling": true}`)); err != nil {
		t.Fatalf("PatchFields: %v", err)
	}
	after, _ := manager.GetByID("codex.json")
	if got, ok := after.DisableCoolingOverride(); !ok || !got {
		t.Fatalf("override = %t, %t; want true", got, ok)
	}
	if state := after.ModelStates["gpt-5"]; state != nil && (state.Unavailable || !state.NextRetryAfter.IsZero()) {
		t.Fatalf("cooldown survived turning cooling off: %+v", state)
	}
}
