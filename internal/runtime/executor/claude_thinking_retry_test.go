package executor

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v6/sdk/translator"
	"github.com/tidwall/gjson"
)

// Anthropic's error bodies, as the API returns them.
const (
	claudeEnabledRejected        = `{"type":"error","error":{"type":"invalid_request_error","message":"\"thinking.type.enabled\" is not supported for this model. Use \"thinking.type.adaptive\" and \"output_config.effort\" to control thinking behavior."}}`
	claudeDisabledRejected       = `{"type":"error","error":{"type":"invalid_request_error","message":"\"thinking.type.disabled\" is not supported for this model. Use \"thinking.type.adaptive\" and \"output_config.effort\" to control thinking behavior."}}`
	claudeDisabledRejectedSonnet = `{"type":"error","error":{"type":"invalid_request_error","message":"\"thinking.type.disabled\" is not supported for this model. Use \"thinking.type.between_tools\" for the lowest thinking setting, or \"thinking.type.adaptive\" and \"output_config.effort\" to control thinking behavior."}}`
	claudeEffortRejectedWhenOff  = `{"type":"error","error":{"type":"invalid_request_error","message":"output_config.effort 'xhigh' is not supported when thinking is disabled on this model. Use effort 'high' or below, or enable thinking."}}`
)

func TestRewriteRejectedClaudeThinking(t *testing.T) {
	cases := []struct {
		name         string
		body         string
		requested    string
		status       int
		errBody      string
		wantOK       bool
		wantThinking string
		wantEffort   string
	}{
		{name: "budget becomes effort", body: `{"thinking":{"type":"enabled","budget_tokens":24576}}`, status: 400, errBody: claudeEnabledRejected, wantOK: true, wantThinking: `{"type":"adaptive","display":"summarized"}`, wantEffort: "high"},
		{name: "max budget", body: `{"thinking":{"type":"enabled","budget_tokens":31999}}`, status: 400, errBody: claudeEnabledRejected, wantOK: true, wantThinking: `{"type":"adaptive","display":"summarized"}`, wantEffort: "xhigh"},
		{name: "small budget is low", body: `{"thinking":{"type":"enabled","budget_tokens":512}}`, status: 400, errBody: claudeEnabledRejected, wantOK: true, wantThinking: `{"type":"adaptive","display":"summarized"}`, wantEffort: "low"},
		{name: "existing effort wins", body: `{"thinking":{"type":"enabled","budget_tokens":1024},"output_config":{"effort":"max"}}`, status: 400, errBody: claudeEnabledRejected, wantOK: true, wantThinking: `{"type":"adaptive","display":"summarized"}`, wantEffort: "max"},
		{name: "enabled without budget", body: `{"thinking":{"type":"enabled"}}`, status: 400, errBody: claudeEnabledRejected, wantOK: true, wantThinking: `{"type":"adaptive","display":"summarized"}`},
		{name: "disabled on an always-on model", body: `{"thinking":{"type":"disabled"}}`, status: 400, errBody: claudeDisabledRejected, wantOK: true, wantEffort: "low"},
		{name: "disabled keeps the client's effort", body: `{"thinking":{"type":"disabled"},"output_config":{"effort":"medium"}}`, status: 400, errBody: claudeDisabledRejected, wantOK: true, wantEffort: "medium"},
		{name: "Sonnet 5.5 names between_tools", body: `{"thinking":{"type":"disabled"},"output_config":{"effort":"xhigh"}}`, status: 400, errBody: claudeDisabledRejectedSonnet, wantOK: true, wantThinking: `{"type":"between_tools"}`, wantEffort: "high"},
		{name: "plain text error", body: `{"thinking":{"type":"enabled","budget_tokens":8192}}`, status: 400, errBody: `"thinking.type.enabled" is not supported for this model.`, wantOK: true, wantThinking: `{"type":"adaptive","display":"summarized"}`, wantEffort: "medium"},
		{name: "other 400", body: `{"thinking":{"type":"enabled","budget_tokens":8192}}`, status: 400, errBody: `{"type":"error","error":{"type":"invalid_request_error","message":"temperature: not supported"}}`},
		{name: "not a 400", body: `{"thinking":{"type":"enabled","budget_tokens":8192}}`, status: 500, errBody: claudeEnabledRejected},
		{name: "body already adaptive", body: `{"thinking":{"type":"adaptive"}}`, status: 400, errBody: claudeEnabledRejected},
		{name: "error names the other type", body: `{"thinking":{"type":"enabled","budget_tokens":8192}}`, status: 400, errBody: claudeDisabledRejected},
		// The budget in the body may already be capped below max_tokens (max
		// becomes 31999 under the default 32000), so the client's own level wins.
		{name: "requested level beats a capped budget", body: `{"max_tokens":32000,"thinking":{"type":"enabled","budget_tokens":31999}}`, requested: "max", status: 400, errBody: claudeEnabledRejected, wantOK: true, wantThinking: `{"type":"adaptive","display":"summarized"}`, wantEffort: "max"},
		{name: "requested budget", body: `{"max_tokens":4096,"thinking":{"type":"enabled","budget_tokens":4095}}`, requested: "24576", status: 400, errBody: claudeEnabledRejected, wantOK: true, wantThinking: `{"type":"adaptive","display":"summarized"}`, wantEffort: "high"},
		{name: "requested minimal", body: `{"thinking":{"type":"enabled","budget_tokens":1024}}`, requested: "minimal", status: 400, errBody: claudeEnabledRejected, wantOK: true, wantThinking: `{"type":"adaptive","display":"summarized"}`, wantEffort: "low"},
		{name: "requested none falls back to the budget", body: `{"thinking":{"type":"enabled","budget_tokens":8192}}`, requested: "none", status: 400, errBody: claudeEnabledRejected, wantOK: true, wantThinking: `{"type":"adaptive","display":"summarized"}`, wantEffort: "medium"},
		{name: "client effort beats requested level", body: `{"thinking":{"type":"enabled","budget_tokens":8192},"output_config":{"effort":"low"}}`, requested: "max", status: 400, errBody: claudeEnabledRejected, wantOK: true, wantThinking: `{"type":"adaptive","display":"summarized"}`, wantEffort: "low"},
		{name: "effort too high to turn thinking off", body: `{"thinking":{"type":"disabled"},"output_config":{"effort":"xhigh"}}`, status: 400, errBody: claudeEffortRejectedWhenOff, wantOK: true, wantThinking: `{"type":"disabled"}`, wantEffort: "high"},
		{name: "effort already at high", body: `{"thinking":{"type":"between_tools"},"output_config":{"effort":"high"}}`, status: 400, errBody: claudeEffortRejectedWhenOff},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, ok := rewriteRejectedClaudeThinking([]byte(tc.body), tc.requested, tc.status, []byte(tc.errBody))
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
			if !ok {
				return
			}
			if got := gjson.GetBytes(out, "thinking").Raw; got != tc.wantThinking {
				t.Fatalf("thinking = %s, want %q", got, tc.wantThinking)
			}
			if got := gjson.GetBytes(out, "output_config.effort").String(); got != tc.wantEffort {
				t.Fatalf("effort = %q, want %q", got, tc.wantEffort)
			}
		})
	}
}

