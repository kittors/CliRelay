package management

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
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/identity"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

type managementImageExecutor struct {
	alt             string
	model           string
	payload         string
	payloads        []string
	originalRequest string
	metadata        map[string]any
	authID          string
	calls           int
	err             error
}

type managementUpstreamStatusError struct {
	code         int
	message      string
	upstreamBody []byte
}

func (e managementUpstreamStatusError) Error() string   { return e.message }
func (e managementUpstreamStatusError) StatusCode() int { return e.code }
func (e managementUpstreamStatusError) UpstreamErrorBody() []byte {
	return append([]byte(nil), e.upstreamBody...)
}

func (e *managementImageExecutor) Identifier() string { return "codex" }

func (e *managementImageExecutor) Execute(ctx context.Context, auth *coreauth.Auth, req coreexecutor.Request, opts coreexecutor.Options) (coreexecutor.Response, error) {
	e.calls++
	e.alt = opts.Alt
	e.model = req.Model
	e.payload = string(req.Payload)
	e.payloads = append(e.payloads, e.payload)
	e.originalRequest = string(opts.OriginalRequest)
	e.metadata = opts.Metadata
	if auth != nil {
		e.authID = auth.ID
	}
	if e.err != nil {
		return coreexecutor.Response{}, e.err
	}
	b64 := "dGVzdA=="
	if e.calls > 1 {
		b64 = "dGVzdA" + strconv.Itoa(e.calls) + "="
	}
	return coreexecutor.Response{Payload: []byte(`{"created":1,"data":[{"b64_json":"` + b64 + `"}]}`)}, nil
}

func (e *managementImageExecutor) ExecuteStream(context.Context, *coreauth.Auth, coreexecutor.Request, coreexecutor.Options) (*coreexecutor.StreamResult, error) {
	return nil, errors.New("not implemented")
}

func (e *managementImageExecutor) Refresh(ctx context.Context, auth *coreauth.Auth) (*coreauth.Auth, error) {
	return auth, nil
}

func (e *managementImageExecutor) CountTokens(context.Context, *coreauth.Auth, coreexecutor.Request, coreexecutor.Options) (coreexecutor.Response, error) {
	return coreexecutor.Response{}, errors.New("not implemented")
}

func (e *managementImageExecutor) HttpRequest(context.Context, *coreauth.Auth, *http.Request) (*http.Response, error) {
	return nil, errors.New("not implemented")
}

func registerManagementImageAuth(t *testing.T, manager *coreauth.Manager, id string, models ...string) {
	t.Helper()
	registerManagementImageAuthForTenant(t, manager, identity.SystemTenantID, id, models...)
}

func registerManagementImageAuthForTenant(t *testing.T, manager *coreauth.Manager, tenantID, id string, models ...string) {
	t.Helper()
	if _, err := manager.Register(context.Background(), &coreauth.Auth{
		ID:       id,
		TenantID: tenantID,
		Provider: "codex",
		Status:   coreauth.StatusActive,
		Metadata: map[string]any{"access_token": "token"},
	}); err != nil {
		t.Fatalf("Register auth: %v", err)
	}

	modelInfos := make([]*registry.ModelInfo, 0, len(models))
	for _, model := range models {
		if strings.TrimSpace(model) != "" {
			modelInfos = append(modelInfos, &registry.ModelInfo{ID: model})
		}
	}
	registry.GetGlobalRegistry().RegisterClient(id, "codex", modelInfos)
	t.Cleanup(func() {
		registry.GetGlobalRegistry().UnregisterClient(id)
	})
}

