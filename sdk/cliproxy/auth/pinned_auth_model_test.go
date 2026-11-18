package auth

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/registry"
)

func TestAuthServesModel(t *testing.T) {
	reg := registry.GetGlobalRegistry()
	manager := NewManager(nil, nil, nil)
	manager.SetConfig(&internalconfig.Config{})

	register := func(auth *Auth, models ...string) {
		t.Helper()
		if _, err := manager.Register(context.Background(), auth); err != nil {
			t.Fatalf("Register(%s): %v", auth.ID, err)
		}
		if len(models) == 0 {
			return
		}
		infos := make([]*registry.ModelInfo, 0, len(models))
		for _, id := range models {
			infos = append(infos, &registry.ModelInfo{ID: id})
		}
		reg.RegisterClient(auth.ID, auth.Provider, infos)
		id := auth.ID
		t.Cleanup(func() { reg.UnregisterClient(id) })
	}
	register(&Auth{ID: "asm-claude", Provider: "claude", Status: StatusActive}, "asm-claude-model")
	register(&Auth{ID: "asm-pro", Provider: "codex", Status: StatusActive, Prefix: "pro"}, "pro/asm-codex-model")
	register(&Auth{ID: "asm-open", Provider: "openai-compatibility", Status: StatusActive})
	register(&Auth{ID: "asm-off", Provider: "claude", Status: StatusActive, Disabled: true}, "asm-claude-model")

	tests := []struct {
		name      string
		authID    string
		providers []string
		model     string
		want      bool
	}{
		{"listed model", "asm-claude", []string{"claude"}, "asm-claude-model", true},
		{"thinking suffix", "asm-claude", []string{"claude"}, "asm-claude-model(high)", true},
		{"other provider", "asm-claude", []string{"gemini"}, "asm-claude-model", false},
		{"model not listed", "asm-claude", []string{"claude"}, "asm-other-model", false},
		{"channel group prefix", "asm-pro", []string{"codex"}, "asm-codex-model", true},
		{"no model list", "asm-open", []string{"openai-compatibility"}, "anything", true},
		{"disabled", "asm-off", []string{"claude"}, "asm-claude-model", false},
		{"unknown auth", "asm-missing", []string{"claude"}, "asm-claude-model", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := manager.AuthServesModel("", tt.authID, tt.providers, tt.model); got != tt.want {
				t.Fatalf("AuthServesModel(%s, %v, %s) = %v, want %v", tt.authID, tt.providers, tt.model, got, tt.want)
			}
		})
	}
}

// A pin gives way to an account that is out of service for longer than a
// moment, and holds through a back-off the selector still lets through.
func TestPinnedAuthHeldBack(t *testing.T) {
	const model = "pahb-model"
	failed := func(status int, retryAfter time.Duration) Result {
		return Result{
			Provider:   "codex",
			Model:      model,
			Error:      &Error{HTTPStatus: status, Message: "upstream failure"},
			RetryAfter: &retryAfter,
		}
	}
	tests := []struct {
		name       string
		result     *Result
		wantHeld   bool
		wantReason string
	}{
		{name: "healthy"},
		{name: "quota exhausted", result: ptr(failed(http.StatusTooManyRequests, 2*time.Hour)), wantHeld: true, wantReason: CooldownReasonQuotaExhausted + " until "},
		{name: "credential rejected", result: ptr(failed(http.StatusUnauthorized, 0)), wantHeld: true, wantReason: CooldownReasonUnauthorized},
		{name: "load shed for a minute", result: ptr(failed(http.StatusServiceUnavailable, time.Minute)), wantHeld: true},
		{name: "load shed for a moment", result: ptr(failed(http.StatusServiceUnavailable, 2*time.Second))},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			manager := NewManager(nil, nil, nil)
			auth := &Auth{ID: "pahb-" + strings.ReplaceAll(tt.name, " ", "-"), Provider: "codex", Status: StatusActive}
			if _, err := manager.Register(context.Background(), auth); err != nil {
				t.Fatalf("Register: %v", err)
			}
			if tt.result != nil {
				result := *tt.result
				result.AuthID = auth.ID
				manager.MarkResult(context.Background(), result)
			}
			held, reason := manager.PinnedAuthHeldBack(auth.ID, model)
			if held != tt.wantHeld || !strings.HasPrefix(reason, tt.wantReason) {
				t.Fatalf("PinnedAuthHeldBack = (%v, %q), want (%v, %q...)", held, reason, tt.wantHeld, tt.wantReason)
			}
		})
	}
}

func ptr[T any](v T) *T { return &v }
