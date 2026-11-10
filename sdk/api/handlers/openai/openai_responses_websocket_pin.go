package openai

import (
	"context"
	"strings"

	"github.com/gin-gonic/gin"
	sdkrequestctx "github.com/router-for-me/CLIProxyAPI/v6/sdk/requestctx"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// pinnedAuthForTurn returns the credential the next turn of a Responses
// WebSocket session should stay on.
//
// The first turn pins the session to the credential that served it, so later
// turns keep their upstream state. A client may switch models in the middle of
// a session, though, and the pinned credential may be unable to serve the new
// one: a Claude account cannot answer a Gemini model. Keeping the pin then
// leaves selection with no candidate and the turn fails with auth_not_found.
// In that case the pin is released and the turn selects, and pins, afresh.
//
// The decision is made before the request is normalized, because whether
// incremental input is allowed depends on the pinned credential.
func (h *OpenAIResponsesAPIHandler) pinnedAuthForTurn(c *gin.Context, pinnedAuthID string, payload, lastRequest []byte, sessionID string) string {
	if pinnedAuthID == "" || h == nil || h.BaseAPIHandler == nil || c == nil || c.Request == nil {
		return pinnedAuthID
	}
	modelName := strings.TrimSpace(gjson.GetBytes(payload, "model").String())
	if modelName == "" {
		modelName = strings.TrimSpace(gjson.GetBytes(lastRequest, "model").String())
	}
	if modelName == "" {
		return pinnedAuthID
	}
	modelOnly, _ := sjson.SetBytes([]byte(`{}`), "model", modelName)
	if _, mapped, _ := rewriteCcSwitchOpenAIRequestModel(modelOnly, c); mapped != "" {
		modelName = mapped
	}
	ctx := context.WithValue(c.Request.Context(), sdkrequestctx.ContextKeyGin, c)
	if h.PinnedAuthServesModel(ctx, pinnedAuthID, modelName) {
		return pinnedAuthID
	}
	log.Infof("responses websocket: released pinned auth id=%s auth=%s model=%s: credential cannot serve the requested model", sessionID, pinnedAuthID, modelName)
	return ""
}