func TestPostImageGenerationTestReturnsStructuredUpstreamError(t *testing.T) {
	gin.SetMode(gin.TestMode)

	executor := &managementImageExecutor{err: errors.New("openai image conversation returned no downloadable images")}
	manager := coreauth.NewManager(nil, nil, nil)
	manager.RegisterExecutor(executor)
	registerManagementImageAuth(t, manager, "codex-auth-structured-error", imageGenerationModel)

	h := &Handler{authManager: manager}
	_, execErr := h.executeImageGenerationTest(context.Background(), []byte(`{
		"model":"gpt-image-2",
		"prompt":"safe test prompt",
		"size":"1024x1792",
		"quality":"medium",
		"n":1
	}`), imageGenerationAlt)
	if execErr == nil {
		t.Fatal("executeImageGenerationTest() error = nil")
	}

	response := imageGenerationErrorResponse(execErr, "upstream_error")
	data, _ := json.Marshal(response)
	var body struct {
		Error struct {
			Message string `json:"message"`
			Type    string `json:"type"`
		} `json:"error"`
	}
	if err := json.Unmarshal(data, &body); err != nil {
		t.Fatalf("Unmarshal response: %v", err)
	}
	if body.Error.Type != "upstream_error" {
		t.Fatalf("error.type = %q, want upstream_error", body.Error.Type)
	}
	if !strings.Contains(body.Error.Message, "no downloadable images") {
		t.Fatalf("error.message = %q, want upstream failure message", body.Error.Message)
	}
}

func TestPostImageGenerationTestIncludesOfficialUpstreamErrorBody(t *testing.T) {
	gin.SetMode(gin.TestMode)

	executor := &managementImageExecutor{
		err: managementUpstreamStatusError{
			code:    http.StatusTooManyRequests,
			message: "rate limit exceeded",
			upstreamBody: []byte(
				`{"error":{"message":"rate limit exceeded","type":"rate_limit_error","code":"rate_limit"}}`,
			),
		},
	}
	manager := coreauth.NewManager(nil, nil, nil)
	manager.RegisterExecutor(executor)
	registerManagementImageAuth(t, manager, "codex-auth-official-error", imageGenerationModel)

	h := &Handler{authManager: manager}
	_, execErr := h.executeImageGenerationTest(context.Background(), []byte(`{
		"model":"gpt-image-2",
		"prompt":"safe test prompt"
	}`), imageGenerationAlt)
	if execErr == nil {
		t.Fatal("executeImageGenerationTest() error = nil")
	}

	response := imageGenerationErrorResponse(execErr, "upstream_error")
	data, _ := json.Marshal(response)
	var body struct {
		Error struct {
			Message  string `json:"message"`
			Type     string `json:"type"`
			Upstream struct {
				Error struct {
					Message string `json:"message"`
					Type    string `json:"type"`
					Code    string `json:"code"`
				} `json:"error"`
			} `json:"upstream"`
		} `json:"error"`
	}
	if err := json.Unmarshal(data, &body); err != nil {
		t.Fatalf("Unmarshal response: %v", err)
	}
	if body.Error.Type != "upstream_error" {
		t.Fatalf("error.type = %q, want upstream_error", body.Error.Type)
	}
	if body.Error.Upstream.Error.Type != "rate_limit_error" {
		t.Fatalf("upstream.error.type = %q, want rate_limit_error", body.Error.Upstream.Error.Type)
	}
	if body.Error.Upstream.Error.Code != "rate_limit" {
		t.Fatalf("upstream.error.code = %q, want rate_limit", body.Error.Upstream.Error.Code)
	}
}

