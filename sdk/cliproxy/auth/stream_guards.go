package auth

import (
	"time"

	sdkconfig "github.com/router-for-me/CLIProxyAPI/v6/sdk/config"
)

// streamGuards are the limits a channel group puts on waiting for an upstream:
// the first-token timeout of a streaming attempt and the cap on waiting while
// every account is cooling down. A zero value means "off".
type streamGuards struct {
	firstTokenTimeout time.Duration
	busyWaitMax       time.Duration
}

// builtInStreamGuards apply when no channel group entry governs the request.
// The values come from the config type so both sides read the same defaults.
var builtInStreamGuards = streamGuards{
	firstTokenTimeout: sdkconfig.RoutingChannelGroup{}.FirstTokenTimeout(),
	busyWaitMax:       sdkconfig.RoutingChannelGroup{}.BusyWaitMax(),
}

// streamGuardsFor resolves the guards governing a request.
//
// A request confined to one group (a route group, or an API key allowed into
// exactly one group) uses that group's values, as scopedScheduling does for the
// scheduling block. Every other request uses the values of the group named
// "default", which is where an operator sets them for the whole tenant; with no
// such entry the built-in defaults apply.
func (m *Manager) streamGuardsFor(meta map[string]any) streamGuards {
	cfg := m.currentRuntimeConfigForTenant(tenantIDFromMetadata(meta))
	name := normalizeGroupName(routeGroupFromMetadata(meta))
	if name == "" {
		name = onlyAllowedGroupName(allowedChannelGroupsFromMetadata(meta))
	}
	if name == "" {
		name = "default"
	}
	for i := range cfg.Routing.ChannelGroups {
		group := cfg.Routing.ChannelGroups[i]
		if normalizeGroupName(group.Name) == name {
			return streamGuards{firstTokenTimeout: group.FirstTokenTimeout, busyWaitMax: group.BusyWaitMax}
		}
	}
	return builtInStreamGuards
}
