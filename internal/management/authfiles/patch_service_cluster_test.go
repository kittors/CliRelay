package authfiles

import (
	"context"
	"sync"
	"testing"

	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

// versionedPatchStore is a minimal versioned store: Save is a compare-and-set
// on the metadata version stamp.
type versionedPatchStore struct {
	mu     sync.Mutex
	latest *coreauth.Auth
}

func (s *versionedPatchStore) List(context.Context) ([]*coreauth.Auth, error) { return nil, nil }
func (s *versionedPatchStore) Delete(context.Context, string) error           { return nil }

func (s *versionedPatchStore) Get(context.Context, string) (*coreauth.Auth, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.latest.Clone(), nil
}

func (s *versionedPatchStore) Save(_ context.Context, auth *coreauth.Auth) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if coreauth.CredentialVersion(auth) != coreauth.CredentialVersion(s.latest) {
		return "", coreauth.ErrCredentialConflict
	}
	next := auth.Clone()
	next.Metadata[coreauth.CredentialVersionMetadataKey] = float64(coreauth.CredentialVersion(s.latest) + 1)
	s.latest = next
	return "", nil
}

func TestPatchStatusInClusterModeKeepsAnotherNodesRotation(t *testing.T) {
	stale := &coreauth.Auth{ID: "claude.json", FileName: "claude.json", Provider: "claude", Metadata: map[string]any{
		"type": "claude", "email": "a@example.com", "access_token": "at-old", coreauth.CredentialVersionMetadataKey: float64(1),
	}}
	store := &versionedPatchStore{latest: stale.Clone()}
	store.latest.Metadata["access_token"] = "at-rotated"
	store.latest.Metadata[coreauth.CredentialVersionMetadataKey] = float64(2)
	manager := coreauth.NewManager(store, nil, nil)
	if _, err := manager.Register(coreauth.WithSkipPersist(context.Background()), stale); err != nil {
		t.Fatal(err)
	}
	disabled := true
	if _, err := (PatchService{Manager: manager}).PatchStatus(context.Background(), StatusPatch{Name: "claude.json", Disabled: &disabled}); err != nil {
		t.Fatalf("PatchStatus: %v", err)
	}
	if store.latest.Metadata["access_token"] != "at-rotated" || store.latest.Metadata["disabled"] != true {
		t.Fatalf("persisted = %v, want the rotated token kept and the credential disabled", store.latest.Metadata)
	}
	current, _ := manager.GetByID("claude.json")
	if !current.Disabled || current.Metadata["access_token"] != "at-rotated" {
		t.Fatalf("in-memory copy = disabled:%v token:%v", current.Disabled, current.Metadata["access_token"])
	}
}
