package management

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	imagegeneration "github.com/router-for-me/CLIProxyAPI/v6/internal/management/imagegeneration"
	settingsstore "github.com/router-for-me/CLIProxyAPI/v6/internal/management/settings/store"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v6/sdk/translator"
	"github.com/tidwall/gjson"
)

const (
	imageGenerationModel        = "gpt-image-2"
	imageGenerationAlt          = "images/generations"
	imageEditsAlt               = "images/edits"
	imageMaxUploads             = 5
	imageGenerationSystemAPIKey = "POST /image-generation/test"
)

var defaultImageGenerationSizePresets = []string{
	"1024x1024",
	"1792x1024",
	"1024x1792",
	"2560x1440",
	"2160x3840",
}

func (h *Handler) GetImageGenerationSizePresets(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{"sizes": imageGenerationSizePresets()})
}

func (h *Handler) PutImageGenerationSizePresets(c *gin.Context) {
	var body settingsstore.ImageSizePresetsSetting
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body"})
		return
	}
	sizes, err := normalizeImageGenerationSizePresets(body.Sizes)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	customSizes := customImageGenerationSizePresets(sizes)
	if err := settingsstore.StoreImageSizePresetsSetting(customSizes); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("failed to save size presets: %v", err)})
		return
	}
	c.JSON(http.StatusOK, gin.H{"sizes": mergeImageGenerationSizePresets(defaultImageGenerationSizePresets, customSizes)})
}

func imageGenerationSizePresets() []string {
	customSizes := loadImageGenerationCustomSizePresets()
	return mergeImageGenerationSizePresets(defaultImageGenerationSizePresets, customSizes)
}

func loadImageGenerationCustomSizePresets() []string {
	return filterValidImageGenerationSizePresets(settingsstore.LoadImageSizePresetsSetting())
}

func filterValidImageGenerationSizePresets(values []string) []string {
	sizes := make([]string, 0, len(values))
	seen := make(map[string]struct{})
	for _, value := range values {
		size, ok := normalizeImageGenerationSizePreset(value)
		if !ok {
			continue
		}
		if _, exists := seen[size]; exists {
			continue
		}
		seen[size] = struct{}{}
		sizes = append(sizes, size)
	}
	return sizes
}

func normalizeImageGenerationSizePresets(values []string) ([]string, error) {
	sizes := make([]string, 0, len(values))
	seen := make(map[string]struct{})
	for _, value := range values {
		if strings.TrimSpace(value) == "" {
			continue
		}
		size, ok := normalizeImageGenerationSizePreset(value)
		if !ok {
			return nil, fmt.Errorf("invalid size %q", value)
		}
		if _, exists := seen[size]; exists {
			continue
		}
		seen[size] = struct{}{}
		sizes = append(sizes, size)
	}
	return sizes, nil
}

func customImageGenerationSizePresets(values []string) []string {
	defaults := make(map[string]struct{}, len(defaultImageGenerationSizePresets))
	for _, value := range defaultImageGenerationSizePresets {
		defaults[value] = struct{}{}
	}
	custom := make([]string, 0, len(values))
	for _, value := range values {
		if _, ok := defaults[value]; ok {
			continue
		}
		custom = append(custom, value)
	}
	return custom
}

func mergeImageGenerationSizePresets(lists ...[]string) []string {
	merged := make([]string, 0)
	seen := make(map[string]struct{})
	for _, list := range lists {
		for _, value := range list {
			size, ok := normalizeImageGenerationSizePreset(value)
			if !ok {
				continue
			}
			if _, exists := seen[size]; exists {
				continue
			}
			seen[size] = struct{}{}
			merged = append(merged, size)
		}
	}
	return merged
}

func normalizeImageGenerationSizePreset(value string) (string, bool) {
	normalized := strings.NewReplacer("X", "x", "×", "x", "*", "x").Replace(strings.TrimSpace(value))
	parts := strings.Split(normalized, "x")
	if len(parts) != 2 {
		return "", false
	}
	width := strings.TrimSpace(parts[0])
	height := strings.TrimSpace(parts[1])
	if !validImageGenerationSizeDimension(width) || !validImageGenerationSizeDimension(height) {
		return "", false
	}
	return width + "x" + height, true
}

func validImageGenerationSizeDimension(value string) bool {
	if value == "" || value[0] == '0' {
		return false
	}
	for _, char := range value {
		if char < '0' || char > '9' {
			return false
		}
	}
	return true
}

