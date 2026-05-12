package openai

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v6/sdk/api/handlers"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v6/sdk/config"
	"github.com/tidwall/gjson"
)

func TestNormalizeResponsesWebsocketRequestCreate(t *testing.T) {
	raw := []byte(`{"type":"response.create","model":"test-model","stream":false,"input":[{"type":"message","id":"msg-1"}]}`)

	normalized, last, errMsg := normalizeResponsesWebsocketRequest(raw, nil, nil)
	if errMsg != nil {
		t.Fatalf("unexpected error: %v", errMsg.Error)
	}
	if gjson.GetBytes(normalized, "type").Exists() {
		t.Fatalf("normalized create request must not include type field")
	}
	if !gjson.GetBytes(normalized, "stream").Bool() {
		t.Fatalf("normalized create request must force stream=true")
	}
	if gjson.GetBytes(normalized, "model").String() != "test-model" {
		t.Fatalf("unexpected model: %s", gjson.GetBytes(normalized, "model").String())
	}
	if !bytes.Equal(last, normalized) {
		t.Fatalf("last request snapshot should match normalized request")
	}
}

func TestResponsesWebsocketPrewarmPayloads(t *testing.T) {
	requestJSON := []byte(`{"model":"test-model","generate":false,"stream":true,"input":[]}`)

	payloads, ok := responsesWebsocketPrewarmPayloads(requestJSON)
	if !ok {
		t.Fatalf("expected prewarm payloads")
	}
	if len(payloads) != 2 {
		t.Fatalf("payload len = %d, want 2", len(payloads))
	}
	if gjson.GetBytes(payloads[0], "type").String() != "response.created" {
		t.Fatalf("unexpected created payload type: %s", gjson.GetBytes(payloads[0], "type").String())
	}
	if gjson.GetBytes(payloads[1], "type").String() != wsEventTypeDone {
		t.Fatalf("unexpected completed payload type: %s", gjson.GetBytes(payloads[1], "type").String())
	}
	responseID := gjson.GetBytes(payloads[0], "response.id").String()
	if responseID == "" {
		t.Fatalf("created payload missing response id")
	}
	if gjson.GetBytes(payloads[1], "response.id").String() != responseID {
		t.Fatalf("done payload response id mismatch")
	}
	if len(gjson.GetBytes(payloads[1], "response.output").Array()) != 0 {
		t.Fatalf("done payload output must be empty")
	}
	if gjson.GetBytes(payloads[1], "response.usage.total_tokens").Int() != 0 {
		t.Fatalf("done payload total_tokens must be 0")
	}
	if gjson.GetBytes(payloads[0], "response.object").String() != "response" {
		t.Fatalf("created payload missing response.object: %s", gjson.GetBytes(payloads[0], "response.object").String())
	}
	if gjson.GetBytes(payloads[0], "response.status").String() != "in_progress" {
		t.Fatalf("created payload response.status = %s, want in_progress", gjson.GetBytes(payloads[0], "response.status").String())
	}
	if gjson.GetBytes(payloads[0], "response.model").String() != "test-model" {
		t.Fatalf("created payload response.model = %s, want test-model", gjson.GetBytes(payloads[0], "response.model").String())
	}
	if gjson.GetBytes(payloads[1], "response.object").String() != "response" {
		t.Fatalf("completed payload missing response.object: %s", gjson.GetBytes(payloads[1], "response.object").String())
	}
	if gjson.GetBytes(payloads[1], "response.status").String() != "completed" {
		t.Fatalf("completed payload response.status = %s, want completed", gjson.GetBytes(payloads[1], "response.status").String())
	}
	if gjson.GetBytes(payloads[1], "response.model").String() != "test-model" {
		t.Fatalf("completed payload response.model = %s, want test-model", gjson.GetBytes(payloads[1], "response.model").String())
	}
	if gjson.GetBytes(payloads[1], "response.created_at").Int() == 0 {
		t.Fatalf("completed payload missing response.created_at")
	}
	if gjson.GetBytes(payloads[1], "response.completed_at").Int() == 0 {
		t.Fatalf("completed payload missing response.completed_at")
	}
}

func TestResponsesWebsocketPrewarmPayloadsDisabledForGenerateTrue(t *testing.T) {
	requestJSON := []byte(`{"model":"test-model","generate":true,"stream":true,"input":[]}`)

	payloads, ok := responsesWebsocketPrewarmPayloads(requestJSON)
	if ok {
		t.Fatalf("generate=true must not be treated as prewarm")
	}
	if payloads != nil {
		t.Fatalf("expected nil payloads when generate=true")
	}
}

