package auth

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/util"
)

func TestFileTokenStoreListSkipsClusterBackupDirectories(t *testing.T) {
	dir := t.TempDir()
	writeJSON := func(rel, body string) {
		t.Helper()
		path := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	writeJSON("live.json", `{"type":"claude","email":"live@example.com"}`)
	writeJSON(util.PreClusterBackupDirPrefix+"20260101-000000/old.json", `{"type":"claude","email":"old@example.com"}`)
	writeJSON(util.ClusterStrayDirPrefix+"20260101-000000/stray.json", `{"type":"claude","email":"stray@example.com"}`)

	store := NewFileTokenStore()
	store.SetBaseDir(dir)
	auths, err := store.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(auths) != 1 || auths[0].ID != "live.json" {
		ids := make([]string, 0, len(auths))
		for _, a := range auths {
			ids = append(ids, a.ID)
		}
		t.Fatalf("listed %v, want only live.json", ids)
	}
}

func TestNewFileAuthRecordMatchesDiskLoad(t *testing.T) {
	dir := t.TempDir()
	tenantDir := filepath.Join(dir, "11111111-1111-1111-1111-111111111111")
	if err := os.MkdirAll(tenantDir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(tenantDir, "claude.json")
	body := `{"type":"claude","email":"a@example.com","prefix":"team","proxy_url":"http://proxy.invalid:1","disabled":true,"auth_kind":"oauth","access_token":"at","refresh_token":"rt"}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	store := NewFileTokenStore()
	store.SetBaseDir(dir)
	loaded, err := store.readAuthFile(path, dir)
	if err != nil {
		t.Fatal(err)
	}
	built := NewFileAuthRecord(loaded.ID, path, loaded.Provider, loaded.Metadata, loaded.CreatedAt)
	if !reflect.DeepEqual(built, loaded) {
		t.Fatalf("builder diverged from disk load:\n built=%#v\nloaded=%#v", built, loaded)
	}
	if loaded.EnsureIndex() != built.EnsureIndex() || loaded.FileName != loaded.ID {
		t.Fatal("auth_index must derive from the relative ID for both paths")
	}
}
