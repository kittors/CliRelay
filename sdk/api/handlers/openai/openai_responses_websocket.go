package openai

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	internalconfig "github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/interfaces"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/util"
	"github.com/router-for-me/CLIProxyAPI/v6/sdk/api/handlers"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const (
	wsRequestTypeCreate                          = "response.create"
	wsRequestTypeAppend                          = "response.append"
	wsEventTypeError                             = "error"
	wsEventTypeCompleted                         = "response.completed"
	wsDoneMarker                                 = "[DONE]"
	wsTurnStateHeader                            = "x-codex-turn-state"
	wsRequestBodyKey                             = "REQUEST_BODY_OVERRIDE"
	wsPayloadLogMaxSize                          = 2048
	wsPayloadPreviewDefaultBytes                 = 512
	wsResponseTraceDefaultSlowMS                 = 20000
	responsesWebsocketHeartbeatInterval          = 30 * time.Second
	responsesWebsocketHeartbeatWriteLimit        = 10 * time.Second
	responsesWebsocketWriteTimeout               = 10 * time.Second
	responsesWebsocketClientMessageIdleTimeout   = 60 * time.Second
	responsesWebsocketApplicationKeepAlivePeriod = 30 * time.Second
	responsesWebsocketUpstreamIdleTimeout        = 60 * time.Second
)

type websocketResponseTrace struct {
	Enabled             bool
	SlowThreshold       time.Duration
	LogPayloadPreview   bool
	PayloadPreviewBytes int
	LogHeaders          bool
}

var responsesWebsocketUpgrader = websocket.Upgrader{
	ReadBufferSize:  4096,
	WriteBufferSize: 4096,
	CheckOrigin:     util.WebsocketOriginAllowed,
}

func startResponsesWebsocketHeartbeat(conn *websocket.Conn, sessionID string) func() {
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(responsesWebsocketHeartbeatInterval)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				if err := conn.WriteControl(websocket.PingMessage, []byte("ping"), time.Now().Add(responsesWebsocketHeartbeatWriteLimit)); err != nil {
					log.Debugf("responses websocket: heartbeat failed id=%s error=%v", sessionID, err)
					return
				}
			}
		}
	}()
	return func() {
		select {
		case <-done:
			return
		default:
		}
		close(stop)
		<-done
	}
}

