package authfiles

import (
	"context"
	"fmt"
	"strings"

	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

type Repository struct {
	Store        coreauth.Store
	BaseDir      string
	PostAuthHook coreauth.PostAuthHook
}

func (r Repository) storeWithBaseDir() coreauth.Store {
	store := r.Store
	if store == nil {
		return nil
	}
	if dirSetter, ok := store.(interface{ SetBaseDir(string) }); ok {
		dirSetter.SetBaseDir(r.BaseDir)
	}
	return store
}

func (r Repository) Delete(ctx context.Context, path string) error {
	if strings.TrimSpace(path) == "" {
		return fmt.Errorf("auth path is empty")
	}
	store := r.storeWithBaseDir()
	if store == nil {
		return fmt.Errorf("token store unavailable")
	}
	return store.Delete(ctx, path)
}

func (r Repository) PersistChange(ctx context.Context, message string, paths ...string) error {
	store := r.storeWithBaseDir()
	if store == nil {
		return nil
	}
	persister, ok := store.(interface {
		PersistAuthFiles(context.Context, string, ...string) error
	})
	if !ok {
		return nil
	}
	if strings.TrimSpace(message) == "" {
		message = "Update auth file"
	}
	if err := persister.PersistAuthFiles(ctx, message, paths...); err != nil {
		return fmt.Errorf("failed to persist auth file: %w", err)
	}
	return nil
}

func (r Repository) Save(ctx context.Context, record *coreauth.Auth) (string, error) {
	if record == nil {
		return "", fmt.Errorf("token record is nil")
	}
	store := r.storeWithBaseDir()
	if store == nil {
		return "", fmt.Errorf("token store unavailable")
	}
	if r.PostAuthHook != nil {
		if err := r.PostAuthHook(ctx, record); err != nil {
			return "", fmt.Errorf("post-auth hook failed: %w", err)
		}
	}
	return store.Save(ctx, record)
}
