package authfiles

import (
	"context"
	"errors"
	"testing"

	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

type repositoryStore struct {
	baseDir          string
	saved            *coreauth.Auth
	deleted          string
	persistMessage   string
	persistedPaths   []string
	persistShouldErr error
}

func (s *repositoryStore) List(context.Context) ([]*coreauth.Auth, error) {
	return nil, nil
}

func (s *repositoryStore) Save(_ context.Context, auth *coreauth.Auth) (string, error) {
	s.saved = auth
	return auth.ID, nil
}

func (s *repositoryStore) Delete(_ context.Context, id string) error {
	s.deleted = id
	return nil
}

func (s *repositoryStore) SetBaseDir(dir string) {
	s.baseDir = dir
}

func (s *repositoryStore) PersistAuthFiles(_ context.Context, message string, paths ...string) error {
	s.persistMessage = message
	s.persistedPaths = append([]string(nil), paths...)
	return s.persistShouldErr
}

func TestRepositorySaveAppliesBaseDirAndPostAuthHook(t *testing.T) {
	store := &repositoryStore{}
	record := &coreauth.Auth{ID: "auth.json"}
	var hookCalled bool

	path, err := (Repository{
		Store:   store,
		BaseDir: "/auth-dir",
		PostAuthHook: func(ctx context.Context, auth *coreauth.Auth) error {
			_ = ctx
			hookCalled = true
			if auth != record {
				t.Fatalf("hook auth = %#v, want original record", auth)
			}
			return nil
		},
	}).Save(context.Background(), record)
	if err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	if path != "auth.json" {
		t.Fatalf("Save() path = %q, want auth.json", path)
	}
	if store.baseDir != "/auth-dir" {
		t.Fatalf("baseDir = %q, want /auth-dir", store.baseDir)
	}
	if store.saved != record {
		t.Fatalf("saved = %#v, want original record", store.saved)
	}
	if !hookCalled {
		t.Fatal("expected post-auth hook")
	}
}

func TestRepositoryDeleteRejectsEmptyPath(t *testing.T) {
	err := (Repository{Store: &repositoryStore{}}).Delete(context.Background(), " ")
	if err == nil {
		t.Fatal("Delete() error = nil, want error")
	}
}

func TestRepositoryPersistChangeUsesDefaultMessageAndWrapsError(t *testing.T) {
	wantErr := errors.New("boom")
	store := &repositoryStore{persistShouldErr: wantErr}

	err := (Repository{Store: store}).PersistChange(context.Background(), "", "auth.json")
	if !errors.Is(err, wantErr) {
		t.Fatalf("PersistChange() error = %v, want wrapped boom", err)
	}
	if store.persistMessage != "Update auth file" {
		t.Fatalf("persist message = %q, want default message", store.persistMessage)
	}
	if len(store.persistedPaths) != 1 || store.persistedPaths[0] != "auth.json" {
		t.Fatalf("persisted paths = %#v, want [auth.json]", store.persistedPaths)
	}
}