// ResponsesWebsocket handles websocket requests for /v1/responses.
// It accepts `response.create` and `response.append` requests and streams
// response events back as JSON websocket text messages.
func (h *OpenAIResponsesAPIHandler) ResponsesWebsocket(c *gin.Context) {
	conn, err := responsesWebsocketUpgrader.Upgrade(c.Writer, c.Request, websocketUpgradeHeaders(c.Request))
	if err != nil {
		return
	}
	trace := websocketResponseTraceFromConfig(nil)
	if h != nil {
		trace = websocketResponseTraceFromConfig(h.Cfg)
	}
	connectedAt := time.Now()
	passthroughSessionID := uuid.NewString()
	clientRemoteAddr := ""
	if c != nil && c.Request != nil {
		clientRemoteAddr = strings.TrimSpace(c.Request.RemoteAddr)
	}
	if trace.Enabled {
		log.Infof("responses websocket trace: client connected id=%s remote=%s headers=%s", passthroughSessionID, clientRemoteAddr, websocketTraceHeaders(c, trace))
	} else {
		log.Infof("responses websocket: client connected id=%s remote=%s", passthroughSessionID, clientRemoteAddr)
	}
	stopHeartbeat := startResponsesWebsocketHeartbeat(conn, passthroughSessionID)
	defer stopHeartbeat()
	var wsTerminateErr error
	var wsBodyLog strings.Builder
	defer func() {
		if trace.Enabled {
			log.Infof("responses websocket trace: session finished id=%s total_ms=%d error=%v", passthroughSessionID, time.Since(connectedAt).Milliseconds(), wsTerminateErr)
		}
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

	var lastRequest []byte
	lastResponseOutput := []byte("[]")
	pinnedAuthID := ""

	for {
		waitStart := time.Now()
		if errDeadline := conn.SetReadDeadline(time.Now().Add(responsesWebsocketClientMessageIdleTimeout)); errDeadline != nil {
			wsTerminateErr = errDeadline
			log.Warnf("responses websocket: set read deadline failed id=%s error=%v", passthroughSessionID, errDeadline)
			return
		}
		msgType, payload, errReadMessage := conn.ReadMessage()
		if errReadMessage != nil {
			wsTerminateErr = errReadMessage
			appendWebsocketEvent(&wsBodyLog, "disconnect", []byte(errReadMessage.Error()))
			if isResponsesWebsocketReadTimeout(errReadMessage) {
				log.Infof("responses websocket: client message idle timeout id=%s timeout=%s", passthroughSessionID, responsesWebsocketClientMessageIdleTimeout)
				if errClose := closeResponsesWebsocketWithReason(conn, websocket.CloseNormalClosure, "idle timeout"); errClose != nil {
					log.Debugf("responses websocket: idle close failed id=%s error=%v", passthroughSessionID, errClose)
				}
			} else if websocket.IsCloseError(errReadMessage, websocket.CloseNormalClosure, websocket.CloseGoingAway, websocket.CloseNoStatusReceived) {
				log.Infof("responses websocket: client disconnected id=%s error=%v", passthroughSessionID, errReadMessage)
			} else {
				log.Debugf("responses websocket: read message failed id=%s error=%v", passthroughSessionID, errReadMessage)
			}
			return
		}
		if msgType != websocket.TextMessage && msgType != websocket.BinaryMessage {
			continue
		}
		logResponsesWebsocketPayload(trace, "downstream_in", passthroughSessionID, msgType, websocketPayloadEventType(payload), false, 0, payload)
		if trace.Enabled {
			log.Infof("responses websocket trace: client message received id=%s wait_ms=%d bytes=%d", passthroughSessionID, time.Since(waitStart).Milliseconds(), len(payload))
			log.Infof("responses websocket trace: request summary id=%s %s", passthroughSessionID, websocketRequestSummary(payload))
		}
		appendWebsocketEvent(&wsBodyLog, "request", payload)

		allowIncrementalInputWithPreviousResponseID := websocketUpstreamSupportsIncrementalInput(nil, nil)
		if pinnedAuthID != "" && h != nil && h.AuthManager != nil {
			if pinnedAuth, ok := h.AuthManager.GetByID(pinnedAuthID); ok && pinnedAuth != nil {
				allowIncrementalInputWithPreviousResponseID = websocketUpstreamSupportsIncrementalInput(pinnedAuth.Attributes, pinnedAuth.Metadata)
			}
		}

		var requestJSON []byte
		var updatedLastRequest []byte
		var errMsg *interfaces.ErrorMessage
		requestJSON, updatedLastRequest, errMsg = normalizeResponsesWebsocketRequestWithMode(
			payload,
			lastRequest,
			lastResponseOutput,
			allowIncrementalInputWithPreviousResponseID,
		)
		if errMsg != nil {
			h.LoggingAPIResponseError(context.WithValue(c.Request.Context(), util.ContextKeyGin, c), errMsg)
			markAPIResponseTimestamp(c)
			errorPayload, errWrite := writeResponsesWebsocketError(conn, errMsg)
			appendWebsocketEvent(&wsBodyLog, "response", errorPayload)
			logResponsesWebsocketPayload(trace, "downstream_out", passthroughSessionID, websocket.TextMessage, websocketPayloadEventType(errorPayload), false, 0, errorPayload)
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
		lastRequest = updatedLastRequest

		if prewarmPayloads, ok := responsesWebsocketPrewarmPayloads(requestJSON); ok {
			log.Infof("responses websocket: prewarm response handled locally id=%s model=%s", passthroughSessionID, gjson.GetBytes(requestJSON, "model").String())
			for _, prewarmPayload := range prewarmPayloads {
				markAPIResponseTimestamp(c)
				appendWebsocketEvent(&wsBodyLog, "response", prewarmPayload)
				logResponsesWebsocketPayload(trace, "downstream_out", passthroughSessionID, websocket.TextMessage, websocketPayloadEventType(prewarmPayload), websocketPayloadEventType(prewarmPayload) == wsEventTypeCompleted, 0, prewarmPayload)
				if errWrite := writeResponsesWebsocketMessage(conn, websocket.TextMessage, prewarmPayload); errWrite != nil {
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
			lastResponseOutput = []byte("[]")
			log.Infof("responses websocket: prewarm completed id=%s", passthroughSessionID)
			continue
		}

		modelName := gjson.GetBytes(requestJSON, "model").String()
		requestStart := time.Now()
		if trace.Enabled {
			log.Infof("responses websocket trace: upstream execution start id=%s model=%s request_bytes=%d", passthroughSessionID, modelName, len(requestJSON))
		}
		cliCtx, cliCancel := h.GetContextWithCancel(h, c, c.Request.Context())
		cliCtx = cliproxyexecutor.WithDownstreamWebsocket(cliCtx)
		cliCtx = handlers.WithExecutionSessionID(cliCtx, passthroughSessionID)
		if pinnedAuthID != "" {
			cliCtx = handlers.WithPinnedAuthID(cliCtx, pinnedAuthID)
		} else {
			cliCtx = handlers.WithSelectedAuthIDCallback(cliCtx, func(authID string) {
				pinnedAuthID = strings.TrimSpace(authID)
				log.Infof("responses websocket: selected auth id=%s auth=%s model=%s", passthroughSessionID, pinnedAuthID, modelName)
			})
		}
		cliCtx = handlers.WithCooldownWaitDisabled(cliCtx)
		log.Infof("responses websocket: upstream execution start id=%s model=%s pinned_auth=%s", passthroughSessionID, modelName, pinnedAuthID)
		dataChan, _, errChan := h.ExecuteStreamWithAuthManager(cliCtx, h.HandlerType(), modelName, requestJSON, "")
		log.Infof("responses websocket: upstream execution stream opened id=%s model=%s pinned_auth=%s", passthroughSessionID, modelName, pinnedAuthID)
		if trace.Enabled {
			log.Infof("responses websocket trace: upstream stream returned id=%s model=%s open_ms=%d", passthroughSessionID, modelName, time.Since(requestStart).Milliseconds())
		}

		completedOutput, completedResponse, errForward := h.forwardResponsesWebsocket(c, conn, cliCancel, dataChan, errChan, &wsBodyLog, passthroughSessionID, trace, requestStart)
		logResponsesWebsocketTraceSummary(trace, passthroughSessionID, modelName, requestStart, errForward)
		if errForward != nil {
			wsTerminateErr = errForward
			appendWebsocketEvent(&wsBodyLog, "disconnect", []byte(errForward.Error()))
			log.Warnf("responses websocket: forward failed id=%s error=%v", passthroughSessionID, errForward)
			return
		}
		lastResponseOutput = completedOutput
		if completedResponse {
			if errClose := closeResponsesWebsocketNormally(conn); errClose != nil {
				log.Debugf("responses websocket: normal close failed id=%s error=%v", passthroughSessionID, errClose)
			}
			log.Infof("responses websocket: completed session closed id=%s", passthroughSessionID)
			return
		}
	}
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

func websocketResponseTraceFromConfig(cfg *internalconfig.SDKConfig) websocketResponseTrace {
	raw := internalconfig.ResponseTraceConfig{}
	if cfg != nil {
		raw = cfg.Observability.ResponseTrace
	}
	previewBytes := raw.PayloadPreviewBytes
	if previewBytes <= 0 {
		previewBytes = wsPayloadPreviewDefaultBytes
	}
	if previewBytes > wsPayloadLogMaxSize {
		previewBytes = wsPayloadLogMaxSize
	}
	slowThresholdMS := raw.SlowThresholdMS
	if slowThresholdMS <= 0 {
		slowThresholdMS = wsResponseTraceDefaultSlowMS
	}
	return websocketResponseTrace{
		Enabled:             raw.Enabled,
		SlowThreshold:       time.Duration(slowThresholdMS) * time.Millisecond,
		LogPayloadPreview:   raw.LogPayloadPreview,
		PayloadPreviewBytes: previewBytes,
		LogHeaders:          raw.LogHeaders,
	}
}

func websocketTraceHeaders(c *gin.Context, trace websocketResponseTrace) string {
	if !trace.LogHeaders || c == nil || c.Request == nil {
		return "-"
	}
	headers := []string{}
	for _, name := range []string{"User-Agent", "Originator", "Version", "OpenAI-Beta", wsTurnStateHeader} {
		if value := strings.TrimSpace(c.Request.Header.Get(name)); value != "" {
			headers = append(headers, fmt.Sprintf("%s=%q", name, value))
		}
	}
	if len(headers) == 0 {
		return "-"
	}
	return strings.Join(headers, " ")
}

func logResponsesWebsocketPayload(trace websocketResponseTrace, direction, sessionID string, msgType int, eventType string, completed bool, status int, payload []byte) {
	if trace.Enabled {
		fields := []string{
			fmt.Sprintf("responses websocket trace: %s", direction),
			fmt.Sprintf("id=%s", sessionID),
			fmt.Sprintf("type=%d", msgType),
			fmt.Sprintf("event=%s", eventType),
			fmt.Sprintf("bytes=%d", len(payload)),
		}
		if status > 0 {
			fields = append(fields, fmt.Sprintf("status=%d", status))
		}
		if completed {
			fields = append(fields, "completed=true")
		}
		if trace.LogPayloadPreview {
			fields = append(fields, fmt.Sprintf("payload=%s", websocketPayloadPreviewLimit(payload, trace.PayloadPreviewBytes)))
		}
		log.Info(strings.Join(fields, " "))
		return
	}
	log.Debugf("responses websocket: %s id=%s type=%d event=%s bytes=%d", direction, sessionID, msgType, eventType, len(payload))
}

func logResponsesWebsocketTraceSummary(trace websocketResponseTrace, sessionID, modelName string, requestStart time.Time, errForward error) {
	if !trace.Enabled || requestStart.IsZero() {
		return
	}
	elapsed := time.Since(requestStart)
	if elapsed < trace.SlowThreshold && errForward == nil {
		return
	}
	log.Infof("responses websocket trace: request summary id=%s model=%s total_ms=%d slow_threshold_ms=%d error=%v", sessionID, modelName, elapsed.Milliseconds(), trace.SlowThreshold.Milliseconds(), errForward)
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

func websocketUpstreamSupportsIncrementalInput(attributes map[string]string, metadata map[string]any) bool {
	if len(attributes) > 0 {
		if raw := strings.TrimSpace(attributes["websockets"]); raw != "" {
			parsed, errParse := strconv.ParseBool(raw)
			if errParse == nil {
				return parsed
			}
		}
	}
	if len(metadata) == 0 {
		return false
	}
	raw, ok := metadata["websockets"]
	if !ok || raw == nil {
		return false
	}
	switch value := raw.(type) {
	case bool:
		return value
	case string:
		parsed, errParse := strconv.ParseBool(strings.TrimSpace(value))
		if errParse == nil {
			return parsed
		}
	default:
	}
	return false
}

func mergeJSONArrayRaw(existingRaw, appendRaw string) (string, error) {
	existingRaw = strings.TrimSpace(existingRaw)
	appendRaw = strings.TrimSpace(appendRaw)
	if existingRaw == "" {
		existingRaw = "[]"
	}
	if appendRaw == "" {
		appendRaw = "[]"
	}

	var existing []json.RawMessage
	if err := json.Unmarshal([]byte(existingRaw), &existing); err != nil {
		return "", err
	}
	var appendItems []json.RawMessage
	if err := json.Unmarshal([]byte(appendRaw), &appendItems); err != nil {
		return "", err
	}

	merged := append(existing, appendItems...)
	out, err := json.Marshal(merged)
	if err != nil {
		return "", err
	}
	return string(out), nil
}

func normalizeJSONArrayRaw(raw []byte) string {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" {
		return "[]"
	}
	result := gjson.Parse(trimmed)
	if result.Type == gjson.JSON && result.IsArray() {
		return trimmed
	}
	return "[]"
}

func responsesWebsocketKeepAlivePayload(sessionID string) []byte {
	responseID := "resp_keepalive_" + strings.ReplaceAll(sessionID, "-", "")
	payload, err := json.Marshal(map[string]any{
		"type": "response.in_progress",
		"response": map[string]any{
			"id":     responseID,
			"status": "in_progress",
		},
	})
	if err != nil {
		return []byte(`{"type":"response.in_progress","response":{"status":"in_progress"}}`)
	}
	return payload
}

func responsesWebsocketPrewarmPayloads(requestJSON []byte) ([][]byte, bool) {
	if !gjson.GetBytes(requestJSON, "generate").Exists() || gjson.GetBytes(requestJSON, "generate").Bool() {
		return nil, false
	}

	syntheticID := "resp_prewarm_" + uuid.NewString()
	created := []byte(fmt.Sprintf(`{"type":"response.created","response":{"id":%q}}`, syntheticID))
	done := []byte(fmt.Sprintf(`{"type":"response.completed","response":{"id":%q,"output":[],"usage":{"input_tokens":0,"output_tokens":0,"total_tokens":0}}}`, syntheticID))
	return [][]byte{created, done}, true
}

func websocketRequestSummary(rawJSON []byte) string {
	requestType := strings.TrimSpace(gjson.GetBytes(rawJSON, "type").String())
	modelName := strings.TrimSpace(gjson.GetBytes(rawJSON, "model").String())
	prevResponseID := strings.TrimSpace(gjson.GetBytes(rawJSON, "previous_response_id").String())
	generateResult := gjson.GetBytes(rawJSON, "generate")
	generateValue := "<unset>"
	if generateResult.Exists() {
		if generateResult.Bool() {
			generateValue = "true"
		} else {
			generateValue = "false"
		}
	}
	inputCount := 0
	inputResult := gjson.GetBytes(rawJSON, "input")
	if inputResult.Exists() && inputResult.IsArray() {
		inputCount = len(inputResult.Array())
	}
	return fmt.Sprintf("type=%s model=%s generate=%s previous_response_id=%s input_items=%d", requestType, modelName, generateValue, prevResponseID, inputCount)
}

func (h *OpenAIResponsesAPIHandler) forwardResponsesWebsocket(
	c *gin.Context,
	conn *websocket.Conn,
	cancel handlers.APIHandlerCancelFunc,
	data <-chan []byte,
	errs <-chan *interfaces.ErrorMessage,
	wsBodyLog *strings.Builder,
	sessionID string,
	trace websocketResponseTrace,
	requestStart time.Time,
) ([]byte, bool, error) {
	completed := false
	completedOutput := []byte("[]")
	lastUpstreamEventAt := time.Now()
	receivedUpstreamEvent := false
	keepAlive := time.NewTicker(responsesWebsocketApplicationKeepAlivePeriod)
	defer keepAlive.Stop()
	upstreamIdleTimer := time.NewTimer(responsesWebsocketUpstreamIdleTimeout)
	defer upstreamIdleTimer.Stop()

	for {
		select {
		case <-c.Request.Context().Done():
			cancel(c.Request.Context().Err())
			return completedOutput, completed, c.Request.Context().Err()
		case <-keepAlive.C:
			payload := responsesWebsocketKeepAlivePayload(sessionID)
			markAPIResponseTimestamp(c)
			appendWebsocketEvent(wsBodyLog, "response", payload)
			log.Infof("responses websocket: keepalive sent id=%s event=%s", sessionID, websocketPayloadEventType(payload))
			if errWrite := writeResponsesWebsocketMessage(conn, websocket.TextMessage, payload); errWrite != nil {
				log.Warnf("responses websocket: keepalive write failed id=%s event=%s error=%v", sessionID, websocketPayloadEventType(payload), errWrite)
				cancel(errWrite)
				return completedOutput, completed, errWrite
			}
		case <-upstreamIdleTimer.C:
			errMsg := &interfaces.ErrorMessage{StatusCode: http.StatusGatewayTimeout, Error: fmt.Errorf("upstream websocket did not send another event within %s", responsesWebsocketUpstreamIdleTimeout)}
			h.LoggingAPIResponseError(context.WithValue(c.Request.Context(), util.ContextKeyGin, c), errMsg)
			markAPIResponseTimestamp(c)
			errorPayload, errWrite := writeResponsesWebsocketError(conn, errMsg)
			appendWebsocketEvent(wsBodyLog, "response", errorPayload)
			log.Warnf("responses websocket: upstream event idle timeout id=%s timeout=%s idle=%s", sessionID, responsesWebsocketUpstreamIdleTimeout, time.Since(lastUpstreamEventAt).Truncate(time.Second))
			logResponsesWebsocketPayload(trace, "downstream_out", sessionID, websocket.TextMessage, websocketPayloadEventType(errorPayload), false, 0, errorPayload)
			if errWrite != nil {
				log.Warnf("responses websocket: downstream_out write failed id=%s event=%s error=%v", sessionID, websocketPayloadEventType(errorPayload), errWrite)
				cancel(errMsg.Error)
				return completedOutput, completed, errWrite
			}
			cancel(errMsg.Error)
			return completedOutput, completed, nil
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
				logResponsesWebsocketPayload(trace, "downstream_out", sessionID, websocket.TextMessage, websocketPayloadEventType(errorPayload), false, errMsg.StatusCode, errorPayload)
				if errWrite != nil {
					// log.Warnf(
					// 	"responses websocket: downstream_out write failed id=%s event=%s error=%v",
					// 	sessionID,
					// 	websocketPayloadEventType(errorPayload),
					// 	errWrite,
					// )
					cancel(errMsg.Error)
					return completedOutput, completed, errWrite
				}
			}
			if errMsg != nil {
				cancel(errMsg.Error)
			} else {
				cancel(nil)
			}
			return completedOutput, completed, nil
		case chunk, ok := <-data:
			if !ok {
				if !completed {
					errMsg := &interfaces.ErrorMessage{
						StatusCode: http.StatusRequestTimeout,
						Error:      fmt.Errorf("stream closed before response.completed"),
					}
					h.LoggingAPIResponseError(context.WithValue(c.Request.Context(), util.ContextKeyGin, c), errMsg)
					markAPIResponseTimestamp(c)
					errorPayload, errWrite := writeResponsesWebsocketError(conn, errMsg)
					appendWebsocketEvent(wsBodyLog, "response", errorPayload)
					logResponsesWebsocketPayload(trace, "downstream_out", sessionID, websocket.TextMessage, websocketPayloadEventType(errorPayload), false, 0, errorPayload)
					if errWrite != nil {
						log.Warnf(
							"responses websocket: downstream_out write failed id=%s event=%s error=%v",
							sessionID,
							websocketPayloadEventType(errorPayload),
							errWrite,
						)
						cancel(errMsg.Error)
						return completedOutput, completed, errWrite
					}
					cancel(errMsg.Error)
					return completedOutput, completed, nil
				}
				cancel(nil)
				return completedOutput, completed, nil
			}

			payloads := websocketJSONPayloadsFromChunk(chunk)
			if len(payloads) > 0 {
				firstUpstreamEvent := !receivedUpstreamEvent
				receivedUpstreamEvent = true
				lastUpstreamEventAt = time.Now()
				if upstreamIdleTimer.Stop() == false {
					select {
					case <-upstreamIdleTimer.C:
					default:
					}
				}
				upstreamIdleTimer.Reset(responsesWebsocketUpstreamIdleTimeout)
				if firstUpstreamEvent {
					log.Infof("responses websocket: first upstream event received id=%s event=%s", sessionID, websocketPayloadEventType(payloads[0]))
					if trace.Enabled {
						log.Infof("responses websocket trace: first upstream event id=%s event=%s first_event_ms=%d bytes=%d", sessionID, websocketPayloadEventType(payloads[0]), time.Since(requestStart).Milliseconds(), len(payloads[0]))
					}
				} else {
					log.Debugf("responses websocket: upstream event received id=%s event=%s", sessionID, websocketPayloadEventType(payloads[0]))
				}
			}
			for i := range payloads {
				eventType := gjson.GetBytes(payloads[i], "type").String()
				if eventType == wsEventTypeCompleted {
					completed = true
					completedOutput = responseCompletedOutputFromPayload(payloads[i])
				}
				markAPIResponseTimestamp(c)
				appendWebsocketEvent(wsBodyLog, "response", payloads[i])
				logResponsesWebsocketPayload(trace, "downstream_out", sessionID, websocket.TextMessage, websocketPayloadEventType(payloads[i]), completed, 0, payloads[i])
				if errWrite := writeResponsesWebsocketMessage(conn, websocket.TextMessage, payloads[i]); errWrite != nil {
					log.Warnf(
						"responses websocket: downstream_out write failed id=%s event=%s error=%v",
						sessionID,
						websocketPayloadEventType(payloads[i]),
						errWrite,
					)
					cancel(errWrite)
					return completedOutput, completed, errWrite
				}
				if completed {
					log.Infof("responses websocket: completed forwarded id=%s", sessionID)
					cancel(nil)
					return completedOutput, completed, nil
				}
			}
		}
	}
}

func responseCompletedOutputFromPayload(payload []byte) []byte {
	output := gjson.GetBytes(payload, "response.output")
	if output.Exists() && output.IsArray() {
		return bytes.Clone([]byte(output.Raw))
	}
	return []byte("[]")
}

func websocketJSONPayloadsFromChunk(chunk []byte) [][]byte {
	payloads := make([][]byte, 0, 2)
	lines := bytes.Split(chunk, []byte("\n"))
	for i := range lines {
		line := bytes.TrimSpace(lines[i])
		if len(line) == 0 || bytes.HasPrefix(line, []byte("event:")) {
			continue
		}
		if bytes.HasPrefix(line, []byte("data:")) {
			line = bytes.TrimSpace(line[len("data:"):])
		}
		if len(line) == 0 || bytes.Equal(line, []byte(wsDoneMarker)) {
			continue
		}
		if json.Valid(line) {
			payloads = append(payloads, bytes.Clone(line))
		}
	}

	if len(payloads) > 0 {
		return payloads
	}

	trimmed := bytes.TrimSpace(chunk)
	if bytes.HasPrefix(trimmed, []byte("data:")) {
		trimmed = bytes.TrimSpace(trimmed[len("data:"):])
	}
	if len(trimmed) > 0 && !bytes.Equal(trimmed, []byte(wsDoneMarker)) && json.Valid(trimmed) {
		payloads = append(payloads, bytes.Clone(trimmed))
	}
	return payloads
}

func writeResponsesWebsocketMessage(conn *websocket.Conn, messageType int, data []byte) error {
	if err := conn.SetWriteDeadline(time.Now().Add(responsesWebsocketWriteTimeout)); err != nil {
		return err
	}
	return conn.WriteMessage(messageType, data)
}

func closeResponsesWebsocketNormally(conn *websocket.Conn) error {
	return closeResponsesWebsocketWithReason(conn, websocket.CloseNormalClosure, "completed")
}

func closeResponsesWebsocketWithReason(conn *websocket.Conn, code int, reason string) error {
	if err := conn.SetWriteDeadline(time.Now().Add(responsesWebsocketWriteTimeout)); err != nil {
		return err
	}
	message := websocket.FormatCloseMessage(code, reason)
	return conn.WriteControl(websocket.CloseMessage, message, time.Now().Add(responsesWebsocketWriteTimeout))
}

func isResponsesWebsocketReadTimeout(err error) bool {
	timeout, ok := err.(interface{ Timeout() bool })
	return ok && timeout.Timeout()
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

	body := handlers.BuildErrorResponseBody(status, errText)
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
	return data, writeResponsesWebsocketMessage(conn, websocket.TextMessage, data)
}

func appendWebsocketEvent(builder *strings.Builder, eventType string, payload []byte) {
	if builder == nil {
		return
	}
	trimmedPayload := bytes.TrimSpace(payload)
	if len(trimmedPayload) == 0 {
		return
	}
	if builder.Len() > 0 {
		builder.WriteString("\n")
	}
	builder.WriteString("websocket.")
	builder.WriteString(eventType)
	builder.WriteString("\n")
	builder.Write(trimmedPayload)
	builder.WriteString("\n")
}

func websocketPayloadEventType(payload []byte) string {
	eventType := strings.TrimSpace(gjson.GetBytes(payload, "type").String())
	if eventType == "" {
		return "-"
	}
	return eventType
}

func websocketPayloadPreview(payload []byte) string {
	return websocketPayloadPreviewLimit(payload, wsPayloadLogMaxSize)
}

func websocketPayloadPreviewLimit(payload []byte, maxBytes int) string {
	trimmedPayload := bytes.TrimSpace(payload)
	if len(trimmedPayload) == 0 {
		return "<empty>"
	}
	if maxBytes <= 0 {
		maxBytes = wsPayloadPreviewDefaultBytes
	}
	if maxBytes > wsPayloadLogMaxSize {
		maxBytes = wsPayloadLogMaxSize
	}
	preview := trimmedPayload
	if len(preview) > maxBytes {
		preview = preview[:maxBytes]
	}
	previewText := strings.ReplaceAll(string(preview), "\n", "\\n")
	previewText = strings.ReplaceAll(previewText, "\r", "\\r")
	if len(trimmedPayload) > maxBytes {
		return fmt.Sprintf("%s...(truncated,total=%d)", previewText, len(trimmedPayload))
	}
	return previewText
}

func setWebsocketRequestBody(c *gin.Context, body string) {
	if c == nil {
		return
	}
	trimmedBody := strings.TrimSpace(body)
	if trimmedBody == "" {
		return
	}
	c.Set(wsRequestBodyKey, []byte(trimmedBody))
}

func markAPIResponseTimestamp(c *gin.Context) {
	if c == nil {
		return
	}
	if _, exists := c.Get("API_RESPONSE_TIMESTAMP"); exists {
		return
	}
	c.Set("API_RESPONSE_TIMESTAMP", time.Now())
}
