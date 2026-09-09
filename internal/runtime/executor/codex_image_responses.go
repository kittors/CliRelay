package executor

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/codexcarrier"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

type codexResponsesImageResult struct {
	Result        string
	RevisedPrompt string
	OutputFormat  string
	Size          string
	Background    string
	Quality       string
}

func (e *CodexExecutor) executeCodexImageViaResponses(
	execCtx *ExecutionContext,
	parsed *codexImageRequest,
) ([]byte, http.Header, error) {
	apiKey, baseURL := codexCreds(execCtx.Auth)
	if baseURL == "" {
		baseURL = "https://chatgpt.com/backend-api/codex"
	}
	// The caller's image model is forwarded rather than the compiled-in default, so
	// selecting gpt-image-2.5-* actually reaches upstream. Upstream currently ignores
	// this field on the subscription channel, which is why the catalog descriptions
	// say so; sending it anyway is what makes the ids work the moment that changes.
	body, err := buildCodexImageResponsesRequest(parsed, codexImageToolModel(parsed), e.resolveCodexImageBaseModel())
	if err != nil {
		return nil, nil, statusErr{code: http.StatusBadRequest, msg: err.Error()}
	}
	url := strings.TrimSuffix(baseURL, "/") + "/responses"
	recorder := execCtx.Recorder()
	httpClient := execCtx.HTTPClient(0)
	var lastErr error
	for attempt := 1; attempt <= codexImageResponsesMaxTries; attempt++ {
		httpReq, err := http.NewRequestWithContext(execCtx.Context, http.MethodPost, url, bytes.NewReader(body))
		if err != nil {
			return nil, nil, err
		}
		applyCodexHeaders(httpReq, e.cfg, execCtx.Auth, apiKey, true)
		recorder.RecordRequest(url, http.MethodPost, httpReq.Header.Clone(), body)

		httpResp, err := httpClient.Do(httpReq)
		if err != nil {
			recorder.RecordResponseError(err)
			return nil, nil, err
		}
		recorder.RecordResponseMetadata(httpResp.StatusCode, httpResp.Header.Clone())
		if httpResp.StatusCode < http.StatusOK || httpResp.StatusCode >= http.StatusMultipleChoices {
			upstreamBody := readUpstreamErrorBody(e.Identifier(), httpResp.Body)
			_ = httpResp.Body.Close()
			recorder.AppendResponseChunk(upstreamBody)
			return nil, nil, newCodexStatusErr(httpResp.StatusCode, upstreamBody, httpResp.Header)
		}

		rawBody, err := readUpstreamResponseBody(e.Identifier(), httpResp.Body)
		responseHeaders := httpResp.Header.Clone()
		_ = httpResp.Body.Close()
		if err != nil {
			recorder.RecordResponseError(err)
			return nil, nil, err
		}
		recorder.AppendResponseChunk(rawBody)

		results, createdAt, err := collectCodexImagesFromResponsesBody(rawBody)
		if err != nil {
			lastErr = err
			if retryDelay, ok := codexImageResponsesRetryDelay(err, attempt); ok {
				if waitErr := waitCodexImageResponsesRetry(execCtx.Context, retryDelay); waitErr != nil {
					return nil, nil, waitErr
				}
				continue
			}
			return nil, nil, err
		}
		if len(results) == 0 {
			return nil, nil, statusErr{code: http.StatusBadGateway, msg: "responses image request returned no generated images"}
		}
		payload, err := buildCodexImageOpenAIResponseFromResults(results, createdAt)
		if err != nil {
			return nil, nil, err
		}
		return payload, responseHeaders, nil
	}
	if lastErr != nil {
		return nil, nil, lastErr
	}
	return nil, nil, statusErr{code: http.StatusBadGateway, msg: "responses image request failed"}
}

// codexImageToolModel resolves the image model to attach to the image_generation
// tool, falling back to the compiled-in default when a caller omitted it.
func codexImageToolModel(parsed *codexImageRequest) string {
	if parsed == nil {
		return codexImageModel
	}
	if model := strings.TrimSpace(parsed.Model); model != "" {
		return model
	}
	return codexImageModel
}

