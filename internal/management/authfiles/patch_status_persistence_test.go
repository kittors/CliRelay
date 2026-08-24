package authfiles

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	codexauth "github.com/router-for-me/CLIProxyAPI/v6/internal/auth/codex"
	sdkauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/auth"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

// The management toggle is only useful if it survives a reload: the auth file on
// disk must carry the new state, and loading that file back must reproduce it.
func TestPatchStatusPersistsDisabledAcrossReload(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "codex-toggle.json")
	metadata := map[string]any{
		"type":          "codex",
		"email":         "toggle@example.com",
		"access_token":  "at",
		"refresh_token": "rt",
		"disabled":      false,
	}
	seed, errMarshal := json.Marshal(metadata)
	if errMarshal != nil {
		t.Fatalf("Marshal: %v", errMarshal)
	}
	if errWrite := os.WriteFile(path, seed, 0o600); errWrite != nil {
		t.Fatalf("WriteFile: %v", errWrite)
	}

	store := sdkauth.NewFileTokenStore()
	store.SetBaseDir(dir)
	manager := coreauth.NewManager(store, nil, nil)
	if _, errRegister := manager.Register(context.Background(), &coreauth.Auth{
		ID:         "codex-toggle.json",
		FileName:   "codex-toggle.json",
		Provider:   "codex",
		Status:     coreauth.StatusActive,
		Attributes: map[string]string{"path": path},
		Metadata:   metadata,
		// OAuth credentials keep a TokenStorage; the disabled flag has to be
		// mirrored through it too.
		Storage: &codexauth.CodexTokenStorage{
			AccessToken:  "at",
			RefreshToken: "rt",
			Email:        "toggle@example.com",
			Type:         "codex",
		},
	}); errRegister != nil {
		t.Fatalf("Register: %v", errRegister)
	}
	service := PatchService{Manager: manager, Repository: Repository{Store: store, BaseDir: dir}}

	for _, step := range []struct {
		name     string
		disabled bool
	}{
		{name: "disable", disabled: true},
		{name: "re-enable", disabled: false},
	} {
		t.Run(step.name, func(t *testing.T) {
			disabled := step.disabled
			result, errPatch := service.PatchStatus(context.Background(), StatusPatch{
				Name:     "codex-toggle.json",
				Disabled: &disabled,
			})
			if errPatch != nil {
				t.Fatalf("PatchStatus: %v", errPatch)
			}
			if result.Disabled != step.disabled {
				t.Fatalf("result disabled = %v, want %v", result.Disabled, step.disabled)
			}

			raw, errRead := os.ReadFile(path)
			if errRead != nil {
				t.Fatalf("ReadFile: %v", errRead)
			}
			onDisk := map[string]any{}
			if errUnmarshal := json.Unmarshal(raw, &onDisk); errUnmarshal != nil {
				t.Fatalf("Unmarshal: %v", errUnmarshal)
			}
			if onDisk["disabled"] != step.disabled {
				t.Fatalf("on-disk disabled = %#v, want %v", onDisk["disabled"], step.disabled)
			}

			reloaded, errList := store.List(context.Background())
			if errList != nil {
				t.Fatalf("List: %v", errList)
			}
			if len(reloaded) != 1 {
				t.Fatalf("reloaded auth count = %d, want 1", len(reloaded))
			}
			if reloaded[0].Disabled != step.disabled {
				t.Fatalf("reloaded Disabled = %v, want %v", reloaded[0].Disabled, step.disabled)
			}
			wantStatus := coreauth.StatusActive
			if step.disabled {
				wantStatus = coreauth.StatusDisabled
			}
			if reloaded[0].Status != wantStatus {
				t.Fatalf("reloaded Status = %q, want %q", reloaded[0].Status, wantStatus)
			}
		})
	}
}

// ApplyStatusPatch owns the in-memory projection; stores mirror it again on save.
func TestApplyStatusPatchSyncsMetadata(t *testing.T) {
	auth := &coreauth.Auth{Status: coreauth.StatusActive, Metadata: map[string]any{"type": "codex"}}
	if err := ApplyStatusPatch(auth, true, time.Time{}); err != nil {
		t.Fatalf("ApplyStatusPatch: %v", err)
	}
	if auth.Metadata[coreauth.DisabledMetadataKey] != true {
		t.Fatalf("metadata disabled = %#v, want true", auth.Metadata[coreauth.DisabledMetadataKey])
	}
	if err := ApplyStatusPatch(auth, false, time.Time{}); err != nil {
		t.Fatalf("ApplyStatusPatch: %v", err)
	}
	if auth.Metadata[coreauth.DisabledMetadataKey] != false {
		t.Fatalf("metadata disabled = %#v, want false", auth.Metadata[coreauth.DisabledMetadataKey])
	}
}

// Config-derived API keys live in config.yaml and own no auth file. Toggling one
// must stay in memory instead of materializing a JSON file in the auth dir.
func TestApplyStatusPatchLeavesConfigDerivedAuthUnpersisted(t *testing.T) {
	auth := &coreauth.Auth{
		Status:     coreauth.StatusActive,
		Attributes: map[string]string{"source": "config:gemini[abc]", "api_key": "key"},
	}
	if err := ApplyStatusPatch(auth, true, time.Time{}); err != nil {
		t.Fatalf("ApplyStatusPatch: %v", err)
	}
	if auth.Metadata != nil {
		t.Fatalf("Metadata = %#v, want nil", auth.Metadata)
	}
	if !auth.Disabled || auth.Status != coreauth.StatusDisabled {
		t.Fatalf("Disabled=%v Status=%q, want disabled", auth.Disabled, auth.Status)
	}
}
