package openai

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v6/sdk/api/handlers"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v6/sdk/config"
)

type imageCaptureExecutor struct {
	alt      string
	model    string
	payload  string
	payloads []string
	metadata map[string]any
	calls    int
}

func (e *imageCaptureExecutor) Identifier() string { return "codex" }

type imageProviderCaptureExecutor struct {
	imageCaptureExecutor
	identifier string
}

func (e *imageProviderCaptureExecutor) Identifier() string { return e.identifier }

func (e *imageCaptureExecutor) Execute(ctx context.Context, auth *coreauth.Auth, req coreexecutor.Request, opts coreexecutor.Options) (coreexecutor.Response, error) {
	e.calls++
	e.alt = opts.Alt
	e.model = req.Model
	e.payload = string(req.Payload)
	e.payloads = append(e.payloads, e.payload)
	e.metadata = opts.Metadata
	b64 := "aGVsbG8="
	if e.calls > 1 {
		b64 = "aGVsbG8" + strconv.Itoa(e.calls) + "="
	}
	return coreexecutor.Response{Payload: []byte(`{"created":1,"data":[{"b64_json":"` + b64 + `"}]}`)}, nil
}

func (e *imageCaptureExecutor) ExecuteStream(context.Context, *coreauth.Auth, coreexecutor.Request, coreexecutor.Options) (*coreexecutor.StreamResult, error) {
	return nil, errors.New("not implemented")
}

func (e *imageCaptureExecutor) Refresh(ctx context.Context, auth *coreauth.Auth) (*coreauth.Auth, error) {
	return auth, nil
}

func (e *imageCaptureExecutor) CountTokens(context.Context, *coreauth.Auth, coreexecutor.Request, coreexecutor.Options) (coreexecutor.Response, error) {
	return coreexecutor.Response{}, errors.New("not implemented")
}

func (e *imageCaptureExecutor) HttpRequest(context.Context, *coreauth.Auth, *http.Request) (*http.Response, error) {
	return nil, errors.New("not implemented")
}

func registerImageTestCodexAuth(t *testing.T, manager *coreauth.Manager, id string, models ...string) {
	t.Helper()
	registerImageTestProviderAuth(t, manager, id, "codex", "Team Codex", models...)
}

func registerImageTestProviderAuth(t *testing.T, manager *coreauth.Manager, id, provider, label string, models ...string) {
	t.Helper()
	if _, err := manager.Register(context.Background(), &coreauth.Auth{
		ID:       id,
		Provider: provider,
		Label:    label,
		Status:   coreauth.StatusActive,
		Metadata: map[string]any{"access_token": "token", "email": "team@example.com"},
	}); err != nil {
		t.Fatalf("Register auth: %v", err)
	}

	modelInfos := make([]*registry.ModelInfo, 0, len(models))
	for _, model := range models {
		if strings.TrimSpace(model) != "" {
			modelInfos = append(modelInfos, &registry.ModelInfo{ID: model})
		}
	}
	registry.GetGlobalRegistry().RegisterClient(id, provider, modelInfos)
	t.Cleanup(func() {
		registry.GetGlobalRegistry().UnregisterClient(id)
	})
}

