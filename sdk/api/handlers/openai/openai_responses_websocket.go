package openai

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/interfaces"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/util"
	"github.com/router-for-me/CLIProxyAPI/v6/sdk/api/handlers"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/routetrace"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const (
	wsRequestTypeCreate   = "response.create"
	wsRequestTypeAppend   = "response.append"
	wsEventTypeError      = "error"
	wsEventTypeCompleted  = "response.completed"
	wsEventTypeIncomplete = "response.incomplete"
	wsEventTypeDone       = "response.done"
	wsDoneMarker          = "[DONE]"
	wsTurnStateHeader     = "x-codex-turn-state"
	wsRequestBodyKey      = "REQUEST_BODY_OVERRIDE"
	wsPayloadLogMaxSize   = 2048
)

var responsesWebsocketUpgrader = websocket.Upgrader{
	ReadBufferSize:  4096,
	WriteBufferSize: 4096,
	CheckOrigin:     util.WebsocketOriginAllowed,
}

// ResponsesWebsocket handles websocket requests for /v1/responses.
// It accepts `response.create` and `response.append` requests and streams
// response events back as JSON websocket text messages.
func (h *OpenAIResponsesAPIHandler) ResponsesWebsocket(c *gin.Context) {
	settings := h.websocketSettings()
	if !settings.enabled {
		// Codex takes 426 as "use HTTP" and switches without retrying
		// (codex-rs core/src/client.rs, try_switch_fallback_transport).
		status, body := handlers.FormatErrorBody(handlers.ErrorFormatOpenAI, http.StatusUpgradeRequired, "the Responses WebSocket is turned off on this server; use HTTP")
		c.Data(status, "application/json", body)
		return
	}
	upgrader := responsesWebsocketUpgrader
	// Codex offers permessage-deflate on every connection; gorilla accepts it
	// without context takeover.
	upgrader.EnableCompression = settings.compression
	conn, err := upgrader.Upgrade(c.Writer, c.Request, websocketUpgradeHeaders(c.Request))
	if err != nil {
		return
	}
	// Gorilla's read limit caps the wire payload, before permessage-deflate is
	// decoded. Keep that guard too; readResponsesWebsocketMessage bounds the
	// decompressed input before it can enter the log, transcript, or executor.
	conn.SetReadLimit(settings.maxMessage)
	passthroughSessionID := uuid.NewString()
	clientRemoteAddr := ""
	if c != nil && c.Request != nil {
		clientRemoteAddr = strings.TrimSpace(c.Request.RemoteAddr)
	}
	log.Infof("responses websocket: client connected id=%s remote=%s", passthroughSessionID, clientRemoteAddr)
	var wsTerminateErr error
	var wsBodyLog strings.Builder
	defer func() {
		if wsTerminateErr != nil {
			// log.Infof("responses websocket: session closing id=%s reason=%v", passthroughSessionID, wsTerminateErr)
		} else {
			log.Infof("responses websocket: session closing id=%s", passthroughSessionID)
		}
		if h != nil && h.AuthManager != nil {
			h.AuthManager.CloseExecutionSession(passthroughSessionID)
			log.Infof("responses websocket: upstream execution session closed id=%s", passthroughSessionID)
		}
		setWebsocketRequestBody(c, wsBodyLog.String())
		if errClose := conn.Close(); errClose != nil {
			log.Warnf("responses websocket: close connection error: %v", errClose)
		}
	}()

	transcript := newResponsesWebsocketTranscript()
	// transcriptTooLarge closes the connection once a turn has been answered if
	// the transcript it keeps has outgrown the limit; the client's next request
	// opens a new connection with the full input.
	transcriptTooLarge := func() bool {
		if transcript.sizeBytes() <= settings.maxTranscript {
			return false
		}
		log.Infof("responses websocket: transcript of %d bytes is over the %d byte limit, closing id=%s", transcript.sizeBytes(), settings.maxTranscript, passthroughSessionID)
		closeResponsesWebsocket(conn, "transcript limit reached")
		return true
	}
	pinnedAuthID := ""
	// Published by the quota middleware on the upgrade request when the key has
	// limits; it stays the same for the life of the connection.
	quotaGate := handlers.QuotaGateFromGin(c)
	// One connection serves many requests: between them it is idle, not in
	// flight, and each one gets a route trace of its own.
	trace := routetrace.FromContext(c.Request.Context())

	for {
		trace.Idle()
		if settings.idleTimeout > 0 {
			_ = conn.SetReadDeadline(time.Now().Add(settings.idleTimeout))
		}
		msgType, payload, errReadMessage := readResponsesWebsocketMessage(conn, settings.maxMessage)
		if errReadMessage != nil {
			wsTerminateErr = errReadMessage
			appendWebsocketEvent(&wsBodyLog, "disconnect", []byte(errReadMessage.Error()))
			switch {
			case isWebsocketReadTimeout(errReadMessage):
				// Idle between turns: free the transcript. The client reconnects on
				// its next request.
				log.Infof("responses websocket: closing idle connection id=%s idle=%s", passthroughSessionID, settings.idleTimeout)
				closeResponsesWebsocket(conn, "idle timeout")
			case errors.Is(errReadMessage, websocket.ErrReadLimit):
				log.Warnf("responses websocket: message over %d bytes, closing id=%s", settings.maxMessage, passthroughSessionID)
			case websocket.IsCloseError(errReadMessage, websocket.CloseNormalClosure, websocket.CloseGoingAway, websocket.CloseNoStatusReceived):
				log.Infof("responses websocket: client disconnected id=%s error=%v", passthroughSessionID, errReadMessage)
			default:
				log.Debugf("responses websocket: read message failed id=%s error=%v", passthroughSessionID, errReadMessage)
			}
			return
		}
		_ = conn.SetReadDeadline(time.Time{})
		if msgType != websocket.TextMessage && msgType != websocket.BinaryMessage {
			continue
		}
		trace.Restart()
		// log.Infof(
		// 	"responses websocket: downstream_in id=%s type=%d event=%s payload=%s",
		// 	passthroughSessionID,
		// 	msgType,
		// 	websocketPayloadEventType(payload),
		// 	websocketPayloadPreview(payload),
		// )
		appendWebsocketEvent(&wsBodyLog, "request", payload)

		pinnedAuthID = h.pinnedAuthForTurn(c, pinnedAuthID, payload, lastRequest, passthroughSessionID)
		allowIncrementalInputWithPreviousResponseID := websocketUpstreamSupportsIncrementalInput(nil, nil)
		if pinnedAuthID != "" && h != nil && h.AuthManager != nil {
			if pinnedAuth, ok := h.AuthManager.GetByID(pinnedAuthID); ok && pinnedAuth != nil {
				allowIncrementalInputWithPreviousResponseID = websocketUpstreamSupportsIncrementalInput(pinnedAuth.Attributes, pinnedAuth.Metadata)
			}
		}

		var requestJSON []byte
		var updatedLastRequest []byte
		var errMsg *interfaces.ErrorMessage
		switch transcript.classify(payload, allowIncrementalInputWithPreviousResponseID) {
		case wsTurnNewRoot:
			requestJSON, updatedLastRequest, errMsg = normalizeResponseCreateRequest(payload)
		case wsTurnStaleRoot:
			log.Infof("responses websocket: new context window, previous_response_id dropped id=%s", passthroughSessionID)
			requestJSON, updatedLastRequest, errMsg = normalizeResponseCreateRequest(dropPreviousResponseID(payload))
		case wsTurnUnknownPrevious:
			// The increment names a response other than the one this connection
			// answered last, so it cannot be expanded. Codex sends it again in full.
			markAPIResponseTimestamp(c)
			notFound, errWrite := writeResponsesWebsocketPreviousResponseNotFound(conn, payload)
			appendWebsocketEvent(&wsBodyLog, "response", notFound)
			log.Infof("responses websocket: increment names an unknown response id=%s", passthroughSessionID)
			if errWrite != nil {
				wsTerminateErr = errWrite
				return
			}
			continue
		default:
			requestJSON, updatedLastRequest, errMsg = normalizeResponsesWebsocketRequestWithMode(
				payload,
				transcript.lastRequest,
				transcript.lastResponseOutput,
				allowIncrementalInputWithPreviousResponseID,
			)
		}
		if errMsg != nil {
			h.LoggingAPIResponseError(context.WithValue(c.Request.Context(), util.ContextKeyGin, c), errMsg)
			markAPIResponseTimestamp(c)
			errorPayload, errWrite := writeResponsesWebsocketError(conn, errMsg)
			appendWebsocketEvent(&wsBodyLog, "response", errorPayload)
			log.Infof(
				"responses websocket: downstream_out id=%s type=%d event=%s payload=%s",
				passthroughSessionID,
				websocket.TextMessage,
				websocketPayloadEventType(errorPayload),
				websocketPayloadPreview(errorPayload),
			)
			if errWrite != nil {
				log.Warnf(
					"responses websocket: downstream_out write failed id=%s event=%s error=%v",
					passthroughSessionID,
					websocketPayloadEventType(errorPayload),
					errWrite,
				)
				return
			}
			continue
		}
		transcript.lastRequest = updatedLastRequest
		transcript.observeWindow(payload)
		requestJSON, _, _ = rewriteCcSwitchOpenAIRequestModel(requestJSON, c)

		if prewarmPayloads, ok := responsesWebsocketPrewarmPayloads(requestJSON); ok {
			for _, prewarmPayload := range prewarmPayloads {
				markAPIResponseTimestamp(c)
				appendWebsocketEvent(&wsBodyLog, "response", prewarmPayload)
				if errWrite := conn.WriteMessage(websocket.TextMessage, prewarmPayload); errWrite != nil {
					log.Warnf(
						"responses websocket: prewarm write failed id=%s event=%s error=%v",
						passthroughSessionID,
						websocketPayloadEventType(prewarmPayload),
						errWrite,
					)
					wsTerminateErr = errWrite
					return
				}
			}
			transcript.lastResponseOutput = []byte("[]")
			transcript.lastResponseID = gjson.GetBytes(prewarmPayloads[len(prewarmPayloads)-1], "response.id").String()
			if transcriptTooLarge() {
				return
			}
			continue
		}

		modelName := gjson.GetBytes(requestJSON, "model").String()
		// A turn reaches the executor and spends upstream quota just like a POST,
		// so it passes the same quota checks, once per turn. They run before the
		// model gate, as the quota middleware runs before model restriction on
		// POST. An admitted turn holds its concurrency slot until releaseQuota.
		releaseQuota := func() {}
		if quotaGate != nil {
			release, rejection := quotaGate()
			if rejection != nil {
				refusal := &interfaces.ErrorMessage{StatusCode: rejection.StatusCode, Error: rejection, Addon: rejection.Headers}
				if !h.refuseResponsesWebsocketTurn(c, conn, refusal, &wsBodyLog, "turn refused by quota", passthroughSessionID) {
					return
				}
				continue
			}
			releaseQuota = release
		}
		// The account's model routes and per-model quotas, as the model policy
		// middleware applies them to a POST. They run before the model gate so the
		// gate judges the model the turn will actually use, which is also the model
		// the turn is billed under.
		if policyGate := handlers.ModelPolicyGateFromGin(c); policyGate != nil {
			finalModel, rejection := policyGate(modelName)
			if rejection != nil {
				releaseQuota()
				refusal := &interfaces.ErrorMessage{StatusCode: rejection.StatusCode, Error: rejection, Addon: rejection.Headers}
				if !h.refuseResponsesWebsocketTurn(c, conn, refusal, &wsBodyLog, "turn refused by model quota", passthroughSessionID) {
					return
				}
				continue
			}
			if finalModel != "" && finalModel != modelName {
				if rewritten, errSet := sjson.SetBytes(requestJSON, "model", finalModel); errSet == nil {
					requestJSON = rewritten
					modelName = finalModel
				}
			}
		}
		// The HTTP middleware could not see this model: it ran on the upgrade
		// request, which has no body. Apply its verdict here, per turn, so a
		// disabled or not-allowed model is refused on WebSocket exactly as on POST.
		if gate := handlers.ModelGateFromGin(c); gate != nil {
			if verdict := gate(modelName); verdict != nil {
				releaseQuota()
				if !h.refuseResponsesWebsocketTurn(c, conn, verdict, &wsBodyLog, fmt.Sprintf("model %q refused", modelName), passthroughSessionID) {
					return
				}
				continue
			}
		}
		completedOutput, responseID, errForward := func() ([]byte, string, error) {
			// Deferred so the slot comes back however the turn ends: completed,
			// failed upstream, client gone, or a panic.
			defer releaseQuota()
			cliCtx, cliCancel := h.GetContextWithCancel(h, c, c.Request.Context())
			cliCtx = cliproxyexecutor.WithDownstreamWebsocket(cliCtx)
			cliCtx = handlers.WithExecutionSessionID(cliCtx, passthroughSessionID)
			if pinnedAuthID != "" {
				cliCtx = handlers.WithPinnedAuthID(cliCtx, pinnedAuthID)
			} else {
				cliCtx = handlers.WithSelectedAuthIDCallback(cliCtx, func(authID string) {
					pinnedAuthID = strings.TrimSpace(authID)
				})
			}
			dataChan, _, errChan := h.ExecuteStreamWithAuthManager(cliCtx, h.HandlerType(), modelName, requestJSON, "")
			return h.forwardResponsesWebsocket(c, conn, cliCancel, dataChan, errChan, &wsBodyLog, passthroughSessionID)
		}()
		if errForward != nil {
			wsTerminateErr = errForward
			appendWebsocketEvent(&wsBodyLog, "disconnect", []byte(errForward.Error()))
			log.Warnf("responses websocket: forward failed id=%s error=%v", passthroughSessionID, errForward)
			return
		}
		transcript.lastResponseOutput = completedOutput
		transcript.lastResponseID = responseID
		if transcriptTooLarge() {
			return
		}
	}
}