func TestPostImageGenerationTestExecutesCodexImageAlt(t *testing.T) {
	gin.SetMode(gin.TestMode)

	executor := &managementImageExecutor{}
	manager := coreauth.NewManager(nil, nil, nil)
	manager.RegisterExecutor(executor)
	registerManagementImageAuth(t, manager, "codex-auth-execute-alt", imageGenerationModel)

	h := &Handler{authManager: manager}
	_, err := h.executeImageGenerationTest(context.Background(), []byte(`{"model":"gpt-image-2","prompt":"test prompt"}`), imageGenerationAlt)

	if err != nil {
		t.Fatalf("executeImageGenerationTest() error = %v", err)
	}
	if executor.calls != 1 {
		t.Fatalf("executor calls = %d, want 1", executor.calls)
	}
	if executor.alt != "images/generations" {
		t.Fatalf("alt = %q, want images/generations", executor.alt)
	}
	if executor.model != imageGenerationModel {
		t.Fatalf("model = %q, want %q", executor.model, imageGenerationModel)
	}
	if !strings.Contains(executor.payload, "test prompt") || !strings.Contains(executor.payload, "gpt-image-2") {
		t.Fatalf("payload = %s, want prompt and model", executor.payload)
	}
	if !strings.Contains(executor.payload, `"size":"1024x1024"`) && strings.Contains(executor.payload, "size") {
		t.Fatalf("payload = %s, should only include explicit size", executor.payload)
	}
	if executor.metadata[coreexecutor.SinglePickMetadataKey] != true {
		t.Fatalf("single-pick metadata = %#v, want true", executor.metadata[coreexecutor.SinglePickMetadataKey])
	}
	if executor.originalRequest != `{"model":"gpt-image-2","prompt":"test prompt"}` {
		t.Fatalf("original request = %q, want image request payload", executor.originalRequest)
	}
}

func TestPostImageGenerationTestForwardsGenerationOptions(t *testing.T) {
	gin.SetMode(gin.TestMode)

	executor := &managementImageExecutor{}
	manager := coreauth.NewManager(nil, nil, nil)
	manager.RegisterExecutor(executor)
	registerManagementImageAuth(t, manager, "codex-auth-generation-options", imageGenerationModel)

	h := &Handler{authManager: manager}
	result, err := h.executeImageGenerationTest(context.Background(), []byte(`{
		"prompt":"test prompt",
		"size":"1024x1792",
		"quality":"high",
		"n":2
	}`), imageGenerationAlt)

	if err != nil {
		t.Fatalf("executeImageGenerationTest() error = %v", err)
	}
	if executor.calls != 2 {
		t.Fatalf("executor calls = %d, want 2", executor.calls)
	}
	if executor.alt != "images/generations" {
		t.Fatalf("alt = %q, want images/generations", executor.alt)
	}
	if executor.model != imageGenerationModel {
		t.Fatalf("model = %q, want %q", executor.model, imageGenerationModel)
	}
	for i, payload := range executor.payloads {
		for _, want := range []string{`"size":"1024x1792"`, `"quality":"high"`, `"n":1`} {
			if !strings.Contains(payload, want) {
				t.Fatalf("payload[%d] = %s, want %s", i, payload, want)
			}
		}
	}
	var body struct {
		Data []struct {
			B64JSON string `json:"b64_json"`
		} `json:"data"`
	}
	if err := json.Unmarshal(result, &body); err != nil {
		t.Fatalf("Unmarshal response: %v", err)
	}
	if len(body.Data) != 2 {
		t.Fatalf("data length = %d, want 2", len(body.Data))
	}
}