func TestOpenAIImagesGenerationsRoutesByImageProvider(t *testing.T) {
	tests := []struct {
		model        string
		wantProvider string
	}{
		{model: "grok-imagine-image", wantProvider: "xai"},
		{model: "grok-imagine-image-quality", wantProvider: "xai"},
		{model: "gpt-image-2", wantProvider: "codex"},
	}

	for _, tt := range tests {
		t.Run(tt.model, func(t *testing.T) {
			gin.SetMode(gin.TestMode)

			codexExecutor := &imageProviderCaptureExecutor{identifier: "codex"}
			xaiExecutor := &imageProviderCaptureExecutor{identifier: "xai"}
			manager := coreauth.NewManager(nil, nil, nil)
			manager.RegisterExecutor(codexExecutor)
			manager.RegisterExecutor(xaiExecutor)
			registerImageTestProviderAuth(t, manager, "codex-auth-"+tt.model, "codex", "Team Codex", tt.model)
			registerImageTestProviderAuth(t, manager, "xai-auth-"+tt.model, "xai", "Team XAI", tt.model)

			base := handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, manager)
			h := NewOpenAIImagesAPIHandler(base)
			router := gin.New()
			router.POST("/v1/images/generations", h.Generations)

			req := httptest.NewRequest(http.MethodPost, "/v1/images/generations", strings.NewReader(`{"model":"`+tt.model+`","prompt":"draw a fox"}`))
			req.Header.Set("Content-Type", "application/json")
			resp := httptest.NewRecorder()
			router.ServeHTTP(resp, req)

			if resp.Code != http.StatusOK {
				t.Fatalf("status = %d, want %d, body=%s", resp.Code, http.StatusOK, resp.Body.String())
			}
			calls := map[string]int{
				"codex": codexExecutor.calls,
				"xai":   xaiExecutor.calls,
			}
			if calls[tt.wantProvider] != 1 {
				t.Fatalf("%s executor calls = %d, want 1; all calls=%v", tt.wantProvider, calls[tt.wantProvider], calls)
			}
			for provider, count := range calls {
				if provider != tt.wantProvider && count != 0 {
					t.Fatalf("%s executor calls = %d, want 0; all calls=%v", provider, count, calls)
				}
			}
		})
	}
}

func TestOpenAIImagesGenerationsExecutesCodexImageAlt(t *testing.T) {
	gin.SetMode(gin.TestMode)

	executor := &imageCaptureExecutor{}
	manager := coreauth.NewManager(nil, nil, nil)
	manager.RegisterExecutor(executor)
	registerImageTestCodexAuth(t, manager, "codex-auth", openAIImageModelID)

	base := handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, manager)
	h := NewOpenAIImagesAPIHandler(base)
	router := gin.New()
	router.POST("/v1/images/generations", func(c *gin.Context) {
		c.Set("accessMetadata", map[string]string{"allowed-channels": "Team Codex"})
		h.Generations(c)
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/images/generations", strings.NewReader(`{"model":"gpt-image-2","prompt":"draw a fox"}`))
	req.Header.Set("Content-Type", "application/json")
	resp := httptest.NewRecorder()
	router.ServeHTTP(resp, req)

	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d, body=%s", resp.Code, http.StatusOK, resp.Body.String())
	}
	if executor.calls != 1 {
		t.Fatalf("executor calls = %d, want 1", executor.calls)
	}
	if executor.alt != "images/generations" {
		t.Fatalf("alt = %q, want %q", executor.alt, "images/generations")
	}
	if executor.model != openAIImageModelID {
		t.Fatalf("model = %q, want %q", executor.model, openAIImageModelID)
	}
	if !strings.Contains(executor.payload, "draw a fox") || !strings.Contains(executor.payload, "gpt-image-2") {
		t.Fatalf("payload = %s, want prompt", executor.payload)
	}
	if executor.metadata["allowed-channels"] != "Team Codex" {
		t.Fatalf("allowed channel metadata = %#v", executor.metadata["allowed-channels"])
	}
	if executor.metadata[coreexecutor.SinglePickMetadataKey] != true {
		t.Fatalf("single-pick metadata = %#v, want true", executor.metadata[coreexecutor.SinglePickMetadataKey])
	}
	if strings.TrimSpace(resp.Body.String()) != `{"created":1,"data":[{"b64_json":"aGVsbG8="}]}` {
		t.Fatalf("body = %s", resp.Body.String())
	}
}

func TestOpenAIImagesGenerationsDefaultsModel(t *testing.T) {
	gin.SetMode(gin.TestMode)

	executor := &imageCaptureExecutor{}
	manager := coreauth.NewManager(nil, nil, nil)
	manager.RegisterExecutor(executor)
	registerImageTestCodexAuth(t, manager, "codex-auth-default-model", openAIImageModelID)

	base := handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, manager)
	h := NewOpenAIImagesAPIHandler(base)
	router := gin.New()
	router.POST("/v1/images/generations", h.Generations)

	req := httptest.NewRequest(http.MethodPost, "/v1/images/generations", strings.NewReader(`{"prompt":"draw a fox"}`))
	req.Header.Set("Content-Type", "application/json")
	resp := httptest.NewRecorder()
	router.ServeHTTP(resp, req)

	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d, body=%s", resp.Code, http.StatusOK, resp.Body.String())
	}
	if executor.model != openAIImageModelID {
		t.Fatalf("model = %q, want %q", executor.model, openAIImageModelID)
	}
	if !strings.Contains(executor.payload, "gpt-image-2") {
		t.Fatalf("payload = %s, want default model in payload", executor.payload)
	}
}