func TestResponsesWebsocketPrewarmKeepsConnectionForRealRequest(t *testing.T) {
	gin.SetMode(gin.TestMode)
	executor := &responsesCaptureExecutor{}
	manager := coreauth.NewManager(nil, nil, nil)
	manager.RegisterExecutor(executor)

	auth := &coreauth.Auth{ID: "auth-ws-prewarm", Provider: executor.Identifier(), Status: coreauth.StatusActive}
	if _, err := manager.Register(context.Background(), auth); err != nil {
		t.Fatalf("Register auth: %v", err)
	}
	registry.GetGlobalRegistry().RegisterClient(auth.ID, auth.Provider, []*registry.ModelInfo{{ID: "test-model"}})
	t.Cleanup(func() {
		registry.GetGlobalRegistry().UnregisterClient(auth.ID)
	})

	base := handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, manager)
	h := NewOpenAIResponsesAPIHandler(base)
	router := gin.New()
	router.GET("/v1/responses", h.ResponsesWebsocket)

	server := httptest.NewServer(router)
	defer server.Close()

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http") + "/v1/responses"
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("dial websocket: %v", err)
	}
	defer conn.Close()

	if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.create","model":"test-model","generate":false}`)); err != nil {
		t.Fatalf("write prewarm request: %v", err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, createdPayload, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("read prewarm created: %v", err)
	}
	if gjson.GetBytes(createdPayload, "type").String() != "response.created" {
		t.Fatalf("created type = %s, want response.created", gjson.GetBytes(createdPayload, "type").String())
	}
	prewarmID := gjson.GetBytes(createdPayload, "response.id").String()
	if prewarmID == "" {
		t.Fatalf("prewarm response id is empty")
	}

	_, completedPayload, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("read prewarm completed: %v", err)
	}
	if gjson.GetBytes(completedPayload, "type").String() != wsEventTypeDone {
		t.Fatalf("completed type = %s, want %s", gjson.GetBytes(completedPayload, "type").String(), wsEventTypeDone)
	}
	if executor.streamCalls != 0 {
		t.Fatalf("stream calls after prewarm = %d, want 0", executor.streamCalls)
	}

	followUp := []byte(`{"type":"response.create","previous_response_id":"` + prewarmID + `","input":[{"type":"message","id":"msg-1"}]}`)
	if err := conn.WriteMessage(websocket.TextMessage, followUp); err != nil {
		t.Fatalf("write follow-up request: %v", err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, upstreamPayload, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("read follow-up response: %v", err)
	}
	if gjson.GetBytes(upstreamPayload, "type").String() != wsEventTypeDone {
		t.Fatalf("follow-up type = %s, want %s", gjson.GetBytes(upstreamPayload, "type").String(), wsEventTypeDone)
	}
	if executor.streamCalls != 1 {
		t.Fatalf("stream calls after follow-up = %d, want 1", executor.streamCalls)
	}
}

func TestNormalizeResponsesWebsocketRequestCreateWithHistory(t *testing.T) {
	lastRequest := []byte(`{"model":"test-model","stream":true,"input":[{"type":"message","id":"msg-1"}]}`)
	lastResponseOutput := []byte(`[
		{"type":"function_call","id":"fc-1","call_id":"call-1"},
		{"type":"message","id":"assistant-1"}
	]`)
	raw := []byte(`{"type":"response.create","input":[{"type":"function_call_output","call_id":"call-1","id":"tool-out-1"}]}`)

	normalized, next, errMsg := normalizeResponsesWebsocketRequest(raw, lastRequest, lastResponseOutput)
	if errMsg != nil {
		t.Fatalf("unexpected error: %v", errMsg.Error)
	}
	if gjson.GetBytes(normalized, "type").Exists() {
		t.Fatalf("normalized subsequent create request must not include type field")
	}
	if gjson.GetBytes(normalized, "model").String() != "test-model" {
		t.Fatalf("unexpected model: %s", gjson.GetBytes(normalized, "model").String())
	}

	input := gjson.GetBytes(normalized, "input").Array()
	if len(input) != 4 {
		t.Fatalf("merged input len = %d, want 4", len(input))
	}
	if input[0].Get("id").String() != "msg-1" ||
		input[1].Get("id").String() != "fc-1" ||
		input[2].Get("id").String() != "assistant-1" ||
		input[3].Get("id").String() != "tool-out-1" {
		t.Fatalf("unexpected merged input order")
	}
	if !bytes.Equal(next, normalized) {
		t.Fatalf("next request snapshot should match normalized request")
	}
}