// refuseResponsesWebsocketTurn answers a turn refused before it reached the
// executor with an error event. The connection stays open for the next turn,
// just as only that one request fails on POST. It returns false when the event
// could not be written and the connection has to close.
func (h *OpenAIResponsesAPIHandler) refuseResponsesWebsocketTurn(c *gin.Context, conn *websocket.Conn, refusal *interfaces.ErrorMessage, wsBodyLog *strings.Builder, reason, sessionID string) bool {
	h.LoggingAPIResponseError(context.WithValue(c.Request.Context(), util.ContextKeyGin, c), refusal)
	markAPIResponseTimestamp(c)
	errorPayload, errWrite := writeResponsesWebsocketError(conn, refusal)
	appendWebsocketEvent(wsBodyLog, "response", errorPayload)
	log.Infof("responses websocket: %s id=%s status=%d", reason, sessionID, refusal.StatusCode)
	if errWrite != nil {
		log.Warnf("responses websocket: refusal write failed id=%s error=%v", sessionID, errWrite)
		return false
	}
	return true
}

func websocketUpgradeHeaders(req *http.Request) http.Header {
	headers := http.Header{}
	if req == nil {
		return headers
	}

	// Keep the same sticky turn-state across reconnects when provided by the client.
	turnState := strings.TrimSpace(req.Header.Get(wsTurnStateHeader))
	if turnState != "" {
		headers.Set(wsTurnStateHeader, turnState)
	}
	return headers
}