func TestOpenAIImagesGenerationsRequiresImageModelSupport(t *testing.T) {
	gin.SetMode(gin.TestMode)

	executor := &imageCaptureExecutor{}
	manager := coreauth.NewManager(nil, nil, nil)
	manager.RegisterExecutor(executor)
	registerImageTestCodexAuth(t, manager, "codex-auth-text-only", "gpt-5.5")

	base := handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, manager)
	h := NewOpenAIImagesAPIHandler(base)
	router := gin.New()
	router.POST("/v1/images/generations", h.Generations)

	req := httptest.NewRequest(http.MethodPost, "/v1/images/generations", strings.NewReader(`{"model":"gpt-image-2","prompt":"draw a fox"}`))
	req.Header.Set("Content-Type", "application/json")
	resp := httptest.NewRecorder()
	router.ServeHTTP(resp, req)

	if resp.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want %d, body=%s", resp.Code, http.StatusBadGateway, resp.Body.String())
	}
	if executor.calls != 0 {
		t.Fatalf("executor calls = %d, want 0", executor.calls)
	}
	if !strings.Contains(resp.Body.String(), "auth_not_found") {
		t.Fatalf("body = %s, want auth_not_found", resp.Body.String())
	}
}

func TestOpenAIImagesGenerationsRejectsNonImageModelBeforeAuth(t *testing.T) {
	gin.SetMode(gin.TestMode)

	executor := &imageProviderCaptureExecutor{identifier: "xai"}
	manager := coreauth.NewManager(nil, nil, nil)
	manager.RegisterExecutor(executor)
	registerImageTestProviderAuth(t, manager, "xai-auth-chat-model", "xai", "Team XAI", "grok-4.5")

	base := handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, manager)
	h := NewOpenAIImagesAPIHandler(base)
	router := gin.New()
	router.POST("/v1/images/generations", h.Generations)

	req := httptest.NewRequest(http.MethodPost, "/v1/images/generations", strings.NewReader(`{"model":"grok-4.5","prompt":"draw a fox"}`))
	req.Header.Set("Content-Type", "application/json")
	resp := httptest.NewRecorder()
	router.ServeHTTP(resp, req)

	if resp.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d, body=%s", resp.Code, http.StatusBadRequest, resp.Body.String())
	}
	if executor.calls != 0 {
		t.Fatalf("executor calls = %d, want 0", executor.calls)
	}
	if !strings.Contains(resp.Body.String(), `"type":"invalid_request_error"`) || !strings.Contains(resp.Body.String(), "grok-4.5") {
		t.Fatalf("body = %s, want local invalid_request_error naming the model", resp.Body.String())
	}
}

