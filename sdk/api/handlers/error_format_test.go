package handlers

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/interfaces"
	"github.com/tidwall/gjson"
)

// Upstream error bodies as the providers send them.
const (
	openAIUpstreamError  = `{"error":{"message":"You exceeded your current quota.","type":"insufficient_quota","param":null,"code":"insufficient_quota"}}`
	codexUsageLimitError = `{"error":{"type":"usage_limit_reached","message":"The usage limit has been reached","resets_at":1793300000}}`
	claudeUpstreamError  = `{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"},"request_id":"req_1"}`
	geminiUpstreamError  = `{"error":{"code":429,"message":"Resource has been exhausted (e.g. check quota).","status":"RESOURCE_EXHAUSTED"}}`
	xaiUpstreamError     = `{"code":"invalid-argument","error":"Incorrect API key provided."}`
	openAIContextError   = `{"error":{"message":"This model's maximum context length is 128000 tokens. However, your messages resulted in 130512 tokens.","type":"invalid_request_error","param":"messages","code":"context_length_exceeded"}}`
	claudeContextError   = `{"type":"error","error":{"type":"invalid_request_error","message":"prompt is too long: 210450 tokens > 200000 maximum"}}`
	geminiContextError   = `{"error":{"code":400,"message":"The input token count (1048577) exceeds the maximum number of tokens allowed (1048576).","status":"INVALID_ARGUMENT"}}`
	codexContextError    = `{"error":{"message":"Your input exceeds the context window of this model. Please adjust your input and try again.","type":"invalid_request_error","param":"input","code":"context_length_exceeded"}}`
	grokContextError     = `{"code":"invalid-argument","error":"This model's maximum prompt length is 500000 but the request contains 816381 tokens."}`
	// A rate limit can talk about prompt tokens; it is still not an
	// over-long prompt.
	tokenRateLimitError    = `{"error":{"message":"Rate limit reached: maximum prompt tokens per minute exceeded.","type":"rate_limit_error"}}`
	compatibleContextError = `{"error":{"message":"Input is too long for requested model.","type":"BadRequestError","code":400}}`
)

func TestFormatErrorBodyForClaudeClients(t *testing.T) {
	cases := []struct {
		name        string
		status      int
		errText     string
		wantStatus  int
		wantType    string
		wantMessage string
		passthrough bool
	}{
		{name: "Claude upstream passes through", status: 529, errText: claudeUpstreamError, wantStatus: 529, passthrough: true},
		{name: "Claude context error passes through", status: 400, errText: claudeContextError, wantStatus: 400, passthrough: true},
		{name: "Codex usage limit", status: 429, errText: codexUsageLimitError, wantStatus: 429, wantType: "rate_limit_error", wantMessage: "The usage limit has been reached (usage_limit_reached)"},
		{name: "OpenAI quota", status: 403, errText: openAIUpstreamError, wantStatus: 403, wantType: "permission_error", wantMessage: "You exceeded your current quota. (insufficient_quota)"},
		{name: "Gemini throttle", status: 429, errText: geminiUpstreamError, wantStatus: 429, wantType: "rate_limit_error", wantMessage: "Resource has been exhausted (e.g. check quota). (RESOURCE_EXHAUSTED)"},
		{name: "xAI key", status: 401, errText: xaiUpstreamError, wantStatus: 401, wantType: "authentication_error", wantMessage: "Incorrect API key provided. (invalid-argument)"},
		{name: "plain text", status: 502, errText: "upstream connect error", wantStatus: 502, wantType: "api_error", wantMessage: "upstream connect error"},
		{name: "overloaded", status: 503, errText: "busy", wantStatus: 503, wantType: "overloaded_error", wantMessage: "busy"},
		{name: "too large", status: 413, errText: "body too large", wantStatus: 413, wantType: "request_too_large", wantMessage: "body too large"},
		{name: "not found", status: 404, errText: "no such model", wantStatus: 404, wantType: "not_found_error", wantMessage: "no such model"},
		// Claude Code compacts the conversation when a 400 says "prompt is
		// too long", whichever upstream ran out of context, and reads how far
		// over the window the prompt is from "N tokens > M maximum".
		{name: "OpenAI context error", status: 400, errText: openAIContextError, wantStatus: 400, wantType: "invalid_request_error", wantMessage: "prompt is too long: 130512 tokens > 128000 maximum. This model's maximum context length is 128000 tokens. However, your messages resulted in 130512 tokens. (context_length_exceeded)"},
		{name: "Gemini context error", status: 400, errText: geminiContextError, wantStatus: 400, wantType: "invalid_request_error", wantMessage: "prompt is too long: 1048577 tokens > 1048576 maximum. The input token count (1048577) exceeds the maximum number of tokens allowed (1048576). (INVALID_ARGUMENT)"},
		{name: "Grok context error", status: 400, errText: grokContextError, wantStatus: 400, wantType: "invalid_request_error", wantMessage: "prompt is too long: 816381 tokens > 500000 maximum. This model's maximum prompt length is 500000 but the request contains 816381 tokens. (invalid-argument)"},
		{name: "Codex context error without counts", status: 400, errText: codexContextError, wantStatus: 400, wantType: "invalid_request_error", wantMessage: "prompt is too long: Your input exceeds the context window of this model. Please adjust your input and try again. (context_length_exceeded)"},
		{name: "context error with another status", status: 413, errText: compatibleContextError, wantStatus: 400, wantType: "invalid_request_error", wantMessage: "prompt is too long: Input is too long for requested model."},
		{name: "a rate limit is not a context error", status: 429, errText: tokenRateLimitError, wantStatus: 429, wantType: "rate_limit_error", wantMessage: "Rate limit reached: maximum prompt tokens per minute exceeded. (rate_limit_error)"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			status, body := FormatErrorBody(ErrorFormatClaude, tc.status, tc.errText)
			if status != tc.wantStatus {
				t.Fatalf("status = %d, want %d", status, tc.wantStatus)
			}
			if tc.passthrough {
				if string(body) != tc.errText {
					t.Fatalf("body = %s, want the upstream's", body)
				}
				return
			}
			if gjson.GetBytes(body, "type").String() != "error" || gjson.GetBytes(body, "error.type").String() != tc.wantType {
				t.Fatalf("body = %s, want an Anthropic %s", body, tc.wantType)
			}
			if got := gjson.GetBytes(body, "error.message").String(); got != tc.wantMessage {
				t.Fatalf("message = %q, want %q", got, tc.wantMessage)
			}
		})
	}
}

