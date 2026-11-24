package management

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	imagegeneration "github.com/router-for-me/CLIProxyAPI/v6/internal/management/imagegeneration"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/registry"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v6/sdk/translator"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// Console-side video generation.
//
// The public API keeps xAI's two-step shape, because a client cannot hold a
// request open for the minutes a clip takes. The console has the opposite need: an
// operator pressing "test" wants one thing to watch. So the console task, which is
// already an async job with its own id and polling, absorbs the upstream polling
// and finishes when the clip is ready. Same primitive as the image test, one more
// wait inside it.
const (
	videoGenerationAlt          = "videos/generations"
	videoStatusAlt              = "videos/status"
	videoGenerationSystemAPIKey = "POST /video-generation/test"

	// Ceiling for one console test. The task service applies its own timeout as
	// well; this bound exists so a generation that upstream never finishes cannot
	// hold a worker until that outer timeout fires.
	videoPollBudget = 8 * time.Minute
)

// videoPollInterval spaces the console's status polls; tests shorten it.
var videoPollInterval = 5 * time.Second

// ListVideoGenerationModels reports the video models together with the channels
// this tenant can actually reach.
//
// The catalog alone is not enough for the console: a tenant with no xAI credential
// would still see a selectable model and a live "generate" button, and only learn
// at request time that nothing can serve it ("auth_not_found: no auth available").
// Availability is therefore computed per effective tenant and returned alongside,
// so the page can disable the action and say why.
func (h *Handler) ListVideoGenerationModels(c *gin.Context) {
	models := registry.ListVideoGenerationModels()
	channelsByProvider := h.videoGenerationChannelsByProvider(c)

	items := make([]gin.H, 0, len(models))
	for _, model := range models {
		channels := channelsByProvider[strings.ToLower(strings.TrimSpace(model.Provider))]
		items = append(items, gin.H{
			"id":                      model.ID,
			"provider":                model.Provider,
			"display_name":            model.DisplayName,
			"description":             model.Description,
			"supports_image_to_video": model.SupportsImage,
			"max_duration_seconds":    model.MaxDurationSeconds,
			"price_per_call":          model.PricePerCall,
			"channels":                channels,
			"available":               len(channels) > 0,
		})
	}

	flat := make([]string, 0)
	flatSeen := make(map[string]struct{})
	for _, channels := range channelsByProvider {
		for _, name := range channels {
			key := strings.ToLower(name)
			if _, ok := flatSeen[key]; ok {
				continue
			}
			flatSeen[key] = struct{}{}
			flat = append(flat, name)
		}
	}
	sort.Strings(flat)

	c.JSON(http.StatusOK, gin.H{"models": items, "channels": flat})
}

// videoGenerationChannelsByProvider lists the enabled credentials of this tenant
// that can serve a video model, keyed by provider.
//
// Unlike the image equivalent this does not require an OAuth account: xAI serves
// the media host with an API key just as well, and filtering those out would hide
// working credentials.
func (h *Handler) videoGenerationChannelsByProvider(c *gin.Context) map[string][]string {
	byProvider := make(map[string][]string)
	if h == nil || h.authManager == nil {
		return byProvider
	}
	providers := make(map[string]struct{})
	for _, model := range registry.ListVideoGenerationModels() {
		providers[strings.ToLower(strings.TrimSpace(model.Provider))] = struct{}{}
	}

	seen := make(map[string]struct{})
	for _, auth := range h.authManager.ListForTenant(effectiveTenantID(c)) {
		if auth == nil || auth.Disabled || auth.Status == coreauth.StatusDisabled {
			continue
		}
		provider := strings.ToLower(strings.TrimSpace(auth.Provider))
		if _, ok := providers[provider]; !ok {
			continue
		}
		name := strings.TrimSpace(auth.ChannelName())
		if name == "" {
			continue
		}
		key := provider + "\x00" + strings.ToLower(name)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		byProvider[provider] = append(byProvider[provider], name)
	}
	for provider := range byProvider {
		sort.Strings(byProvider[provider])
	}
	return byProvider
}

func (h *Handler) PostVideoGenerationTest(c *gin.Context) {
	payload, err := parseVideoGenerationTestPayload(c)
	if err != nil {
		writeError(c, http.StatusBadRequest, "invalid_video_generation_request", err.Error())
		return
	}
	if h == nil || h.authManager == nil {
		writeServiceUnavailableError(c, "核心凭据管理服务")
		return
	}
	service := h.ensureVideoGenerationService()
	if service == nil {
		writeServiceUnavailableError(c, "视频生成测试服务")
		return
	}

	task := service.Start(effectiveTenantID(c), payload, videoGenerationAlt)
	c.JSON(http.StatusAccepted, imageGenerationTaskSnapshot(task))
}

