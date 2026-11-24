package management

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	imagegeneration "github.com/router-for-me/CLIProxyAPI/v6/internal/management/imagegeneration"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/util"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdkmodelcatalog "github.com/router-for-me/CLIProxyAPI/v6/sdk/modelcatalog"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v6/sdk/translator"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// Model connectivity test.
//
// The panel used to answer "can this model be reached" from the browser: it
// listed the tenant's API keys, picked one itself, and sent a chat completion to
// the public /v1 endpoint. That answers a different question — whether that
// particular business identity may use the model. An operator checking a healthy
// kimi account got "no auth available" because the key it happened to pick was
// bound to an end user restricted to one channel group, and no amount of channel
// or model configuration on the account side could change that. So the probe runs
// here instead: management authority, tenant scope, no API key in the loop.
//
// It also used to send that same chat completion for every model in the catalog,
// including the ones that generate images and video. See model_test_modality.go
// for why that is wrong and how the shape is chosen now.

const modelTestPromptLimit = 4000

// modelTestTimeout bounds one synchronous probe. Long enough for a cold upstream,
// short enough that an operator is not left watching a spinner. Media modes do
// not use it — they run as tasks, because a video takes minutes and an image can
// outlive an intermediate proxy's idle timeout.
const modelTestTimeout = 120 * time.Second

const modelTestSystemAPIKey = "POST /models/test"

type modelTestRequest struct {
	Model   string `json:"model"`
	Prompt  string `json:"prompt"`
	Channel string `json:"channel"`
	// Mode selects the probe shape. Empty means "the model's default", which is
	// what the panel sends when the operator did not switch tabs.
	Mode string `json:"mode"`
	// Images are reference frames for the edit / vision / image-to-video modes,
	// as data URIs or https URLs.
	Images []string `json:"images"`
	// Image options. Gemini image models read AspectRatio (shared with video) and
	// ImageSize instead of Size and Quality.
	Size      string `json:"size"`
	Quality   string `json:"quality"`
	ImageSize string `json:"image_size"`
	N         int    `json:"n"`
	// Video options.
	Duration    int    `json:"duration"`
	AspectRatio string `json:"aspect_ratio"`
	Resolution  string `json:"resolution"`
}

// modelTestEnvelope carries a probe through the task service.
//
// The service hands its ExecuteFunc one payload, but a media probe needs the
// upstream body *and* the channel the operator picked. Putting the channel in the
// upstream body would forward it to the provider as an unknown field, so the two
// travel wrapped together and are unwrapped on the worker side. Nothing in this
// envelope reaches a provider.
type modelTestEnvelope struct {
	Mode     modelTestMode   `json:"mode"`
	Model    string          `json:"model"`
	Channel  string          `json:"channel"`
	Upstream json.RawMessage `json:"upstream"`
}

// GetModelTestOptions describes the probe form for one model: which modes it
// supports, their default prompts, and the option values this deployment accepts.
func (h *Handler) GetModelTestOptions(c *gin.Context) {
	model := strings.TrimSpace(c.Query("model"))
	if model == "" {
		writeMissingFieldError(c, "model", "请通过查询参数 model 指定要查看测试选项的模型 ID")
		return
	}
	c.JSON(http.StatusOK, buildModelTestOptions(model, effectiveTenantID(c)))
}

