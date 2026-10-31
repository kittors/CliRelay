package auth

import (
	"testing"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v6/sdk/config"
)

func TestStreamGuardsFollowTheGoverningGroup(t *testing.T) {
	mgr := NewManager(nil, nil, nil)
	if got := mgr.streamGuardsFor(nil); got != builtInStreamGuards {
		t.Fatalf("no config: %+v, want the built-in defaults", got)
	}
	// Both guards are opt-in: a group nobody configured behaves as it did before.
	if builtInStreamGuards.firstTokenTimeout != 0 || builtInStreamGuards.busyWaitMax != 0 {
		t.Fatalf("built-in defaults = %+v, want both guards off", builtInStreamGuards)
	}

	mgr.SetConfig(&sdkconfig.Config{Routing: sdkconfig.RoutingConfig{ChannelGroups: []sdkconfig.RoutingChannelGroup{
		{Name: "default", FirstTokenTimeoutSeconds: 20, BusyWaitMaxSeconds: new(0)},
		{Name: "team", FirstTokenTimeoutSeconds: 45, BusyWaitMaxSeconds: new(60)},
		{Name: "other", BusyWaitMaxSeconds: new(120)},
	}}})
	cases := []struct {
		name string
		meta map[string]any
		want streamGuards
	}{
		{"route group", map[string]any{cliproxyexecutor.RouteGroupMetadataKey: "team"}, streamGuards{45 * time.Second, 60 * time.Second}},
		{"only allowed group", map[string]any{"allowed-channel-groups": []string{"other"}}, streamGuards{0, 120 * time.Second}},
		{"several allowed groups use default", map[string]any{"allowed-channel-groups": []string{"team", "other"}}, streamGuards{20 * time.Second, 0}},
		{"unscoped uses default", map[string]any{}, streamGuards{20 * time.Second, 0}},
		{"route group without an entry", map[string]any{cliproxyexecutor.RouteGroupMetadataKey: "ghost"}, builtInStreamGuards},
	}
	for _, tc := range cases {
		if got := mgr.streamGuardsFor(tc.meta); got != tc.want {
			t.Fatalf("%s: %+v, want %+v", tc.name, got, tc.want)
		}
	}

	// A routing save reaches the next request without a restart.
	mgr.SetConfig(&sdkconfig.Config{Routing: sdkconfig.RoutingConfig{ChannelGroups: []sdkconfig.RoutingChannelGroup{
		{Name: "team", FirstTokenTimeoutSeconds: 10},
	}}})
	if got := mgr.streamGuardsFor(map[string]any{cliproxyexecutor.RouteGroupMetadataKey: "team"}); got.firstTokenTimeout != 10*time.Second {
		t.Fatalf("after reload: %+v, want the new 10s timeout", got)
	}
	// Each tenant has its own routing.
	other := map[string]any{cliproxyexecutor.TenantMetadataKey: "tenant-b", cliproxyexecutor.RouteGroupMetadataKey: "team"}
	if got := mgr.streamGuardsFor(other); got != builtInStreamGuards {
		t.Fatalf("another tenant read this tenant's group: %+v", got)
	}
}