func normalizeResponsesWebsocketRequest(rawJSON []byte, lastRequest []byte, lastResponseOutput []byte) ([]byte, []byte, *interfaces.ErrorMessage) {
	return normalizeResponsesWebsocketRequestWithMode(rawJSON, lastRequest, lastResponseOutput, true)
}

func normalizeResponsesWebsocketRequestWithMode(rawJSON []byte, lastRequest []byte, lastResponseOutput []byte, allowIncrementalInputWithPreviousResponseID bool) ([]byte, []byte, *interfaces.ErrorMessage) {
	requestType := strings.TrimSpace(gjson.GetBytes(rawJSON, "type").String())
	switch requestType {
	case wsRequestTypeCreate:
		// log.Infof("responses websocket: response.create request")
		if len(lastRequest) == 0 {
			return normalizeResponseCreateRequest(rawJSON)
		}
		return normalizeResponseSubsequentRequest(rawJSON, lastRequest, lastResponseOutput, allowIncrementalInputWithPreviousResponseID)
	case wsRequestTypeAppend:
		// log.Infof("responses websocket: response.append request")
		return normalizeResponseSubsequentRequest(rawJSON, lastRequest, lastResponseOutput, allowIncrementalInputWithPreviousResponseID)
	default:
		return nil, lastRequest, &interfaces.ErrorMessage{
			StatusCode: http.StatusBadRequest,
			Error:      fmt.Errorf("unsupported websocket request type: %s", requestType),
		}
	}
}