func TestPostImageGenerationTestCreatesTaskAndPollsSucceededResult(t *testing.T) {
	gin.SetMode(gin.TestMode)

	executor := &managementImageExecutor{}
	manager := coreauth.NewManager(nil, nil, nil)
	manager.RegisterExecutor(executor)
	registerManagementImageAuth(t, manager, "codex-auth-task-success", imageGenerationModel)

	h := &Handler{authManager: manager}
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	req := httptest.NewRequest(http.MethodPost, "/image-generation/test", strings.NewReader(`{
		"model":"gpt-image-2",
		"prompt":"safe test prompt",
		"n":2
	}`))
	req.Header.Set("Content-Type", "application/json")
	c.Request = req

	h.PostImageGenerationTest(c)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want %d, body=%s", rec.Code, http.StatusAccepted, rec.Body.String())
	}
	var created struct {
		TaskID string `json:"task_id"`
		Status string `json:"status"`
		Phase  string `json:"phase"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("Unmarshal create task response: %v", err)
	}
	if created.TaskID == "" {
		t.Fatal("task_id is empty")
	}
	if created.Status != "queued" {
		t.Fatalf("status = %q, want queued", created.Status)
	}

	var polled struct {
		TaskID string `json:"task_id"`
		Status string `json:"status"`
		Phase  string `json:"phase"`
		Result struct {
			Data []struct {
				B64JSON string `json:"b64_json"`
			} `json:"data"`
		} `json:"result"`
	}
	waitForImageGenerationTask(t, h, created.TaskID, func(body []byte) bool {
		if err := json.Unmarshal(body, &polled); err != nil {
			t.Fatalf("Unmarshal poll response: %v", err)
		}
		return polled.Status == "succeeded"
	})
	if polled.TaskID != created.TaskID {
		t.Fatalf("task_id = %q, want %q", polled.TaskID, created.TaskID)
	}
	if polled.Phase != "completed" {
		t.Fatalf("phase = %q, want completed", polled.Phase)
	}
	if len(polled.Result.Data) != 2 {
		t.Fatalf("result.data length = %d, want 2", len(polled.Result.Data))
	}
}

func TestPostImageGenerationTestUsesEffectiveTenantAuth(t *testing.T) {
	gin.SetMode(gin.TestMode)

	const tenantID = "00000000-0000-0000-0000-00000000000a"
	systemExecutor := &managementImageExecutor{}
	tenantExecutor := &managementImageExecutor{}
	manager := coreauth.NewManager(nil, nil, nil)
	manager.RegisterExecutor(systemExecutor)
	manager.RegisterExecutorForTenant(tenantID, tenantExecutor)
	registerManagementImageAuth(t, manager, "codex-auth-system", imageGenerationModel)
	registerManagementImageAuthForTenant(t, manager, tenantID, "codex-auth-tenant-a", imageGenerationModel)

	h := &Handler{authManager: manager}
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Set(managementPrincipalKey, identity.Principal{EffectiveTenant: identity.Tenant{ID: tenantID}})
	req := httptest.NewRequest(http.MethodPost, "/image-generation/test", strings.NewReader(`{
		"model":"gpt-image-2",
		"prompt":"safe tenant test prompt"
	}`))
	req.Header.Set("Content-Type", "application/json")
	c.Request = req

	h.PostImageGenerationTest(c)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want %d, body=%s", rec.Code, http.StatusAccepted, rec.Body.String())
	}
	var created struct {
		TaskID string `json:"task_id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("Unmarshal create task response: %v", err)
	}
	waitForImageGenerationTaskForTenant(t, h, tenantID, created.TaskID, func(body []byte) bool {
		var task struct {
			Status string `json:"status"`
		}
		if err := json.Unmarshal(body, &task); err != nil {
			t.Fatalf("Unmarshal poll response: %v", err)
		}
		return task.Status == "succeeded"
	})

	if tenantExecutor.calls != 1 || tenantExecutor.authID != "codex-auth-tenant-a" {
		t.Fatalf("tenant executor calls=%d auth=%q, want one call with tenant auth", tenantExecutor.calls, tenantExecutor.authID)
	}
	if systemExecutor.calls != 0 {
		t.Fatalf("system executor calls = %d, want 0", systemExecutor.calls)
	}
	if got := tenantExecutor.metadata[coreexecutor.TenantMetadataKey]; got != tenantID {
		t.Fatalf("tenant metadata = %v, want %q", got, tenantID)
	}
}

