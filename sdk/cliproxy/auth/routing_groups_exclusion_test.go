package auth

import (
	"context"
	"testing"
	"time"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/registry"
)

// newGroupGateTestManager registers one credential serving models and a single
// channel group, which is all the runtime model gate looks at.
func newGroupGateTestManager(t *testing.T, clientID, prefix string, models []string, group internalconfig.RoutingChannelGroup) *Manager {
	t.Helper()

	reg := registry.GetGlobalRegistry()
	now := time.Now().Unix()
	infos := make([]*registry.ModelInfo, 0, len(models))
	for _, id := range models {
		infos = append(infos, &registry.ModelInfo{ID: id, Created: now})
	}
	reg.RegisterClient(clientID, "openai", infos)
	t.Cleanup(func() { reg.UnregisterClient(clientID) })

	manager := NewManager(nil, nil, nil)
	manager.SetConfig(&internalconfig.Config{
		Routing: internalconfig.RoutingConfig{
			ChannelGroups: []internalconfig.RoutingChannelGroup{group},
		},
	})
	if _, err := manager.Register(context.Background(), &Auth{ID: clientID, Provider: "openai", Prefix: prefix}); err != nil {
		t.Fatalf("Register() error = %v", err)
	}
	return manager
}

// The panel lists a prefixed credential's models with the prefix, so unchecking
// one writes "xai/grok-imagine-video-1.5", while a client routed by group path
// asks for the bare id. The exclusion used to compare only the request side's
// group prefix, so that spelling walked straight past it.
func TestCanServeModelWithScopesExclusionHoldsForEitherSpelling(t *testing.T) {
	t.Parallel()

	t.Run("prefixed entry, bare request", func(t *testing.T) {
		t.Parallel()
		manager := newGroupGateTestManager(t, "group-gate-prefixed-entry", "xai",
			[]string{"xai/grok-imagine-video-1.5", "xai/grok-4.7"},
			internalconfig.RoutingChannelGroup{Name: "xai", ExcludedModels: []string{"xai/grok-imagine-video-1.5"}})

		for _, model := range []string{"grok-imagine-video-1.5", "xai/grok-imagine-video-1.5"} {
			if manager.CanServeModelWithScopes(model, nil, nil, "xai") {
				t.Errorf("CanServeModelWithScopes(%q) = true, want the exclusion to hold", model)
			}
		}
		if !manager.CanServeModelWithScopes("grok-4.7", nil, nil, "xai") {
			t.Error("expected a model outside the exclusion to stay available")
		}
	})

	t.Run("bare entry, group named apart from the prefix", func(t *testing.T) {
		t.Parallel()
		manager := newGroupGateTestManager(t, "group-gate-bare-entry", "xai",
			[]string{"xai/grok-imagine-video-1.5", "xai/grok-4.7"},
			internalconfig.RoutingChannelGroup{
				Name:           "team",
				Match:          internalconfig.ChannelGroupMatch{Prefixes: []string{"xai"}},
				ExcludedModels: []string{"grok-imagine-video-1.5"},
			})

		if manager.CanServeModelWithScopes("xai/grok-imagine-video-1.5", nil, nil, "team") {
			t.Error("expected the bare exclusion to cover the prefixed request")
		}
		if !manager.CanServeModelWithScopes("xai/grok-4.7", nil, nil, "team") {
			t.Error("expected a model outside the exclusion to stay available")
		}
	})
}

// "*" in an allow list never meant "everything". Before exclusions existed it
// matched no model, so a group configured that way served nothing; reading it
// as a wildcard would silently open that group to every model.
func TestCanServeModelWithScopesAllowListStarIsNotAWildcard(t *testing.T) {
	t.Parallel()

	manager := newGroupGateTestManager(t, "group-gate-allow-star", "star",
		[]string{"star/gpt-5"},
		internalconfig.RoutingChannelGroup{Name: "star", AllowedModels: []string{"*"}})

	if manager.CanServeModelWithScopes("gpt-5", nil, nil, "star") {
		t.Fatal(`expected an allow list of "*" to keep refusing models`)
	}
}

// Group exclusions accept the same wildcards as provider-level excluded-models.
// They used to be compared literally, so "grok-imagine-*" excluded nothing.
func TestCanServeModelWithScopesExclusionWildcards(t *testing.T) {
	t.Parallel()

	manager := newGroupGateTestManager(t, "group-gate-wildcard", "wild",
		[]string{"wild/grok-imagine-video-1.5", "wild/grok-imagine-image", "wild/grok-4.7"},
		internalconfig.RoutingChannelGroup{Name: "wild", ExcludedModels: []string{"grok-imagine-*"}})

	for _, model := range []string{"grok-imagine-video-1.5", "grok-imagine-image"} {
		if manager.CanServeModelWithScopes(model, nil, nil, "wild") {
			t.Errorf("CanServeModelWithScopes(%q) = true, want the wildcard exclusion to hold", model)
		}
	}
	if !manager.CanServeModelWithScopes("grok-4.7", nil, nil, "wild") {
		t.Error("expected a model outside the wildcard to stay available")
	}
}
