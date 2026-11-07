package claude

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/interfaces"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v6/sdk/api/handlers"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v6/sdk/config"
	"github.com/tidwall/gjson"
)

// openAIContextError is how Codex and other OpenAI-compatible upstreams reject
// an over-long prompt.
const openAIContextError = `{"error":{"message":"Your input exceeds the context window of this model. Please adjust your input and try again.","type":"invalid_request_error","param":"input","code":"context_length_exceeded"}}`

// failingExecutor answers every request with err, like an upstream of another
// protocol that Claude Code was routed to.
type failingExecutor struct{ err error }

func (e *failingExecutor) Identifier() string { return "codex" }

func (e *failingExecutor) Execute(context.Context, *coreauth.Auth, coreexecutor.Request, coreexecutor.Options) (coreexecutor.Response, error) {
	return coreexecutor.Response{}, e.err
}

func (e *failingExecutor) ExecuteStream(context.Context, *coreauth.Auth, coreexecutor.Request, coreexecutor.Options) (*coreexecutor.StreamResult, error) {
	return nil, e.err
}

func (e *failingExecutor) Refresh(_ context.Context, auth *coreauth.Auth) (*coreauth.Auth, error) {
	return auth, nil
}

func (e *failingExecutor) CountTokens(context.Context, *coreauth.Auth, coreexecutor.Request, coreexecutor.Options) (coreexecutor.Response, error) {
	return coreexecutor.Response{}, e.err
}

func (e *failingExecutor) HttpRequest(context.Context, *coreauth.Auth, *http.Request) (*http.Response, error) {
	return nil, errors.New("not implemented")
}

func newFailingClaudeHandler(t *testing.T, err error) *ClaudeCodeAPIHandler {
	t.Helper()
	executor := &failingExecutor{err: err}
	manager := coreauth.NewManager(nil, nil, nil)
	manager.RegisterExecutor(executor)
	auth := &coreauth.Auth{ID: "auth-claude-error-format", Provider: executor.Identifier(), Status: coreauth.StatusActive}
	if _, regErr := manager.Register(context.Background(), auth); regErr != nil {
		t.Fatalf("Register auth: %v", regErr)
	}
	registry.GetGlobalRegistry().RegisterClient(auth.ID, auth.Provider, []*registry.ModelInfo{{ID: "test-model"}})
	t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(auth.ID) })
	return NewClaudeCodeAPIHandler(handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, manager))
}

// Claude Code compacts the conversation and retries when an Anthropic error
// says the prompt is too long; an OpenAI-shaped body would end the session.
func TestClaudeMessagesWritesAnthropicErrorForForeignUpstream(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, stream := range []bool{false, true} {
		h := newFailingClaudeHandler(t, &coreauth.Error{HTTPStatus: http.StatusBadRequest, Message: openAIContextError})
		recorder := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(recorder)
		body := `{"model":"test-model","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`
		if stream {
			body = `{"model":"test-model","max_tokens":16,"stream":true,"messages":[{"role":"user","content":"hi"}]}`
		}
		c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
		c.Request.Header.Set("Content-Type", "application/json")

		h.ClaudeMessages(c)

		got := recorder.Body.Bytes()
		if recorder.Code != http.StatusBadRequest {
			t.Fatalf("stream=%v: status = %d, want 400; body=%s", stream, recorder.Code, got)
		}
		if gjson.GetBytes(got, "type").String() != "error" || gjson.GetBytes(got, "error.type").String() != "invalid_request_error" {
			t.Fatalf("stream=%v: body is not an Anthropic invalid_request_error: %s", stream, got)
		}
		if message := gjson.GetBytes(got, "error.message").String(); !strings.HasPrefix(message, "prompt is too long") {
			t.Fatalf("stream=%v: message = %q, want the prompt-is-too-long wording", stream, message)
		}
	}
}

func TestForwardClaudeStreamTerminalErrorIsAnthropicEvent(t *testing.T) {
	gin.SetMode(gin.TestMode)
	h := NewClaudeCodeAPIHandler(handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, nil))
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	flusher, ok := c.Writer.(http.Flusher)
	if !ok {
		t.Fatal("gin writer does not implement http.Flusher")
	}

	data := make(chan []byte)
	errs := make(chan *interfaces.ErrorMessage, 1)
	errs <- &interfaces.ErrorMessage{StatusCode: http.StatusServiceUnavailable, Error: errors.New(`{"error":{"message":"The server is overloaded.","type":"server_error"}}`)}
	close(errs)
	h.forwardClaudeStream(c, flusher, func(error) {}, data, errs)

	out := recorder.Body.String()
	payload, found := strings.CutPrefix(out, "event: error\ndata: ")
	if !found {
		t.Fatalf("expected an Anthropic error event, got %q", out)
	}
	payload = strings.TrimSpace(payload)
	if gjson.Get(payload, "type").String() != "error" || gjson.Get(payload, "error.type").String() != "overloaded_error" {
		t.Fatalf("error event = %s, want an overloaded_error", payload)
	}
	if got := gjson.Get(payload, "error.message").String(); got != "The server is overloaded. (server_error)" {
		t.Fatalf("message = %q, want the upstream message and type", got)
	}
}