func TestPostImageGenerationTestCreatesTaskAndPollsFailedError(t *testing.T) {
	gin.SetMode(gin.TestMode)

	executor := &managementImageExecutor{
		err: managementUpstreamStatusError{
			code:    http.StatusTooManyRequests,
			message: "rate limit exceeded",
			upstreamBody: []byte(
				`{"error":{"message":"rate limit exceeded","type":"rate_limit_error"}}`,
			),
		},
	}
	manager := coreauth.NewManager(nil, nil, nil)
	manager.RegisterExecutor(executor)
	registerManagementImageAuth(t, manager, "codex-auth-task-failed", imageGenerationModel)

	h := &Handler{authManager: manager}
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	req := httptest.NewRequest(http.MethodPost, "/image-generation/test", strings.NewReader(`{
		"model":"gpt-image-2",
		"prompt":"safe test prompt"
	}`))
	req.Header.Set("Content-Type", "application/json")
	c.Request = req

	h.PostImageGenerationTest(c)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want %d, body=%s", rec.Code, http.StatusAccepted, rec.Body.String())
	}
	var created struct {
		TaskID string `json:"task_id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("Unmarshal create task response: %v", err)
	}

	var polled struct {
		Status string `json:"status"`
		Error  struct {
			Status int `json:"status"`
			Body   struct {
				Error struct {
					Message  string `json:"message"`
					Type     string `json:"type"`
					Upstream struct {
						Error struct {
							Type string `json:"type"`
						} `json:"error"`
					} `json:"upstream"`
				} `json:"error"`
			} `json:"body"`
		} `json:"error"`
	}
	waitForImageGenerationTask(t, h, created.TaskID, func(body []byte) bool {
		if err := json.Unmarshal(body, &polled); err != nil {
			t.Fatalf("Unmarshal poll response: %v", err)
		}
		return polled.Status == "failed"
	})
	if polled.Error.Status != http.StatusTooManyRequests {
		t.Fatalf("error.status = %d, want %d", polled.Error.Status, http.StatusTooManyRequests)
	}
	if polled.Error.Body.Error.Type != "upstream_error" {
		t.Fatalf("error.body.error.type = %q, want upstream_error", polled.Error.Body.Error.Type)
	}
	if polled.Error.Body.Error.Upstream.Error.Type != "rate_limit_error" {
		t.Fatalf("upstream.error.type = %q, want rate_limit_error", polled.Error.Body.Error.Upstream.Error.Type)
	}
}

func waitForImageGenerationTask(t *testing.T, h *Handler, taskID string, done func([]byte) bool) {
	t.Helper()
	waitForImageGenerationTaskForTenant(t, h, identity.SystemTenantID, taskID, done)
}

func waitForImageGenerationTaskForTenant(t *testing.T, h *Handler, tenantID, taskID string, done func([]byte) bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		rec := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(rec)
		c.Set(managementPrincipalKey, identity.Principal{EffectiveTenant: identity.Tenant{ID: tenantID}})
		c.Params = gin.Params{{Key: "task_id", Value: taskID}}
		c.Request = httptest.NewRequest(http.MethodGet, "/image-generation/test/"+taskID, nil)

		h.GetImageGenerationTestTask(c)

		if rec.Code != http.StatusOK {
			t.Fatalf("poll status = %d, want %d, body=%s", rec.Code, http.StatusOK, rec.Body.String())
		}
		if done(rec.Body.Bytes()) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for image generation task %s, last body=%s", taskID, rec.Body.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestImageGenerationSizePresetsReturnDefaults(t *testing.T) {
	initManagementModelsTestDB(t)

	h := &Handler{}
	rec := performModelsRequest(http.MethodGet, "/image-generation/size-presets", nil, h.GetImageGenerationSizePresets)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d, body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	var body struct {
		Sizes []string `json:"sizes"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("Unmarshal response: %v", err)
	}
	want := []string{"1024x1024", "1792x1024", "1024x1792", "2560x1440", "2160x3840"}
	if got := strings.Join(body.Sizes, ","); got != strings.Join(want, ",") {
		t.Fatalf("sizes = %v, want %v", body.Sizes, want)
	}
}

