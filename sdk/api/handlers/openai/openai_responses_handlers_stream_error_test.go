package openai

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
)

type successfulResponsesStreamExecutor struct{}

func (e *successfulResponsesStreamExecutor) Identifier() string { return "codex" }

func (e *successfulResponsesStreamExecutor) Execute(context.Context, *coreauth.Auth, coreexecutor.Request, coreexecutor.Options) (coreexecutor.Response, error) {
	return coreexecutor.Response{}, errors.New("not implemented")
}

func (e *successfulResponsesStreamExecutor) ExecuteStream(context.Context, *coreauth.Auth, coreexecutor.Request, coreexecutor.Options) (*coreexecutor.StreamResult, error) {
	ch := make(chan coreexecutor.StreamChunk, 1)
	ch <- coreexecutor.StreamChunk{Payload: []byte("event: response.completed\ndata: {\"type\":\"response.completed\"}")}
	close(ch)
	return &coreexecutor.StreamResult{Chunks: ch}, nil
}

func (e *successfulResponsesStreamExecutor) Refresh(ctx context.Context, auth *coreauth.Auth) (*coreauth.Auth, error) {
	return auth, nil
}

func (e *successfulResponsesStreamExecutor) CountTokens(context.Context, *coreauth.Auth, coreexecutor.Request, coreexecutor.Options) (coreexecutor.Response, error) {
	return coreexecutor.Response{}, errors.New("not implemented")
}

func (e *successfulResponsesStreamExecutor) HttpRequest(context.Context, *coreauth.Auth, *http.Request) (*http.Response, error) {
	return nil, errors.New("not implemented")
}

type startFailureResponsesStreamExecutor struct{}

func (e *startFailureResponsesStreamExecutor) Identifier() string { return "codex" }

func (e *startFailureResponsesStreamExecutor) Execute(context.Context, *coreauth.Auth, coreexecutor.Request, coreexecutor.Options) (coreexecutor.Response, error) {
	return coreexecutor.Response{}, errors.New("not implemented")
}

func (e *startFailureResponsesStreamExecutor) ExecuteStream(context.Context, *coreauth.Auth, coreexecutor.Request, coreexecutor.Options) (*coreexecutor.StreamResult, error) {
	return nil, &coreauth.Error{
		Code:       "no_auth_available",
		Message:    "no auth available",
		Retryable:  true,
		HTTPStatus: http.StatusServiceUnavailable,
	}
}

func (e *startFailureResponsesStreamExecutor) Refresh(ctx context.Context, auth *coreauth.Auth) (*coreauth.Auth, error) {
	return auth, nil
}

func (e *startFailureResponsesStreamExecutor) CountTokens(context.Context, *coreauth.Auth, coreexecutor.Request, coreexecutor.Options) (coreexecutor.Response, error) {
	return coreexecutor.Response{}, errors.New("not implemented")
}

func (e *startFailureResponsesStreamExecutor) HttpRequest(context.Context, *coreauth.Auth, *http.Request) (*http.Response, error) {
	return nil, errors.New("not implemented")
}

func TestForwardResponsesStreamTerminalErrorUsesResponsesErrorChunk(t *testing.T) {
	gin.SetMode(gin.TestMode)
	base := handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, nil)
	h := NewOpenAIResponsesAPIHandler(base)

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)

	flusher, ok := c.Writer.(http.Flusher)
	if !ok {
		t.Fatalf("expected gin writer to implement http.Flusher")
	}

	data := make(chan []byte)
	errs := make(chan *interfaces.ErrorMessage, 1)
	errs <- &interfaces.ErrorMessage{StatusCode: http.StatusInternalServerError, Error: errors.New("unexpected EOF")}
	close(errs)

	h.forwardResponsesStream(c, flusher, func(error) {}, data, errs)
	body := recorder.Body.String()
	if !strings.Contains(body, `"type":"error"`) {
		t.Fatalf("expected responses error chunk, got: %q", body)
	}
	if strings.Contains(body, `"error":{`) {
		t.Fatalf("expected streaming error chunk (top-level type), got HTTP error body: %q", body)
	}
}

func TestOpenAIResponsesStreamingSuccessOverridesPreset404Status(t *testing.T) {
	gin.SetMode(gin.TestMode)

	executor := &successfulResponsesStreamExecutor{}
	manager := coreauth.NewManager(nil, nil, nil)
	manager.RegisterExecutor(executor)

	auth := &coreauth.Auth{ID: "auth-success", Provider: executor.Identifier(), Status: coreauth.StatusActive}
	if _, err := manager.Register(context.Background(), auth); err != nil {
		t.Fatalf("Register auth: %v", err)
	}
	registry.GetGlobalRegistry().RegisterClient(auth.ID, auth.Provider, []*registry.ModelInfo{{ID: "test-model"}})
	t.Cleanup(func() {
		registry.GetGlobalRegistry().UnregisterClient(auth.ID)
	})

	base := handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, manager)
	h := NewOpenAIResponsesAPIHandler(base)

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"test-model","stream":true}`))
	c.Request.Header.Set("Content-Type", "application/json")

	// Simulate the nested grouped-path rewrite path, which enters the handler
	// after Gin has already preloaded a 404 status in NoRoute.
	c.Status(http.StatusNotFound)

	h.Responses(c)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", recorder.Code, http.StatusOK, recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), `"type":"response.completed"`) {
		t.Fatalf("expected successful SSE body, got %q", recorder.Body.String())
	}
}

func TestOpenAIResponsesStreamingStartFailureReturnsJSONErrorBeforeSSE(t *testing.T) {
	gin.SetMode(gin.TestMode)

	executor := &startFailureResponsesStreamExecutor{}
	manager := coreauth.NewManager(nil, nil, nil)
	manager.RegisterExecutor(executor)

	auth := &coreauth.Auth{ID: "auth-start-failure", Provider: executor.Identifier(), Status: coreauth.StatusActive}
	if _, err := manager.Register(context.Background(), auth); err != nil {
		t.Fatalf("Register auth: %v", err)
	}
	registry.GetGlobalRegistry().RegisterClient(auth.ID, auth.Provider, []*registry.ModelInfo{{ID: "test-model"}})
	t.Cleanup(func() {
		registry.GetGlobalRegistry().UnregisterClient(auth.ID)
	})

	base := handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, manager)
	h := NewOpenAIResponsesAPIHandler(base)

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"test-model","stream":true}`))
	c.Request.Header.Set("Content-Type", "application/json")

	h.Responses(c)

	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d; body=%s", recorder.Code, http.StatusServiceUnavailable, recorder.Body.String())
	}
	if got := recorder.Header().Get("Content-Type"); !strings.Contains(got, "application/json") {
		t.Fatalf("Content-Type = %q, want application/json", got)
	}
	if strings.Contains(recorder.Body.String(), "event: error") {
		t.Fatalf("expected JSON error before SSE starts, got %q", recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), "no auth available") {
		t.Fatalf("expected upstream start error body, got %q", recorder.Body.String())
	}
}
