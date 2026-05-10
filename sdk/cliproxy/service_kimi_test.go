package cliproxy

import (
	"context"
	"testing"

	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v6/sdk/config"
)

func TestRegisterModelsForAuth_KimiRegistersK2Dot6(t *testing.T) {
	service := &Service{cfg: &config.Config{}}
	auth := &coreauth.Auth{
		ID:       "kimi-auth-models",
		Provider: "kimi",
		Status:   coreauth.StatusActive,
	}

	registry := GlobalModelRegistry()
	registry.UnregisterClient(auth.ID)
	t.Cleanup(func() {
		registry.UnregisterClient(auth.ID)
	})

	service.registerModelsForAuth(context.Background(), auth)

	if !registry.ClientSupportsModel(auth.ID, "kimi-k2.6") {
		t.Fatalf("expected Kimi auth to register kimi-k2.6 support")
	}
	if !hasModelID(registry.GetAvailableModelsByProvider("kimi"), "kimi-k2.6") {
		t.Fatalf("expected kimi-k2.6 in available Kimi provider models")
	}
}
