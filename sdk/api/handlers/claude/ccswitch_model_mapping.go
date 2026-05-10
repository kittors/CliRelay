package claude

import (
	"strings"

	"github.com/gin-gonic/gin"
	internalrouting "github.com/router-for-me/CLIProxyAPI/v6/internal/routing"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

func routeContextFromGin(c *gin.Context) *internalrouting.PathRouteContext {
	if c == nil {
		return nil
	}
	if raw, exists := c.Get(internalrouting.GinPathRouteContextKey); exists {
		route, _ := raw.(*internalrouting.PathRouteContext)
		if route != nil {
			return route
		}
	}
	if c.Request != nil {
		return internalrouting.PathRouteContextFromContext(c.Request.Context())
	}
	return nil
}

func rewriteCcSwitchClaudeRequestModel(rawJSON []byte, route *internalrouting.PathRouteContext) ([]byte, string, bool) {
	modelName := strings.TrimSpace(gjson.GetBytes(rawJSON, "model").String())
	if modelName == "" || route == nil || route.CcSwitch == nil {
		return rawJSON, modelName, false
	}
	if !strings.EqualFold(strings.TrimSpace(route.CcSwitch.ClientType), "claude") {
		return rawJSON, modelName, false
	}
	for _, mapping := range route.CcSwitch.ModelMappings {
		requestModel := strings.TrimSpace(mapping.RequestModel)
		targetModel := strings.TrimSpace(mapping.TargetModel)
		if requestModel == "" || targetModel == "" {
			continue
		}
		if !strings.EqualFold(requestModel, modelName) {
			continue
		}
		rewritten, err := sjson.SetBytes(rawJSON, "model", targetModel)
		if err != nil {
			return rawJSON, modelName, false
		}
		return rewritten, targetModel, true
	}
	return rawJSON, modelName, false
}