func buildCodexImageResponsesRequest(parsed *codexImageRequest, toolModel string, baseModel string) ([]byte, error) {
	if parsed == nil {
		return nil, fmt.Errorf("parsed images request is required")
	}
	prompt := strings.TrimSpace(parsed.Prompt)
	if prompt == "" {
		return nil, fmt.Errorf("prompt is required")
	}
	inputImages := make([]string, 0, len(parsed.InputImageURLs)+len(parsed.Uploads))
	for _, imageURL := range parsed.InputImageURLs {
		if trimmed := strings.TrimSpace(imageURL); trimmed != "" {
			inputImages = append(inputImages, trimmed)
		}
	}
	for _, upload := range parsed.Uploads {
		dataURL, err := codexImageUploadToDataURL(upload)
		if err != nil {
			return nil, err
		}
		inputImages = append(inputImages, dataURL)
	}
	if parsed.hasEditInputs() && len(inputImages) == 0 {
		return nil, fmt.Errorf("image input is required")
	}

	req := []byte(`{"instructions":"","stream":true,"reasoning":{"effort":"medium","summary":"auto"},"parallel_tool_calls":true,"include":["reasoning.encrypted_content"],"model":"","store":false,"tool_choice":{"type":"image_generation"}}`)
	if strings.TrimSpace(baseModel) == "" {
		baseModel = codexcarrier.Resolve()
	}
	req, _ = sjson.SetBytes(req, "model", strings.TrimSpace(baseModel))

	input := []byte(`[{"type":"message","role":"user","content":[{"type":"input_text","text":""}]}]`)
	input, _ = sjson.SetBytes(input, "0.content.0.text", buildCodexImageResponsesInputText(parsed, prompt))
	for index, imageURL := range inputImages {
		part := []byte(`{"type":"input_image","image_url":""}`)
		part, _ = sjson.SetBytes(part, "image_url", imageURL)
		input, _ = sjson.SetRawBytes(input, fmt.Sprintf("0.content.%d", index+1), part)
	}
	req, _ = sjson.SetRawBytes(req, "input", input)

	action := "generate"
	if parsed.hasEditInputs() {
		action = "edit"
	}
	tool := []byte(`{"type":"image_generation","action":"","model":""}`)
	tool, _ = sjson.SetBytes(tool, "action", action)
	tool, _ = sjson.SetBytes(tool, "model", strings.TrimSpace(toolModel))
	for _, field := range []struct {
		path  string
		value string
	}{
		{path: "quality", value: parsed.Quality},
		{path: "background", value: parsed.Background},
		{path: "output_format", value: parsed.OutputFormat},
		{path: "moderation", value: parsed.Moderation},
		{path: "style", value: parsed.Style},
	} {
		if trimmed := strings.TrimSpace(field.value); trimmed != "" {
			tool, _ = sjson.SetBytes(tool, field.path, trimmed)
		}
	}
	if parsed.OutputCompression != nil {
		tool, _ = sjson.SetBytes(tool, "output_compression", *parsed.OutputCompression)
	}
	if parsed.PartialImages != nil {
		tool, _ = sjson.SetBytes(tool, "partial_images", *parsed.PartialImages)
	}
	maskImageURL := strings.TrimSpace(parsed.MaskImageURL)
	if parsed.MaskUpload != nil {
		dataURL, err := codexImageUploadToDataURL(*parsed.MaskUpload)
		if err != nil {
			return nil, err
		}
		maskImageURL = dataURL
	}
	if maskImageURL != "" {
		tool, _ = sjson.SetBytes(tool, "input_image_mask.image_url", maskImageURL)
	}
	req, _ = sjson.SetRawBytes(req, "tools", []byte(`[]`))
	req, _ = sjson.SetRawBytes(req, "tools.-1", tool)
	return req, nil
}

func buildCodexImageResponsesInputText(parsed *codexImageRequest, prompt string) string {
	if parsed == nil {
		return prompt
	}
	size := strings.TrimSpace(parsed.Size)
	if size == "" {
		return prompt
	}
	hint := "Preferred image size: " + size + "."
	if strings.Contains(prompt, hint) || strings.Contains(prompt, "Preferred image size: ") {
		return prompt
	}
	prompt = strings.TrimRight(prompt, " \t\r\n")
	if prompt == "" {
		return hint
	}
	return prompt + "\n\n" + hint
}

