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

type compactCaptureExecutor struct {
	provider     string
	alt          string
	sourceFormat string
	calls        int
	models       []string
	failures     map[string]error
}

func (e *compactCaptureExecutor) Identifier() string {
	if strings.TrimSpace(e.provider) != "" {
		return e.provider
	}
	return "test-provider"
}

func (e *compactCaptureExecutor) Execute(ctx context.Context, auth *coreauth.Auth, req coreexecutor.Request, opts coreexecutor.Options) (coreexecutor.Response, error) {
	e.calls++
	e.alt = opts.Alt
	e.sourceFormat = opts.SourceFormat.String()
	e.models = append(e.models, req.Model)
	if err := e.failures[req.Model]; err != nil {
		return coreexecutor.Response{}, err
	}
	return coreexecutor.Response{Payload: []byte(`{"ok":true}`)}, nil
}

func (e *compactCaptureExecutor) ExecuteStream(context.Context, *coreauth.Auth, coreexecutor.Request, coreexecutor.Options) (*coreexecutor.StreamResult, error) {
	return nil, errors.New("not implemented")
}

func (e *compactCaptureExecutor) Refresh(ctx context.Context, auth *coreauth.Auth) (*coreauth.Auth, error) {
	return auth, nil
}

func (e *compactCaptureExecutor) CountTokens(context.Context, *coreauth.Auth, coreexecutor.Request, coreexecutor.Options) (coreexecutor.Response, error) {
	return coreexecutor.Response{}, errors.New("not implemented")
}

func (e *compactCaptureExecutor) HttpRequest(context.Context, *coreauth.Auth, *http.Request) (*http.Response, error) {
	return nil, errors.New("not implemented")
}

func TestOpenAIResponsesCompactRejectsStream(t *testing.T) {
	gin.SetMode(gin.TestMode)
	executor := &compactCaptureExecutor{provider: "codex"}
	manager := coreauth.NewManager(nil, nil, nil)
	manager.RegisterExecutor(executor)

	auth := &coreauth.Auth{ID: "auth1", Provider: executor.Identifier(), Status: coreauth.StatusActive}
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
	router.POST("/v1/responses/compact", h.Compact)

	req := httptest.NewRequest(http.MethodPost, "/v1/responses/compact", strings.NewReader(`{"model":"test-model","stream":true}`))
	req.Header.Set("Content-Type", "application/json")
	resp := httptest.NewRecorder()
	router.ServeHTTP(resp, req)

	if resp.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", resp.Code, http.StatusBadRequest)
	}
	if executor.calls != 0 {
		t.Fatalf("executor calls = %d, want 0", executor.calls)
	}
}

func TestOpenAIResponsesCompactExecute(t *testing.T) {
	gin.SetMode(gin.TestMode)
	executor := &compactCaptureExecutor{provider: "codex"}
	manager := coreauth.NewManager(nil, nil, nil)
	manager.RegisterExecutor(executor)

	auth := &coreauth.Auth{ID: "auth2", Provider: executor.Identifier(), Status: coreauth.StatusActive}
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
	router.POST("/v1/responses/compact", h.Compact)

	req := httptest.NewRequest(http.MethodPost, "/v1/responses/compact", strings.NewReader(`{"model":"test-model","input":"hello"}`))
	req.Header.Set("Content-Type", "application/json")
	resp := httptest.NewRecorder()
	router.ServeHTTP(resp, req)

	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", resp.Code, http.StatusOK)
	}
	if executor.alt != "responses/compact" {
		t.Fatalf("alt = %q, want %q", executor.alt, "responses/compact")
	}
	if executor.sourceFormat != "openai-response" {
		t.Fatalf("source format = %q, want %q", executor.sourceFormat, "openai-response")
	}
	if strings.TrimSpace(resp.Body.String()) != `{"ok":true}` {
		t.Fatalf("body = %s", resp.Body.String())
	}
}

type compactStatusErr struct {
	code int
	msg  string
}

func (e compactStatusErr) Error() string   { return e.msg }
func (e compactStatusErr) StatusCode() int { return e.code }

func TestOpenAIResponsesCompactFallsBackToSupportedModel(t *testing.T) {
	gin.SetMode(gin.TestMode)
	executor := &compactCaptureExecutor{
		provider: "codex",
		failures: map[string]error{
			"test-model": compactStatusErr{code: http.StatusNotFound, msg: `{"error":"compact unsupported"}`},
		},
	}
	manager := coreauth.NewManager(nil, nil, nil)
	manager.RegisterExecutor(executor)

	auth := &coreauth.Auth{ID: "auth3", Provider: executor.Identifier(), Status: coreauth.StatusActive}
	if _, err := manager.Register(context.Background(), auth); err != nil {
		t.Fatalf("Register auth: %v", err)
	}
	registry.GetGlobalRegistry().RegisterClient(auth.ID, auth.Provider, []*registry.ModelInfo{
		{ID: "test-model"},
		{ID: "gpt-5.5"},
	})
	t.Cleanup(func() {
		registry.GetGlobalRegistry().UnregisterClient(auth.ID)
	})

	base := handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, manager)
	h := NewOpenAIResponsesAPIHandler(base)
	router := gin.New()
	router.POST("/v1/responses/compact", h.Compact)

	req := httptest.NewRequest(http.MethodPost, "/v1/responses/compact", strings.NewReader(`{"model":"test-model","input":"hello"}`))
	req.Header.Set("Content-Type", "application/json")
	resp := httptest.NewRecorder()
	router.ServeHTTP(resp, req)

	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", resp.Code, http.StatusOK)
	}
	if len(executor.models) != 2 {
		t.Fatalf("models = %#v, want 2 attempts", executor.models)
	}
	if executor.models[0] != "test-model" {
		t.Fatalf("first model = %q, want %q", executor.models[0], "test-model")
	}
	if executor.models[1] != "gpt-5.5" {
		t.Fatalf("fallback model = %q, want %q", executor.models[1], "gpt-5.5")
	}
}

