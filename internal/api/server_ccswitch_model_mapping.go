package api

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/api/bodyutil"
	internalrouting "github.com/router-for-me/CLIProxyAPI/v6/internal/routing"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/usage"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const ccSwitchOpenAIModelMappingContextKey = "cliproxy.ccswitch_openai_model_mapping"

func ccSwitchOpenAIModelMappingMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		mapping := ccSwitchOpenAIModelMapping(pathRouteContextFromGin(c))
		if len(mapping) == 0 {
			c.Next()
			return
		}
		c.Set(ccSwitchOpenAIModelMappingContextKey, mapping)
		if c.Request.Method != http.MethodPost {
			c.Next()
			return
		}

		bodyBytes, err := bodyutil.ReadRequestBody(c, bodyutil.ModelRequestBodyLimit())
		if err != nil {
			if bodyutil.IsTooLarge(err) {
				c.AbortWithStatusJSON(http.StatusRequestEntityTooLarge, gin.H{"error": "request body too large"})
				return
			}
			if bodyutil.IsStorageUnavailable(err) {
				c.AbortWithStatusJSON(http.StatusInsufficientStorage, gin.H{"error": "request body temporary storage unavailable"})
				return
			}
			c.Next()
			return
		}

		rewritten, _, mapped := rewriteCcSwitchOpenAIModel(bodyBytes, mapping)
		if mapped {
			bodyutil.SetRequestBody(c, rewritten)
		} else {
			bodyutil.SetRequestBody(c, bodyBytes)
		}
		c.Next()
	}
}

// ccSwitchOpenAIModelMapping returns the request-model → target-model rewrite
// for presets whose app speaks the OpenAI protocol (Codex, OpenCode, OpenClaw,
// Hermes). The middleware is mounted on /v1, /:group/v1 and
// /backend-api/codex, which covers /chat/completions, /completions,
// /responses (HTTP and WebSocket), /responses/compact and /alpha/search.
func ccSwitchOpenAIModelMapping(route *internalrouting.PathRouteContext) map[string]string {
	if route == nil || route.CcSwitch == nil || !usage.CcSwitchClientUsesOpenAIProtocol(route.CcSwitch.ClientType) {
		return nil
	}
	mapping := make(map[string]string)
	for _, item := range route.CcSwitch.ModelMappings {
		requestModel := strings.TrimSpace(item.RequestModel)
		targetModel := strings.TrimSpace(item.TargetModel)
		if requestModel != "" && targetModel != "" {
			mapping[requestModel] = targetModel
		}
	}
	return mapping
}

func rewriteCcSwitchOpenAIModel(rawJSON []byte, mapping map[string]string) ([]byte, string, bool) {
	modelName := strings.TrimSpace(gjson.GetBytes(rawJSON, "model").String())
	if modelName == "" || len(mapping) == 0 {
		return rawJSON, modelName, false
	}
	for requestModel, targetModel := range mapping {
		if !strings.EqualFold(strings.TrimSpace(requestModel), modelName) {
			continue
		}
		targetModel = strings.TrimSpace(targetModel)
		if targetModel == "" {
			return rawJSON, modelName, false
		}
		rewritten, err := sjson.SetBytes(rawJSON, "model", targetModel)
		if err != nil {
			return rawJSON, modelName, false
		}
		return rewritten, targetModel, true
	}
	return rawJSON, modelName, false
}
