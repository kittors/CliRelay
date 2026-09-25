package serviceapp

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

func TestModelListSnapshotPathStaysOutOfTheAuthDirectory(t *testing.T) {
	t.Setenv("WRITABLE_PATH", "")
	t.Setenv("writable_path", "")
	authDir := filepath.Join(t.TempDir(), "auths")

	path := ModelListSnapshotPath(authDir)

	// The watcher reads every .json file under the auth directory as a credential.
	if rel, err := filepath.Rel(authDir, path); err == nil && !strings.HasPrefix(rel, "..") {
		t.Fatalf("snapshot %s is inside the auth directory %s", path, authDir)
	}
	if filepath.Base(path) != modelListSnapshotFile {
		t.Fatalf("snapshot file = %s, want %s", filepath.Base(path), modelListSnapshotFile)
	}
}

func TestOnlySelfListingProvidersListPerCredential(t *testing.T) {
	for _, provider := range []string{"antigravity", "xai", "kimi", " Kimi "} {
		if !IsSelfListingProvider(provider) {
			t.Fatalf("%q should list its models per credential", provider)
		}
	}
	for _, provider := range []string{"codex", "claude", "gemini", ""} {
		if IsSelfListingProvider(provider) {
			t.Fatalf("%q does not list its models per credential", provider)
		}
		if models := CredentialModelFloor(provider); models != nil {
			t.Fatalf("%q has no per-credential floor, got %d models", provider, len(models))
		}
		if _, err := DiscoverCredentialModels(context.Background(), &coreauth.Auth{Provider: provider}, nil, provider); err == nil {
			t.Fatalf("%q listing must be refused", provider)
		}
	}
}