func TestOpenAIResponsesCompactFallsBackOnModelNotSupported(t *testing.T) {
	gin.SetMode(gin.TestMode)
	executor := &compactCaptureExecutor{
		provider: "codex",
		failures: map[string]error{
			"test-model": compactStatusErr{
				code: http.StatusBadRequest,
				msg:  `{"detail":"The 'test-model' model is not supported when using Codex with a ChatGPT account."}`,
			},
			"gpt-5.5": compactStatusErr{
				code: http.StatusBadRequest,
				msg:  `{"detail":"The 'gpt-5.5' model is not supported when using Codex with a ChatGPT account."}`,
			},
			"gpt-5.1-codex-max": compactStatusErr{
				code: http.StatusBadRequest,
				msg:  `{"detail":"The 'gpt-5.1-codex-max' model is not supported when using Codex with a ChatGPT account."}`,
			},
		},
	}
	manager := coreauth.NewManager(nil, nil, nil)
	manager.RegisterExecutor(executor)

	auth := &coreauth.Auth{ID: "auth-unsupported", Provider: executor.Identifier(), Status: coreauth.StatusActive}
	if _, err := manager.Register(context.Background(), auth); err != nil {
		t.Fatalf("Register auth: %v", err)
	}
	registry.GetGlobalRegistry().RegisterClient(auth.ID, auth.Provider, []*registry.ModelInfo{
		{ID: "test-model"},
		{ID: "gpt-5.5"},
		{ID: "gpt-5.1-codex-max"},
		{ID: "gpt-5.3-codex"},
	})
	t.Cleanup(func() {
		registry.GetGlobalRegistry().UnregisterClient(auth.ID)
	})

	base := handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, manager)
	h := NewOpenAIResponsesAPIHandler(base)
	router := gin.New()
	router.POST("/v1/responses/compact", h.Compact)

	req := httptest.NewRequest(http.MethodPost, "/v1/responses/compact", strings.NewReader(`{"model":"test-model","input":"hello"}`))
	req.Header.Set("Content-Type", "application/json")
	resp := httptest.NewRecorder()
	router.ServeHTTP(resp, req)

	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d (should fall back to gpt-5.3-codex)", resp.Code, http.StatusOK)
	}
	found := false
	for _, m := range executor.models {
		if m == "gpt-5.3-codex" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("models = %#v, want gpt-5.3-codex to be attempted", executor.models)
	}
}

func TestIsCompactModelUnsupportedError(t *testing.T) {
	tests := []struct {
		name       string
		errMsg     *interfaces.ErrorMessage
		wantResult bool
	}{
		{
			name:       "nil error message",
			errMsg:     nil,
			wantResult: false,
		},
		{
			name: "400 model is not supported",
			errMsg: &interfaces.ErrorMessage{
				StatusCode: http.StatusBadRequest,
				Error:      errors.New(`{"detail":"The 'gpt-5.1-codex-max' model is not supported when using Codex with a ChatGPT account."}`),
			},
			wantResult: true,
		},
		{
			name: "400 model not supported short form",
			errMsg: &interfaces.ErrorMessage{
				StatusCode: http.StatusBadRequest,
				Error:      errors.New("model not supported"),
			},
			wantResult: true,
		},
		{
			name: "400 invalid_request_error should not match",
			errMsg: &interfaces.ErrorMessage{
				StatusCode: http.StatusBadRequest,
				Error:      errors.New("invalid_request_error: bad input"),
			},
			wantResult: false,
		},
		{
			name: "404 not found should not match",
			errMsg: &interfaces.ErrorMessage{
				StatusCode: http.StatusNotFound,
				Error:      errors.New("model is not supported"),
			},
			wantResult: false,
		},
		{
			name: "400 nil error",
			errMsg: &interfaces.ErrorMessage{
				StatusCode: http.StatusBadRequest,
				Error:      nil,
			},
			wantResult: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := isCompactModelUnsupportedError(tt.errMsg)
			if got != tt.wantResult {
				t.Errorf("isCompactModelUnsupportedError() = %v, want %v", got, tt.wantResult)
			}
		})
	}
}