func (h *Handler) GetVideoGenerationTestTask(c *gin.Context) {
	taskID := strings.TrimSpace(c.Param("task_id"))
	if taskID == "" {
		writeMissingFieldError(c, "task_id", "请在路径参数中提供视频生成测试任务 ID")
		return
	}
	service := h.ensureVideoGenerationService()
	if service == nil {
		writeServiceUnavailableError(c, "视频生成测试服务")
		return
	}
	task, ok := service.Get(effectiveTenantID(c), taskID)
	if !ok {
		writeNotFoundError(c, "视频生成测试任务", taskID)
		return
	}
	c.JSON(http.StatusOK, imageGenerationTaskSnapshot(task))
}

// parseVideoGenerationTestPayload validates the console form into an upstream body.
func parseVideoGenerationTestPayload(c *gin.Context) ([]byte, error) {
	raw, err := c.GetRawData()
	if err != nil {
		return nil, fmt.Errorf("读取视频生成请求体失败，请重试")
	}
	if !gjson.ValidBytes(raw) {
		return nil, fmt.Errorf("请求体不是合法的 JSON 格式，请检查视频生成参数")
	}

	model := strings.TrimSpace(gjson.GetBytes(raw, "model").String())
	if model == "" {
		return nil, fmt.Errorf("缺少必填参数 model，请指定要测试的视频生成模型")
	}
	if registry.VideoGenerationProvider(model) == "" {
		return nil, fmt.Errorf("模型 %q 不支持视频生成功能，请选择支持的视频模型", model)
	}
	if strings.TrimSpace(gjson.GetBytes(raw, "prompt").String()) == "" {
		return nil, fmt.Errorf("缺少必填参数 prompt，请输入视频生成提示词")
	}
	if duration := gjson.GetBytes(raw, "duration"); duration.Exists() {
		if duration.Type != gjson.Number || duration.Int() < 1 {
			return nil, fmt.Errorf("参数 duration 必须是大于 0 的秒数")
		}
	}
	return raw, nil
}

// executeVideoGenerationTestForTenant submits the job and waits for the clip.
func (h *Handler) executeVideoGenerationTestForTenant(ctx context.Context, tenantID string, payload []byte, alt string) ([]byte, error) {
	_ = alt
	modelName := strings.TrimSpace(gjson.GetBytes(payload, "model").String())
	provider := registry.VideoGenerationProvider(modelName)
	if provider == "" {
		return nil, fmt.Errorf("model %q is not a supported video generation model", modelName)
	}

	submission, authID, err := h.executeVideoCall(ctx, tenantID, provider, modelName, payload, videoGenerationAlt, "")
	if err != nil {
		return nil, err
	}
	requestID := strings.TrimSpace(gjson.GetBytes(submission, "request_id").String())
	if requestID == "" {
		// Nothing to poll: either the upstream already returned the clip or it
		// answered in a shape this build does not know. Hand the body back rather
		// than inventing a failure.
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

		status, _, pollErr := h.executeVideoCall(ctx, tenantID, provider, modelName, statusPayload, videoStatusAlt, authID)
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

// executeVideoCall runs one video request and reports the credential that
// served it. A video request id only resolves on the account that created it,
// so the polls of a job pass that account as pinnedAuthID; left to the
// scheduler, a tenant with two xAI accounts had some polls answered 404.
func (h *Handler) executeVideoCall(ctx context.Context, tenantID, provider, model string, payload []byte, alt, pinnedAuthID string) ([]byte, string, error) {
	var selectedMu sync.Mutex
	selected := ""
	meta := probeMetadata(tenantID)
	meta[coreexecutor.SelectedAuthCallbackMetadataKey] = func(authID string) {
		selectedMu.Lock()
		selected = strings.TrimSpace(authID)
		selectedMu.Unlock()
	}
	if pinnedAuthID != "" {
		meta[coreexecutor.PinnedAuthMetadataKey] = pinnedAuthID
	}
	resp, err := h.authManager.Execute(ctx, []string{provider}, coreexecutor.Request{
		Model:   model,
		Payload: payload,
		Format:  sdktranslator.FromString("openai"),
	}, coreexecutor.Options{
		Alt:             alt,
		OriginalRequest: payload,
		SourceFormat:    sdktranslator.FromString("openai"),
		Metadata:        meta,
	})
	if err != nil {
		return nil, "", err
	}
	selectedMu.Lock()
	defer selectedMu.Unlock()
	return resp.Payload, selected, nil
}

func (h *Handler) newVideoGenerationService() *imagegeneration.Service {
	if h == nil {
		return nil
	}
	return shareTaskSnapshots(imagegeneration.NewService(func(ctx context.Context, tenantID string, payload []byte, alt string) ([]byte, error) {
		return h.executeVideoGenerationTestForTenant(ctx, tenantID, payload, alt)
	}, videoGenerationSystemAPIKey), jobKindVideoGenerationTest)
}
