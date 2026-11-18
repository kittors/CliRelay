package authfiles

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func TestPatchServicePatchStatusUpdatesManager(t *testing.T) {
	store := &managerStore{}
	manager := coreauth.NewManager(store, nil, nil)
	if _, err := manager.Register(context.Background(), &coreauth.Auth{
		ID:       "codex.json",
		FileName: "codex.json",
		Provider: "codex",
		Status:   coreauth.StatusActive,
	}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	disabled := true

	result, err := (PatchService{Manager: manager}).PatchStatus(context.Background(), StatusPatch{
		Name:     "codex.json",
		Disabled: &disabled,
	})
	if err != nil {
		t.Fatalf("PatchStatus() error = %v", err)
	}
	if !result.Disabled {
		t.Fatalf("result disabled = false, want true")
	}
	auth, ok := manager.GetByID("codex.json")
	if !ok || auth == nil {
		t.Fatal("patched auth not found")
	}
	if !auth.Disabled || auth.Status != coreauth.StatusDisabled {
		t.Fatalf("auth disabled/status = %v/%s, want disabled", auth.Disabled, auth.Status)
	}
}

func TestPatchServicePatchFieldsPersistsAndRenamesChannels(t *testing.T) {
	authDir := t.TempDir()
	authPath := filepath.Join(authDir, "codex.json")
	store := &repositoryStore{}
	manager := coreauth.NewManager(store, nil, nil)
	if _, err := manager.Register(context.Background(), &coreauth.Auth{
		ID:       "codex.json",
		FileName: "codex.json",
		Provider: "codex",
		Label:    "Old Codex",
		Attributes: map[string]string{
			"path": authPath,
		},
		Metadata: map[string]any{
			"email": "codex@example.com",
		},
	}); err != nil {
		t.Fatalf("Register: %v", err)
	}

	label := " Team Codex "
	var renamedOld []string
	var renamedNew string
	err := (PatchService{
		Manager: manager,
		Repository: Repository{
			Store:   store,
			BaseDir: authDir,
		},
		ValidateLabel: func(label, excludeAuthID string) (string, error) {
			if excludeAuthID != "codex.json" {
				t.Fatalf("excludeAuthID = %q, want codex.json", excludeAuthID)
			}
			return strings.TrimSpace(label), nil
		},
		RenameChannels: func(oldNames []string, newName string) error {
			renamedOld = append([]string(nil), oldNames...)
			renamedNew = newName
			return nil
		},
	}).PatchFields(context.Background(), FieldPatch{
		Name:  "codex.json",
		Label: &label,
	})
	if err != nil {
		t.Fatalf("PatchFields() error = %v", err)
	}
	if store.persistMessage != "Update auth codex.json" {
		t.Fatalf("persist message = %q, want update message", store.persistMessage)
	}
	if len(store.persistedPaths) != 1 || store.persistedPaths[0] != authPath {
		t.Fatalf("persisted paths = %#v, want [%q]", store.persistedPaths, authPath)
	}
	if len(renamedOld) == 0 || renamedNew != "Team Codex" {
		t.Fatalf("rename = %#v -> %q, want old identifiers -> Team Codex", renamedOld, renamedNew)
	}
}

func TestPatchServiceValidationErrors(t *testing.T) {
	manager := coreauth.NewManager(nil, nil, nil)
	disabled := true

	_, err := (PatchService{}).PatchStatus(context.Background(), StatusPatch{Name: "codex.json", Disabled: &disabled})
	if !errors.Is(err, ErrAuthManagerUnavailable) {
		t.Fatalf("PatchStatus() error = %v, want manager unavailable", err)
	}
	_, err = (PatchService{Manager: manager}).PatchStatus(context.Background(), StatusPatch{Disabled: &disabled})
	if !errors.Is(err, ErrNameRequired) {
		t.Fatalf("PatchStatus() error = %v, want name required", err)
	}
	_, err = (PatchService{Manager: manager}).PatchStatus(context.Background(), StatusPatch{Name: "codex.json"})
	if !errors.Is(err, ErrDisabledRequired) {
		t.Fatalf("PatchStatus() error = %v, want disabled required", err)
	}
	err = (PatchService{Manager: manager}).PatchFields(context.Background(), FieldPatch{Name: "missing.json"})
	if !errors.Is(err, ErrAuthFileNotFound) {
		t.Fatalf("PatchFields() error = %v, want not found", err)
	}
}