// PostModelTest runs one probe against a model using management authority and
// reports what came back.
//
// Text modes answer inline. Image and video modes start a task and return its id:
// a clip takes minutes upstream, and even a single image regularly runs past the
// idle timeout of whatever sits between the browser and this process, so holding
// the request open would report a gateway timeout as a model failure.
func (h *Handler) PostModelTest(c *gin.Context) {
	var body modelTestRequest
	if err := c.ShouldBindJSON(&body); err != nil {
		writeInvalidBodyError(c, err)
		return
	}
	body.Model = strings.TrimSpace(body.Model)
	body.Prompt = strings.TrimSpace(body.Prompt)
	if body.Model == "" {
		writeMissingFieldError(c, "model", "请指定要测试的模型 ID（model）")
		return
	}
	if body.Prompt == "" {
		writeMissingFieldError(c, "prompt", "请输入用于测试模型的提示词（prompt）")
		return
	}
	if len(body.Prompt) > modelTestPromptLimit {
		writeInvalidFieldError(c, "prompt", fmt.Sprintf("测试提示词过长，最多支持 %d 个字符，请精简后重试", modelTestPromptLimit))
		return
	}
	if h == nil || h.authManager == nil {
		writeServiceUnavailableError(c, "核心凭据管理服务")
		return
	}

	mode, err := resolveModelTestMode(body.Model, body.Mode)
	if err != nil {
		writeInvalidFieldError(c, "mode", err.Error())
		return
	}
	options := buildModelTestOptions(body.Model, effectiveTenantID(c))
	if body.Images, err = validateModelTestImages(body.Images, mode, options.MaxImages); err != nil {
		writeInvalidFieldError(c, "images", err.Error())
		return
	}
	if err = validateModelTestProviders(body.Model, mode); err != nil {
		writeError(c, http.StatusNotFound, "model_provider_not_found", fmt.Sprintf("未找到可承接该模型的可用提供商：%s", err.Error()))
		return
	}

	upstream, err := buildUpstreamPayload(body, mode)
	if err != nil {
		writeInternalError(c, "构造模型测试上游请求", err)
		return
	}

	if upstreamAlt(mode) == "" {
		h.runSynchronousModelTest(c, body, mode, upstream)
		return
	}
	h.startModelTestTask(c, body, mode, upstream)
}

// validateModelTestProviders fails fast when nothing can serve the model, so the
// operator gets "no provider serves this" rather than a generic routing error
// several layers down.
func validateModelTestProviders(model string, mode modelTestMode) error {
	switch mode {
	case modeImage, modeImageEdit:
		if registry.ImageGenerationProvider(model) == "" {
			return fmt.Errorf("model %q is not a supported image generation model", model)
		}
	case modeVideo, modeVideoFromImage:
		if registry.VideoGenerationProvider(model) == "" {
			return fmt.Errorf("model %q is not a supported video generation model", model)
		}
	default:
		if len(modelTestProviders(model)) == 0 {
			return fmt.Errorf("no provider serves model %q", model)
		}
	}
	return nil
}

// modelTestProviders lists the credential pools that serve a chat model.
func modelTestProviders(model string) []string {
	return sdkmodelcatalog.GetProviderName(model)
}

func (h *Handler) runSynchronousModelTest(c *gin.Context, body modelTestRequest, mode modelTestMode, upstream []byte) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), modelTestTimeout)
	defer cancel()
	// Same request identity as the asynchronous tests (imagegeneration.Service):
	// without it the request log row has no key to resolve a tenant from and is
	// filed, and priced, under the system tenant instead of the tenant testing.
	ctx = context.WithValue(ctx, util.ContextKeyTrustedTenantID, effectiveTenantID(c))
	ctx = context.WithValue(ctx, util.ContextKeyAPIKey, modelTestSystemAPIKey)

	startedAt := time.Now()
	resp, execErr := h.authManager.Execute(ctx, modelTestProviders(body.Model), coreexecutor.Request{
		Model:   body.Model,
		Payload: upstream,
		Format:  sdktranslator.FromString("openai"),
	}, coreexecutor.Options{
		OriginalRequest: upstream,
		SourceFormat:    sdktranslator.FromString("openai"),
		Metadata:        modelTestMetadata(effectiveTenantID(c), body.Channel),
	})
	elapsed := time.Since(startedAt).Milliseconds()

	snapshot := redactModelTestRequest(upstream)
	if execErr != nil {
		// The upstream reason is the whole point of the probe, so it is reported
		// as a result rather than swallowed into a 5xx.
		c.JSON(http.StatusOK, gin.H{
			"ok":          false,
			"mode":        string(mode),
			"error":       execErr.Error(),
			"duration_ms": elapsed,
			"request":     snapshot,
		})
		return
	}

	result, metadata := buildModelTestResult(mode, body.Model, body.Channel, resp.Payload)
	c.JSON(http.StatusOK, gin.H{
		"ok":   true,
		"mode": string(mode),
		// content stays in the response for callers that only ever rendered text.
		"content":     result.Text,
		"result":      result,
		"metadata":    metadata,
		"request":     snapshot,
		"duration_ms": elapsed,
	})
}

