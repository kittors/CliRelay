package auth

import (
	"context"
	"testing"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

const alphaSearchTestModel = "gpt-alpha-search-test"

func alphaSearchTestOAuth(id string) *Auth {
	return &Auth{ID: id, Provider: "codex", Label: id, Status: StatusActive,
		Metadata: map[string]any{"access_token": "test-oauth-token"}}
}

func alphaSearchTestManager(t *testing.T, candidates ...*Auth) *Manager {
	t.Helper()
	manager := NewManager(nil, &FillFirstSelector{}, nil)
	manager.SetConfig(&internalconfig.Config{Routing: internalconfig.RoutingConfig{IncludeDefaultGroup: true}})
	models := make(map[string][]string, len(candidates))
	for _, candidate := range candidates {
		manager.RegisterExecutorForTenant(normalizedTenantID(candidate.TenantID), &stubExecutor{id: candidate.Provider})
		if _, err := manager.Register(context.Background(), candidate); err != nil {
			t.Fatalf("register %s: %v", candidate.ID, err)
		}
		models[candidate.ID] = []string{alphaSearchTestModel}
	}
	manager.SetModelRegistry(&scopedModelRegistry{byClient: models})
	return manager
}

func alphaSearchTestPick(manager *Manager, mixed bool, opts cliproxyexecutor.Options, tried map[string]struct{}) (*Auth, error) {
	if mixed {
		selected, _, _, err := manager.pickNextMixed(context.Background(), []string{"codex", "openai"}, alphaSearchTestModel, opts, tried)
		return selected, err
	}
	selected, _, err := manager.pickNext(context.Background(), "codex", alphaSearchTestModel, opts, tried)
	return selected, err
}

func TestAlphaSearchCredentialPolicy(t *testing.T) {
	tests := []struct {
		name string
		auth *Auth
		want bool
	}{
		{name: "oauth", auth: alphaSearchTestOAuth("oauth"), want: true},
		{name: "missing token", auth: &Auth{ID: "missing", Provider: "codex", Metadata: map[string]any{"email": "test@example.invalid"}}},
		{name: "api key disabled by default", auth: &Auth{ID: "key", Provider: "codex", Attributes: map[string]string{"api_key": "test-key", "base_url": "https://example.invalid/v1"}}},
		{name: "email and OAuth token do not bypass API key opt in", auth: &Auth{ID: "email-key", Provider: "codex", Attributes: map[string]string{"api_key": "test-key", "base_url": "https://example.invalid/v1"}, Metadata: map[string]any{"email": "test@example.invalid", "access_token": "test-token", "alpha_search": true}}},
		{name: "API key explicit opt in", auth: &Auth{ID: "enabled-key", Provider: "codex", Attributes: map[string]string{"api_key": "test-key", "base_url": "https://example.invalid/v1", "alpha_search": "true"}}, want: true},
		{name: "API key opt in without base URL", auth: &Auth{ID: "no-base", Provider: "codex", Attributes: map[string]string{"api_key": "test-key", "alpha_search": "true"}}},
		{name: "other provider cannot opt in", auth: &Auth{ID: "openai", Provider: "openai", Attributes: map[string]string{"api_key": "test-key", "base_url": "https://example.invalid/v1", "alpha_search": "true"}, Metadata: map[string]any{"access_token": "test-token"}}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := SupportsCodexAlphaSearch(test.auth); got != test.want {
				t.Fatalf("SupportsCodexAlphaSearch = %v, want %v", got, test.want)
			}
			for _, mixed := range []bool{false, true} {
				manager := alphaSearchTestManager(t, test.auth)
				// Both selector entry points must apply the policy before scheduling.
				manager.RegisterExecutor(&stubExecutor{id: "codex"})
				selected, err := alphaSearchTestPick(manager, mixed, cliproxyexecutor.Options{Alt: "alpha/search"}, nil)
				if test.want {
					if err != nil || selected == nil || selected.ID != test.auth.ID {
						t.Fatalf("mixed=%v selected=%v err=%v", mixed, selected, err)
					}
				} else if err == nil || selected != nil {
					t.Fatalf("mixed=%v selected prohibited credential %v, err=%v", mixed, selected, err)
				}
			}
		})
	}
	if SupportsCodexAlphaSearch(nil) {
		t.Fatal("nil credential must not support search")
	}
}

