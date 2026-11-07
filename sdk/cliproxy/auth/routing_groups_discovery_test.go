package auth

import (
	"testing"

	sdkconfig "github.com/router-for-me/CLIProxyAPI/v6/sdk/config"
	sdkrouting "github.com/router-for-me/CLIProxyAPI/v6/sdk/routing"
)

// The groups are the ones of the 2026-10-28 report: a CC Switch preset routed to
// "deepseekv4flash+chatgpt", whose allow list stopped at gpt-6.1-sol, refused
// gpt-6.1-terra while the root path served it from discovery.
func TestChannelGroupsFollowModelsTheUpstreamAddsLaterOnlyWhenSetTo(t *testing.T) {
	const tenant = "9e003dfb-751f-4898-b186-45f765c763a6"
	sdkrouting.SetDiscoveredModel(func(tenantID, modelID string) bool {
		return tenantID == tenant && modelID == "gpt-6.1-terra"
	})
	t.Cleanup(func() { sdkrouting.SetDiscoveredModel(nil) })

	on, off := true, false
	allow := []string{"gpt-6.1-sol"}
	cases := []struct {
		name   string
		group  sdkconfig.RoutingChannelGroup
		tenant string
		model  string
		want   bool
	}{
		{"default group follows when unset", sdkconfig.RoutingChannelGroup{Name: "default", AllowedModels: allow}, tenant, "gpt-6.1-terra", true},
		{"default group can be frozen", sdkconfig.RoutingChannelGroup{Name: "default", AllowedModels: allow, FollowUpstreamModels: &off}, tenant, "gpt-6.1-terra", false},
		{"curated group stays frozen when unset", sdkconfig.RoutingChannelGroup{Name: "deepseekv4flash+chatgpt", AllowedModels: allow}, tenant, "gpt-6.1-terra", false},
		{"curated group follows once turned on", sdkconfig.RoutingChannelGroup{Name: "deepseekv4flash+chatgpt", AllowedModels: allow, FollowUpstreamModels: &on}, tenant, "gpt-6.1-terra", true},
		{"an exclusion still wins", sdkconfig.RoutingChannelGroup{Name: "deepseekv4flash+chatgpt", AllowedModels: allow, ExcludedModels: []string{"gpt-6.1-*"}, FollowUpstreamModels: &on}, tenant, "gpt-6.1-terra", false},
		{"the allow list still applies to what discovery did not list", sdkconfig.RoutingChannelGroup{Name: "deepseekv4flash+chatgpt", AllowedModels: allow, FollowUpstreamModels: &on}, tenant, "not-on-the-manifest", false},
		{"another tenant's discovery does not count", sdkconfig.RoutingChannelGroup{Name: "deepseekv4flash+chatgpt", AllowedModels: allow, FollowUpstreamModels: &on}, "another-tenant", "gpt-6.1-terra", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Through the snapshot, so the unset default is resolved where the
			// selector resolves it.
			groups := cloneRuntimeRoutingChannelGroups([]sdkconfig.RoutingChannelGroup{tc.group})
			if got := routingGroupModelAllowed(groups[0], tc.model, tc.tenant); got != tc.want {
				t.Fatalf("routingGroupModelAllowed(%q) = %v, want %v", tc.model, got, tc.want)
			}
		})
	}
}
