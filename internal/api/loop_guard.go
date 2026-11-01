package api

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/api/middleware"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/cluster"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/loopguard"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/runtime/executor"
	"github.com/router-for-me/CLIProxyAPI/v6/sdk/api/handlers"
	"github.com/router-for-me/CLIProxyAPI/v6/sdk/loopdetect"
)

// apiKeyIDContextKey is the gin context key AuthMiddleware stores the id of the
// API key that authenticated the request under.
const apiKeyIDContextKey = "apiKeyID"

// useInferenceMiddleware applies what every inference route group runs: the
// request stages, and after them loop protection, which is not a stage. A stage
// admits or reroutes a request and the routing simulator reports it; loop
// protection only publishes the settings the stream handlers apply, so it is
// kept out of the list the simulator walks.
func (s *Server) useInferenceMiddleware(group *gin.RouterGroup, stages []requestStage) {
	useRequestStages(group, stages)
	group.Use(loopGuardMiddleware(loopguard.Default()))
}

// loopGuardMiddleware publishes the loop protection of the request for the
// handlers: a gate that resolves the settings of the request's tenant and API
// key whenever they ask. It decides nothing itself and reads no body.
//
// Only the requests that carry a conversation are given a gate: a POST, and the
// WebSocket upgrade of the Responses API, whose turns arrive later in frames. The
// gate is a function and not a value because a WebSocket serves turns for hours,
// and a setting changed meanwhile applies from the next turn on.
func loopGuardMiddleware(registry *loopguard.Registry) gin.HandlerFunc {
	return func(c *gin.Context) {
		if c.Request.Method == http.MethodPost || middleware.IsWebsocketUpgrade(c.Request) {
			tenantID := requestTenantID(c)
			keyID := strings.TrimSpace(c.GetString(apiKeyIDContextKey))
			c.Set(handlers.LoopGuardGateContextKey, handlers.LoopGuardGate(func() *loopdetect.Policy {
				return loopGuardPolicy(registry.Effective(tenantID, keyID))
			}))
		}
		c.Next()
	}
}

// loopGuardPolicy turns what the settings resolve to into the policy the
// handlers apply, or nil when nothing is on.
func loopGuardPolicy(effective loopguard.Effective) *loopdetect.Policy {
	if !effective.Any() {
		return nil
	}
	return &loopdetect.Policy{
		Reply: effective.Reply,
		Tool:  effective.Tool,
		Observer: executor.LoopGuardObserver{
			ReplySource: string(effective.ReplySource),
			ToolSource:  string(effective.ToolSource),
		},
	}
}

// reloadLoopGuardSettings drops the cached loop protection settings of the
// tenants another node changed them for; the next request of each reads the new
// ones. An event that names no tenant (a collection-level change, a resync)
// drops every tenant's, since then any of them may be stale.
func reloadLoopGuardSettings(events []cluster.ConfigEvent) {
	registry := loopguard.Default()
	if len(events) == 0 {
		registry.InvalidateAll()
		return
	}
	for _, event := range events {
		if strings.TrimSpace(event.TenantID) == "" {
			registry.InvalidateAll()
			return
		}
	}
	for _, tenant := range tenantsOf(events) {
		registry.Invalidate(tenant)
	}
}
