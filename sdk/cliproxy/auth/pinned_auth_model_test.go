package auth

import (
	"context"
	"testing"

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