// claudeRecordingServer answers each request with the next status and body,
// repeating the last pair, and keeps the request bodies it saw.
type claudeRecordingServer struct {
	mu        sync.Mutex
	responses []claudeCannedResponse
	bodies    [][]byte
}

type claudeCannedResponse struct {
	status      int
	contentType string
	body        string
}

func (s *claudeRecordingServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	s.mu.Lock()
	s.bodies = append(s.bodies, body)
	next := s.responses[min(len(s.bodies), len(s.responses))-1]
	s.mu.Unlock()
	w.Header().Set("Content-Type", next.contentType)
	w.WriteHeader(next.status)
	_, _ = w.Write([]byte(next.body))
}

func (s *claudeRecordingServer) requests() [][]byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([][]byte(nil), s.bodies...)
}

func claudeRetryTestAuth(baseURL, apiKey string) *cliproxyauth.Auth {
	return &cliproxyauth.Auth{Attributes: map[string]string{"api_key": apiKey, "base_url": baseURL}}
}

const claudeOKMessage = `{"id":"msg_1","type":"message","model":"claude-opus-6","role":"assistant","content":[{"type":"text","text":"ok"}],"usage":{"input_tokens":1,"output_tokens":1}}`

// claude-opus-6 is not in the catalog, so nothing knows it is adaptive-only
// until the upstream says so.
const claudeUnknownModelBudgetRequest = `{"model":"claude-opus-6","max_tokens":32000,"thinking":{"type":"enabled","budget_tokens":24576},"messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}]}`

