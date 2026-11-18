package auth

import (
	"testing"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

func TestSetConfigSnapshotsRuntimeConfig(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	cfg := &internalconfig.Config{
		Routing: internalconfig.RoutingConfig{
			ChannelGroups: []internalconfig.RoutingChannelGroup{
				{
					Name:          "team-a",
					AllowedModels: []string{"gemini-2.5-pro"},
				},
			},
		},
		GeminiKey: []internalconfig.GeminiKey{
			{
				APIKey: "gemini-key",
				Models: []internalconfig.GeminiModel{
					{Name: "gemini-2.5-pro", Alias: "gp"},
				},
			},
		},
	}
	manager.SetConfig(cfg)

	cfg.Routing.ChannelGroups[0].Name = "mutated-group"
	cfg.Routing.ChannelGroups[0].AllowedModels[0] = "mutated-model"
	cfg.GeminiKey[0].Models[0].Name = "mutated-model"

	if resolved := manager.applyAPIKeyModelAlias(&Auth{
		Provider: "gemini",
		Attributes: map[string]string{
			"api_key": "gemini-key",
		},
	}, "gp"); resolved != "gemini-2.5-pro" {
		t.Fatalf("resolved alias = %q, want %q", resolved, "gemini-2.5-pro")
	}

	groups := manager.KnownChannelGroups()
	if _, ok := groups["team-a"]; !ok {
		t.Fatalf("expected original group name to remain in snapshot, got %#v", groups)
	}
	if _, ok := groups["mutated-group"]; ok {
		t.Fatalf("unexpected mutated group name leaked into snapshot: %#v", groups)
	}
}
