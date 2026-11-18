package auth

import (
	"context"
	"strings"
	"testing"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

func TestPickNextRouteGroupFallbacksToDefault(t *testing.T) {
	t.Parallel()

	manager := NewManager(nil, &FillFirstSelector{}, nil)
	manager.SetConfig(&internalconfig.Config{
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
	})
	manager.RegisterExecutor(&stubExecutor{id: "codex"})

	for _, auth := range []*Auth{
		{
			ID:       "codex-default-auth",
			Label:    "Default Codex",
			Provider: "codex",
			Status:   StatusActive,
		},
		{
			ID:       "kimi-isolated-auth",
			Label:    "Kimi Channel",
			Provider: "kimi",
			Status:   StatusActive,
		},
	} {
		if _, err := manager.Register(context.Background(), auth); err != nil {
			t.Fatalf("register %s: %v", auth.ID, err)
		}
	}

	auth, executor, err := manager.pickNext(context.Background(), "codex", "", cliproxyexecutor.Options{
		Metadata: map[string]any{
			cliproxyexecutor.RouteGroupMetadataKey:    "kimicode",
			cliproxyexecutor.RouteFallbackMetadataKey: "default",
		},
	}, map[string]struct{}{})
	if err != nil {
		t.Fatalf("pickNext() error = %v", err)
	}
	if auth == nil || auth.ID != "codex-default-auth" {
		t.Fatalf("pickNext() auth = %#v, want codex-default-auth", auth)
	}
	if executor == nil || executor.Identifier() != "codex" {
		t.Fatalf("pickNext() executor = %#v, want codex executor", executor)
	}
}

func TestPickNextMixedRouteGroupFallbacksToDefaultWhenScopedExecutorMissing(t *testing.T) {
	t.Parallel()

	manager := NewManager(nil, &FillFirstSelector{}, nil)
	manager.SetConfig(&internalconfig.Config{
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
	})
	manager.RegisterExecutor(&stubExecutor{id: "codex"})

	for _, auth := range []*Auth{
		{
			ID:       "codex-default-auth",
			Label:    "Default Codex",
			Provider: "codex",
			Status:   StatusActive,
		},
		{
			ID:       "kimi-isolated-auth",
			Label:    "Kimi Channel",
			Provider: "kimi",
			Status:   StatusActive,
		},
	} {
		if _, err := manager.Register(context.Background(), auth); err != nil {
			t.Fatalf("register %s: %v", auth.ID, err)
		}
	}

	auth, executor, provider, err := manager.pickNextMixed(context.Background(), []string{"kimi", "codex"}, "", cliproxyexecutor.Options{
		Metadata: map[string]any{
			cliproxyexecutor.RouteGroupMetadataKey:    "kimicode",
			cliproxyexecutor.RouteFallbackMetadataKey: "default",
		},
	}, map[string]struct{}{})
	if err != nil {
		t.Fatalf("pickNextMixed() error = %v", err)
	}
	if provider != "codex" {
		t.Fatalf("pickNextMixed() provider = %q, want codex", provider)
	}
	if auth == nil || auth.ID != "codex-default-auth" {
		t.Fatalf("pickNextMixed() auth = %#v, want codex-default-auth", auth)
	}
	if executor == nil || executor.Identifier() != "codex" {
		t.Fatalf("pickNextMixed() executor = %#v, want codex executor", executor)
	}
}

func TestSelectionScopeRejectsCrossTenantCredential(t *testing.T) {
	t.Parallel()

	tenantA := "00000000-0000-0000-0000-00000000000a"
	tenantB := "00000000-0000-0000-0000-00000000000b"
	scope := newSelectionScope(nil, "", map[string]any{cliproxyexecutor.TenantMetadataKey: tenantA})

	if !scope.allowsCandidate(&Auth{ID: "a", TenantID: tenantA}, "", nil, nil) {
		t.Fatal("tenant A credential should be selectable for tenant A")
	}
	if scope.allowsCandidate(&Auth{ID: "b", TenantID: tenantB}, "", nil, nil) {
		t.Fatal("tenant B credential must not be selectable for tenant A")
	}
}

func TestPickNextUsesTenantExecutorWithoutSystemFallback(t *testing.T) {
	manager := NewManager(nil, &FillFirstSelector{}, nil)
	const tenantA = "00000000-0000-0000-0000-00000000000a"
	const tenantB = "00000000-0000-0000-0000-00000000000b"
	systemExecutor := &stubExecutor{id: "codex"}
	tenantExecutor := &stubExecutor{id: "codex"}
	manager.RegisterExecutor(systemExecutor)
	manager.RegisterExecutorForTenant(tenantA, tenantExecutor)
	for _, auth := range []*Auth{
		{ID: "system-codex", TenantID: defaultTenantID, Provider: "codex", Status: StatusActive},
		{ID: "tenant-a-codex", TenantID: tenantA, Provider: "codex", Status: StatusActive},
		{ID: "tenant-b-codex", TenantID: tenantB, Provider: "codex", Status: StatusActive},
	} {
		if _, err := manager.Register(context.Background(), auth); err != nil {
			t.Fatalf("Register(%s): %v", auth.ID, err)
		}
	}

	auth, exec, err := manager.pickNext(context.Background(), "codex", "", cliproxyexecutor.Options{
		Metadata: map[string]any{cliproxyexecutor.TenantMetadataKey: tenantA},
	}, map[string]struct{}{})
	if err != nil || auth == nil || auth.ID != "tenant-a-codex" || exec != tenantExecutor {
		t.Fatalf("tenant A pick = auth %#v exec %#v err %v", auth, exec, err)
	}
	if _, _, err := manager.pickNext(context.Background(), "codex", "", cliproxyexecutor.Options{
		Metadata: map[string]any{cliproxyexecutor.TenantMetadataKey: tenantB},
	}, map[string]struct{}{}); err == nil || !strings.Contains(err.Error(), "executor not registered") {
		t.Fatalf("tenant B unexpectedly fell back to system executor: %v", err)
	}
}
