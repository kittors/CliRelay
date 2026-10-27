package api

import (
	"context"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

// Turning the global disable-cooling switch on lifts the cooldowns already
// running: the flag alone only governs failures recorded after the reload.
func TestApplyAuthRuntimeConfigReleasesCooldownsWhenCoolingTurnsOff(t *testing.T) {
	server := newTestServer(t)
	t.Cleanup(func() { auth.SetQuotaCooldownDisabled(false) })
	manager := server.handlers.AuthManager
	if _, err := manager.Register(context.Background(), &auth.Auth{
		ID:         "relay",
		Provider:   "openai-compatibility",
		Status:     auth.StatusActive,
		Attributes: map[string]string{"base_url": "https://relay.example/v1"},
	}); err != nil {
		t.Fatalf("register: %v", err)
	}
	manager.MarkResult(context.Background(), auth.Result{
		AuthID: "relay", Provider: "openai-compatibility", Model: "glm-5",
		Error: &auth.Error{HTTPStatus: 429, Message: "Rate limit reached for requests"},
	})
	cooling := func() bool {
		current, _ := manager.GetByID("relay")
		state := current.ModelStates["glm-5"]
		return state != nil && state.NextRetryAfter.After(time.Now())
	}
	if !cooling() {
		t.Fatal("429 did not start a cooldown")
	}

	next := *server.cfg
	next.DisableCooling = true
	server.applyAuthRuntimeConfig(server.cfg, &next)
	if cooling() {
		t.Fatal("cooldown survived turning disable-cooling on")
	}
}
