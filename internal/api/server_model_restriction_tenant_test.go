package api

import (
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/identity"
)

func TestScopedRoutingAllowedModelsUsesSystemCfgWhenNoDB(t *testing.T) {
	server := &Server{cfg: &config.Config{
		Routing: config.RoutingConfig{
			IncludeDefaultGroup: true,
			ChannelGroups: []config.RoutingChannelGroup{
				{Name: "default", AllowedModels: []string{"gpt-5.5"}},
			},
		},
	}}
	if !server.hasScopedRoutingModelRestrictionForTenant(identity.SystemTenantID, "", nil) {
		t.Fatal("expected the default group's allowed-models to count as a restriction")
	}
	if server.modelAllowedByScopedRoutingGroupsForTenant(identity.SystemTenantID, "minimax-m2.7", "", nil) {
		t.Fatal("expected minimax-m2.7 to be forbidden by default group allowed-models")
	}
	if !server.modelAllowedByScopedRoutingGroupsForTenant(identity.SystemTenantID, "gpt-5.5", "", nil) {
		t.Fatal("expected gpt-5.5 to be allowed")
	}
}

// The /v1/models listing has to apply exclusions too, or a model the operator
// blocked would still be advertised to clients — and, more importantly, a model
// the upstream added later must not be hidden just because the group narrows
// anything at all.
func TestScopedRoutingModelGateHonorsExcludedModels(t *testing.T) {
	server := &Server{cfg: &config.Config{
		Routing: config.RoutingConfig{
			IncludeDefaultGroup: true,
			ChannelGroups: []config.RoutingChannelGroup{
				{Name: "default", ExcludedModels: []string{"grok-imagine-video-1.5"}},
			},
		},
	}}
	if !server.hasScopedRoutingModelRestrictionForTenant(identity.SystemTenantID, "", nil) {
		t.Fatal("expected an exclusion-only group to count as a restriction")
	}
	if server.modelAllowedByScopedRoutingGroupsForTenant(identity.SystemTenantID, "grok-imagine-video-1.5", "", nil) {
		t.Fatal("expected excluded model to be filtered out of the listing")
	}
	if !server.modelAllowedByScopedRoutingGroupsForTenant(identity.SystemTenantID, "grok-4.7", "", nil) {
		t.Fatal("expected a newly added upstream model to stay listed")
	}
}

func TestScopedRoutingAllowedModelsDoesNotUseSystemCfgForOtherTenant(t *testing.T) {
	server := &Server{cfg: &config.Config{
		Routing: config.RoutingConfig{
			IncludeDefaultGroup: true,
			ChannelGroups: []config.RoutingChannelGroup{
				{Name: "default", AllowedModels: []string{"gpt-5.5"}},
			},
		},
	}}
	// Non-system tenant without a DB routing row must not inherit system allowed-models.
	other := "cccccccc-dddd-eeee-ffff-000000000001"
	if server.hasScopedRoutingModelRestrictionForTenant(other, "", nil) {
		t.Fatal("expected no restriction when the tenant has no routing row")
	}
	if !server.modelAllowedByScopedRoutingGroupsForTenant(other, "minimax-m2.7", "", nil) {
		t.Fatal("expected unrestricted when tenant has no routing row")
	}
}

// The request and listing gate must read exclusions exactly like the runtime
// gate: either side may carry a route prefix, and entries take wildcards. The
// panel writes "xai/grok-imagine-video-1.5" for a prefixed credential, and a
// client routed by group path asks for "grok-imagine-video-1.5".
func TestScopedRoutingModelGateExclusionMatching(t *testing.T) {
	server := &Server{cfg: &config.Config{
		Routing: config.RoutingConfig{
			IncludeDefaultGroup: true,
			ChannelGroups: []config.RoutingChannelGroup{
				{Name: "default", ExcludedModels: []string{"xai/grok-imagine-video-1.5", "gemini-*-preview"}},
			},
		},
	}}
	for model, want := range map[string]bool{
		"grok-imagine-video-1.5":        false,
		"xai/grok-imagine-video-1.5":    false,
		"gemini-3-flash-preview":        false,
		"ollama/gemini-3-flash-preview": false,
		"grok-4.7":                      true,
		"gemini-3-flash":                true,
	} {
		if got := server.modelAllowedByScopedRoutingGroupsForTenant(identity.SystemTenantID, model, "", nil); got != want {
			t.Errorf("modelAllowedByScopedRoutingGroupsForTenant(%q) = %v, want %v", model, got, want)
		}
	}
}

// "*" in an allow list never meant "everything": before exclusions existed it
// matched no model, and reading it as a wildcard would open the group.
func TestScopedRoutingModelGateAllowListStarIsNotAWildcard(t *testing.T) {
	server := &Server{cfg: &config.Config{
		Routing: config.RoutingConfig{
			IncludeDefaultGroup: true,
			ChannelGroups: []config.RoutingChannelGroup{
				{Name: "default", AllowedModels: []string{"*"}},
			},
		},
	}}
	if server.modelAllowedByScopedRoutingGroupsForTenant(identity.SystemTenantID, "gpt-5.5", "", nil) {
		t.Fatal(`expected an allow list of "*" to keep refusing models`)
	}
}
