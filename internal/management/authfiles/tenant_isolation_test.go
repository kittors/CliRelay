package authfiles

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

type tenantMemoryStore struct{}

func (tenantMemoryStore) List(context.Context) ([]*coreauth.Auth, error) { return nil, nil }
func (tenantMemoryStore) Save(_ context.Context, auth *coreauth.Auth) (string, error) {
	if auth == nil || auth.Attributes == nil {
		return "", nil
	}
	return auth.Attributes["path"], nil
}
func (tenantMemoryStore) Delete(context.Context, string) error { return nil }

func TestTenantAuthFilesKeepSameNameIsolated(t *testing.T) {
	authDir := t.TempDir()
	manager := coreauth.NewManager(tenantMemoryStore{}, nil, nil)
	const tenantA = "00000000-0000-0000-0000-00000000000a"
	const tenantB = "00000000-0000-0000-0000-00000000000b"
	payload := []byte(`{"type":"codex","email":"same@example.com"}`)

	resultA, err := (UploadService{AuthDir: authDir, TenantID: tenantA, Manager: manager}).UploadRaw(context.Background(), "same.json", payload)
	if err != nil {
		t.Fatalf("upload tenant A: %v", err)
	}
	resultB, err := (UploadService{AuthDir: authDir, TenantID: tenantB, Manager: manager}).UploadRaw(context.Background(), "same.json", payload)
	if err != nil {
		t.Fatalf("upload tenant B: %v", err)
	}
	if resultA.Path == resultB.Path {
		t.Fatalf("tenant paths must differ: %q", resultA.Path)
	}
	if got := manager.ListForTenant(tenantA); len(got) != 1 || got[0].ID != filepath.ToSlash(filepath.Join(tenantA, "same.json")) {
		t.Fatalf("tenant A auths = %#v", got)
	}
	if got := manager.ListForTenant(tenantB); len(got) != 1 || got[0].ID != filepath.ToSlash(filepath.Join(tenantB, "same.json")) {
		t.Fatalf("tenant B auths = %#v", got)
	}

	disabled := true
	if _, err = (PatchService{Manager: manager, TenantID: tenantA}).PatchStatus(context.Background(), StatusPatch{Name: "same.json", Disabled: &disabled}); err != nil {
		t.Fatalf("disable tenant A auth: %v", err)
	}
	if auth := FindByNameOrIDForTenant(manager, tenantB, "same.json"); auth == nil || auth.Disabled {
		t.Fatalf("tenant B auth was modified: %#v", auth)
	}

	if _, err = (DeleteService{AuthDir: authDir, TenantID: tenantA, Manager: manager, Repository: Repository{Store: tenantMemoryStore{}, BaseDir: authDir}}).DeleteOne(context.Background(), "same.json"); err != nil {
		t.Fatalf("delete tenant A auth: %v", err)
	}
	if _, err = os.Stat(resultB.Path); err != nil {
		t.Fatalf("tenant B auth file missing after tenant A delete: %v", err)
	}
	if got := manager.ListForTenant(tenantB); len(got) != 1 {
		t.Fatalf("tenant B manager entries = %d, want 1", len(got))
	}
}

func TestUploadReplacesSymlinkWithoutFollowingIt(t *testing.T) {
	authDir := t.TempDir()
	tenantDir := TenantAuthDir(authDir, "tenant-a")
	if err := os.MkdirAll(tenantDir, 0o700); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "outside.json")
	if err := os.WriteFile(outside, []byte("outside"), 0o600); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(tenantDir, "same.json")
	if err := os.Symlink(outside, dst); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	manager := coreauth.NewManager(tenantMemoryStore{}, nil, nil)
	if _, err := (UploadService{AuthDir: authDir, TenantID: "tenant-a", Manager: manager}).UploadRaw(context.Background(), "same.json", []byte(`{"type":"codex"}`)); err != nil {
		t.Fatalf("upload: %v", err)
	}
	outsideData, err := os.ReadFile(outside)
	if err != nil || string(outsideData) != "outside" {
		t.Fatalf("outside file changed: %q, %v", outsideData, err)
	}
	info, err := os.Lstat(dst)
	if err != nil || !info.Mode().IsRegular() {
		t.Fatalf("destination is not a regular file: %v, %v", info, err)
	}
}