func TestFormatErrorBodyForOpenAIClients(t *testing.T) {
	cases := []struct {
		name        string
		status      int
		errText     string
		wantStatus  int
		passthrough bool
		wantType    string
		wantCode    string
		wantMessage string
		wantParam   string
	}{
		{name: "OpenAI upstream passes through", status: 429, errText: openAIUpstreamError, wantStatus: 429, passthrough: true},
		{name: "OpenAI context error passes through", status: 400, errText: openAIContextError, wantStatus: 400, passthrough: true},
		{name: "Codex usage limit passes through", status: 429, errText: codexUsageLimitError, wantStatus: 429, passthrough: true},
		{name: "Claude upstream", status: 529, errText: claudeUpstreamError, wantStatus: 529, wantType: "server_error", wantCode: "internal_server_error", wantMessage: "Overloaded (overloaded_error)"},
		{name: "Gemini upstream", status: 429, errText: geminiUpstreamError, wantStatus: 429, wantType: "rate_limit_error", wantCode: "rate_limit_exceeded", wantMessage: "Resource has been exhausted (e.g. check quota). (RESOURCE_EXHAUSTED)"},
		{name: "xAI keeps its code", status: 401, errText: xaiUpstreamError, wantStatus: 401, wantType: "authentication_error", wantCode: "invalid-argument", wantMessage: "Incorrect API key provided."},
		{name: "plain text", status: 500, errText: "boom", wantStatus: 500, wantType: "server_error", wantCode: "internal_server_error", wantMessage: "boom"},
		// Codex CLI and the OpenAI SDKs key on the code.
		{name: "Claude context error", status: 400, errText: claudeContextError, wantStatus: 400, wantType: "invalid_request_error", wantCode: "context_length_exceeded", wantMessage: "prompt is too long: 210450 tokens > 200000 maximum (invalid_request_error)"},
		{name: "compatible context error", status: 413, errText: compatibleContextError, wantStatus: 400, wantType: "invalid_request_error", wantCode: "context_length_exceeded", wantMessage: "Input is too long for requested model."},
		{name: "Gemini context error", status: 400, errText: geminiContextError, wantStatus: 400, wantType: "invalid_request_error", wantCode: "context_length_exceeded", wantMessage: "The input token count (1048577) exceeds the maximum number of tokens allowed (1048576). (INVALID_ARGUMENT)"},
		{name: "a rate limit keeps its code", status: 429, errText: tokenRateLimitError, wantStatus: 429, passthrough: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			status, body := FormatErrorBody(ErrorFormatOpenAI, tc.status, tc.errText)
			if status != tc.wantStatus {
				t.Fatalf("status = %d, want %d", status, tc.wantStatus)
			}
			if tc.passthrough {
				if string(body) != tc.errText {
					t.Fatalf("body = %s, want the upstream's", body)
				}
				return
			}
			detail := gjson.GetBytes(body, "error")
			if detail.Get("type").String() != tc.wantType || detail.Get("code").String() != tc.wantCode {
				t.Fatalf("type/code = %s/%s, want %s/%s: %s", detail.Get("type"), detail.Get("code"), tc.wantType, tc.wantCode, body)
			}
			if got := detail.Get("message").String(); got != tc.wantMessage {
				t.Fatalf("message = %q, want %q", got, tc.wantMessage)
			}
			if !detail.Get("param").Exists() {
				t.Fatalf("param missing: %s", body)
			}
		})
	}
}