func collectCodexImagesFromResponsesBody(body []byte) ([]codexResponsesImageResult, int64, error) {
	var (
		fallbackResults []codexResponsesImageResult
		fallbackSeen    = make(map[string]struct{})
		createdAt       int64
		foundFinal      bool
		streamErr       error
	)
	for _, line := range bytes.Split(body, []byte("\n")) {
		line = bytes.TrimRight(line, "\r")
		data, ok := codexExtractSSEDataLine(string(line))
		if !ok || data == "" || data == "[DONE]" {
			continue
		}
		payload := []byte(data)
		if !gjson.ValidBytes(payload) {
			continue
		}
		eventType := gjson.GetBytes(payload, "type").String()
		switch eventType {
		case "error":
			streamErr = codexResponsesFailedStatusErr(payload)
		case "response.output_item.done":
			result, itemID, ok := extractCodexImageFromResponsesOutputItemDone(payload)
			if !ok {
				continue
			}
			key := itemID + "|" + result.Result
			if _, exists := fallbackSeen[key]; exists {
				continue
			}
			fallbackSeen[key] = struct{}{}
			fallbackResults = append(fallbackResults, result)
		case "response.completed":
			results, completedAt, err := extractCodexImagesFromResponsesCompleted(payload)
			if err != nil {
				return nil, 0, err
			}
			if completedAt > 0 {
				createdAt = completedAt
			}
			if len(results) > 0 {
				return results, createdAt, nil
			}
			foundFinal = true
		case "response.failed":
			return nil, createdAt, codexResponsesFailedStatusErr(payload)
		case "response.created":
			if createdAt == 0 {
				createdAt = gjson.GetBytes(payload, "response.created_at").Int()
			}
		}
	}
	if createdAt == 0 {
		createdAt = time.Now().Unix()
	}
	if len(fallbackResults) > 0 {
		return fallbackResults, createdAt, nil
	}
	if streamErr != nil {
		return nil, createdAt, streamErr
	}
	if foundFinal {
		return nil, createdAt, nil
	}
	return nil, createdAt, fmt.Errorf("stream disconnected before response.completed")
}

func codexResponsesFailedStatusErr(payload []byte) statusErr {
	message := codexResponsesFailedField(payload, "response.error.message", "error.message", "message")
	code := codexResponsesFailedField(payload, "response.error.code", "error.code", "code")
	errType := codexResponsesFailedField(payload, "response.error.type", "error.type", "type")
	statusCode := codexResponsesFailedHTTPStatus(code, errType)
	if statusCode == http.StatusTooManyRequests && errType == "" {
		errType = "rate_limit_error"
	}
	if message == "" {
		message = "responses request failed"
	}
	if errType == "" || errType == "response.failed" || errType == "error" {
		errType = "upstream_error"
	}
	body, _ := json.Marshal(map[string]any{
		"error": map[string]any{
			"message": message,
			"type":    errType,
			"code":    code,
		},
	})
	err := statusErr{code: statusCode, msg: string(body), upstreamBody: body}
	if retryAfter := codexImageResponsesRetryAfter(message); retryAfter > 0 {
		err.retryAfter = &retryAfter
	}
	return err
}

func codexResponsesFailedField(payload []byte, paths ...string) string {
	for _, path := range paths {
		if value := strings.TrimSpace(gjson.GetBytes(payload, path).String()); value != "" {
			return value
		}
	}
	return ""
}

func codexResponsesFailedHTTPStatus(code string, errType string) int {
	lower := strings.ToLower(code + " " + errType)
	switch {
	case strings.Contains(lower, "rate_limit") || strings.Contains(lower, "quota"):
		return http.StatusTooManyRequests
	case strings.Contains(lower, "invalid_request"):
		return http.StatusBadRequest
	case strings.Contains(lower, "invalid_api_key") || strings.Contains(lower, "unauthorized") || strings.Contains(lower, "authentication"):
		return http.StatusUnauthorized
	case strings.Contains(lower, "forbidden") || strings.Contains(lower, "permission"):
		return http.StatusForbidden
	case strings.Contains(lower, "timeout"):
		return http.StatusRequestTimeout
	default:
		return http.StatusBadGateway
	}
}

func codexImageResponsesRetryDelay(err error, attempt int) (time.Duration, bool) {
	if attempt >= codexImageResponsesMaxTries {
		return 0, false
	}
	status, ok := err.(statusErr)
	if !ok || status.code != http.StatusTooManyRequests || status.retryAfter == nil {
		return 0, false
	}
	if *status.retryAfter <= 0 || *status.retryAfter > codexImageResponsesMaxRetry {
		return 0, false
	}
	return *status.retryAfter, true
}