func TestImageGenerationSizePresetsPersistCustomSizes(t *testing.T) {
	initManagementModelsTestDB(t)

	h := &Handler{}
	putRec := performModelsRequest(
		http.MethodPut,
		"/image-generation/size-presets",
		[]byte(`{"sizes":["1024x1024","4096X2304","4096×2304","8192 * 4608"]}`),
		h.PutImageGenerationSizePresets,
	)
	if putRec.Code != http.StatusOK {
		t.Fatalf("PUT status = %d, want %d, body=%s", putRec.Code, http.StatusOK, putRec.Body.String())
	}

	getRec := performModelsRequest(http.MethodGet, "/image-generation/size-presets", nil, h.GetImageGenerationSizePresets)
	if getRec.Code != http.StatusOK {
		t.Fatalf("GET status = %d, want %d, body=%s", getRec.Code, http.StatusOK, getRec.Body.String())
	}
	var body struct {
		Sizes []string `json:"sizes"`
	}
	if err := json.Unmarshal(getRec.Body.Bytes(), &body); err != nil {
		t.Fatalf("Unmarshal response: %v", err)
	}
	want := []string{"1024x1024", "1792x1024", "1024x1792", "2560x1440", "2160x3840", "4096x2304", "8192x4608"}
	if got := strings.Join(body.Sizes, ","); got != strings.Join(want, ",") {
		t.Fatalf("sizes = %v, want %v", body.Sizes, want)
	}
}

func TestImageGenerationSizePresetsRejectInvalidSize(t *testing.T) {
	initManagementModelsTestDB(t)

	h := &Handler{}
	rec := performModelsRequest(
		http.MethodPut,
		"/image-generation/size-presets",
		[]byte(`{"sizes":["0x1024"]}`),
		h.PutImageGenerationSizePresets,
	)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d, body=%s", rec.Code, http.StatusBadRequest, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "invalid size") {
		t.Fatalf("body = %s, want invalid size error", rec.Body.String())
	}
}

func TestImageGenerationSizePresetsAcceptMaxSize(t *testing.T) {
	initManagementModelsTestDB(t)

	h := &Handler{}
	rec := performModelsRequest(
		http.MethodPut,
		"/image-generation/size-presets",
		[]byte(`{"sizes":["8192x8192"]}`),
		h.PutImageGenerationSizePresets,
	)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d, body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	var body struct {
		Sizes []string `json:"sizes"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("Unmarshal response: %v", err)
	}
	if got := strings.Join(body.Sizes, ","); !strings.Contains(got, "8192x8192") {
		t.Fatalf("sizes = %v, want 8192x8192", body.Sizes)
	}
}

func TestImageGenerationSizePresetsRejectOversizedSize(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{name: "edge", body: `{"sizes":["8193x1024"]}`},
		{name: "pixels", body: `{"sizes":["8192x8193"]}`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			initManagementModelsTestDB(t)

			h := &Handler{}
			rec := performModelsRequest(
				http.MethodPut,
				"/image-generation/size-presets",
				[]byte(tt.body),
				h.PutImageGenerationSizePresets,
			)

			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want %d, body=%s", rec.Code, http.StatusBadRequest, rec.Body.String())
			}
			if !strings.Contains(rec.Body.String(), "exceeds maximum") {
				t.Fatalf("body = %s, want oversized error", rec.Body.String())
			}
		})
	}
}

func TestPostImageGenerationTestAcceptsMultipartImageEdits(t *testing.T) {
	gin.SetMode(gin.TestMode)

	executor := &managementImageExecutor{}
	manager := coreauth.NewManager(nil, nil, nil)
	manager.RegisterExecutor(executor)
	registerManagementImageAuth(t, manager, "codex-auth-multipart-edits", imageGenerationModel)

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	_ = writer.WriteField("model", "gpt-image-2")
	_ = writer.WriteField("prompt", "make it blue")
	_ = writer.WriteField("size", "1792x1024")
	_ = writer.WriteField("quality", "low")
	_ = writer.WriteField("background", "transparent")
	_ = writer.WriteField("output_format", "webp")
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

	h := &Handler{authManager: manager}
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	req := httptest.NewRequest(http.MethodPost, "/image-generation/test", &body)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	c.Request = req

	h.PostImageGenerationTest(c)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want %d, body=%s", rec.Code, http.StatusAccepted, rec.Body.String())
	}
	var created struct {
		TaskID string `json:"task_id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("Unmarshal create task response: %v", err)
	}

	var polled struct {
		Status string `json:"status"`
		Result struct {
			Data []struct {
				B64JSON string `json:"b64_json"`
			} `json:"data"`
		} `json:"result"`
	}
	waitForImageGenerationTask(t, h, created.TaskID, func(body []byte) bool {
		if err := json.Unmarshal(body, &polled); err != nil {
			t.Fatalf("Unmarshal poll response: %v", err)
		}
		return polled.Status == "succeeded"
	})
	if executor.calls != 2 {
		t.Fatalf("executor calls = %d, want 2", executor.calls)
	}
	if executor.alt != imageEditsAlt {
		t.Fatalf("alt = %q, want %q", executor.alt, imageEditsAlt)
	}
	if executor.model != imageGenerationModel {
		t.Fatalf("model = %q, want %q", executor.model, imageGenerationModel)
	}
	if !strings.Contains(executor.payload, `"mask_file"`) {
		t.Fatalf("payload = %s, want mask_file", executor.payload)
	}
	if !strings.Contains(executor.payload, `"output_format":"webp"`) {
		t.Fatalf("payload = %s, want output_format", executor.payload)
	}
}

