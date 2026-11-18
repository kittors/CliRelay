package cliproxy

import (
	"context"
	"testing"

	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
)

func TestServiceHandleAuthDeleteRemovesAuthFromManager(t *testing.T) {
	authID := "kimi-deleted.json"
	manager := coreauth.NewManager(nil, nil, nil)
	if _, err := manager.Register(coreauth.WithSkipPersist(context.Background()), &coreauth.Auth{
		ID:       authID,
		Provider: "kimi",
		Status:   coreauth.StatusActive,
		Metadata: map[string]any{"type": "kimi"},
	}); err != nil {
		t.Fatalf("Register failed: %v", err)
	}

	service := &Service{
		cfg:         &config.Config{},
		coreManager: manager,
	}

	service.handleAuthUpdate(context.Background(), runtimeAuthUpdate{
		Action: runtimeAuthUpdateActionDelete,
		ID:     authID,
	})

	if _, ok := manager.GetByID(authID); ok {
		t.Fatal("expected deleted auth to be removed from manager")
	}
}
