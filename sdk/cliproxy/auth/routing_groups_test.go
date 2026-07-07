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

func TestCanServeModelWithScopesHonorsDisableAllModelsMetadata(t *testing.T) {
	t.Parallel()

	manager := NewManager(nil, nil, nil)
	manager.SetConfig(&internalconfig.Config{})
	if _, err := manager.Register(context.Background(), &Auth{
		ID:         "model-access-disabled-auth",
		Provider:   "opencode-go",
		Attributes: map[string]string{"excluded_models": "*"},
	}); err != nil {
		t.Fatalf("Register() error = %v", err)
	}

	if manager.CanServeModelWithScopes("qwen3.7-max", nil, nil, "") {
		t.Fatal("expected excluded_models=* auth not to serve any model")
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

	runtimeCfg := newRuntimeConfigSnapshot(cfg)
	groups := authGroups(runtimeCfg, auth)
	if _, ok := groups["team-alpha"]; !ok {
		t.Fatalf("expected group match through legacy email alias, got %v", groups)
	}
	if got, ok := derivedGroupPriority(runtimeCfg, auth, map[string]struct{}{"team-alpha": {}}); !ok || got != 100 {
		t.Fatalf("derivedGroupPriority() = %d, want 100", got)
	}
}

func TestAuthGroupsExcludeFromDefaultKeepsExplicitGroup(t *testing.T) {
	t.Parallel()

	cfg := &internalconfig.Config{
		Routing: internalconfig.RoutingConfig{
			IncludeDefaultGroup: true,
			ChannelGroups: []internalconfig.RoutingChannelGroup{
				{
					Name:               "kimicode",
					ExcludeFromDefault: true,
					Match: internalconfig.ChannelGroupMatch{
						Channels: []string{"Kimi Channel"},
					},
				},
			},
		},
	}
	auth := &Auth{
		Label:    "Kimi Channel",
		Provider: "kimi",
	}

	runtimeCfg := newRuntimeConfigSnapshot(cfg)
	groups := authGroups(runtimeCfg, auth)
	if _, ok := groups["kimicode"]; !ok {
		t.Fatalf("expected explicit kimicode group, got %v", groups)
	}
	if _, ok := groups["default"]; ok {
		t.Fatalf("exclusive group member should not be in default, got %v", groups)
	}
	if authAllowedByGroups(runtimeCfg, auth, map[string]struct{}{"default": {}}) {
		t.Fatal("expected default group restriction to reject isolated auth")
	}
	if !authAllowedByGroups(runtimeCfg, auth, map[string]struct{}{"kimicode": {}}) {
		t.Fatal("expected explicit group restriction to allow isolated auth")
	}
}

func TestEffectiveRouteGroupForSelectionDefaultsOnlyForUnprefixedRootModels(t *testing.T) {
	t.Parallel()

	cfg := &internalconfig.Config{
		Routing: internalconfig.RoutingConfig{IncludeDefaultGroup: true},
	}

	runtimeCfg := newRuntimeConfigSnapshot(cfg)
	if got := effectiveRouteGroupForSelection(runtimeCfg, "", nil, "gpt-5.5"); got != "default" {
		t.Fatalf("unprefixed root route group = %q, want default", got)
	}
	if got := effectiveRouteGroupForSelection(runtimeCfg, "", nil, "kimicode/gpt-5.5"); got != "" {
		t.Fatalf("prefixed root route group = %q, want empty", got)
	}
	if got := effectiveRouteGroupForSelection(runtimeCfg, "kimicode", nil, "gpt-5.5"); got != "kimicode" {
		t.Fatalf("explicit route group = %q, want kimicode", got)
	}
	if got := effectiveRouteGroupForSelection(runtimeCfg, "", map[string]struct{}{"kimicode": {}}, "gpt-5.5"); got != "" {
		t.Fatalf("allowed group route group = %q, want empty", got)
	}
}

func TestAuthGroupsMatchesAnyDisplayTagDynamically(t *testing.T) {
	t.Parallel()

	cfg := &internalconfig.Config{
		Routing: internalconfig.RoutingConfig{
			ChannelGroups: []internalconfig.RoutingChannelGroup{
				{
					Name: "tag-pool",
					Match: internalconfig.ChannelGroupMatch{
						Tags: []string{"vip", "team-a"},
					},
				},
			},
		},
	}
	auth := &Auth{
		Label:    "codex-account",
		Provider: "codex",
		Metadata: map[string]any{
			"custom_tags": []string{"vip"},
		},
	}

	runtimeCfg := newRuntimeConfigSnapshot(cfg)
	groups := authGroups(runtimeCfg, auth)
	if _, ok := groups["tag-pool"]; !ok {
		t.Fatalf("expected tag-pool from matching tag, got %v", groups)
	}

	auth.Metadata["custom_tags"] = []string{"other"}
	groups = authGroups(runtimeCfg, auth)
	if _, ok := groups["tag-pool"]; ok {
		t.Fatalf("tag-pool should disappear after the matching tag is removed, got %v", groups)
	}
}

func TestAuthGroupsIgnoresHiddenDisplayTags(t *testing.T) {
	t.Parallel()

	cfg := &internalconfig.Config{
		Routing: internalconfig.RoutingConfig{
			ChannelGroups: []internalconfig.RoutingChannelGroup{
				{
					Name: "pro-pool",
					Match: internalconfig.ChannelGroupMatch{
						Tags: []string{"pro"},
					},
				},
			},
		},
	}
	auth := &Auth{
		Label:    "codex-account",
		Provider: "codex",
		Metadata: map[string]any{
			"plan_type":           "pro",
			"hidden_default_tags": []string{"pro"},
		},
	}

	runtimeCfg := newRuntimeConfigSnapshot(cfg)
	groups := authGroups(runtimeCfg, auth)
	if _, ok := groups["pro-pool"]; ok {
		t.Fatalf("pro-pool should not match a hidden display tag, got %v", groups)
	}

	auth.Metadata["display_tags"] = []string{}
	groups = authGroups(runtimeCfg, auth)
	if _, ok := groups["pro-pool"]; ok {
		t.Fatalf("pro-pool should not match an explicit empty display tag list, got %v", groups)
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

	runtimeCfg := newRuntimeConfigSnapshot(cfg)
	got, ok := derivedGroupPriority(runtimeCfg, auth, map[string]struct{}{"team-alpha": {}})
	if !ok {
		t.Fatal("derivedGroupPriority() did not report an explicit priority")
	}
	if got != 0 {
		t.Fatalf("derivedGroupPriority() = %d, want 0", got)
	}

	prepared := prepareCandidateForSelection(runtimeCfg, auth, "", map[string]struct{}{"team-alpha": {}})
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

	prepared := prepareCandidateForSelection(newRuntimeConfigSnapshot(cfg), auth, "", nil)
	if prepared == nil {
		t.Fatal("prepareCandidateForSelection() = nil")
	}
	if got := prepared.Attributes["priority"]; got != "" {
		t.Fatalf("prepared priority = %q, want empty outside scoped selection", got)
	}
}

func TestRouteFallbackFromMetadataNormalizesKnownValues(t *testing.T) {
	t.Parallel()

	if got := routeFallbackFromMetadata(map[string]any{"route_fallback": " default "}); got != "default" {
		t.Fatalf("routeFallbackFromMetadata(default) = %q, want default", got)
	}
	if got := routeFallbackFromMetadata(map[string]any{"route_fallback": "invalid"}); got != "none" {
		t.Fatalf("routeFallbackFromMetadata(invalid) = %q, want none", got)
	}
}

func TestAuthDisplayTagsRemapsStaleCodexPlanTagToCurrentPlan(t *testing.T) {
	t.Parallel()

	auth := &Auth{
		Provider: "codex",
		Metadata: map[string]any{
			"plan_type":    "pro",
			"display_tags": []string{"plus", "codex"},
		},
	}

	got := authDisplayTags(auth)
	if len(got) != 2 {
		t.Fatalf("authDisplayTags() len = %d, want 2 (%v)", len(got), got)
	}
	if got[0] != "pro" && got[1] != "pro" {
		t.Fatalf("authDisplayTags() = %v, want remapped current plan tag pro", got)
	}
}