func TestListImageGenerationChannelsUsesCurrentChannelLabels(t *testing.T) {
	gin.SetMode(gin.TestMode)

	manager := coreauth.NewManager(nil, nil, nil)
	_, err := manager.Register(context.Background(), &coreauth.Auth{
		ID:       "codex-oauth-1",
		Provider: "codex",
		Label:    "设计号 A",
		Status:   coreauth.StatusActive,
		Metadata: map[string]any{"type": "codex", "email": "a@example.com"},
	})
	if err != nil {
		t.Fatalf("Register first auth: %v", err)
	}
	_, err = manager.Register(context.Background(), &coreauth.Auth{
		ID:       "codex-oauth-2",
		Provider: "codex",
		Metadata: map[string]any{"type": "codex", "label": "设计号 B", "email": "b@example.com"},
		Status:   coreauth.StatusActive,
	})
	if err != nil {
		t.Fatalf("Register second auth: %v", err)
	}
	_, err = manager.Register(context.Background(), &coreauth.Auth{
		ID:       "gemini-oauth-1",
		Provider: "gemini-cli",
		Label:    "Gemini",
		Status:   coreauth.StatusActive,
		Metadata: map[string]any{"type": "gemini-cli"},
	})
	if err != nil {
		t.Fatalf("Register third auth: %v", err)
	}

	h := &Handler{authManager: manager}
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodGet, "/image-generation/channels", nil)

	h.ListImageGenerationChannels(c)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d, body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "设计号 A") || !strings.Contains(body, "设计号 B") {
		t.Fatalf("body = %s, want codex channel labels", body)
	}
	if strings.Contains(body, "Gemini") {
		t.Fatalf("body = %s, should not include non-codex channel", body)
	}
}

func TestImageGenerationSizePresetsAreTenantScoped(t *testing.T) {
	initManagementModelsTestDB(t)
	h := &Handler{}

	request := func(tenantID, method string, body []byte, handler gin.HandlerFunc) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(rec)
		c.Set(managementPrincipalKey, identity.Principal{EffectiveTenant: identity.Tenant{ID: tenantID}})
		c.Request = httptest.NewRequest(method, "/image-generation/size-presets", bytes.NewReader(body))
		c.Request.Header.Set("Content-Type", "application/json")
		handler(c)
		return rec
	}

	const tenantA = "00000000-0000-0000-0000-00000000000a"
	const tenantB = "00000000-0000-0000-0000-00000000000b"
	if rec := request(tenantA, http.MethodPut, []byte(`{"sizes":["4096x2304"]}`), h.PutImageGenerationSizePresets); rec.Code != http.StatusOK {
		t.Fatalf("tenant A PUT status=%d body=%s", rec.Code, rec.Body.String())
	}
	rec := request(tenantB, http.MethodGet, nil, h.GetImageGenerationSizePresets)
	if rec.Code != http.StatusOK {
		t.Fatalf("tenant B GET status=%d body=%s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "4096x2304") {
		t.Fatalf("tenant B inherited tenant A presets: %s", rec.Body.String())
	}
}

