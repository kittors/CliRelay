package cliproxy

import (
	"context"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/watcher"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v6/sdk/config"
)

type startupStoreStub struct {
	auths []*coreauth.Auth
}

func (s *startupStoreStub) Save(context.Context, *coreauth.Auth) (string, error) { return "", nil }
func (s *startupStoreStub) Delete(context.Context, string) error                  { return nil }
func (s *startupStoreStub) List(context.Context) ([]*coreauth.Auth, error) {
	return s.auths, nil
}

type startupTokenProviderStub struct{}

func (startupTokenProviderStub) Load(context.Context, *config.Config) (*TokenClientResult, error) {
	return &TokenClientResult{}, nil
}

type startupAPIKeyProviderStub struct{}

func (startupAPIKeyProviderStub) Load(context.Context, *config.Config) (*APIKeyClientResult, error) {
	return &APIKeyClientResult{}, nil
}

func TestServiceRun_RegistersModelsForLoadedAuths(t *testing.T) {
	reg := GlobalModelRegistry()
	authID := "codex-free"
	reg.UnregisterClient(authID)
	t.Cleanup(func() { reg.UnregisterClient(authID) })

	store := &startupStoreStub{auths: []*coreauth.Auth{{
		ID:       authID,
		Provider: "codex",
		Status:   coreauth.StatusActive,
		Metadata: map[string]any{"plan_type": "free", "account_id": "acct_123"},
	}}}
	manager := coreauth.NewManager(store, &coreauth.RoundRobinSelector{}, nil)

	service := &Service{
		cfg: &config.Config{AuthDir: t.TempDir(), Port: 0},
		configPath:     "/tmp/config.yaml",
		tokenProvider:  startupTokenProviderStub{},
		apiKeyProvider: startupAPIKeyProviderStub{},
		watcherFactory: func(string, string, func(*config.Config)) (*WatcherWrapper, error) {
			return &WatcherWrapper{
				start: func(context.Context) error { return nil },
				stop:  func() error { return nil },
				setConfig: func(*config.Config) {},
				setUpdateQueue: func(chan<- watcher.AuthUpdate) {},
				dispatchRuntimeUpdate: func(watcher.AuthUpdate) bool { return false },
			}, nil
		},
		coreManager:    manager,
		accessManager:  nil,
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_ = service.Run(ctx)

	models := reg.GetAvailableModelsByProvider("codex")
	if len(models) == 0 {
		t.Fatal("expected codex models to be registered from loaded auths")
	}
}