func TestFormatErrorBodyForGeminiClients(t *testing.T) {
	status, body := FormatErrorBody(ErrorFormatGemini, 429, geminiUpstreamError)
	if status != 429 || string(body) != geminiUpstreamError {
		t.Fatalf("Gemini upstream = %d %s, want it passed through", status, body)
	}
	status, body = FormatErrorBody(ErrorFormatGemini, 529, claudeUpstreamError)
	if status != 529 || gjson.GetBytes(body, "error.code").Int() != 529 || gjson.GetBytes(body, "error.status").String() != "UNAVAILABLE" {
		t.Fatalf("Claude upstream = %d %s", status, body)
	}
	if got := gjson.GetBytes(body, "error.message").String(); got != "Overloaded (overloaded_error)" {
		t.Fatalf("message = %q", got)
	}
	status, body = FormatErrorBody(ErrorFormatGemini, 400, openAIContextError)
	if status != 400 || gjson.GetBytes(body, "error.status").String() != "INVALID_ARGUMENT" {
		t.Fatalf("context error = %d %s", status, body)
	}
}

func TestFormatErrorBodyDefaults(t *testing.T) {
	status, body := FormatErrorBody(ErrorFormatClaude, 0, "")
	if status != http.StatusInternalServerError || gjson.GetBytes(body, "error.message").String() != "Internal Server Error" {
		t.Fatalf("defaults = %d %s", status, body)
	}
	// JSON that is not an object is plain text to the client.
	status, body = FormatErrorBody(ErrorFormatOpenAI, 502, `["a"]`)
	if status != 502 || gjson.GetBytes(body, "error.message").String() != `["a"]` {
		t.Fatalf("array body = %d %s", status, body)
	}
}

func TestWriteErrorResponseAs(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)

	NewBaseAPIHandlers(nil, nil).WriteErrorResponseAs(c, &interfaces.ErrorMessage{
		StatusCode: http.StatusRequestEntityTooLarge,
		Error:      errors.New(compatibleContextError),
	}, ErrorFormatClaude)

	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for an over-long prompt", recorder.Code)
	}
	body := recorder.Body.Bytes()
	if gjson.GetBytes(body, "error.type").String() != "invalid_request_error" || !gjson.GetBytes(body, "error.message").Exists() {
		t.Fatalf("body = %s", body)
	}
	if got := recorder.Header().Get("Content-Type"); got != "application/json" {
		t.Fatalf("Content-Type = %q", got)
	}
}

// The legacy writer keeps passing upstream JSON through, for SDK users that
// call it directly.
func TestWriteErrorResponseKeepsUpstreamJSON(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/", nil)
	NewBaseAPIHandlers(nil, nil).WriteErrorResponse(c, &interfaces.ErrorMessage{StatusCode: 529, Error: errors.New(claudeUpstreamError)})
	if recorder.Code != 529 || recorder.Body.String() != claudeUpstreamError {
		t.Fatalf("WriteErrorResponse = %d %s", recorder.Code, recorder.Body.String())
	}
}

func TestResponsesStreamErrorChunkMarksContextErrors(t *testing.T) {
	chunk := BuildOpenAIResponsesStreamErrorChunk(400, claudeContextError, 3)
	if got := gjson.GetBytes(chunk, "code").String(); got != "context_length_exceeded" {
		t.Fatalf("code = %q: %s", got, chunk)
	}
	chunk = BuildOpenAIResponsesStreamErrorChunk(429, codexUsageLimitError, 0)
	if got := gjson.GetBytes(chunk, "code").String(); got == "context_length_exceeded" {
		t.Fatalf("a usage limit was marked as a context error: %s", chunk)
	}
}