func TestAlphaSearchSelectionPreservesAuthorizationScopes(t *testing.T) {
	const tenantA = "00000000-0000-0000-0000-00000000000a"
	const tenantB = "00000000-0000-0000-0000-00000000000b"
	tests := []struct {
		name  string
		setup func(*Manager, *Auth, map[string]any) map[string]struct{}
	}{
		{name: "tenant", setup: func(manager *Manager, candidate *Auth, meta map[string]any) map[string]struct{} {
			meta[cliproxyexecutor.TenantMetadataKey] = tenantA
			manager.RegisterExecutorForTenant(tenantA, &stubExecutor{id: "codex"})
			candidate.TenantID = tenantB
			return nil
		}},
		{name: "allowed channel", setup: func(_ *Manager, _ *Auth, meta map[string]any) map[string]struct{} {
			meta["allowed-channels"] = "different-channel"
			return nil
		}},
		{name: "allowed channel group", setup: func(_ *Manager, _ *Auth, meta map[string]any) map[string]struct{} {
			meta["allowed-channel-groups"] = "different-group"
			return nil
		}},
		{name: "route group", setup: func(_ *Manager, _ *Auth, meta map[string]any) map[string]struct{} {
			meta[cliproxyexecutor.RouteGroupMetadataKey] = "different-group"
			return nil
		}},
		{name: "registered model", setup: func(manager *Manager, candidate *Auth, _ map[string]any) map[string]struct{} {
			manager.SetModelRegistry(&scopedModelRegistry{byClient: map[string][]string{candidate.ID: {"different-model"}}})
			return nil
		}},
		{name: "group allowed models", setup: func(manager *Manager, _ *Auth, _ map[string]any) map[string]struct{} {
			manager.SetConfig(&internalconfig.Config{Routing: internalconfig.RoutingConfig{IncludeDefaultGroup: true,
				ChannelGroups: []internalconfig.RoutingChannelGroup{{Name: "default", AllowedModels: []string{"different-model"}}}}})
			return nil
		}},
		{name: "pinned", setup: func(_ *Manager, _ *Auth, meta map[string]any) map[string]struct{} {
			meta[cliproxyexecutor.PinnedAuthMetadataKey] = "different-auth"
			return nil
		}},
		{name: "tried", setup: func(_ *Manager, candidate *Auth, _ map[string]any) map[string]struct{} {
			return map[string]struct{}{candidate.ID: {}}
		}},
		{name: "disabled", setup: func(_ *Manager, candidate *Auth, _ map[string]any) map[string]struct{} {
			candidate.Disabled = true
			return nil
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			for _, mixed := range []bool{false, true} {
				candidate := alphaSearchTestOAuth("scoped-oauth")
				manager := alphaSearchTestManager(t, candidate)
				opts := cliproxyexecutor.Options{Alt: "alpha/search", Metadata: map[string]any{}}
				if selected, err := alphaSearchTestPick(manager, mixed, opts, nil); err != nil || selected == nil {
					t.Fatalf("positive control mixed=%v selected=%v err=%v", mixed, selected, err)
				}
				tried := test.setup(manager, candidate, opts.Metadata)
				if _, err := manager.Register(context.Background(), candidate); err != nil {
					t.Fatal(err)
				}
				selected, err := alphaSearchTestPick(manager, mixed, opts, tried)
				if err == nil || selected != nil {
					t.Fatalf("mixed=%v scope bypass: selected=%v err=%v", mixed, selected, err)
				}
			}
		})
	}
}

func TestAlphaSearchManagerExecuteSkipsIneligibleCandidates(t *testing.T) {
	oauth := alphaSearchTestOAuth("z-eligible-oauth")
	key := alphaSearchTestOAuth("a-ineligible-key")
	key.Attributes = map[string]string{"api_key": "test-key", "base_url": "https://example.invalid/v1"}
	other := alphaSearchTestOAuth("b-other-provider")
	other.Provider = "openai"
	manager := alphaSearchTestManager(t, key, other, oauth)
	executor := &executionMetadataExecutor{}
	manager.RegisterExecutor(executor)
	opts := cliproxyexecutor.Options{Alt: "alpha/search", Metadata: map[string]any{}}
	response, err := manager.Execute(context.Background(), []string{"openai", "codex"}, cliproxyexecutor.Request{
		Model: alphaSearchTestModel, Payload: []byte(`{"model":"gpt-alpha-search-test","commands":{"search_query":[{"q":"test"}]}}`),
	}, opts)
	if err != nil || string(response.Payload) != "ok" || executor.seenSelectedAuthID != oauth.ID {
		t.Fatalf("Execute payload=%q selected=%q err=%v", response.Payload, executor.seenSelectedAuthID, err)
	}
	// Pinning must narrow the eligible set, never grant a missing capability.
	opts.Metadata[cliproxyexecutor.PinnedAuthMetadataKey] = key.ID
	if _, err := manager.Execute(context.Background(), []string{"codex"}, cliproxyexecutor.Request{Model: alphaSearchTestModel}, opts); err == nil {
		t.Fatal("pinning selected a default-disabled API key")
	}
	// The API key remains eligible for its original Responses capability.
	selected, err := alphaSearchTestPick(manager, false, cliproxyexecutor.Options{Metadata: map[string]any{
		cliproxyexecutor.PinnedAuthMetadataKey: key.ID,
	}}, nil)
	if err != nil || selected == nil || selected.ID != key.ID {
		t.Fatalf("ordinary request selected=%v err=%v", selected, err)
	}
}

func TestAlphaSearchRouteFallbackStillFiltersCapabilityAndCallerScope(t *testing.T) {
	for _, mixed := range []bool{false, true} {
		key := alphaSearchTestOAuth("primary-key")
		key.Attributes = map[string]string{"api_key": "test-key", "base_url": "https://example.invalid/v1"}
		oauth := alphaSearchTestOAuth("fallback-oauth")
		manager := alphaSearchTestManager(t, key, oauth)
		manager.SetConfig(&internalconfig.Config{Routing: internalconfig.RoutingConfig{
			IncludeDefaultGroup: true,
			ChannelGroups: []internalconfig.RoutingChannelGroup{{
				Name: "primary", ExcludeFromDefault: true,
				Match: internalconfig.ChannelGroupMatch{Channels: []string{key.Label}},
			}},
		}})
		opts := cliproxyexecutor.Options{Alt: "alpha/search", Metadata: map[string]any{
			cliproxyexecutor.RouteGroupMetadataKey: "primary", cliproxyexecutor.RouteFallbackMetadataKey: "default",
		}}
		selected, err := alphaSearchTestPick(manager, mixed, opts, nil)
		if err != nil || selected == nil || selected.ID != oauth.ID {
			t.Fatalf("mixed=%v fallback selected=%v err=%v", mixed, selected, err)
		}
		opts.Metadata["allowed-channels"] = key.Label
		if selected, err := alphaSearchTestPick(manager, mixed, opts, nil); err == nil || selected != nil {
			t.Fatalf("mixed=%v fallback bypassed caller channel scope: selected=%v err=%v", mixed, selected, err)
		}
	}
}
