package authfiles

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func TestUploadServiceUploadRawWritesRegistersAndPersists(t *testing.T) {
	authDir := t.TempDir()
	store := &repositoryStore{}
	manager := coreauth.NewManager(store, nil, nil)

	result, err := (UploadService{
		AuthDir: authDir,
		Manager: manager,
		Repository: Repository{
			Store:   store,
			BaseDir: authDir,
		},
	}).UploadRaw(context.Background(), "codex.json", []byte(`{"type":"codex","email":"codex@example.com"}`))
	if err != nil {
		t.Fatalf("UploadRaw() error = %v", err)
	}

	wantPath := filepath.Join(authDir, "codex.json")
	if result.Name != "codex.json" || result.Path != wantPath {
		t.Fatalf("result = %#v, want codex.json at %q", result, wantPath)
	}
	if _, err := os.Stat(wantPath); err != nil {
		t.Fatalf("uploaded file missing: %v", err)
	}
	auth, ok := manager.GetByID("codex.json")
	if !ok || auth == nil {
		t.Fatal("registered auth not found")
	}
	if auth.Provider != "codex" || auth.Label != "codex@example.com" {
		t.Fatalf("registered provider/label = %q/%q", auth.Provider, auth.Label)
	}
	if store.persistMessage != "Update auth codex.json" {
		t.Fatalf("persist message = %q, want upload message", store.persistMessage)
	}
	if len(store.persistedPaths) != 1 || store.persistedPaths[0] != wantPath {
		t.Fatalf("persisted paths = %#v, want [%q]", store.persistedPaths, wantPath)
	}
}

func TestUploadServiceUploadMultipartUsesBaseFileName(t *testing.T) {
	authDir := t.TempDir()
	store := &repositoryStore{}
	manager := coreauth.NewManager(store, nil, nil)

	result, err := (UploadService{
		AuthDir:    authDir,
		Manager:    manager,
		Repository: Repository{Store: store, BaseDir: authDir},
	}).UploadMultipart(context.Background(), "nested/claude.json", []byte(`{"type":"claude","email":"claude@example.com"}`))
	if err != nil {
		t.Fatalf("UploadMultipart() error = %v", err)
	}
	if result.Name != "claude.json" {
		t.Fatalf("result name = %q, want base file name", result.Name)
	}
	if _, ok := manager.GetByID("claude.json"); !ok {
		t.Fatal("registered auth not found")
	}
}

func TestUploadServiceRejectsUnavailableManager(t *testing.T) {
	_, err := (UploadService{AuthDir: t.TempDir()}).UploadRaw(context.Background(), "codex.json", []byte(`{}`))
	if !errors.Is(err, ErrAuthManagerUnavailable) {
		t.Fatalf("UploadRaw() error = %v, want manager unavailable", err)
	}
}

func TestUploadServiceRejectsInvalidRawName(t *testing.T) {
	manager := coreauth.NewManager(nil, nil, nil)
	_, err := (UploadService{AuthDir: t.TempDir(), Manager: manager}).UploadRaw(context.Background(), "nested/codex.json", []byte(`{}`))
	if err == nil {
		t.Fatal("UploadRaw() error = nil, want invalid name")
	}
}