func (h *Handler) PostImageGenerationTest(c *gin.Context) {
	payload, alt, err := parseImageGenerationTestPayload(c)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if h == nil || h.authManager == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "auth manager unavailable"})
		return
	}
	service := h.ensureImageGenerationService()
	if service == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "image generation service unavailable"})
		return
	}

	task := service.Start(payload, alt)
	c.JSON(http.StatusAccepted, imageGenerationTaskSnapshot(task))
}

func (h *Handler) GetImageGenerationTestTask(c *gin.Context) {
	taskID := strings.TrimSpace(c.Param("task_id"))
	if taskID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "task_id is required"})
		return
	}
	service := h.ensureImageGenerationService()
	if service == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "image generation service unavailable"})
		return
	}
	task, ok := service.Get(taskID)
	if !ok {
		c.JSON(http.StatusNotFound, gin.H{"error": "image generation task not found"})
		return
	}
	c.JSON(http.StatusOK, imageGenerationTaskSnapshot(task))
}

func (h *Handler) executeImageGenerationTest(ctx context.Context, payload []byte, alt string) ([]byte, error) {
	modelName := strings.TrimSpace(gjson.GetBytes(payload, "model").String())
	if modelName == "" {
		modelName = imageGenerationModel
	}
	imageCount, err := imageGenerationRequestCount(payload)
	if err != nil {
		return nil, err
	}
	payloads := make([][]byte, 0, imageCount)
	for i := 0; i < imageCount; i++ {
		execPayload := payload
		if imageCount > 1 {
			var setErr error
			execPayload, setErr = setImageGenerationRequestCount(payload, 1)
			if setErr != nil {
				return nil, fmt.Errorf("invalid image generation request")
			}
		}
		resp, execErr := h.authManager.Execute(ctx, []string{"codex"}, coreexecutor.Request{
			Model:   modelName,
			Payload: execPayload,
			Format:  sdktranslator.FromString("openai"),
		}, coreexecutor.Options{
			Alt:          alt,
			SourceFormat: sdktranslator.FromString("openai"),
			Metadata: map[string]any{
				coreexecutor.SinglePickMetadataKey: true,
			},
		})
		if execErr != nil {
			return nil, execErr
		}
		payloads = append(payloads, resp.Payload)
	}
	mergedPayload, err := mergeImageGenerationResponses(payloads)
	if err != nil {
		return nil, err
	}

	return mergedPayload, nil
}

func imageGenerationTaskSnapshot(task imagegeneration.Snapshot) gin.H {
	if task.ID == "" {
		return gin.H{}
	}
	body := gin.H{
		"task_id":    task.ID,
		"status":     task.Status,
		"phase":      task.Phase,
		"created_at": task.CreatedAt,
		"updated_at": task.UpdatedAt,
		"elapsed_ms": time.Since(task.CreatedAt).Milliseconds(),
	}
	if task.Result != nil {
		body["result"] = task.Result
	}
	if task.Error != nil {
		body["error"] = task.Error
	}
	return body
}

type upstreamErrorBodyProvider interface {
	UpstreamErrorBody() []byte
}

// imageGenerationErrorResponse remains in transport so legacy response contract
// tests keep asserting the public error body shape independent of task lifecycle ownership.
func imageGenerationErrorResponse(err error, errorType string) gin.H {
	msg := ""
	if err != nil {
		msg = strings.TrimSpace(err.Error())
	}
	if msg == "" {
		msg = "Upstream image generation request failed."
	}
	typ := strings.TrimSpace(errorType)
	if typ == "" {
		typ = "upstream_error"
	}
	errorBody := gin.H{
		"message": msg,
		"type":    typ,
	}
	if upstreamErr, ok := err.(upstreamErrorBodyProvider); ok {
		upstreamBody := strings.TrimSpace(string(upstreamErr.UpstreamErrorBody()))
		if upstreamBody != "" {
			errorBody["upstream"] = parseImageGenerationUpstreamBody(upstreamBody)
		}
	}
	return gin.H{"error": errorBody}
}

func parseImageGenerationUpstreamBody(body string) any {
	var decoded any
	if err := json.Unmarshal([]byte(body), &decoded); err == nil {
		return decoded
	}
	return body
}

