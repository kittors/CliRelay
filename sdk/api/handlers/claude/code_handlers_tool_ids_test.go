package claude

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v6/sdk/api/handlers"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v6/sdk/config"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v6/sdk/translator"
	_ "github.com/router-for-me/CLIProxyAPI/v6/sdk/translator/builtin"
	"github.com/tidwall/gjson"
)

// kimiToolCallID is how Kimi names a tool call. The dot and colon are outside
// the alphabet Anthropic documents for tool_use ids, ^[a-zA-Z0-9_-]+$.
const kimiToolCallID = "functions.Bash:0"

var anthropicToolIDPattern = regexp.MustCompile(`^[a-zA-Z0-9_-]+$`)

// kimiExecutor stands in for an OpenAI-compatible upstream like Kimi serving
// a Claude client. It translates the request and the answer through the
// registry the way the real executors do, and keeps both the request it was
// handed and the one the upstream would have received.
type kimiExecutor struct {
	mu       sync.Mutex
	received [][]byte
	upstream [][]byte
}

func (e *kimiExecutor) Identifier() string { return "kimi-tool-ids" }

func (e *kimiExecutor) translate(req coreexecutor.Request, opts coreexecutor.Options) []byte {
	translated := sdktranslator.TranslateRequest(opts.SourceFormat, sdktranslator.FormatOpenAI, req.Model, req.Payload, opts.Stream)
	e.mu.Lock()
	defer e.mu.Unlock()
	e.received = append(e.received, bytes.Clone(req.Payload))
	e.upstream = append(e.upstream, translated)
	return translated
}

func (e *kimiExecutor) Execute(ctx context.Context, _ *coreauth.Auth, req coreexecutor.Request, opts coreexecutor.Options) (coreexecutor.Response, error) {
	translated := e.translate(req, opts)
	answer := `{"id":"chatcmpl-1","object":"chat.completion","created":1,"model":"kimi-k2","choices":[{"index":0,"message":{"role":"assistant","content":null,"tool_calls":[{"id":"` + kimiToolCallID + `","type":"function","function":{"name":"Bash","arguments":"{\"command\":\"ls\"}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":3,"completion_tokens":5,"total_tokens":8}}`
	var param any
	out := sdktranslator.TranslateNonStream(ctx, sdktranslator.FormatOpenAI, opts.SourceFormat, req.Model, opts.OriginalRequest, translated, []byte(answer), &param)
	return coreexecutor.Response{Payload: []byte(out)}, nil
}