func TestNormalizeResponsesWebsocketRequestWithPreviousResponseIDIncremental(t *testing.T) {
	lastRequest := []byte(`{"model":"test-model","stream":true,"instructions":"be helpful","input":[{"type":"message","id":"msg-1"}]}`)
	lastResponseOutput := []byte(`[
		{"type":"function_call","id":"fc-1","call_id":"call-1"},
		{"type":"message","id":"assistant-1"}
	]`)
	raw := []byte(`{"type":"response.create","previous_response_id":"resp-1","input":[{"type":"function_call_output","call_id":"call-1","id":"tool-out-1"}]}`)

	normalized, next, errMsg := normalizeResponsesWebsocketRequestWithMode(raw, lastRequest, lastResponseOutput, true)
	if errMsg != nil {
		t.Fatalf("unexpected error: %v", errMsg.Error)
	}
	if gjson.GetBytes(normalized, "type").Exists() {
		t.Fatalf("normalized request must not include type field")
	}
	if gjson.GetBytes(normalized, "previous_response_id").String() != "resp-1" {
		t.Fatalf("previous_response_id must be preserved in incremental mode")
	}
	input := gjson.GetBytes(normalized, "input").Array()
	if len(input) != 1 {
		t.Fatalf("incremental input len = %d, want 1", len(input))
	}
	if input[0].Get("id").String() != "tool-out-1" {
		t.Fatalf("unexpected incremental input item id: %s", input[0].Get("id").String())
	}
	if gjson.GetBytes(normalized, "model").String() != "test-model" {
		t.Fatalf("unexpected model: %s", gjson.GetBytes(normalized, "model").String())
	}
	if gjson.GetBytes(normalized, "instructions").String() != "be helpful" {
		t.Fatalf("unexpected instructions: %s", gjson.GetBytes(normalized, "instructions").String())
	}
	if !bytes.Equal(next, normalized) {
		t.Fatalf("next request snapshot should match normalized request")
	}
}

func TestNormalizeResponsesWebsocketRequestWithPreviousResponseIDMergedWhenIncrementalDisabled(t *testing.T) {
	lastRequest := []byte(`{"model":"test-model","stream":true,"input":[{"type":"message","id":"msg-1"}]}`)
	lastResponseOutput := []byte(`[
		{"type":"function_call","id":"fc-1","call_id":"call-1"},
		{"type":"message","id":"assistant-1"}
	]`)
	raw := []byte(`{"type":"response.create","previous_response_id":"resp-1","input":[{"type":"function_call_output","call_id":"call-1","id":"tool-out-1"}]}`)

	normalized, next, errMsg := normalizeResponsesWebsocketRequestWithMode(raw, lastRequest, lastResponseOutput, false)
	if errMsg != nil {
		t.Fatalf("unexpected error: %v", errMsg.Error)
	}
	if gjson.GetBytes(normalized, "previous_response_id").Exists() {
		t.Fatalf("previous_response_id must be removed when incremental mode is disabled")
	}
	input := gjson.GetBytes(normalized, "input").Array()
	if len(input) != 4 {
		t.Fatalf("merged input len = %d, want 4", len(input))
	}
	if input[0].Get("id").String() != "msg-1" ||
		input[1].Get("id").String() != "fc-1" ||
		input[2].Get("id").String() != "assistant-1" ||
		input[3].Get("id").String() != "tool-out-1" {
		t.Fatalf("unexpected merged input order")
	}
	if !bytes.Equal(next, normalized) {
		t.Fatalf("next request snapshot should match normalized request")
	}
}