func TestOpenAIChatCompletionsRejectsImageModelsBeforeAuth(t *testing.T) {
	tests := []struct {
		model      string
		wantStatus int
		wantCalls  int
	}{
		{model: "grok-imagine-image", wantStatus: http.StatusBadRequest, wantCalls: 0},
		{model: "grok-4.5", wantStatus: http.StatusOK, wantCalls: 1},
	}

	for _, tt := range tests {
		t.Run(tt.model, func(t *testing.T) {
			gin.SetMode(gin.TestMode)

			executor := &imageProviderCaptureExecutor{identifier: "xai"}
			manager := coreauth.NewManager(nil, nil, nil)
			manager.RegisterExecutor(executor)
			registerImageTestProviderAuth(t, manager, "xai-auth-chat-"+tt.model, "xai", "Team XAI", tt.model)

			base := handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, manager)
			h := NewOpenAIAPIHandler(base)
			router := gin.New()
			router.POST("/v1/chat/completions", h.ChatCompletions)

			req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"`+tt.model+`","messages":[{"role":"user","content":"draw a fox"}]}`))
			req.Header.Set("Content-Type", "application/json")
			resp := httptest.NewRecorder()
			router.ServeHTTP(resp, req)

			if resp.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d, body=%s", resp.Code, tt.wantStatus, resp.Body.String())
			}
			if executor.calls != tt.wantCalls {
				t.Fatalf("executor calls = %d, want %d", executor.calls, tt.wantCalls)
			}
			if tt.wantCalls == 0 {
				body := resp.Body.String()
				if !strings.Contains(body, `"type":"invalid_request_error"`) || !strings.Contains(body, "/v1/images/generations") {
					t.Fatalf("body = %s, want local image-endpoint guidance", body)
				}
			}
		})
	}
}

func TestOpenAIImagesGenerationsSplitsMultipleImagesIntoSingleImageExecutions(t *testing.T) {
	gin.SetMode(gin.TestMode)

	executor := &imageCaptureExecutor{}
	manager := coreauth.NewManager(nil, nil, nil)
	manager.RegisterExecutor(executor)
	registerImageTestCodexAuth(t, manager, "codex-auth-multiple-images", openAIImageModelID)

	base := handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, manager)
	h := NewOpenAIImagesAPIHandler(base)
	router := gin.New()
	router.POST("/v1/images/generations", h.Generations)

	req := httptest.NewRequest(http.MethodPost, "/v1/images/generations", strings.NewReader(`{"model":"gpt-image-2","prompt":"draw a fox","n":3}`))
	req.Header.Set("Content-Type", "application/json")
	resp := httptest.NewRecorder()
	router.ServeHTTP(resp, req)

	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d, body=%s", resp.Code, http.StatusOK, resp.Body.String())
	}
	if executor.calls != 3 {
		t.Fatalf("executor calls = %d, want 3", executor.calls)
	}
	if executor.model != openAIImageModelID {
		t.Fatalf("model = %q, want %q", executor.model, openAIImageModelID)
	}
	for i, payload := range executor.payloads {
		if !strings.Contains(payload, `"n":1`) {
			t.Fatalf("payload[%d] = %s, want n=1", i, payload)
		}
	}
	var body struct {
		Data []struct {
			B64JSON string `json:"b64_json"`
		} `json:"data"`
	}
	if err := json.Unmarshal(resp.Body.Bytes(), &body); err != nil {
		t.Fatalf("Unmarshal response: %v", err)
	}
	if len(body.Data) != 3 {
		t.Fatalf("data length = %d, want 3", len(body.Data))
	}
}

// imageRewritingGuard is a request guard that replaces a word and counts how
// often it is asked.
type imageRewritingGuard struct {
	mu    sync.Mutex
	calls int
}

func (g *imageRewritingGuard) Moderate(context.Context, *coreauth.Auth, coreexecutor.Options) coreauth.RequestModerationResult {
	return coreauth.RequestModerationResult{}
}

func (g *imageRewritingGuard) GuardRequest(_ context.Context, req coreexecutor.Request, opts coreexecutor.Options) coreauth.RequestGuardResult {
	g.mu.Lock()
	g.calls++
	g.mu.Unlock()
	return coreauth.RequestGuardResult{
		OriginalRequest: bytes.ReplaceAll(opts.OriginalRequest, []byte("fox"), []byte("***")),
		Payload:         bytes.ReplaceAll(req.Payload, []byte("fox"), []byte("***")),
	}
}

// n > 1 is one client request run as n executions of one image each. The prompt
// is looked at by the request guard once, and every execution gets the same
// rewritten prompt.
func TestOpenAIImagesGuardsAMultiImageRequestOnce(t *testing.T) {
	gin.SetMode(gin.TestMode)

	executor := &imageCaptureExecutor{}
	manager := coreauth.NewManager(nil, nil, nil)
	manager.RegisterExecutor(executor)
	registerImageTestCodexAuth(t, manager, "codex-auth-guarded-images", openAIImageModelID)
	guard := &imageRewritingGuard{}
	manager.SetRequestModerator(guard)

	base := handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, manager)
	h := NewOpenAIImagesAPIHandler(base)
	router := gin.New()
	router.POST("/v1/images/generations", h.Generations)

	req := httptest.NewRequest(http.MethodPost, "/v1/images/generations", strings.NewReader(`{"model":"gpt-image-2","prompt":"draw a fox","n":3}`))
	req.Header.Set("Content-Type", "application/json")
	resp := httptest.NewRecorder()
	router.ServeHTTP(resp, req)

	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", resp.Code, resp.Body.String())
	}
	if executor.calls != 3 {
		t.Fatalf("executor calls = %d, want 3", executor.calls)
	}
	if guard.calls != 1 {
		t.Fatalf("the guard ran %d times for one client request, want 1", guard.calls)
	}
	for i, payload := range executor.payloads {
		if !strings.Contains(payload, `"prompt":"draw a ***"`) || strings.Contains(payload, "fox") || !strings.Contains(payload, `"n":1`) {
			t.Fatalf("payload[%d] = %s, want the rewritten prompt and n=1", i, payload)
		}
	}
}

func TestOpenAIImagesEditsExecutesCodexImageEditsAlt(t *testing.T) {
	gin.SetMode(gin.TestMode)

	executor := &imageCaptureExecutor{}
	manager := coreauth.NewManager(nil, nil, nil)
	manager.RegisterExecutor(executor)
	registerImageTestCodexAuth(t, manager, "codex-auth-edits", openAIImageModelID)

	base := handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, manager)
	h := NewOpenAIImagesAPIHandler(base)
	router := gin.New()
	router.POST("/v1/images/edits", h.Edits)

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	_ = writer.WriteField("model", "gpt-image-2")
	_ = writer.WriteField("prompt", "make it blue")
	_ = writer.WriteField("size", "1024x1792")
	_ = writer.WriteField("quality", "medium")
	_ = writer.WriteField("background", "transparent")
	_ = writer.WriteField("output_format", "webp")
	_ = writer.WriteField("input_fidelity", "high")
	_ = writer.WriteField("n", "2")
	part, err := writer.CreateFormFile("image", "icon.png")
	if err != nil {
		t.Fatalf("CreateFormFile: %v", err)
	}
	_, _ = part.Write([]byte("hello"))
	maskPart, err := writer.CreateFormFile("mask", "mask.png")
	if err != nil {
		t.Fatalf("CreateFormFile(mask): %v", err)
	}
	_, _ = maskPart.Write([]byte("mask-bytes"))
	_ = writer.Close()

	req := httptest.NewRequest(http.MethodPost, "/v1/images/edits", &body)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	resp := httptest.NewRecorder()
	router.ServeHTTP(resp, req)

	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d, body=%s", resp.Code, http.StatusOK, resp.Body.String())
	}
	if executor.calls != 2 {
		t.Fatalf("executor calls = %d, want 2", executor.calls)
	}
	if executor.alt != "images/edits" {
		t.Fatalf("alt = %q, want %q", executor.alt, "images/edits")
	}
	if executor.model != openAIImageModelID {
		t.Fatalf("model = %q, want %q", executor.model, openAIImageModelID)
	}
	if !strings.Contains(executor.payload, `"output_format":"webp"`) {
		t.Fatalf("payload = %s, want output_format", executor.payload)
	}
	if !strings.Contains(executor.payload, `"background":"transparent"`) {
		t.Fatalf("payload = %s, want background", executor.payload)
	}
	if !strings.Contains(executor.payload, `"input_fidelity":"high"`) {
		t.Fatalf("payload = %s, want input_fidelity", executor.payload)
	}
	if !strings.Contains(executor.payload, `"mask_file"`) {
		t.Fatalf("payload = %s, want mask_file", executor.payload)
	}
	if strings.Count(executor.payload, `"data_base64"`) < 2 {
		t.Fatalf("payload = %s, want base64 encoded image and mask uploads", executor.payload)
	}
}