func TestClaudeExecutorRetriesRejectedBudgetAsAdaptive(t *testing.T) {
	upstream := &claudeRecordingServer{responses: []claudeCannedResponse{
		{status: http.StatusBadRequest, contentType: "application/json", body: claudeEnabledRejected},
		{status: http.StatusOK, contentType: "application/json", body: claudeOKMessage},
	}}
	server := httptest.NewServer(upstream)
	defer server.Close()

	executor := NewClaudeExecutor(&config.Config{})
	resp, err := executor.Execute(context.Background(), claudeRetryTestAuth(server.URL, "key-123"), cliproxyexecutor.Request{
		Model:   "claude-opus-6",
		Payload: []byte(claudeUnknownModelBudgetRequest),
	}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("claude")})
	if err != nil {
		t.Fatalf("Execute error: %v", err)
	}
	if got := gjson.GetBytes(resp.Payload, "content.0.text").String(); got != "ok" {
		t.Fatalf("response = %s", resp.Payload)
	}

	requests := upstream.requests()
	if len(requests) != 2 {
		t.Fatalf("upstream saw %d requests, want 2", len(requests))
	}
	if got := gjson.GetBytes(requests[0], "thinking.type").String(); got != "enabled" {
		t.Fatalf("first attempt thinking.type = %q, want the original budget form", got)
	}
	if got := gjson.GetBytes(requests[1], "thinking").Raw; got != `{"type":"adaptive","display":"summarized"}` {
		t.Fatalf("retry thinking = %s", got)
	}
	if got := gjson.GetBytes(requests[1], "output_config.effort").String(); got != "high" {
		t.Fatalf("retry effort = %q, want high", got)
	}
}

func TestClaudeExecutorStreamRetriesRejectedDisabledThinking(t *testing.T) {
	sse := strings.Join([]string{
		`event: message_start`,
		`data: {"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","model":"claude-opus-6","content":[],"usage":{"input_tokens":1,"output_tokens":1}}}`,
		``,
		`event: message_stop`,
		`data: {"type":"message_stop"}`,
		``,
	}, "\n")
	upstream := &claudeRecordingServer{responses: []claudeCannedResponse{
		{status: http.StatusBadRequest, contentType: "application/json", body: claudeDisabledRejectedSonnet},
		{status: http.StatusOK, contentType: "text/event-stream", body: sse},
	}}
	server := httptest.NewServer(upstream)
	defer server.Close()

	// An OAuth token, so the retry also has to keep the tool-name prefix.
	payload := `{"model":"claude-opus-6","max_tokens":32000,"stream":true,"thinking":{"type":"disabled"},"output_config":{"effort":"max"},"tools":[{"name":"lookup","input_schema":{"type":"object"}}],"messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}]}`
	executor := NewClaudeExecutor(&config.Config{})
	result, err := executor.ExecuteStream(context.Background(), claudeRetryTestAuth(server.URL, "sk-ant-oat01-retry-test"), cliproxyexecutor.Request{
		Model:   "claude-opus-6",
		Payload: []byte(payload),
	}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("claude")})
	if err != nil {
		t.Fatalf("ExecuteStream error: %v", err)
	}
	var streamed strings.Builder
	for chunk := range result.Chunks {
		if chunk.Err != nil {
			t.Fatalf("stream error: %v", chunk.Err)
		}
		streamed.Write(chunk.Payload)
	}
	if !strings.Contains(streamed.String(), "message_stop") {
		t.Fatalf("stream = %q", streamed.String())
	}

	requests := upstream.requests()
	if len(requests) != 2 {
		t.Fatalf("upstream saw %d requests, want 2", len(requests))
	}
	if got := gjson.GetBytes(requests[1], "thinking").Raw; got != `{"type":"between_tools"}` {
		t.Fatalf("retry thinking = %s", got)
	}
	if got := gjson.GetBytes(requests[1], "output_config.effort").String(); got != "high" {
		t.Fatalf("retry effort = %q, want high (between_tools stops there)", got)
	}
	for i, request := range requests {
		if got := gjson.GetBytes(request, "tools.0.name").String(); got != claudeToolPrefix+"lookup" {
			t.Fatalf("attempt %d tool name = %q, want the OAuth prefix", i+1, got)
		}
	}
}

