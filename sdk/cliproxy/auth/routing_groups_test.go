package auth

import (
	"context"
	"testing"
	"time"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/registry"
)

func TestAllowedChannelGroupsFromMetadataParsesStringList(t *testing.T) {
	t.Parallel()

	allowed := allowedChannelGroupsFromMetadata(map[string]any{
		"allowed-channel-groups": " Pro,team-a,,PRO ",
	})

	if len(allowed) != 2 {
		t.Fatalf("allowed group count = %d, want 2", len(allowed))
	}
	if _, ok := allowed["pro"]; !ok {
		t.Fatal("expected normalized group pro")
	}
	if _, ok := allowed["team-a"]; !ok {
		t.Fatal("expected normalized group team-a")
	}
}

func TestCanServeModelWithScopesSupportsAllowedGroupPrefixedModels(t *testing.T) {
	t.Parallel()

	reg := registry.GetGlobalRegistry()
	now := time.Now().Unix()
	reg.RegisterClient("pro-auth", "openai", []*registry.ModelInfo{
		{ID: "pro/gpt-5", Created: now},
	})
	t.Cleanup(func() {
		reg.UnregisterClient("pro-auth")
	})

	manager := NewManager(nil, nil, nil)
	manager.SetConfig(&internalconfig.Config{})
	if _, err := manager.Register(context.Background(), &Auth{
		ID:       "pro-auth",
		Provider: "openai",
		Prefix:   "pro",
	}); err != nil {
		t.Fatalf("Register() error = %v", err)
	}

	allowedGroups := map[string]struct{}{"pro": {}}
	if !manager.CanServeModelWithScopes("gpt-5", nil, allowedGroups, "") {
		t.Fatal("expected unprefixed model to be available through allowed pro group")
	}
}

func TestCanServeModelWithScopesHonorsGroupAllowedModels(t *testing.T) {
	t.Parallel()

	reg := registry.GetGlobalRegistry()
	now := time.Now().Unix()
	reg.RegisterClient("team-auth", "openai", []*registry.ModelInfo{
		{ID: "team/gpt-5", Created: now},
		{ID: "team/claude-opus", Created: now},
	})
	t.Cleanup(func() {
		reg.UnregisterClient("team-auth")
	})

	manager := NewManager(nil, nil, nil)
	manager.SetConfig(&internalconfig.Config{
		Routing: internalconfig.RoutingConfig{
			ChannelGroups: []internalconfig.RoutingChannelGroup{
				{
					Name:          "team",
					AllowedModels: []string{"gpt-5"},
				},
			},
		},
	})
	if _, err := manager.Register(context.Background(), &Auth{
		ID:       "team-auth",
		Provider: "openai",
		Prefix:   "team",
	}); err != nil {
		t.Fatalf("Register() error = %v", err)
	}

	if !manager.CanServeModelWithScopes("gpt-5", nil, nil, "team") {
		t.Fatal("expected allowed model to be available through route group")
	}
	if manager.CanServeModelWithScopes("claude-opus", nil, nil, "team") {
		t.Fatal("expected model outside routing group allowed-models to be unavailable")
	}
}

func TestAuthGroupsMatchesLegacyOAuthEmailAfterRename(t *testing.T) {
	t.Parallel()

	cfg := &internalconfig.Config{
		Routing: internalconfig.RoutingConfig{
			ChannelGroups: []internalconfig.RoutingChannelGroup{
				{
					Name: "team-alpha",
					Match: internalconfig.ChannelGroupMatch{
						Channels: []string{"legacy@example.com"},
					},
					ChannelPriorities: map[string]int{
						"legacy@example.com": 100,
					},
				},
			},
		},
	}
	auth := &Auth{
		Label: "chatgpt-pro1",
		Metadata: map[string]any{
			"email": "legacy@example.com",
		},
	}

	groups := authGroups(cfg, auth)
	if _, ok := groups["team-alpha"]; !ok {
		t.Fatalf("expected group match through legacy email alias, got %v", groups)
	}
	if got, ok := derivedGroupPriority(cfg, auth, map[string]struct{}{"team-alpha": {}}); !ok || got != 100 {
		t.Fatalf("derivedGroupPriority() = %d, want 100", got)
	}
}

func TestDerivedGroupPriorityPreservesExplicitZero(t *testing.T) {
	t.Parallel()

	cfg := &internalconfig.Config{
		Routing: internalconfig.RoutingConfig{
			ChannelGroups: []internalconfig.RoutingChannelGroup{
				{
					Name: "team-alpha",
					Match: internalconfig.ChannelGroupMatch{
						Channels: []string{"chatgpt-pro1"},
					},
					ChannelPriorities: map[string]int{
						"chatgpt-pro1": 0,
					},
				},
			},
		},
	}
	auth := &Auth{Label: "chatgpt-pro1"}

	got, ok := derivedGroupPriority(cfg, auth, map[string]struct{}{"team-alpha": {}})
	if !ok {
		t.Fatal("derivedGroupPriority() did not report an explicit priority")
	}
	if got != 0 {
		t.Fatalf("derivedGroupPriority() = %d, want 0", got)
	}

	prepared := prepareCandidateForSelection(cfg, auth, "", map[string]struct{}{"team-alpha": {}})
	if prepared == nil {
		t.Fatal("prepareCandidateForSelection() = nil")
	}
	if got := prepared.Attributes["priority"]; got != "0" {
		t.Fatalf("prepared priority = %q, want %q", got, "0")
	}
}

func TestPrepareCandidateForSelectionIgnoresPriorityOutsideSelectionScope(t *testing.T) {
	t.Parallel()

	cfg := &internalconfig.Config{
		Routing: internalconfig.RoutingConfig{
			ChannelGroups: []internalconfig.RoutingChannelGroup{
				{
					Name: "team-alpha",
					Match: internalconfig.ChannelGroupMatch{
						Channels: []string{"chatgpt-pro1"},
					},
					ChannelPriorities: map[string]int{
						"chatgpt-pro1": 100,
					},
				},
			},
		},
	}
	auth := &Auth{Label: "chatgpt-pro1"}

	prepared := prepareCandidateForSelection(cfg, auth, "", nil)
	if prepared == nil {
		t.Fatal("prepareCandidateForSelection() = nil")
	}
	if got := prepared.Attributes["priority"]; got != "" {
		t.Fatalf("prepared priority = %q, want empty outside scoped selection", got)
	}
}
