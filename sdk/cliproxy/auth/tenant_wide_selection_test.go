package auth

import (
	"context"
	"errors"
	"testing"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

const (
	tenantWideTestTenant  = "00000000-0000-0000-0000-0000000000c1"
	tenantWideOtherTenant = "00000000-0000-0000-0000-0000000000c2"
	tenantWideImageModel  = "gemini-3.1-flash-image"
)

// tenantWideTestManager reproduces the production tenant: the default group
// excludes an image model so API keys routed there cannot use it, and one
// account sits in a group kept out of the default pool. Before tenant-wide
// selection, the panel's probes were refused for both.
func tenantWideTestManager(t *testing.T) *Manager {
	t.Helper()

	manager := NewManager(nil, &FillFirstSelector{}, nil)
	manager.SetConfigForTenant(tenantWideTestTenant, &internalconfig.Config{
		Routing: internalconfig.RoutingConfig{
			IncludeDefaultGroup: true,
			ChannelGroups: []internalconfig.RoutingChannelGroup{
				{Name: "default", ExcludedModels: []string{tenantWideImageModel}},
				{
					Name:               "isolated",
					ExcludeFromDefault: true,
					Match:              internalconfig.ChannelGroupMatch{Channels: []string{"Isolated Account"}},
				},
			},
		},
	})
	manager.RegisterExecutorForTenant(tenantWideTestTenant, &stubExecutor{id: "antigravity"})
	manager.RegisterExecutorForTenant(tenantWideTestTenant, &stubExecutor{id: "codex"})

	for _, auth := range []*Auth{
		{ID: "tenant-wide-default", TenantID: tenantWideTestTenant, Label: "Default Account", Provider: "antigravity", Status: StatusActive},
		{ID: "tenant-wide-isolated", TenantID: tenantWideTestTenant, Label: "Isolated Account", Provider: "codex", Status: StatusActive},
		{ID: "tenant-wide-other-tenant", TenantID: tenantWideOtherTenant, Label: "Other Tenant", Provider: "antigravity", Status: StatusActive},
	} {
		if _, err := manager.Register(context.Background(), auth); err != nil {
			t.Fatalf("Register(%s): %v", auth.ID, err)
		}
	}
	return manager
}

func pickInTenant(manager *Manager, provider, model string, meta map[string]any) (*Auth, error) {
	scoped := map[string]any{cliproxyexecutor.TenantMetadataKey: tenantWideTestTenant}
	for key, value := range meta {
		scoped[key] = value
	}
	auth, _, err := manager.pickNext(context.Background(), provider, model, cliproxyexecutor.Options{Metadata: scoped}, map[string]struct{}{})
	return auth, err
}

func tenantWide(extra map[string]any) map[string]any {
	meta := map[string]any{cliproxyexecutor.TenantWideSelectionMetadataKey: true}
	for key, value := range extra {
		meta[key] = value
	}
	return meta
}

func TestTenantWideSelectionReachesModelsTheDefaultGroupExcludes(t *testing.T) {
	t.Parallel()
	manager := tenantWideTestManager(t)

	// API traffic keeps the operator's policy.
	_, err := pickInTenant(manager, "antigravity", tenantWideImageModel, nil)
	var selectionErr *Error
	if !errors.As(err, &selectionErr) || selectionErr.Code != "model_not_allowed_by_channel_group" {
		t.Fatalf("without the flag err = %v, want model_not_allowed_by_channel_group", err)
	}

	auth, err := pickInTenant(manager, "antigravity", tenantWideImageModel, tenantWide(nil))
	if err != nil || auth == nil || auth.ID != "tenant-wide-default" {
		t.Fatalf("tenant-wide pick = %#v, %v; want tenant-wide-default", auth, err)
	}
}

func TestTenantWideSelectionReachesAccountsOutsideTheDefaultGroup(t *testing.T) {
	t.Parallel()
	manager := tenantWideTestManager(t)

	if auth, err := pickInTenant(manager, "codex", "gpt-5", nil); err == nil {
		t.Fatalf("without the flag an account kept out of the default group was picked: %#v", auth)
	}

	// The string form is what a caller that serialises metadata sends.
	auth, err := pickInTenant(manager, "codex", "gpt-5", map[string]any{
		cliproxyexecutor.TenantWideSelectionMetadataKey: "true",
	})
	if err != nil || auth == nil || auth.ID != "tenant-wide-isolated" {
		t.Fatalf("tenant-wide pick = %#v, %v; want tenant-wide-isolated", auth, err)
	}
}

// The flag only drops the implicit default group. Restrictions the request
// states itself, and the tenant boundary, still hold.
func TestTenantWideSelectionKeepsExplicitRestrictions(t *testing.T) {
	t.Parallel()
	manager := tenantWideTestManager(t)

	for name, extra := range map[string]map[string]any{
		"route group":    {cliproxyexecutor.RouteGroupMetadataKey: "default"},
		"allowed groups": {"allowed-channel-groups": "default"},
	} {
		if auth, err := pickInTenant(manager, "antigravity", tenantWideImageModel, tenantWide(extra)); err == nil {
			t.Errorf("%s: picked %#v, want the named group's exclusion to apply", name, auth)
		}
	}

	auth, err := pickInTenant(manager, "antigravity", tenantWideImageModel, tenantWide(map[string]any{"allowed-channels": "Default Account"}))
	if err != nil || auth == nil || auth.ID != "tenant-wide-default" {
		t.Fatalf("allowed channel naming the account: pick = %#v, %v", auth, err)
	}
	if auth, err := pickInTenant(manager, "antigravity", tenantWideImageModel, tenantWide(map[string]any{"allowed-channels": "Isolated Account"})); err == nil {
		t.Errorf("allowed channels excluding the account: picked %#v", auth)
	}

	if auth, err := pickInTenant(manager, "antigravity", tenantWideImageModel, tenantWide(map[string]any{
		cliproxyexecutor.PinnedAuthMetadataKey: "tenant-wide-other-tenant",
	})); err == nil {
		t.Fatalf("a credential of another tenant was picked: %#v", auth)
	}
}
