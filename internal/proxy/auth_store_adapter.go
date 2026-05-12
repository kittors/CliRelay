package proxy

import (
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	"context"
	"github.com/router-for-me/CLIProxyAPI/v6/sdk/auth"
)

type CoreManagerAuthStore struct {
	manager *coreauth.Manager
	store   *auth.FileTokenStore
}

func NewCoreManagerAuthStore(manager *coreauth.Manager, store *auth.FileTokenStore) *CoreManagerAuthStore {
	return &CoreManagerAuthStore{
		manager: manager,
		store:   store,
	}
}

func (s *CoreManagerAuthStore) ListAuths() []*coreauth.Auth {
	if s.manager == nil {
		return nil
	}
	return s.manager.List()
}

func (s *CoreManagerAuthStore) UpdateAuth(a *coreauth.Auth) error {
	if s.store == nil {
		return nil
	}
	_, err := s.store.Save(context.Background(), a)
	return err
}
