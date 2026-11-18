package authfiles

import (
	"context"
	"testing"

	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

type managerStore struct {
	items map[string]*coreauth.Auth
}

func (s *managerStore) List(context.Context) ([]*coreauth.Auth, error) {
	out := make([]*coreauth.Auth, 0, len(s.items))
	for _, auth := range s.items {
		out = append(out, auth.Clone())
	}
	return out, nil
}

func (s *managerStore) Save(_ context.Context, auth *coreauth.Auth) (string, error) {
	if s.items == nil {
		s.items = make(map[string]*coreauth.Auth)
	}
	s.items[auth.ID] = auth.Clone()
	return auth.ID, nil
}

func (s *managerStore) Delete(_ context.Context, id string) error {
	delete(s.items, id)
	return nil
}

func TestFindByNameOrIDMatchesIDFileNameAndPathBase(t *testing.T) {
	manager := coreauth.NewManager(&managerStore{}, nil, nil)
	auth := &coreauth.Auth{
		ID:       "auth-id",
		FileName: "account.json",
		Provider: "codex",
		Attributes: map[string]string{
			"path": "/tmp/auth/account.json",
		},
	}
	if _, err := manager.Register(context.Background(), auth); err != nil {
		t.Fatalf("Register() error = %v", err)
	}

	for _, name := range []string{"auth-id", "account.json"} {
		if got := FindByNameOrID(manager, name); got == nil || got.ID != "auth-id" {
			t.Fatalf("FindByNameOrID(%q) = %#v, want auth-id", name, got)
		}
	}
}

func TestDeletedChannelIdentifiersRequiresOAuthAccount(t *testing.T) {
	apiKeyAuth := &coreauth.Auth{
		ID:       "api-key",
		Provider: "openai",
		Attributes: map[string]string{
			"api_key": "key",
		},
	}
	if got := DeletedChannelIdentifiers(apiKeyAuth); got != nil {
		t.Fatalf("DeletedChannelIdentifiers(api key) = %#v, want nil", got)
	}

	oauthAuth := &coreauth.Auth{
		ID:       "oauth",
		Provider: "codex",
		Label:    "Team Codex",
		Metadata: map[string]any{
			"email": "codex@example.com",
		},
	}
	got := DeletedChannelIdentifiers(oauthAuth)
	if len(got) != 2 || got[0] != "Team Codex" || got[1] != "codex@example.com" {
		t.Fatalf("DeletedChannelIdentifiers(oauth) = %#v, want label and email", got)
	}
}

func TestRemoveFromManagerDeletesByPathCandidate(t *testing.T) {
	manager := coreauth.NewManager(&managerStore{}, nil, nil)
	auth := &coreauth.Auth{
		ID:       "account.json",
		FileName: "account.json",
		Provider: "codex",
		Attributes: map[string]string{
			"path": "/tmp/auth/account.json",
		},
	}
	if _, err := manager.Register(context.Background(), auth); err != nil {
		t.Fatalf("Register() error = %v", err)
	}

	RemoveFromManager(context.Background(), manager, "/tmp/auth", "/tmp/auth/account.json")

	if _, ok := manager.GetByID("account.json"); ok {
		t.Fatal("expected auth to be removed")
	}
}