// One retry, and only for the thinking-shape errors.
func TestClaudeExecutorThinkingRetryIsBounded(t *testing.T) {
	cases := []struct {
		name         string
		errBody      string
		wantRequests int
	}{
		{name: "same rejection twice", errBody: claudeEnabledRejected, wantRequests: 2},
		{name: "unrelated 400", errBody: `{"type":"error","error":{"type":"invalid_request_error","message":"messages: at least one message is required"}}`, wantRequests: 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			upstream := &claudeRecordingServer{responses: []claudeCannedResponse{
				{status: http.StatusBadRequest, contentType: "application/json", body: tc.errBody},
			}}
			server := httptest.NewServer(upstream)
			defer server.Close()

			executor := NewClaudeExecutor(&config.Config{})
			_, err := executor.Execute(context.Background(), claudeRetryTestAuth(server.URL, "key-123"), cliproxyexecutor.Request{
				Model:   "claude-opus-6",
				Payload: []byte(claudeUnknownModelBudgetRequest),
			}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("claude")})
			status, ok := err.(statusErr)
			if !ok || status.StatusCode() != http.StatusBadRequest {
				t.Fatalf("err = %v, want the upstream 400", err)
			}
			if !strings.Contains(status.Error(), "not supported") && !strings.Contains(status.Error(), "required") {
				t.Fatalf("err = %v, want the upstream message", err)
			}
			if got := len(upstream.requests()); got != tc.wantRequests {
				t.Fatalf("upstream saw %d requests, want %d", got, tc.wantRequests)
			}
		})
	}
}

// A model the catalog knows gets adaptive thinking on the first attempt.
func TestClaudeExecutorSendsAdaptiveThinkingToKnownModels(t *testing.T) {
	upstream := &claudeRecordingServer{responses: []claudeCannedResponse{
		{status: http.StatusOK, contentType: "application/json", body: claudeOKMessage},
	}}
	server := httptest.NewServer(upstream)
	defer server.Close()

	payload := `{"model":"claude-opus-5-5","max_tokens":32000,"thinking":{"type":"enabled","budget_tokens":24576},"messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}]}`
	executor := NewClaudeExecutor(&config.Config{})
	if _, err := executor.Execute(context.Background(), claudeRetryTestAuth(server.URL, "key-123"), cliproxyexecutor.Request{
		Model:   "claude-opus-5-5",
		Payload: []byte(payload),
	}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("claude")}); err != nil {
		t.Fatalf("Execute error: %v", err)
	}
	requests := upstream.requests()
	if len(requests) != 1 {
		t.Fatalf("upstream saw %d requests, want 1", len(requests))
	}
	if got := gjson.GetBytes(requests[0], "thinking.type").String(); got != "adaptive" {
		t.Fatalf("thinking.type = %q, want adaptive: %s", got, requests[0])
	}
	if got := gjson.GetBytes(requests[0], "output_config.effort").String(); got != "high" {
		t.Fatalf("effort = %q, want high", got)
	}
}

// A Chat client asks for "max" on a model the catalog does not know. The
// budget the proxy sends is capped below max_tokens, but the retry still asks
// for the level the client chose.
func TestClaudeExecutorRetryUsesTheClientsLevel(t *testing.T) {
	sse := strings.Join([]string{
		`event: message_start`,
		`data: {"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","model":"claude-opus-6","content":[],"usage":{"input_tokens":1,"output_tokens":1}}}`,
		``,
		`event: content_block_start`,
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		``,
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"ok"}}`,
		``,
		`event: content_block_stop`,
		`data: {"type":"content_block_stop","index":0}`,
		``,
		`event: message_delta`,
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":1}}`,
		``,
		`event: message_stop`,
		`data: {"type":"message_stop"}`,
		``,
	}, "\n")
	upstream := &claudeRecordingServer{responses: []claudeCannedResponse{
		{status: http.StatusBadRequest, contentType: "application/json", body: claudeEnabledRejected},
		{status: http.StatusOK, contentType: "text/event-stream", body: sse},
	}}
	server := httptest.NewServer(upstream)
	defer server.Close()

	payload := `{"model":"claude-opus-6","reasoning_effort":"max","messages":[{"role":"user","content":"hi"}]}`
	executor := NewClaudeExecutor(&config.Config{})
	resp, err := executor.Execute(context.Background(), claudeRetryTestAuth(server.URL, "key-123"), cliproxyexecutor.Request{
		Model:   "claude-opus-6",
		Payload: []byte(payload),
	}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("openai"), OriginalRequest: []byte(payload)})
	if err != nil {
		t.Fatalf("Execute error: %v", err)
	}
	if got := gjson.GetBytes(resp.Payload, "choices.0.message.content").String(); got != "ok" {
		t.Fatalf("response = %s", resp.Payload)
	}

	requests := upstream.requests()
	if len(requests) != 2 {
		t.Fatalf("upstream saw %d requests, want 2", len(requests))
	}
	if got := gjson.GetBytes(requests[0], "thinking.budget_tokens").Int(); got != 31999 {
		t.Fatalf("first attempt budget_tokens = %d, want 31999 (capped below max_tokens)", got)
	}
	if got := gjson.GetBytes(requests[1], "output_config.effort").String(); got != "max" {
		t.Fatalf("retry effort = %q, want max", got)
	}
}