func normalizeResponseCreateRequest(rawJSON []byte) ([]byte, []byte, *interfaces.ErrorMessage) {
	normalized, errDelete := sjson.DeleteBytes(rawJSON, "type")
	if errDelete != nil {
		normalized = bytes.Clone(rawJSON)
	}
	normalized, _ = sjson.SetBytes(normalized, "stream", true)
	if !gjson.GetBytes(normalized, "input").Exists() {
		normalized, _ = sjson.SetRawBytes(normalized, "input", []byte("[]"))
	}

	modelName := strings.TrimSpace(gjson.GetBytes(normalized, "model").String())
	if modelName == "" {
		return nil, nil, &interfaces.ErrorMessage{
			StatusCode: http.StatusBadRequest,
			Error:      fmt.Errorf("missing model in response.create request"),
		}
	}
	return normalized, bytes.Clone(normalized), nil
}

func normalizeResponseSubsequentRequest(rawJSON []byte, lastRequest []byte, lastResponseOutput []byte, allowIncrementalInputWithPreviousResponseID bool) ([]byte, []byte, *interfaces.ErrorMessage) {
	if len(lastRequest) == 0 {
		return nil, lastRequest, &interfaces.ErrorMessage{
			StatusCode: http.StatusBadRequest,
			Error:      fmt.Errorf("websocket request received before response.create"),
		}
	}

	nextInput := gjson.GetBytes(rawJSON, "input")
	if !nextInput.Exists() || !nextInput.IsArray() {
		return nil, lastRequest, &interfaces.ErrorMessage{
			StatusCode: http.StatusBadRequest,
			Error:      fmt.Errorf("websocket request requires array field: input"),
		}
	}

	// Websocket v2 mode uses response.create with previous_response_id + incremental input.
	// Do not expand it into a full input transcript; upstream expects the incremental payload.
	if allowIncrementalInputWithPreviousResponseID {
		if prev := strings.TrimSpace(gjson.GetBytes(rawJSON, "previous_response_id").String()); prev != "" {
			normalized, errDelete := sjson.DeleteBytes(rawJSON, "type")
			if errDelete != nil {
				normalized = bytes.Clone(rawJSON)
			}
			if !gjson.GetBytes(normalized, "model").Exists() {
				modelName := strings.TrimSpace(gjson.GetBytes(lastRequest, "model").String())
				if modelName != "" {
					normalized, _ = sjson.SetBytes(normalized, "model", modelName)
				}
			}
			if !gjson.GetBytes(normalized, "instructions").Exists() {
				instructions := gjson.GetBytes(lastRequest, "instructions")
				if instructions.Exists() {
					normalized, _ = sjson.SetRawBytes(normalized, "instructions", []byte(instructions.Raw))
				}
			}
			normalized, _ = sjson.SetBytes(normalized, "stream", true)
			return normalized, bytes.Clone(normalized), nil
		}
	}

	existingInput := gjson.GetBytes(lastRequest, "input")
	mergedInput, errMerge := mergeJSONArrayRaw(existingInput.Raw, normalizeJSONArrayRaw(lastResponseOutput))
	if errMerge != nil {
		return nil, lastRequest, &interfaces.ErrorMessage{
			StatusCode: http.StatusBadRequest,
			Error:      fmt.Errorf("invalid previous response output: %w", errMerge),
		}
	}

	mergedInput, errMerge = mergeJSONArrayRaw(mergedInput, nextInput.Raw)
	if errMerge != nil {
		return nil, lastRequest, &interfaces.ErrorMessage{
			StatusCode: http.StatusBadRequest,
			Error:      fmt.Errorf("invalid request input: %w", errMerge),
		}
	}

	normalized, errDelete := sjson.DeleteBytes(rawJSON, "type")
	if errDelete != nil {
		normalized = bytes.Clone(rawJSON)
	}
	normalized, _ = sjson.DeleteBytes(normalized, "previous_response_id")
	var errSet error
	normalized, errSet = sjson.SetRawBytes(normalized, "input", []byte(mergedInput))
	if errSet != nil {
		return nil, lastRequest, &interfaces.ErrorMessage{
			StatusCode: http.StatusBadRequest,
			Error:      fmt.Errorf("failed to merge websocket input: %w", errSet),
		}
	}
	if !gjson.GetBytes(normalized, "model").Exists() {
		modelName := strings.TrimSpace(gjson.GetBytes(lastRequest, "model").String())
		if modelName != "" {
			normalized, _ = sjson.SetBytes(normalized, "model", modelName)
		}
	}
	if !gjson.GetBytes(normalized, "instructions").Exists() {
		instructions := gjson.GetBytes(lastRequest, "instructions")
		if instructions.Exists() {
			normalized, _ = sjson.SetRawBytes(normalized, "instructions", []byte(instructions.Raw))
		}
	}
	normalized, _ = sjson.SetBytes(normalized, "stream", true)
	return normalized, bytes.Clone(normalized), nil
}