func (e *kimiExecutor) ExecuteStream(ctx context.Context, _ *coreauth.Auth, req coreexecutor.Request, opts coreexecutor.Options) (*coreexecutor.StreamResult, error) {
	translated := e.translate(req, opts)
	lines := []string{
		`data: {"id":"chatcmpl-1","object":"chat.completion.chunk","created":1,"model":"kimi-k2","choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"` + kimiToolCallID + `","type":"function","function":{"name":"Bash","arguments":"{\"command\":"}}]},"finish_reason":null}]}`,
		`data: {"id":"chatcmpl-1","object":"chat.completion.chunk","created":1,"model":"kimi-k2","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"ls\"}"}}]},"finish_reason":null}]}`,
		`data: {"id":"chatcmpl-1","object":"chat.completion.chunk","created":1,"model":"kimi-k2","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
		`data: [DONE]`,
	}
	chunks := make(chan coreexecutor.StreamChunk)
	go func() {
		defer close(chunks)
		var param any
		for _, line := range lines {
			for _, out := range sdktranslator.TranslateStream(ctx, sdktranslator.FormatOpenAI, opts.SourceFormat, req.Model, opts.OriginalRequest, translated, []byte(line), &param) {
				chunks <- coreexecutor.StreamChunk{Payload: []byte(out)}
			}
		}
	}()
	return &coreexecutor.StreamResult{Chunks: chunks}, nil
}

func (e *kimiExecutor) Refresh(_ context.Context, auth *coreauth.Auth) (*coreauth.Auth, error) {
	return auth, nil
}

func (e *kimiExecutor) CountTokens(context.Context, *coreauth.Auth, coreexecutor.Request, coreexecutor.Options) (coreexecutor.Response, error) {
	return coreexecutor.Response{}, errors.New("not implemented")
}

func (e *kimiExecutor) HttpRequest(context.Context, *coreauth.Auth, *http.Request) (*http.Response, error) {
	return nil, errors.New("not implemented")
}

func newKimiClaudeHandler(t *testing.T) (*ClaudeCodeAPIHandler, *kimiExecutor) {
	t.Helper()
	executor := &kimiExecutor{}
	manager := coreauth.NewManager(nil, nil, nil)
	manager.RegisterExecutor(executor)
	auth := &coreauth.Auth{ID: "auth-claude-tool-ids", Provider: executor.Identifier(), Status: coreauth.StatusActive}
	if _, err := manager.Register(context.Background(), auth); err != nil {
		t.Fatalf("Register auth: %v", err)
	}
	registry.GetGlobalRegistry().RegisterClient(auth.ID, auth.Provider, []*registry.ModelInfo{{ID: "kimi-tool-ids-model"}})
	t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(auth.ID) })
	return NewClaudeCodeAPIHandler(handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, manager)), executor
}

func postClaudeMessages(t *testing.T, h *ClaudeCodeAPIHandler, body string) string {
	t.Helper()
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	h.ClaudeMessages(c)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status %d: %s", recorder.Code, recorder.Body.String())
	}
	return recorder.Body.String()
}

// streamedToolUseID returns the id of the tool_use block in a Claude stream.
func streamedToolUseID(t *testing.T, stream string) string {
	t.Helper()
	for _, line := range strings.Split(stream, "\n") {
		payload, ok := strings.CutPrefix(line, "data: ")
		if !ok || gjson.Get(payload, "type").String() != "content_block_start" {
			continue
		}
		if block := gjson.Get(payload, "content_block"); block.Get("type").String() == "tool_use" {
			return block.Get("id").String()
		}
	}
	t.Fatalf("no tool_use block in stream:\n%s", stream)
	return ""
}

func TestClaudeMessagesEncodesForeignToolIDsAndDecodesThemForTheUpstream(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, stream := range []bool{false, true} {
		h, executor := newKimiClaudeHandler(t)
		streamField := `"stream":false`
		if stream {
			streamField = `"stream":true`
		}

		reply := postClaudeMessages(t, h, `{"model":"kimi-tool-ids-model","max_tokens":64,`+streamField+`,"messages":[{"role":"user","content":"list the files"}]}`)
		var id string
		if stream {
			id = streamedToolUseID(t, reply)
		} else {
			id = gjson.Get(reply, `content.#(type=="tool_use").id`).String()
		}
		if id == kimiToolCallID || !anthropicToolIDPattern.MatchString(id) {
			t.Fatalf("stream=%v: Claude client got tool_use id %q", stream, id)
		}
		if strings.Contains(reply, kimiToolCallID) {
			t.Fatalf("stream=%v: the upstream id reached the client:\n%s", stream, reply)
		}

		// The client sends the call and its result back with the id it got.
		postClaudeMessages(t, h, `{"model":"kimi-tool-ids-model","max_tokens":64,`+streamField+`,"messages":[`+
			`{"role":"user","content":"list the files"},`+
			`{"role":"assistant","content":[{"type":"tool_use","id":"`+id+`","name":"Bash","input":{"command":"ls"}}]},`+
			`{"role":"user","content":[{"type":"tool_result","tool_use_id":"`+id+`","content":"a.go"}]}]}`)

		executor.mu.Lock()
		received, upstream := executor.received[1], executor.upstream[1]
		executor.mu.Unlock()
		// The executor is handed the request as the client sent it; only its
		// translation for the OpenAI-compatible upstream carries Kimi's id.
		if !bytes.Contains(received, []byte(id)) || bytes.Contains(received, []byte(kimiToolCallID)) {
			t.Fatalf("stream=%v: executor was handed %s", stream, received)
		}
		if got := gjson.GetBytes(upstream, `messages.#(role=="assistant").tool_calls.0.id`).String(); got != kimiToolCallID {
			t.Fatalf("stream=%v: upstream tool_calls id = %q in %s", stream, got, upstream)
		}
		if got := gjson.GetBytes(upstream, `messages.#(role=="tool").tool_call_id`).String(); got != kimiToolCallID {
			t.Fatalf("stream=%v: upstream tool_call_id = %q in %s", stream, got, upstream)
		}
	}
}