func (h *Handler) startModelTestTask(c *gin.Context, body modelTestRequest, mode modelTestMode, upstream []byte) {
	service := h.ensureModelTestService()
	if service == nil {
		writeServiceUnavailableError(c, "模型测试任务服务")
		return
	}
	envelope, err := json.Marshal(modelTestEnvelope{
		Mode:     mode,
		Model:    body.Model,
		Channel:  strings.TrimSpace(body.Channel),
		Upstream: upstream,
	})
	if err != nil {
		writeInternalError(c, "编码异步模型测试任务请求", err)
		return
	}

	task := service.Start(effectiveTenantID(c), envelope, upstreamAlt(mode))
	response := modelTestTaskSnapshot(task)
	response["ok"] = true
	response["mode"] = string(mode)
	response["request"] = redactModelTestRequest(upstream)
	c.JSON(http.StatusAccepted, response)
}

// GetModelTestTask reports the current state of a media probe.
func (h *Handler) GetModelTestTask(c *gin.Context) {
	taskID := strings.TrimSpace(c.Param("task_id"))
	if taskID == "" {
		writeMissingFieldError(c, "task_id", "请在路径参数中提供模型测试任务 ID")
		return
	}
	service := h.ensureModelTestService()
	if service == nil {
		writeServiceUnavailableError(c, "模型测试任务服务")
		return
	}
	task, ok := service.Get(effectiveTenantID(c), taskID)
	if !ok {
		writeNotFoundError(c, "模型测试任务", taskID)
		return
	}
	c.JSON(http.StatusOK, modelTestTaskSnapshot(task))
}

func modelTestTaskSnapshot(task imagegeneration.Snapshot) gin.H {
	if task.ID == "" {
		return gin.H{}
	}
	body := gin.H{
		"task_id":     task.ID,
		"status":      task.Status,
		"phase":       task.Phase,
		"elapsed_ms":  task.ElapsedMs,
		"duration_ms": task.ElapsedMs,
	}
	if task.Result != nil {
		// The worker already assembled result/metadata/request, so the stored
		// payload is spread into the response rather than nested a level deeper.
		if fields, ok := task.Result.(map[string]any); ok {
			for key, value := range fields {
				body[key] = value
			}
		} else {
			body["result"] = task.Result
		}
	}
	if task.Error != nil {
		body["ok"] = false
		body["error"] = modelTestErrorMessage(task.Error)
		body["error_detail"] = task.Error
	} else if task.Status == "succeeded" {
		body["ok"] = true
	}
	return body
}

// modelTestErrorMessage lifts the human-readable reason out of the task's nested
// error envelope, so the panel has one string to show without reaching through
// three levels of map.
func modelTestErrorMessage(detail map[string]any) string {
	body, _ := detail["body"].(map[string]any)
	if body == nil {
		return "model test failed"
	}
	errorBody, _ := body["error"].(map[string]any)
	if errorBody == nil {
		return "model test failed"
	}
	if message, ok := errorBody["message"].(string); ok && strings.TrimSpace(message) != "" {
		return message
	}
	return "model test failed"
}

// newModelTestService builds the task service for media probes.
//
// It deliberately does not require an auth manager, matching the image and video
// services: without one the task store still has to exist so polling an unknown
// id answers "not found" rather than "service unavailable". A run that genuinely
// has nowhere to execute is rejected by PostModelTest instead.
func (h *Handler) newModelTestService() *imagegeneration.Service {
	if h == nil {
		return nil
	}
	return shareTaskSnapshots(imagegeneration.NewService(h.executeModelTestTask, modelTestSystemAPIKey), jobKindModelTest)
}