func (h *OpenAIResponsesAPIHandler) forwardResponsesWebsocket(
	c *gin.Context,
	conn *websocket.Conn,
	cancel handlers.APIHandlerCancelFunc,
	data <-chan []byte,
	errs <-chan *interfaces.ErrorMessage,
	wsBodyLog *strings.Builder,
	sessionID string,
) ([]byte, string, error) {
	completed := false
	completedOutput := []byte("[]")
	responseID := ""
	var collected websocketOutputCollector

	for {
		select {
		case <-c.Request.Context().Done():
			cancel(c.Request.Context().Err())
			return completedOutput, responseID, c.Request.Context().Err()
		case errMsg, ok := <-errs:
			if !ok {
				errs = nil
				continue
			}
			if errMsg != nil {
				h.LoggingAPIResponseError(context.WithValue(c.Request.Context(), util.ContextKeyGin, c), errMsg)
				markAPIResponseTimestamp(c)
				errorPayload, errWrite := writeResponsesWebsocketError(conn, errMsg)
				appendWebsocketEvent(wsBodyLog, "response", errorPayload)
				log.Infof(
					"responses websocket: downstream_out id=%s type=%d event=%s payload=%s",
					sessionID,
					websocket.TextMessage,
					websocketPayloadEventType(errorPayload),
					websocketPayloadPreview(errorPayload),
				)
				if errWrite != nil {
					// log.Warnf(
					// 	"responses websocket: downstream_out write failed id=%s event=%s error=%v",
					// 	sessionID,
					// 	websocketPayloadEventType(errorPayload),
					// 	errWrite,
					// )
					cancel(errMsg.Error)
					return completedOutput, responseID, errWrite
				}
			}
			if errMsg != nil {
				cancel(errMsg.Error)
			} else {
				cancel(nil)
			}
			return completedOutput, responseID, nil
		case chunk, ok := <-data:
			if !ok {
				if !completed {
					errMsg := &interfaces.ErrorMessage{
						StatusCode: http.StatusRequestTimeout,
						Error:      fmt.Errorf("stream closed before response.completed"),
					}
					// A stream that ends with an error closes its data channel in the
					// same moment, and this select picks among ready cases at random:
					// the error, which says why the reply stopped (a loop cut by the
					// loop protection, an upstream failure), must win over the generic
					// one, as it does in ForwardStream for the HTTP entries.
					select {
					case pending, ok := <-errs:
						if ok && pending != nil {
							errMsg = pending
						}
					default:
					}
					h.LoggingAPIResponseError(context.WithValue(c.Request.Context(), util.ContextKeyGin, c), errMsg)
					markAPIResponseTimestamp(c)
					errorPayload, errWrite := writeResponsesWebsocketError(conn, errMsg)
					appendWebsocketEvent(wsBodyLog, "response", errorPayload)
					log.Infof(
						"responses websocket: downstream_out id=%s type=%d event=%s payload=%s",
						sessionID,
						websocket.TextMessage,
						websocketPayloadEventType(errorPayload),
						websocketPayloadPreview(errorPayload),
					)
					if errWrite != nil {
						log.Warnf(
							"responses websocket: downstream_out write failed id=%s event=%s error=%v",
							sessionID,
							websocketPayloadEventType(errorPayload),
							errWrite,
						)
						cancel(errMsg.Error)
						return completedOutput, responseID, errWrite
					}
					cancel(errMsg.Error)
					return completedOutput, responseID, nil
				}
				cancel(nil)
				return completedOutput, responseID, nil
			}

			payloads := websocketJSONPayloadsFromChunk(chunk)
			for i := range payloads {
				eventType := gjson.GetBytes(payloads[i], "type").String()
				collected.observe(payloads[i])
				// The terminal event goes to the client under its own name: Codex 0.110
				// and later end a turn only on response.completed or response.incomplete
				// and wait out the idle timeout on anything else.
				if websocketResponseFinished(eventType) {
					completed = true
					completedOutput = collected.output(payloads[i])
					responseID = strings.TrimSpace(gjson.GetBytes(payloads[i], "response.id").String())
				}
				markAPIResponseTimestamp(c)
				appendWebsocketEvent(wsBodyLog, "response", payloads[i])
				// log.Infof(
				// 	"responses websocket: downstream_out id=%s type=%d event=%s payload=%s",
				// 	sessionID,
				// 	websocket.TextMessage,
				// 	websocketPayloadEventType(payloads[i]),
				// 	websocketPayloadPreview(payloads[i]),
				// )
				if errWrite := conn.WriteMessage(websocket.TextMessage, payloads[i]); errWrite != nil {
					log.Warnf(
						"responses websocket: downstream_out write failed id=%s event=%s error=%v",
						sessionID,
						websocketPayloadEventType(payloads[i]),
						errWrite,
					)
					cancel(errWrite)
					return completedOutput, responseID, errWrite
				}
			}
		}
	}
}