func TestNormalizeResponsesWebsocketRequestAppend(t *testing.T) {
	lastRequest := []byte(`{"model":"test-model","stream":true,"input":[{"type":"message","id":"msg-1"}]}`)
	lastResponseOutput := []byte(`[
		{"type":"message","id":"assistant-1"},
		{"type":"function_call_output","id":"tool-out-1"}
	]`)
	raw := []byte(`{"type":"response.append","input":[{"type":"message","id":"msg-2"},{"type":"message","id":"msg-3"}]}`)

	normalized, next, errMsg := normalizeResponsesWebsocketRequest(raw, lastRequest, lastResponseOutput)
	if errMsg != nil {
		t.Fatalf("unexpected error: %v", errMsg.Error)
	}
	input := gjson.GetBytes(normalized, "input").Array()
	if len(input) != 5 {
		t.Fatalf("merged input len = %d, want 5", len(input))
	}
	if input[0].Get("id").String() != "msg-1" ||
		input[1].Get("id").String() != "assistant-1" ||
		input[2].Get("id").String() != "tool-out-1" ||
		input[3].Get("id").String() != "msg-2" ||
		input[4].Get("id").String() != "msg-3" {
		t.Fatalf("unexpected merged input order")
	}
	if !bytes.Equal(next, normalized) {
		t.Fatalf("next request snapshot should match normalized append request")
	}
}

func TestNormalizeResponsesWebsocketRequestAppendWithoutCreate(t *testing.T) {
	raw := []byte(`{"type":"response.append","input":[]}`)

	_, _, errMsg := normalizeResponsesWebsocketRequest(raw, nil, nil)
	if errMsg == nil {
		t.Fatalf("expected error for append without previous request")
	}
	if errMsg.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", errMsg.StatusCode, http.StatusBadRequest)
	}
}

func TestWebsocketJSONPayloadsFromChunk(t *testing.T) {
	chunk := []byte("event: response.created\n\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp-1\"}}\n\ndata: [DONE]\n")

	payloads := websocketJSONPayloadsFromChunk(chunk)
	if len(payloads) != 1 {
		t.Fatalf("payloads len = %d, want 1", len(payloads))
	}
	if gjson.GetBytes(payloads[0], "type").String() != "response.created" {
		t.Fatalf("unexpected payload type: %s", gjson.GetBytes(payloads[0], "type").String())
	}
}

func TestWebsocketJSONPayloadsFromPlainJSONChunk(t *testing.T) {
	chunk := []byte(`{"type":"response.completed","response":{"id":"resp-1"}}`)

	payloads := websocketJSONPayloadsFromChunk(chunk)
	if len(payloads) != 1 {
		t.Fatalf("payloads len = %d, want 1", len(payloads))
	}
	if gjson.GetBytes(payloads[0], "type").String() != "response.completed" {
		t.Fatalf("unexpected payload type: %s", gjson.GetBytes(payloads[0], "type").String())
	}
}

func TestResponseCompletedOutputFromPayload(t *testing.T) {
	payload := []byte(`{"type":"response.completed","response":{"id":"resp-1","output":[{"type":"message","id":"out-1"}]}}`)

	output := responseCompletedOutputFromPayload(payload)
	items := gjson.ParseBytes(output).Array()
	if len(items) != 1 {
		t.Fatalf("output len = %d, want 1", len(items))
	}
	if items[0].Get("id").String() != "out-1" {
		t.Fatalf("unexpected output id: %s", items[0].Get("id").String())
	}
}

func TestAppendWebsocketEvent(t *testing.T) {
	var builder strings.Builder

	appendWebsocketEvent(&builder, "request", []byte("  {\"type\":\"response.create\"}\n"))
	appendWebsocketEvent(&builder, "response", []byte("{\"type\":\"response.created\"}"))

	got := builder.String()
	if !strings.Contains(got, "websocket.request\n{\"type\":\"response.create\"}\n") {
		t.Fatalf("request event not found in body: %s", got)
	}
	if !strings.Contains(got, "websocket.response\n{\"type\":\"response.created\"}\n") {
		t.Fatalf("response event not found in body: %s", got)
	}
}

func TestSetWebsocketRequestBody(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)

	setWebsocketRequestBody(c, " \n ")
	if _, exists := c.Get(wsRequestBodyKey); exists {
		t.Fatalf("request body key should not be set for empty body")
	}

	setWebsocketRequestBody(c, "event body")
	value, exists := c.Get(wsRequestBodyKey)
	if !exists {
		t.Fatalf("request body key not set")
	}
	bodyBytes, ok := value.([]byte)
	if !ok {
		t.Fatalf("request body key type mismatch")
	}
	if string(bodyBytes) != "event body" {
		t.Fatalf("request body = %q, want %q", string(bodyBytes), "event body")
	}
}