// managementImageProviderExecutor records which credential provider a request was
// routed to.
type managementImageProviderExecutor struct {
	managementImageExecutor
	identifier string
}

func (e *managementImageProviderExecutor) Identifier() string { return e.identifier }

func registerManagementImageProviderAuth(t *testing.T, manager *coreauth.Manager, id string, provider string, models ...string) {
	t.Helper()
	if _, err := manager.Register(context.Background(), &coreauth.Auth{
		ID:       id,
		Provider: provider,
		Status:   coreauth.StatusActive,
		Metadata: map[string]any{"access_token": "token"},
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
	t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(id) })
}

// TestImageGenerationRoutesGrokModelsToXAI is the point of the change: the handler
// used to pin every image request to the codex credential pool, so Grok image
// models were unreachable from the console even once the runtime could serve them.
func TestImageGenerationRoutesGrokModelsToXAI(t *testing.T) {
	gin.SetMode(gin.TestMode)

	executor := &managementImageProviderExecutor{identifier: "xai"}
	manager := coreauth.NewManager(nil, nil, nil)
	manager.RegisterExecutor(executor)
	registerManagementImageProviderAuth(t, manager, "xai-auth-image", "xai", "grok-imagine-image")

	h := &Handler{authManager: manager}
	_, err := h.executeImageGenerationTest(
		context.Background(),
		[]byte(`{"model":"grok-imagine-image","prompt":"a cat"}`),
		imageGenerationAlt,
	)
	if err != nil {
		t.Fatalf("executeImageGenerationTest() error = %v", err)
	}
	if executor.calls != 1 {
		t.Fatalf("xai executor calls = %d, want 1", executor.calls)
	}
	if executor.model != "grok-imagine-image" {
		t.Errorf("model = %q, want grok-imagine-image", executor.model)
	}
	if executor.alt != imageGenerationAlt {
		t.Errorf("alt = %q, want %q", executor.alt, imageGenerationAlt)
	}
}

// TestImageGenerationStillRoutesCodexModelsToCodex guards the existing path from
// regressing while the provider is derived rather than pinned.
func TestImageGenerationStillRoutesCodexModelsToCodex(t *testing.T) {
	gin.SetMode(gin.TestMode)

	executor := &managementImageProviderExecutor{identifier: "codex"}
	manager := coreauth.NewManager(nil, nil, nil)
	manager.RegisterExecutor(executor)
	registerManagementImageProviderAuth(t, manager, "codex-auth-image", "codex", imageGenerationModel)

	h := &Handler{authManager: manager}
	if _, err := h.executeImageGenerationTest(
		context.Background(),
		[]byte(`{"model":"gpt-image-2","prompt":"a cat"}`),
		imageGenerationAlt,
	); err != nil {
		t.Fatalf("executeImageGenerationTest() error = %v", err)
	}
	if executor.calls != 1 {
		t.Fatalf("codex executor calls = %d, want 1", executor.calls)
	}
}

func TestImageGenerationRejectsNonImageModels(t *testing.T) {
	gin.SetMode(gin.TestMode)

	manager := coreauth.NewManager(nil, nil, nil)
	h := &Handler{authManager: manager}
	_, err := h.executeImageGenerationTest(
		context.Background(),
		[]byte(`{"model":"grok-4.5","prompt":"a cat"}`),
		imageGenerationAlt,
	)
	if err == nil {
		t.Fatal("a chat model must not be accepted by the image generation path")
	}
	if !strings.Contains(err.Error(), "grok-4.5") {
		t.Errorf("error should name the rejected model, got %q", err)
	}
}
