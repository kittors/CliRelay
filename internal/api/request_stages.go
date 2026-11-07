package api

import (
	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/api/middleware"
	internalrouting "github.com/router-for-me/CLIProxyAPI/v6/internal/routing"
)

// Stage names. The routing simulator reports them and the panel keys its
// wording on them: add new ones, never rename.
const (
	stageAuth            = "auth"
	stageRoute           = "route"
	stageChannelGroup    = "channel-group"
	stageQuota           = "quota"
	stageModelPolicy     = "model-policy"
	stageModelGate       = "model-gate"
	stageCcSwitchMapping = "ccswitch-mapping"
	stageSystemPrompt    = "system-prompt"
)

// requestStage is one middleware in front of the inference handlers.
type requestStage struct {
	name    string
	handler gin.HandlerFunc
	// writesState marks a stage the routing simulator must not run, and
	// reports as skipped: quota admission counts the request against the
	// key's limits and holds a concurrency slot for it.
	writesState bool
}

// requestStages is the middleware every inference route runs, in order. route
// attaches the request's path route: groupRoutingMiddleware where the group is
// a path parameter, rewrittenGroupRoutingMiddleware where NoRoute rewrote a
// nested group path into the request context.
func (s *Server) requestStages(route gin.HandlerFunc) []requestStage {
	return []requestStage{
		{name: stageAuth, handler: AuthMiddleware(s.accessManager)},
		{name: stageRoute, handler: route},
		{name: stageChannelGroup, handler: channelGroupAuthorizationMiddleware()},
		{name: stageQuota, handler: middleware.QuotaMiddleware(), writesState: true},
		{name: stageModelPolicy, handler: middleware.ModelPolicyMiddleware(s.modelPolicyEffectiveModel)},
		{name: stageModelGate, handler: s.modelRestrictionMiddleware()},
	}
}

// v1RequestStages is requestStages plus the stages of the routes under /v1,
// /:group/v1 and /backend-api/codex. The routing simulator runs this list, so
// a stage added here is simulated too.
func (s *Server) v1RequestStages(route gin.HandlerFunc) []requestStage {
	return append(s.requestStages(route),
		requestStage{name: stageCcSwitchMapping, handler: ccSwitchOpenAIModelMappingMiddleware()},
		requestStage{name: stageSystemPrompt, handler: SystemPromptMiddleware()},
	)
}

func useRequestStages(group *gin.RouterGroup, stages []requestStage) {
	for _, stage := range stages {
		group.Use(stage.handler)
	}
}

// resolveRoute resolves a group path for the request's tenant.
func (s *Server) resolveRoute(tenantID, rawGroup string) (*internalrouting.PathRouteContext, bool) {
	return resolvePathRouteContext(s.cfg, s.handlers.AuthManager, tenantID, rawGroup)
}
