package openai

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/sdk/api/handlers"
	"github.com/tidwall/gjson"
)

// AlphaSearch is a separate Codex JSON protocol, not a Responses translation.
func (h *OpenAIResponsesAPIHandler) AlphaSearch(c *gin.Context) {
	body, ok := handlers.ReadJSONRequestBody(c)
	if !ok {
		return
	}
	model := gjson.GetBytes(body, "model")
	if !gjson.ParseBytes(body).IsObject() || model.Type != gjson.String || strings.TrimSpace(model.String()) == "" || gjson.GetBytes(body, "stream").Type == gjson.True {
		c.JSON(http.StatusBadRequest, handlers.ErrorResponse{Error: handlers.ErrorDetail{Message: "Alpha Search requires a JSON object with a non-empty model and does not support streaming", Type: "invalid_request_error"}})
		return
	}
	body, _, _ = rewriteCcSwitchOpenAIRequestModel(body, c)
	c.Header("Content-Type", "application/json")
	ctx, cancel := h.GetContextWithCancel(h, c, c.Request.Context())
	resp, headers, errMsg := h.ExecuteWithAuthManager(ctx, h.HandlerType(), gjson.GetBytes(body, "model").String(), body, "alpha/search")
	if errMsg != nil {
		h.WriteErrorResponseAs(c, errMsg, handlers.ErrorFormatOpenAI)
		cancel(errMsg.Error)
		return
	}
	handlers.WriteUpstreamHeaders(c.Writer.Header(), headers)
	_, _ = c.Writer.Write(resp)
	cancel()
}