func waitCodexImageResponsesRetry(ctx context.Context, delay time.Duration) error {
	if delay <= 0 {
		return nil
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

var codexImageRetryAfterPattern = regexp.MustCompile(`(?i)try again in\s+([0-9]+(?:\.[0-9]+)?\s*(?:ms|s|sec|secs|second|seconds|m|min|mins|minute|minutes))`)

func codexImageResponsesRetryAfter(message string) time.Duration {
	match := codexImageRetryAfterPattern.FindStringSubmatch(message)
	if len(match) < 2 {
		return 0
	}
	value := strings.ToLower(strings.ReplaceAll(strings.TrimSpace(match[1]), " ", ""))
	replacements := []struct {
		suffix string
		with   string
	}{
		{suffix: "seconds", with: "s"},
		{suffix: "second", with: "s"},
		{suffix: "secs", with: "s"},
		{suffix: "sec", with: "s"},
		{suffix: "minutes", with: "m"},
		{suffix: "minute", with: "m"},
		{suffix: "mins", with: "m"},
		{suffix: "min", with: "m"},
	}
	for _, replacement := range replacements {
		if strings.HasSuffix(value, replacement.suffix) {
			value = strings.TrimSuffix(value, replacement.suffix) + replacement.with
			break
		}
	}
	delay, err := time.ParseDuration(value)
	if err != nil {
		return 0
	}
	return delay
}

func extractCodexImagesFromResponsesCompleted(payload []byte) ([]codexResponsesImageResult, int64, error) {
	if gjson.GetBytes(payload, "type").String() != "response.completed" {
		return nil, 0, fmt.Errorf("unexpected event type")
	}
	createdAt := gjson.GetBytes(payload, "response.created_at").Int()
	if createdAt <= 0 {
		createdAt = time.Now().Unix()
	}
	results := make([]codexResponsesImageResult, 0, 1)
	output := gjson.GetBytes(payload, "response.output")
	if output.IsArray() {
		for _, item := range output.Array() {
			if item.Get("type").String() != "image_generation_call" {
				continue
			}
			result := strings.TrimSpace(item.Get("result").String())
			if result == "" {
				continue
			}
			results = append(results, codexResponsesImageResult{
				Result:        result,
				RevisedPrompt: strings.TrimSpace(item.Get("revised_prompt").String()),
				OutputFormat:  strings.TrimSpace(item.Get("output_format").String()),
				Size:          strings.TrimSpace(item.Get("size").String()),
				Background:    strings.TrimSpace(item.Get("background").String()),
				Quality:       strings.TrimSpace(item.Get("quality").String()),
			})
		}
	}
	return results, createdAt, nil
}

func extractCodexImageFromResponsesOutputItemDone(payload []byte) (codexResponsesImageResult, string, bool) {
	if gjson.GetBytes(payload, "type").String() != "response.output_item.done" {
		return codexResponsesImageResult{}, "", false
	}
	item := gjson.GetBytes(payload, "item")
	if !item.Exists() || item.Get("type").String() != "image_generation_call" {
		return codexResponsesImageResult{}, "", false
	}
	result := strings.TrimSpace(item.Get("result").String())
	if result == "" {
		return codexResponsesImageResult{}, "", false
	}
	return codexResponsesImageResult{
		Result:        result,
		RevisedPrompt: strings.TrimSpace(item.Get("revised_prompt").String()),
		OutputFormat:  strings.TrimSpace(item.Get("output_format").String()),
		Size:          strings.TrimSpace(item.Get("size").String()),
		Background:    strings.TrimSpace(item.Get("background").String()),
		Quality:       strings.TrimSpace(item.Get("quality").String()),
	}, strings.TrimSpace(item.Get("id").String()), true
}

func buildCodexImageOpenAIResponseFromResults(results []codexResponsesImageResult, createdAt int64) ([]byte, error) {
	type responseItem struct {
		B64JSON       string `json:"b64_json"`
		RevisedPrompt string `json:"revised_prompt,omitempty"`
	}
	items := make([]responseItem, 0, len(results))
	for _, result := range results {
		if strings.TrimSpace(result.Result) == "" {
			continue
		}
		items = append(items, responseItem{
			B64JSON:       strings.TrimSpace(result.Result),
			RevisedPrompt: strings.TrimSpace(result.RevisedPrompt),
		})
	}
	if createdAt <= 0 {
		createdAt = time.Now().Unix()
	}
	return json.Marshal(map[string]any{
		"created": createdAt,
		"data":    items,
	})
}
