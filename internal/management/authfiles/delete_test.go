package authfiles

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

func TestDeleteServiceDeleteOneRemovesFileManagerTokenAndChannels(t *testing.T) {
	authDir := t.TempDir()
	path := filepath.Join(authDir, "codex.json")
	if err := os.WriteFile(path, []byte(`{"type":"codex"}`), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	store := &managerStore{}
	manager := coreauth.NewManager(store, nil, nil)
	if _, err := manager.Register(context.Background(), &coreauth.Auth{
		ID:       "codex.json",
		FileName: "codex.json",
		Provider: "codex",
		Label:    "Team Codex",
		Metadata: map[string]any{
			"email": "codex@example.com",
		},
		Attributes: map[string]string{
			"path": path,
		},
	}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	var removedChannels []string

	result, err := DeleteService{
		AuthDir:    authDir,
		Manager:    manager,
		Repository: Repository{Store: store, BaseDir: authDir},
		RemoveChannels: func(channels []string) error {
			removedChannels = append([]string(nil), channels...)
			return nil
		},
	}.DeleteOne(context.Background(), "codex.json")
	if err != nil {
		t.Fatalf("DeleteOne() error = %v", err)
	}
	if result.Deleted != 1 {
		t.Fatalf("Deleted = %d, want 1", result.Deleted)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("deleted file still exists or unexpected stat error: %v", err)
	}
	if _, ok := manager.GetByID("codex.json"); ok {
		t.Fatal("auth still registered")
	}
	if len(removedChannels) != 2 || removedChannels[0] != "Team Codex" || removedChannels[1] != "codex@example.com" {
		t.Fatalf("removed channels = %#v, want label and email", removedChannels)
	}
}

func TestDeleteServiceDeleteOneMissingFile(t *testing.T) {
	_, err := DeleteService{AuthDir: t.TempDir()}.DeleteOne(context.Background(), "missing.json")
	if !errors.Is(err, ErrAuthFileNotFound) {
		t.Fatalf("DeleteOne() error = %v, want ErrAuthFileNotFound", err)
	}
}

func TestDeleteServiceDeleteAllSkipsNonJSONAndCountsDeleted(t *testing.T) {
	authDir := t.TempDir()
	for _, name := range []string{"a.json", "b.json", "notes.txt"} {
		if err := os.WriteFile(filepath.Join(authDir, name), []byte(`{}`), 0o600); err != nil {
			t.Fatalf("WriteFile(%s): %v", name, err)
		}
	}
	if err := os.Mkdir(filepath.Join(authDir, "nested.json"), 0o755); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	store := &managerStore{}
	manager := coreauth.NewManager(store, nil, nil)
	for _, name := range []string{"a.json", "b.json"} {
		if _, err := manager.Register(context.Background(), &coreauth.Auth{
			ID:       name,
			FileName: name,
			Provider: "codex",
			Attributes: map[string]string{
				"path": filepath.Join(authDir, name),
			},
		}); err != nil {
			t.Fatalf("Register(%s): %v", name, err)
		}
	}

	result, err := DeleteService{
		AuthDir:    authDir,
		Manager:    manager,
		Repository: Repository{Store: store, BaseDir: authDir},
	}.DeleteAll(context.Background())
	if err != nil {
		t.Fatalf("DeleteAll() error = %v", err)
	}
	if result.Deleted != 2 {
		t.Fatalf("Deleted = %d, want 2", result.Deleted)
	}
	if _, err := os.Stat(filepath.Join(authDir, "notes.txt")); err != nil {
		t.Fatalf("notes.txt should remain: %v", err)
	}
	if _, ok := manager.GetByID("a.json"); ok {
		t.Fatal("a.json still registered")
	}
}

func TestDeleteServiceDeleteOneAllowsMissingMirrorForRegisteredAuth(t *testing.T) {
	authDir := t.TempDir()
	path := filepath.Join(authDir, "stale.json")
	store := &managerStore{}
	manager := coreauth.NewManager(store, nil, nil)
	if _, err := manager.Register(context.Background(), &coreauth.Auth{
		ID:       "stale.json",
		FileName: "stale.json",
		Provider: "codex",
		Attributes: map[string]string{
			"path": path,
		},
	}); err != nil {
		t.Fatalf("Register: %v", err)
	}

	result, err := (DeleteService{
		AuthDir:    authDir,
		Manager:    manager,
		Repository: Repository{Store: store, BaseDir: authDir},
	}).DeleteOne(context.Background(), "stale.json")
	if err != nil {
		t.Fatalf("DeleteOne() error = %v", err)
	}
	if result.Deleted != 1 {
		t.Fatalf("Deleted = %d, want 1", result.Deleted)
	}
	if _, ok := manager.GetByID("stale.json"); ok {
		t.Fatal("stale auth still registered")
	}
}

func TestDeleteServiceDeleteOnePreservesDeletedResultWhenChannelCleanupFails(t *testing.T) {
	authDir := t.TempDir()
	path := filepath.Join(authDir, "codex.json")
	if err := os.WriteFile(path, []byte(`{"type":"codex"}`), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	store := &managerStore{}
	manager := coreauth.NewManager(store, nil, nil)
	if _, err := manager.Register(context.Background(), &coreauth.Auth{
		ID:       "codex.json",
		FileName: "codex.json",
		Provider: "codex",
		Label:    "Team Codex",
		Metadata: map[string]any{
			"email": "codex@example.com",
		},
		Attributes: map[string]string{
			"path": path,
		},
	}); err != nil {
		t.Fatalf("Register: %v", err)
	}

	result, err := (DeleteService{
		AuthDir:    authDir,
		Manager:    manager,
		Repository: Repository{Store: store, BaseDir: authDir},
		RemoveChannels: func([]string) error {
			return errors.New("cleanup unavailable")
		},
	}).DeleteOne(context.Background(), "codex.json")
	if err == nil || !strings.Contains(err.Error(), "cleanup unavailable") {
		t.Fatalf("DeleteOne() error = %v, want cleanup warning", err)
	}
	if result.Deleted != 1 {
		t.Fatalf("Deleted = %d, want 1", result.Deleted)
	}
	if _, ok := manager.GetByID("codex.json"); ok {
		t.Fatal("auth should stay deleted despite cleanup warning")
	}
}