func imageGenerationRequestCount(payload []byte) (int, error) {
	var body struct {
		N int `json:"n"`
	}
	if err := json.Unmarshal(payload, &body); err != nil {
		return 0, fmt.Errorf("invalid image generation request")
	}
	if body.N == 0 {
		return 1, nil
	}
	if body.N < 1 || body.N > 4 {
		return 0, fmt.Errorf("n must be between 1 and 4")
	}
	return body.N, nil
}

func setImageGenerationRequestCount(payload []byte, n int) ([]byte, error) {
	var body map[string]any
	if err := json.Unmarshal(payload, &body); err != nil {
		return nil, err
	}
	body["n"] = n
	return json.Marshal(body)
}

func mergeImageGenerationResponses(payloads [][]byte) ([]byte, error) {
	if len(payloads) == 0 {
		return nil, fmt.Errorf("image generation returned no responses")
	}
	if len(payloads) == 1 {
		return payloads[0], nil
	}
	var merged map[string]json.RawMessage
	if err := json.Unmarshal(payloads[0], &merged); err != nil {
		return nil, fmt.Errorf("parse image generation response: %w", err)
	}
	data := make([]json.RawMessage, 0, len(payloads))
	for _, payload := range payloads {
		var item struct {
			Data []json.RawMessage `json:"data"`
		}
		if err := json.Unmarshal(payload, &item); err != nil {
			return nil, fmt.Errorf("parse image generation response: %w", err)
		}
		data = append(data, item.Data...)
	}
	encodedData, err := json.Marshal(data)
	if err != nil {
		return nil, fmt.Errorf("encode image generation response: %w", err)
	}
	merged["data"] = encodedData
	return json.Marshal(merged)
}