func (h *Handler) ensureModelTestService() *imagegeneration.Service {
	if h == nil {
		return nil
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.modelTest == nil {
		h.modelTest = h.newModelTestService()
	}
	return h.modelTest
}

// executeModelTestTask runs one media probe and returns the finished report.
//
// Unlike the image and video consoles, this passes the operator's channel through
// to the router. The catalog's channel selector always implied the probe would
// land on that account; without this it only narrowed which credential pool was
// consulted, so a green result could come from a different account than the one
// being investigated.
func (h *Handler) executeModelTestTask(ctx context.Context, tenantID string, payload []byte, alt string) ([]byte, error) {
	var envelope modelTestEnvelope
	if err := json.Unmarshal(payload, &envelope); err != nil {
		return nil, fmt.Errorf("decode model test request")
	}

	startedAt := time.Now()
	response, err := h.executeModelTestUpstream(ctx, tenantID, envelope, alt)
	if err != nil {
		return nil, err
	}
	elapsed := time.Since(startedAt).Milliseconds()

	result, metadata := buildModelTestResult(envelope.Mode, envelope.Model, envelope.Channel, response)
	return json.Marshal(map[string]any{
		"result":      result,
		"metadata":    metadata,
		"request":     redactModelTestRequest(envelope.Upstream),
		"content":     result.Text,
		"duration_ms": elapsed,
	})
}

func (h *Handler) executeModelTestUpstream(ctx context.Context, tenantID string, envelope modelTestEnvelope, alt string) ([]byte, error) {
	if envelope.Mode == modeVideo || envelope.Mode == modeVideoFromImage {
		return h.executeModelTestVideo(ctx, tenantID, envelope)
	}
	provider := registry.ImageGenerationProvider(envelope.Model)
	if provider == "" {
		return nil, fmt.Errorf("model %q is not a supported image generation model", envelope.Model)
	}
	return h.executeModelTestCall(ctx, tenantID, provider, envelope, envelope.Upstream, alt)
}

// executeModelTestVideo submits the clip and waits for it, mirroring the video
// console: the upstream contract is submit-then-poll, and an operator wants one
// thing to watch rather than a request id to chase.
func (h *Handler) executeModelTestVideo(ctx context.Context, tenantID string, envelope modelTestEnvelope) ([]byte, error) {
	provider := registry.VideoGenerationProvider(envelope.Model)
	if provider == "" {
		return nil, fmt.Errorf("model %q is not a supported video generation model", envelope.Model)
	}
	submission, err := h.executeModelTestCall(ctx, tenantID, provider, envelope, envelope.Upstream, videoGenerationAlt)
	if err != nil {
		return nil, err
	}
	requestID := strings.TrimSpace(gjson.GetBytes(submission, "request_id").String())
	if requestID == "" {
		return submission, nil
	}

	statusPayload, err := sjson.SetBytes([]byte(`{}`), "request_id", requestID)
	if err != nil {
		return nil, fmt.Errorf("encode video status request")
	}

	deadline := time.Now().Add(videoPollBudget)
	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(videoPollInterval):
		}

		status, pollErr := h.executeModelTestCall(ctx, tenantID, provider, envelope, statusPayload, videoStatusAlt)
		if pollErr != nil {
			return nil, pollErr
		}
		switch strings.ToLower(strings.TrimSpace(gjson.GetBytes(status, "status").String())) {
		case "done":
			return status, nil
		case "failed":
			return nil, fmt.Errorf("video generation failed upstream")
		case "expired":
			return nil, fmt.Errorf("video generation request expired upstream")
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("video generation did not finish within %s", videoPollBudget)
		}
	}
}

func (h *Handler) executeModelTestCall(
	ctx context.Context,
	tenantID, provider string,
	envelope modelTestEnvelope,
	payload []byte,
	alt string,
) ([]byte, error) {
	resp, err := h.authManager.Execute(ctx, []string{provider}, coreexecutor.Request{
		Model:   envelope.Model,
		Payload: payload,
		Format:  sdktranslator.FromString("openai"),
	}, coreexecutor.Options{
		Alt:             alt,
		OriginalRequest: payload,
		SourceFormat:    sdktranslator.FromString("openai"),
		Metadata:        modelTestMetadata(tenantID, envelope.Channel),
	})
	if err != nil {
		return nil, err
	}
	return resp.Payload, nil
}

// modelTestMetadata scopes the probe to the tenant and, when the operator picked
// a channel, to that channel. Restricting by channel is what the panel's channel
// selector always implied but never did: it only used the value to choose an API
// key, so the request could still land on a different account.
func modelTestMetadata(tenantID, channel string) map[string]any {
	meta := probeMetadata(tenantID)
	if channel = strings.TrimSpace(channel); channel != "" {
		meta["allowed-channels"] = channel
	}
	return meta
}

// modelTestContent pulls the assistant text out of an OpenAI-shaped response,
// falling back to the raw body so an unexpected shape is still inspectable.
func modelTestContent(payload []byte) string {
	if len(payload) == 0 {
		return ""
	}
	if text := gjson.GetBytes(payload, "choices.0.message.content").String(); strings.TrimSpace(text) != "" {
		return text
	}
	if parts := gjson.GetBytes(payload, "choices.0.message.content.#.text"); parts.Exists() {
		texts := make([]string, 0, len(parts.Array()))
		for _, part := range parts.Array() {
			if value := strings.TrimSpace(part.String()); value != "" {
				texts = append(texts, value)
			}
		}
		if len(texts) > 0 {
			return strings.Join(texts, "\n")
		}
	}
	return string(payload)
}
