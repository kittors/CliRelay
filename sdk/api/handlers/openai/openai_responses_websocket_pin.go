package openai

import (
	"context"
	"errors"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/router-for-me/CLIProxyAPI/v6/sdk/api/handlers"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdkrequestctx "github.com/router-for-me/CLIProxyAPI/v6/sdk/requestctx"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// errResponsesWebsocketPinReleased ends a forwarded turn that failed on the
// pinned credential before anything reached the client, after the pin was
// released: the turn is placed again instead of reporting the failure.
var errResponsesWebsocketPinReleased = errors.New("responses websocket: pinned credential released")

// pinnedAuthForTurn returns the credential the next turn of a Responses
// WebSocket session should stay on.
//
// The first turn pins the session to the credential that served it, so later
// turns keep their upstream state. The pin gives way when that credential can
// no longer serve the session: a client may switch to a model the credential
// does not serve (a Claude account cannot answer a Gemini model), and the
// account may run out of quota or be rejected. Keeping the pin then leaves
// selection with no candidate but the pinned one, and every later turn fails
// (auth_not_found, or a 429 model_cooldown naming every credential although
// only the pinned one is cooling down) until the client reconnects, while the
// rest of the pool could serve it. The turn then selects, and pins, afresh.
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
	if h.releasePinnedAuth(c, pinnedAuthID, modelName, sessionID, "") {
		return ""
	}
	return pinnedAuthID
}

// releasePinnedAuth reports whether the session should give up pinnedAuthID
// before serving modelName, and logs why. when says at which point of the turn.
func (h *OpenAIResponsesAPIHandler) releasePinnedAuth(c *gin.Context, pinnedAuthID, modelName, sessionID, when string) bool {
	if pinnedAuthID == "" || h == nil || h.BaseAPIHandler == nil || c == nil || c.Request == nil {
		return false
	}
	ctx := context.WithValue(c.Request.Context(), sdkrequestctx.ContextKeyGin, c)
	reason := h.PinnedAuthReleaseReason(ctx, pinnedAuthID, modelName)
	if reason == "" {
		return false
	}
	log.Infof("responses websocket: released pinned auth id=%s auth=%s model=%s%s: %s", sessionID, pinnedAuthID, modelName, when, reason)
	return true
}

// runResponsesWebsocketTurn sends one turn upstream and forwards the reply.
//
// The turn runs on the pinned credential when there is one, and pins the
// credential selection places it on otherwise. When the pinned credential
// fails the turn before anything reached the client and is then out of service
// (see pinnedAuthForTurn), the pin is released and the turn placed afresh,
// once, as an HTTP request moves to another account. Reporting the failure
// instead would tell the client its quota is exhausted while the rest of the
// pool can answer, and Codex stops the turn on that.
//
// A turn that continues a response only the released credential's upstream
// holds (previous_response_id passed on to an upstream that keeps the context)
// cannot run anywhere else. The client is told that response is unknown, which
// makes Codex send the turn again in full, and that request is placed afresh.
func (h *OpenAIResponsesAPIHandler) runResponsesWebsocketTurn(
	c *gin.Context,
	conn *websocket.Conn,
	pinnedAuthID *string,
	sessionID string,
	modelName string,
	requestJSON []byte,
	wsBodyLog *strings.Builder,
) ([]byte, string, error) {
	for placedAfresh := false; ; placedAfresh = true {
		turnPin := *pinnedAuthID
		cliCtx, cliCancel := h.GetContextWithCancel(h, c, c.Request.Context())
		cliCtx = cliproxyexecutor.WithDownstreamWebsocket(cliCtx)
		cliCtx = handlers.WithExecutionSessionID(cliCtx, sessionID)
		var releaseAfterFailure func() bool
		if turnPin != "" {
			cliCtx = handlers.WithPinnedAuthID(cliCtx, turnPin)
			if !placedAfresh {
				releaseAfterFailure = func() bool {
					return h.releasePinnedAuth(c, turnPin, modelName, sessionID, " after a failed turn")
				}
			}
		} else {
			cliCtx = handlers.WithSelectedAuthIDCallback(cliCtx, func(authID string) {
				*pinnedAuthID = strings.TrimSpace(authID)
			})
		}
		dataChan, _, errChan := h.ExecuteStreamWithAuthManager(cliCtx, h.HandlerType(), modelName, requestJSON, "")
		output, responseID, errForward := h.forwardResponsesWebsocket(c, conn, cliCancel, dataChan, errChan, wsBodyLog, sessionID, releaseAfterFailure)
		if !errors.Is(errForward, errResponsesWebsocketPinReleased) {
			return output, responseID, errForward
		}
		*pinnedAuthID = ""
		if gjson.GetBytes(requestJSON, "previous_response_id").Exists() {
			markAPIResponseTimestamp(c)
			notFound, errWrite := writeResponsesWebsocketPreviousResponseNotFound(conn, requestJSON)
			appendWebsocketEvent(wsBodyLog, "response", notFound)
			return []byte("[]"), "", errWrite
		}
	}
}
