package cliproxy

import (
	"context"
	"testing"

	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v6/sdk/config"
)

type startupStoreStub struct {
	auths []*coreauth.Auth
}

func (s *startupStoreStub) Save(context.Context, *coreauth.Auth) (string, error) { return "", nil }
func (s *startupStoreStub) Delete(context.Context, string) error                 { return nil }
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
		cfg:            &config.Config{AuthDir: t.TempDir(), Port: 0},
		configPath:     "/tmp/config.yaml",
		tokenProvider:  startupTokenProviderStub{},
		apiKeyProvider: startupAPIKeyProviderStub{},
		watcherFactory: func(string, string, func(*config.Config)) (*WatcherWrapper, error) {
			return &WatcherWrapper{
				start:                 func(context.Context) error { return nil },
				stop:                  func() error { return nil },
				setConfig:             func(*config.Config) {},
				setUpdateQueue:        func(chan<- runtimeAuthUpdate) {},
				dispatchRuntimeUpdate: func(runtimeAuthUpdate) bool { return false },
			}, nil
		},
		coreManager:   manager,
		accessManager: nil,
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_ = service.Run(ctx)

	models := reg.GetAvailableModelsByProvider("codex")
	if len(models) == 0 {
		t.Fatal("expected codex models to be registered from loaded auths")
	}
}

func TestLoadInitialState_RegistersConfigDerivedClineModels(t *testing.T) {
	reg := GlobalModelRegistry()

	cfg := &config.Config{
		AuthDir: t.TempDir(),
		Port:    0,
		ClineKey: []config.ClineKey{{
			APIKey: "cline-key",
			Name:   "ClinePass",
			Models: []config.ClineModel{{Name: "cline-pass/mimo-v2.5-pro", Alias: "mimo-v2.5-pro"}},
		}},
	}
	manager := coreauth.NewManager(&startupStoreStub{}, &coreauth.RoundRobinSelector{}, nil)
	service := &Service{
		cfg:            cfg,
		configPath:     "/tmp/config.yaml",
		tokenProvider:  startupTokenProviderStub{},
		apiKeyProvider: startupAPIKeyProviderStub{},
		coreManager:    manager,
	}

	if err := service.loadInitialState(context.Background()); err != nil {
		t.Fatalf("loadInitialState() error = %v", err)
	}

	var clineAuth *coreauth.Auth
	for _, candidate := range manager.List() {
		if candidate != nil && candidate.Provider == "cline" {
			clineAuth = candidate
			break
		}
	}
	if clineAuth == nil {
		t.Fatal("expected config-derived Cline auth")
	}
	t.Cleanup(func() { reg.UnregisterClient(clineAuth.ID) })

	models := reg.GetModelsForClient(clineAuth.ID)
	if len(models) != 1 || !hasModelID(models, "mimo-v2.5-pro") {
		t.Fatalf("expected Cline alias model registered from config auth %+v, got %+v", clineAuth.Attributes, models)
	}
	if models[0].UpstreamModelID != "cline-pass/mimo-v2.5-pro" {
		t.Fatalf("UpstreamModelID = %q, want cline-pass/mimo-v2.5-pro", models[0].UpstreamModelID)
	}
}