func writeResponsesWebsocketError(conn *websocket.Conn, errMsg *interfaces.ErrorMessage) ([]byte, error) {
	status := http.StatusInternalServerError
	errText := http.StatusText(status)
	if errMsg != nil {
		if errMsg.StatusCode > 0 {
			status = errMsg.StatusCode
			errText = http.StatusText(status)
		}
		if errMsg.Error != nil && strings.TrimSpace(errMsg.Error.Error()) != "" {
			errText = errMsg.Error.Error()
		}
	}

	status, body := handlers.FormatErrorBody(handlers.ErrorFormatOpenAI, status, errText)
	payload := map[string]any{
		"type":   wsEventTypeError,
		"status": status,
	}

	if errMsg != nil && errMsg.Addon != nil {
		headers := map[string]any{}
		for key, values := range errMsg.Addon {
			if len(values) == 0 {
				continue
			}
			headers[key] = values[0]
		}
		if len(headers) > 0 {
			payload["headers"] = headers
		}
	}

	if len(body) > 0 && json.Valid(body) {
		var decoded map[string]any
		if errDecode := json.Unmarshal(body, &decoded); errDecode == nil {
			if inner, ok := decoded["error"]; ok {
				payload["error"] = inner
			} else {
				payload["error"] = decoded
			}
		}
	}

	if _, ok := payload["error"]; !ok {
		payload["error"] = map[string]any{
			"type":    "server_error",
			"message": errText,
		}
	}

	data, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	return data, conn.WriteMessage(websocket.TextMessage, data)
}