func parseImageGenerationTestPayload(c *gin.Context) ([]byte, string, error) {
	contentType := strings.TrimSpace(c.GetHeader("Content-Type"))
	mediaType, _, err := mime.ParseMediaType(contentType)
	if err == nil && strings.EqualFold(mediaType, "multipart/form-data") {
		return parseImageGenerationMultipartPayload(c)
	}

	var body struct {
		Model   string `json:"model"`
		Prompt  string `json:"prompt"`
		Size    string `json:"size"`
		Quality string `json:"quality"`
		N       int    `json:"n"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		return nil, "", fmt.Errorf("invalid body")
	}
	model := strings.TrimSpace(body.Model)
	if model == "" {
		model = imageGenerationModel
	}
	prompt := strings.TrimSpace(body.Prompt)
	if prompt == "" {
		return nil, "", fmt.Errorf("prompt is required")
	}
	payload := map[string]any{
		"model":  model,
		"prompt": prompt,
	}
	if size := strings.TrimSpace(body.Size); size != "" {
		payload["size"] = size
	}
	if quality := strings.TrimSpace(body.Quality); quality != "" {
		payload["quality"] = quality
	}
	if body.N > 0 {
		payload["n"] = body.N
	}
	data, _ := json.Marshal(payload)
	return data, imageGenerationAlt, nil
}

func parseImageGenerationMultipartPayload(c *gin.Context) ([]byte, string, error) {
	if err := c.Request.ParseMultipartForm(20 << 20); err != nil {
		return nil, "", fmt.Errorf("invalid body")
	}
	form := c.Request.MultipartForm
	if form == nil {
		return nil, "", fmt.Errorf("invalid body")
	}
	model := firstImageGenerationFormValue(form.Value, "model", imageGenerationModel)
	prompt := firstImageGenerationFormValue(form.Value, "prompt", "")
	if strings.TrimSpace(prompt) == "" {
		return nil, "", fmt.Errorf("prompt is required")
	}
	payload := map[string]any{
		"model":  model,
		"prompt": prompt,
	}
	if size := firstImageGenerationFormValue(form.Value, "size", ""); size != "" {
		payload["size"] = size
	}
	if quality := firstImageGenerationFormValue(form.Value, "quality", ""); quality != "" {
		payload["quality"] = quality
	}
	for _, field := range []string{"background", "output_format", "moderation", "input_fidelity", "style"} {
		if value := firstImageGenerationFormValue(form.Value, field, ""); value != "" {
			payload[field] = value
		}
	}
	for _, field := range []string{"output_compression", "partial_images"} {
		if value := firstImageGenerationFormValue(form.Value, field, ""); value != "" {
			parsed, err := strconv.Atoi(value)
			if err != nil || parsed < 0 {
				return nil, "", fmt.Errorf("%s must be a positive integer", field)
			}
			payload[field] = parsed
		}
	}
	if value := firstImageGenerationFormValue(form.Value, "n", ""); value != "" {
		n, err := strconv.Atoi(value)
		if err != nil || n <= 0 {
			return nil, "", fmt.Errorf("n must be a positive integer")
		}
		payload["n"] = n
	}
	files := form.File["image"]
	for key, value := range form.File {
		if strings.HasPrefix(key, "image[") {
			files = append(files, value...)
		}
	}
	if len(files) == 0 {
		return nil, "", fmt.Errorf("image file is required")
	}
	if len(files) > imageMaxUploads {
		return nil, "", fmt.Errorf("image edit supports at most %d images", imageMaxUploads)
	}
	uploads := make([]map[string]any, 0, len(files))
	for _, fileHeader := range files {
		if fileHeader == nil {
			continue
		}
		file, err := fileHeader.Open()
		if err != nil {
			return nil, "", fmt.Errorf("read image file: %w", err)
		}
		data, err := io.ReadAll(io.LimitReader(file, (20<<20)+1))
		_ = file.Close()
		if err != nil {
			return nil, "", fmt.Errorf("read image file: %w", err)
		}
		if len(data) == 0 {
			return nil, "", fmt.Errorf("image file is empty")
		}
		if len(data) > 20<<20 {
			return nil, "", fmt.Errorf("image file is too large")
		}
		uploads = append(uploads, map[string]any{
			"file_name":    fileHeader.Filename,
			"content_type": strings.TrimSpace(fileHeader.Header.Get("Content-Type")),
			"data_base64":  base64.StdEncoding.EncodeToString(data),
		})
	}
	payload["image_files"] = uploads
	if maskFiles := form.File["mask"]; len(maskFiles) > 0 && maskFiles[0] != nil {
		maskPayload, err := buildImageGenerationUploadPayload(maskFiles[0])
		if err != nil {
			return nil, "", err
		}
		payload["mask_file"] = maskPayload
	}
	data, _ := json.Marshal(payload)
	return data, imageEditsAlt, nil
}

func buildImageGenerationUploadPayload(fileHeader *multipart.FileHeader) (map[string]any, error) {
	if fileHeader == nil {
		return nil, fmt.Errorf("image file is required")
	}
	file, err := fileHeader.Open()
	if err != nil {
		return nil, fmt.Errorf("read image file: %w", err)
	}
	data, err := io.ReadAll(io.LimitReader(file, (20<<20)+1))
	_ = file.Close()
	if err != nil {
		return nil, fmt.Errorf("read image file: %w", err)
	}
	if len(data) == 0 {
		return nil, fmt.Errorf("image file is empty")
	}
	if len(data) > 20<<20 {
		return nil, fmt.Errorf("image file is too large")
	}
	contentType := strings.TrimSpace(fileHeader.Header.Get("Content-Type"))
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	return map[string]any{
		"file_name":    fileHeader.Filename,
		"content_type": contentType,
		"data_base64":  base64.StdEncoding.EncodeToString(data),
	}, nil
}

func firstImageGenerationFormValue(values map[string][]string, key, fallback string) string {
	for _, value := range values[key] {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return trimmed
		}
	}
	return fallback
}

func (h *Handler) ListImageGenerationChannels(c *gin.Context) {
	channels := make([]string, 0)
	seen := make(map[string]struct{})
	if h != nil && h.authManager != nil {
		for _, auth := range h.authManager.List() {
			if auth == nil || auth.Disabled {
				continue
			}
			if !strings.EqualFold(strings.TrimSpace(auth.Provider), "codex") {
				continue
			}
			accountType, _ := auth.AccountInfo()
			if !strings.EqualFold(strings.TrimSpace(accountType), "oauth") {
				continue
			}
			if auth.Status == coreauth.StatusDisabled {
				continue
			}
			name := strings.TrimSpace(auth.ChannelName())
			if name == "" {
				continue
			}
			key := strings.ToLower(name)
			if _, ok := seen[key]; ok {
				continue
			}
			seen[key] = struct{}{}
			channels = append(channels, name)
		}
	}
	sort.Strings(channels)
	c.JSON(http.StatusOK, gin.H{
		"model":    imageGenerationModel,
		"channels": channels,
	})
}
