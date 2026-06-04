package executor

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/thinking"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/util"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/vision"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v6/sdk/translator"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const openCodeGoProvider = "opencode-go"

var opencodeGoBaseURL = "https://opencode.ai/zen/go/v1"

var opencodeGoMessagesModels = map[string]struct{}{
	"minimax-m2.7": {},
	"minimax-m2.5": {},
}

var opencodeGoKnownNativeVisionModels = map[string]struct{}{
	"qwen3.5-plus":  {},
	"qwen3.6-plus":  {},
	"mimo-v2-omni":  {},
	"mimo-v2.5":     {},
	"mimo-v2.5-pro": {},
}

// OpenCodeGoExecutor routes OpenCode Go models to the provider's mixed OpenAI/Anthropic endpoints.
type OpenCodeGoExecutor struct {
	cfg *config.Config
}

func NewOpenCodeGoExecutor(cfg *config.Config) *OpenCodeGoExecutor {
	return &OpenCodeGoExecutor{cfg: cfg}
}

func (e *OpenCodeGoExecutor) Identifier() string { return openCodeGoProvider }

func (e *OpenCodeGoExecutor) PrepareRequest(req *http.Request, auth *cliproxyauth.Auth) error {
	if req == nil {
		return nil
	}
	if apiKey := strings.TrimSpace(opencodeGoAPIKey(auth)); apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	var attrs map[string]string
	if auth != nil {
		attrs = auth.Attributes
	}
	util.ApplyCustomHeadersFromAttrs(req, attrs)
	return nil
}

func (e *OpenCodeGoExecutor) HttpRequest(ctx context.Context, auth *cliproxyauth.Auth, req *http.Request) (*http.Response, error) {
	if req == nil {
		return nil, fmt.Errorf("opencode go executor: request is nil")
	}
	if ctx == nil {
		ctx = req.Context()
	}
	httpReq := req.WithContext(ctx)
	if err := e.PrepareRequest(httpReq, auth); err != nil {
		return nil, err
	}
	httpClient := newProxyAwareHTTPClient(ctx, e.cfg, auth, 0)
	return httpClient.Do(httpReq)
}

func (e *OpenCodeGoExecutor) Execute(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	// Image registry: handle historical image references and inject
	// registry notes for follow-up questions. Current-turn images are
	// left for the existing vision fallback path below.
	sessionKey, _ := vision.ResolveSessionKey(opts, auth)
	processor := vision.NewProcessor(e.newAnalyzer(auth))
	procRes, _ := processor.Process(ctx, req.Payload, sessionKey, 0)
	req.Payload = procRes.Payload

	// Vision fallback: if the current message has new images and the
	// model doesn't natively support vision, route to the configured
	// vision model for direct image analysis.
	fallback := e.applyVisionFallback(auth, req, opts)
	req = fallback.Request

	// A3: If the current turn still has images and no vision fallback was applied,
	// use the analyzer to generate text summaries. This prevents base64 image data
	// from being sent to a text-only model that cannot process it.
	if !fallback.Applied && vision.CurrentTurnHasImages(req.Payload) {
		baseModel := thinking.ParseSuffix(req.Model).ModelName
		if !opencodeGoSupportsNativeVision(baseModel) {
			analyzer := e.newAnalyzer(auth)
			if analyzer != nil {
				a3Processor := vision.NewProcessor(analyzer)
				a3Payload, a3Err := a3Processor.A3ProcessCurrentTurn(ctx, req.Payload, sessionKey, 0)
				if a3Err == nil {
					req.Payload = a3Payload
				}
			} else {
				req.Payload, _ = vision.ReplaceCurrentTurnImages(req.Payload, "[Image Registry] 无可用的图片分析模型，无法生成图片摘要。")
			}
		}
	}

	// Inject cached reasoning_content for models that need it (e.g., DeepSeek thinking mode).
	sessionID := opencodeGoSessionID(opts, auth)
	if opencodeGoNeedsReasoningInjection(req.Model) && sessionID != "" {
		req.Payload = opencodeGoInjectReasoningContentIntoPayload(req.Payload, req.Model, sessionID)
	}
	// Inject Computer Use function tools for models that need them.
	// Codex Desktop skips mcp__computer_use__ when routing through /v1/messages,
	// so DeepSeek models don't see Computer Use capabilities.
	if opencodeGoNeedsReasoningInjection(req.Model) {
		req.Payload = opencodeGoInjectComputerUseTools(req.Payload)
	}
	// Strip old base64 screenshots from tool result messages to save context.
	if opencodeGoNeedsReasoningInjection(req.Model) {
		req.Payload = opencodeGoStripScreenshots(req.Payload)
	}
	// Strip orphaned tool_calls that strict upstreams reject.
	cleaned := opencodeGoStripOrphanedToolCalls(req.Payload)
	if len(cleaned) != len(req.Payload) {
		log.Warnf("opencode: stripped orphaned tool_calls")
	}
	req.Payload = cleaned
	var resp cliproxyexecutor.Response
	var err error
	if opencodeGoUsesMessages(req.Model) {
		resp, err = e.executeMessages(ctx, auth, req, opts)
	} else {
		resp, err = e.openAIExecutor().Execute(ctx, opencodeGoAuthWithBaseURL(auth), req, opts)
	}
	if err != nil {
		return resp, err
	}

	// Capture reasoning_content from the upstream response for caching.
	if opencodeGoNeedsReasoningInjection(req.Model) && sessionID != "" {
		opencodeGoCacheReasoningFromNonStream(resp.Payload, req.Model, sessionID)
	}

	if !fallback.Applied {
		return resp, nil
	}
	resp.Payload = opencodeGoRewriteFallbackResponseModel(resp.Payload, fallback.OriginalModel)
	return resp, nil
}

func (e *OpenCodeGoExecutor) ExecuteStream(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	// Image registry: handle historical image references and inject
	// registry notes for follow-up questions. Current-turn images are
	// left for the existing vision fallback path below.
	sessionKey, _ := vision.ResolveSessionKey(opts, auth)
	processor := vision.NewProcessor(e.newAnalyzer(auth))
	procRes, _ := processor.Process(ctx, req.Payload, sessionKey, 0)
	req.Payload = procRes.Payload

	// Vision fallback: if the current message has new images and the
	// model doesn't natively support vision, route to the configured
	// vision model for direct image analysis.
	fallback := e.applyVisionFallback(auth, req, opts)
	req = fallback.Request

	// A3: If the current turn still has images and no vision fallback was applied,
	// use the analyzer to generate text summaries.
	if !fallback.Applied && vision.CurrentTurnHasImages(req.Payload) {
		baseModel := thinking.ParseSuffix(req.Model).ModelName
		if !opencodeGoSupportsNativeVision(baseModel) {
			analyzer := e.newAnalyzer(auth)
			if analyzer != nil {
				a3Processor := vision.NewProcessor(analyzer)
				a3Payload, a3Err := a3Processor.A3ProcessCurrentTurn(ctx, req.Payload, sessionKey, 0)
				if a3Err == nil {
					req.Payload = a3Payload
				}
			} else {
				req.Payload, _ = vision.ReplaceCurrentTurnImages(req.Payload, "[Image Registry] 无可用的图片分析模型，无法生成图片摘要。")
			}
		}
	}

	sessionID := opencodeGoSessionID(opts, auth)
	if opencodeGoNeedsReasoningInjection(req.Model) && sessionID != "" {
		req.Payload = opencodeGoInjectReasoningContentIntoPayload(req.Payload, req.Model, sessionID)
	}
	// Inject Computer Use function tools for models that need them.
	// Codex Desktop skips mcp__computer_use__ when routing through /v1/messages,
	// so DeepSeek models don't see Computer Use capabilities.
	if opencodeGoNeedsReasoningInjection(req.Model) {
		req.Payload = opencodeGoInjectComputerUseTools(req.Payload)
	}
	// Strip old base64 screenshots from tool result messages to save context.
	if opencodeGoNeedsReasoningInjection(req.Model) {
		req.Payload = opencodeGoStripScreenshots(req.Payload)
	}
	// Strip orphaned tool_calls that strict upstreams reject.
	cleaned := opencodeGoStripOrphanedToolCalls(req.Payload)
	if len(cleaned) != len(req.Payload) {
		log.Warnf("opencode: stripped orphaned tool_calls")
	}
	req.Payload = cleaned
	var result *cliproxyexecutor.StreamResult
	var err error
	if opencodeGoUsesMessages(req.Model) {
		result, err = e.executeMessagesStream(ctx, auth, req, opts)
	} else {
		result, err = e.openAIExecutor().ExecuteStream(ctx, opencodeGoAuthWithBaseURL(auth), req, opts)
	}
	if err != nil {
		return result, err
	}

	// Wrap stream to capture reasoning_content from streaming chunks.
	// This must happen before the fallback check so non-fallback requests also get caching.
	if opencodeGoNeedsReasoningInjection(req.Model) && sessionID != "" {
		result = opencodeGoWrapStreamCacheReasoning(result, req.Model, sessionID)
	}

	if !fallback.Applied {
		return result, nil
	}

	return opencodeGoRewriteFallbackStreamResult(result, fallback.OriginalModel), nil
}

func (e *OpenCodeGoExecutor) CountTokens(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	if opencodeGoUsesMessages(req.Model) {
		return cliproxyexecutor.Response{}, statusErr{code: http.StatusNotImplemented, msg: "token counting is not supported for OpenCode Go messages models"}
	}
	return e.openAIExecutor().CountTokens(ctx, opencodeGoAuthWithBaseURL(auth), req, opts)
}

func (e *OpenCodeGoExecutor) Refresh(ctx context.Context, auth *cliproxyauth.Auth) (*cliproxyauth.Auth, error) {
	_ = ctx
	return auth, nil
}

func (e *OpenCodeGoExecutor) openAIExecutor() *OpenAICompatExecutor {
	return NewOpenAICompatExecutor(openCodeGoProvider, e.cfg)
}

type opencodeGoVisionFallbackResult struct {
	Request       cliproxyexecutor.Request
	OriginalModel string
	FallbackModel string
	Applied       bool
}

func (e *OpenCodeGoExecutor) applyVisionFallback(auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) opencodeGoVisionFallbackResult {
	result := opencodeGoVisionFallbackResult{
		Request:       req,
		OriginalModel: payloadRequestedModel(opts, req.Model),
	}
	fallback := opencodeGoVisionFallbackModel(e.cfg, auth)
	if fallback == "" || !opencodeGoCurrentRequestHasImage(req.Payload) {
		return result
	}
	if !opencodeGoSupportsNativeVision(fallback) {
		return result
	}
	baseModel := thinking.ParseSuffix(req.Model).ModelName
	if strings.EqualFold(baseModel, fallback) || opencodeGoSupportsNativeVision(baseModel) {
		return result
	}
	req.Model = fallback
	req.Payload, _ = sjson.SetBytes(req.Payload, "model", fallback)
	if opencodeGoDisablesThinkingForVisionFallback(fallback) {
		req.Payload, _ = sjson.SetBytes(req.Payload, "enable_thinking", false)
	}
	result.Request = req
	result.FallbackModel = fallback
	result.Applied = true
	return result
}

func opencodeGoVisionFallbackModel(cfg *config.Config, auth *cliproxyauth.Auth) string {
	if auth == nil {
		return ""
	}
	if auth.Attributes != nil {
		if fallback := strings.TrimSpace(auth.Attributes["vision_fallback_model"]); fallback != "" {
			if opencodeGoModelExcluded(fallback, auth.Attributes["excluded_models"]) {
				return ""
			}
			return fallback
		}
	}
	if cfg == nil {
		return ""
	}
	apiKey := ""
	if auth.Attributes != nil {
		apiKey = strings.TrimSpace(auth.Attributes["api_key"])
	}
	if apiKey == "" {
		return ""
	}
	for i := range cfg.OpenCodeGoKey {
		entry := &cfg.OpenCodeGoKey[i]
		if strings.EqualFold(strings.TrimSpace(entry.APIKey), apiKey) {
			fallback := strings.TrimSpace(entry.VisionFallbackModel)
			if opencodeGoModelExcluded(fallback, strings.Join(entry.ExcludedModels, ",")) {
				return ""
			}
			return fallback
		}
	}
	return ""
}

func opencodeGoModelExcluded(model, excluded string) bool {
	model = strings.ToLower(strings.TrimSpace(thinking.ParseSuffix(model).ModelName))
	if model == "" {
		return true
	}
	for _, entry := range strings.Split(excluded, ",") {
		entry = strings.ToLower(strings.TrimSpace(entry))
		if entry == "" {
			continue
		}
		if entry == "*" || entry == model {
			return true
		}
	}
	return false
}

func opencodeGoPayloadHasImage(payload []byte) bool {
	if len(payload) == 0 || !json.Valid(payload) {
		return false
	}
	var value any
	if err := json.Unmarshal(payload, &value); err != nil {
		return false
	}
	return opencodeGoValueHasImage(value)
}

func opencodeGoCurrentRequestHasImage(payload []byte) bool {
	if len(payload) == 0 || !json.Valid(payload) {
		return false
	}
	var value any
	if err := json.Unmarshal(payload, &value); err != nil {
		return false
	}
	if hasImage, recognized := opencodeGoCurrentValueHasImage(value); recognized {
		return hasImage
	}
	return opencodeGoValueHasImage(value)
}

func opencodeGoSanitizeHistoricalImages(payload []byte) ([]byte, bool) {
	if len(payload) == 0 || !json.Valid(payload) {
		return payload, false
	}
	var root map[string]any
	if err := json.Unmarshal(payload, &root); err != nil {
		return payload, false
	}
	changed := false
	if messages, ok := root["messages"].([]any); ok {
		if opencodeGoSanitizeMessageArray(messages) {
			root["messages"] = messages
			changed = true
		}
	}
	if input, ok := root["input"]; ok {
		if sanitized, ok := opencodeGoSanitizeResponsesInput(input); ok {
			root["input"] = sanitized
			changed = true
		}
	}
	if !changed {
		return payload, false
	}
	out, err := json.Marshal(root)
	if err != nil {
		return payload, false
	}
	return out, true
}

func opencodeGoSanitizeMessageArray(messages []any) bool {
	changed := false
	for _, raw := range messages {
		message, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		content, ok := message["content"]
		if !ok {
			continue
		}
		if sanitized, ok := opencodeGoSanitizeContentParts(content, "text"); ok {
			message["content"] = sanitized
			changed = true
		}
	}
	return changed
}

func opencodeGoSanitizeResponsesInput(input any) (any, bool) {
	switch typed := input.(type) {
	case []any:
		changed := false
		for i, raw := range typed {
			item, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			if opencodeGoContentPartIsImage(item) {
				typed[i] = opencodeGoImagePlaceholderPart("input_text")
				changed = true
				continue
			}
			content, ok := item["content"]
			if !ok {
				continue
			}
			if sanitized, ok := opencodeGoSanitizeContentParts(content, "input_text"); ok {
				item["content"] = sanitized
				changed = true
			}
		}
		return typed, changed
	case map[string]any:
		if opencodeGoContentPartIsImage(typed) {
			return opencodeGoImagePlaceholderPart("input_text"), true
		}
		content, ok := typed["content"]
		if !ok {
			return input, false
		}
		if sanitized, ok := opencodeGoSanitizeContentParts(content, "input_text"); ok {
			typed["content"] = sanitized
			return typed, true
		}
	}
	return input, false
}

func opencodeGoSanitizeContentParts(content any, placeholderType string) (any, bool) {
	parts, ok := content.([]any)
	if !ok {
		return content, false
	}
	changed := false
	out := make([]any, 0, len(parts))
	for _, part := range parts {
		partMap, ok := part.(map[string]any)
		if !ok || !opencodeGoContentPartIsImage(partMap) {
			out = append(out, part)
			continue
		}
		if !opencodeGoContentPartsHaveText(out) {
			out = append(out, opencodeGoImagePlaceholderPart(placeholderType))
		}
		changed = true
	}
	if !changed {
		return content, false
	}
	if len(out) == 0 {
		out = append(out, opencodeGoImagePlaceholderPart(placeholderType))
	}
	return out, true
}

func opencodeGoContentPartIsImage(part map[string]any) bool {
	rawType, _ := part["type"].(string)
	switch strings.ToLower(strings.TrimSpace(rawType)) {
	case "image", "image_url", "input_image":
		return true
	}
	_, ok := part["image_url"]
	return ok
}

func opencodeGoContentPartsHaveText(parts []any) bool {
	for _, part := range parts {
		partMap, ok := part.(map[string]any)
		if !ok {
			continue
		}
		rawType, _ := partMap["type"].(string)
		switch strings.ToLower(strings.TrimSpace(rawType)) {
		case "text", "input_text", "output_text":
			if text, _ := partMap["text"].(string); strings.TrimSpace(text) != "" {
				return true
			}
		}
	}
	return false
}

func opencodeGoImagePlaceholderPart(partType string) map[string]any {
	partType = strings.TrimSpace(partType)
	if partType == "" {
		partType = "text"
	}
	return map[string]any{
		"type": partType,
		"text": "[Image omitted from previous turn.]",
	}
}

func opencodeGoCurrentValueHasImage(value any) (bool, bool) {
	root, ok := value.(map[string]any)
	if !ok {
		return false, false
	}
	if messages, ok := root["messages"].([]any); ok {
		return opencodeGoLatestUserMessageHasImage(messages)
	}
	if input, ok := root["input"]; ok {
		return opencodeGoInputHasImage(input)
	}
	return false, false
}

func opencodeGoLatestUserMessageHasImage(messages []any) (bool, bool) {
	// Only check the VERY LAST message. Scanning backwards for any user
	// message causes the vision fallback to trigger on follow-up tool-call
	// requests where the most recent user message is an old image message.
	if len(messages) == 0 {
		return false, false
	}
	message, ok := messages[len(messages)-1].(map[string]any)
	if !ok {
		return false, false
	}
	role, _ := message["role"].(string)
	if !strings.EqualFold(strings.TrimSpace(role), "user") {
		return false, false
	}
	if content, ok := message["content"]; ok {
		return opencodeGoValueHasImage(content), true
	}
	return opencodeGoValueHasImage(message), true
}

func opencodeGoInputHasImage(input any) (bool, bool) {
	switch typed := input.(type) {
	case string:
		return false, true
	case map[string]any:
		return opencodeGoValueHasImage(typed), true
	case []any:
		if hasImage, recognized := opencodeGoLatestUserInputItemHasImage(typed); recognized {
			return hasImage, true
		}
		return opencodeGoValueHasImage(typed), true
	default:
		return false, false
	}
}

func opencodeGoLatestUserInputItemHasImage(items []any) (bool, bool) {
	sawRole := false
	for i := len(items) - 1; i >= 0; i-- {
		item, ok := items[i].(map[string]any)
		if !ok {
			continue
		}
		role, ok := item["role"].(string)
		if !ok {
			continue
		}
		sawRole = true
		if !strings.EqualFold(strings.TrimSpace(role), "user") {
			continue
		}
		if content, ok := item["content"]; ok {
			return opencodeGoValueHasImage(content), true
		}
		return opencodeGoValueHasImage(item), true
	}
	if sawRole {
		return false, true
	}
	return false, false
}

func opencodeGoValueHasImage(value any) bool {
	switch typed := value.(type) {
	case map[string]any:
		if rawType, ok := typed["type"].(string); ok {
			switch strings.ToLower(strings.TrimSpace(rawType)) {
			case "image", "image_url", "input_image":
				return true
			}
		}
		if _, ok := typed["image_url"]; ok {
			return true
		}
		for _, child := range typed {
			if opencodeGoValueHasImage(child) {
				return true
			}
		}
	case []any:
		for _, child := range typed {
			if opencodeGoValueHasImage(child) {
				return true
			}
		}
	}
	return false
}

func opencodeGoSupportsNativeVision(model string) bool {
	baseModel := strings.ToLower(strings.TrimSpace(thinking.ParseSuffix(model).ModelName))
	if baseModel == "" {
		return false
	}
	if _, ok := opencodeGoKnownNativeVisionModels[baseModel]; ok {
		return true
	}
	return opencodeGoModelNameImpliesVision(baseModel)
}

func opencodeGoModelNameImpliesVision(model string) bool {
	if strings.Contains(model, "vision") ||
		strings.Contains(model, "multimodal") ||
		strings.Contains(model, "omni") {
		return true
	}
	for _, token := range strings.FieldsFunc(model, func(r rune) bool {
		return r == '-' || r == '_' || r == '.' || r == '/' || r == ':'
	}) {
		if token == "vl" {
			return true
		}
	}
	return false
}

func opencodeGoDisablesThinkingForVisionFallback(model string) bool {
	baseModel := strings.ToLower(strings.TrimSpace(thinking.ParseSuffix(model).ModelName))
	return strings.HasPrefix(baseModel, "qwen")
}

var opencodeGoFallbackModelFieldPaths = []string{
	"model",
	"modelVersion",
	"message.model",
	"response.model",
	"response.modelVersion",
}

func opencodeGoRewriteFallbackResponseModel(data []byte, originalModel string) []byte {
	originalModel = strings.TrimSpace(originalModel)
	if originalModel == "" || len(data) == 0 || !gjson.ValidBytes(bytes.TrimSpace(data)) {
		return data
	}
	out := data
	for _, path := range opencodeGoFallbackModelFieldPaths {
		if !gjson.GetBytes(out, path).Exists() {
			continue
		}
		updated, err := sjson.SetBytes(out, path, originalModel)
		if err == nil {
			out = updated
		}
	}
	return out
}

func opencodeGoRewriteFallbackStreamResult(result *cliproxyexecutor.StreamResult, originalModel string) *cliproxyexecutor.StreamResult {
	if result == nil || strings.TrimSpace(originalModel) == "" {
		return result
	}
	out := make(chan cliproxyexecutor.StreamChunk)
	go func() {
		defer close(out)
		for chunk := range result.Chunks {
			if chunk.Err == nil && len(chunk.Payload) > 0 {
				chunk.Payload = opencodeGoRewriteFallbackStreamPayload(chunk.Payload, originalModel)
			}
			out <- chunk
		}
	}()
	return &cliproxyexecutor.StreamResult{Headers: result.Headers, Chunks: out}
}

func opencodeGoRewriteFallbackStreamPayload(payload []byte, originalModel string) []byte {
	trimmed := bytes.TrimSpace(payload)
	if len(trimmed) == 0 {
		return payload
	}
	if gjson.ValidBytes(trimmed) {
		return opencodeGoRewriteFallbackResponseModel(payload, originalModel)
	}
	lines := bytes.Split(payload, []byte("\n"))
	modified := false
	for i, line := range lines {
		dataIdx := bytes.Index(line, []byte("data:"))
		if dataIdx < 0 {
			continue
		}
		raw := bytes.TrimSpace(line[dataIdx+len("data:"):])
		if len(raw) == 0 || bytes.Equal(raw, []byte("[DONE]")) || !gjson.ValidBytes(raw) {
			continue
		}
		rewritten := opencodeGoRewriteFallbackResponseModel(raw, originalModel)
		if bytes.Equal(rewritten, raw) {
			continue
		}
		rebuilt := make([]byte, 0, len(line)-len(raw)+len(rewritten)+1)
		rebuilt = append(rebuilt, line[:dataIdx]...)
		rebuilt = append(rebuilt, []byte("data: ")...)
		rebuilt = append(rebuilt, rewritten...)
		lines[i] = rebuilt
		modified = true
	}
	if !modified {
		return payload
	}
	return bytes.Join(lines, []byte("\n"))
}

func (e *OpenCodeGoExecutor) executeMessages(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (resp cliproxyexecutor.Response, err error) {
	if opts.Alt == "responses/compact" {
		return resp, statusErr{code: http.StatusNotImplemented, msg: "/responses/compact not supported"}
	}
	baseModel := thinking.ParseSuffix(req.Model).ModelName
	apiKey := opencodeGoAPIKey(auth)
	if strings.TrimSpace(apiKey) == "" {
		return resp, statusErr{code: http.StatusUnauthorized, msg: "missing OpenCode Go API key"}
	}

	reporter := newUsageReporter(ctx, e.Identifier(), baseModel, auth)
	defer reporter.trackFailure(ctx, &err)

	from := opts.SourceFormat
	to := sdktranslator.FormatClaude
	originalPayloadSource := req.Payload
	if len(opts.OriginalRequest) > 0 {
		originalPayloadSource = opts.OriginalRequest
	}
	originalTranslated := sdktranslator.TranslateRequest(from, to, baseModel, originalPayloadSource, from != to)
	body := sdktranslator.TranslateRequest(from, to, baseModel, req.Payload, from != to)
	body, _ = sjson.SetBytes(body, "model", baseModel)

	body, err = thinking.ApplyThinking(body, req.Model, from.String(), to.String(), e.Identifier())
	if err != nil {
		return resp, err
	}
	requestedModel := payloadRequestedModel(opts, req.Model)
	body = applyPayloadConfigWithRoot(e.cfg, baseModel, to.String(), "", body, originalTranslated, requestedModel)

	url := strings.TrimSuffix(opencodeGoBaseURL, "/") + "/messages"
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return resp, err
	}
	e.applyMessagesHeaders(httpReq, auth, apiKey, false)
	recordAPIRequest(ctx, e.cfg, e.requestLog(url, httpReq, body, auth))

	httpClient := newProxyAwareHTTPClient(ctx, e.cfg, auth, 0)
	httpResp, err := httpClient.Do(httpReq)
	if err != nil {
		recordAPIResponseError(ctx, e.cfg, err)
		reporter.publishFailureWithContent(ctx, string(req.Payload), err.Error())
		return resp, err
	}
	defer func() {
		if errClose := httpResp.Body.Close(); errClose != nil {
			log.Errorf("opencode go executor: close response body error: %v", errClose)
		}
	}()
	recordAPIResponseMetadata(ctx, e.cfg, httpResp.StatusCode, httpResp.Header.Clone())
	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		b := readUpstreamErrorBody(e.Identifier(), httpResp.Body)
		appendAPIResponseChunk(ctx, e.cfg, b)
		reporter.publishFailureWithContent(ctx, string(req.Payload), string(b))
		return resp, statusErr{code: httpResp.StatusCode, msg: string(b)}
	}
	data, err := readUpstreamResponseBody(e.Identifier(), httpResp.Body)
	if err != nil {
		recordAPIResponseError(ctx, e.cfg, err)
		return resp, err
	}
	appendAPIResponseChunk(ctx, e.cfg, data)
	reporter.publishWithContent(ctx, parseClaudeUsage(data), string(req.Payload), string(data))
	reporter.ensurePublished(ctx)

	bodyForTranslation := data
	if from != to {
		bodyForTranslation = opencodeGoClaudeMessageToSSE(data)
	}
	var param any
	out := sdktranslator.TranslateNonStream(ctx, to, from, req.Model, opts.OriginalRequest, body, bodyForTranslation, &param)
	return cliproxyexecutor.Response{Payload: []byte(out), Headers: httpResp.Header.Clone()}, nil
}

func (e *OpenCodeGoExecutor) executeMessagesStream(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (_ *cliproxyexecutor.StreamResult, err error) {
	if opts.Alt == "responses/compact" {
		return nil, statusErr{code: http.StatusNotImplemented, msg: "/responses/compact not supported"}
	}
	baseModel := thinking.ParseSuffix(req.Model).ModelName
	apiKey := opencodeGoAPIKey(auth)
	if strings.TrimSpace(apiKey) == "" {
		return nil, statusErr{code: http.StatusUnauthorized, msg: "missing OpenCode Go API key"}
	}

	reporter := newUsageReporter(ctx, e.Identifier(), baseModel, auth)
	defer reporter.trackFailure(ctx, &err)

	from := opts.SourceFormat
	to := sdktranslator.FormatClaude
	originalPayloadSource := req.Payload
	if len(opts.OriginalRequest) > 0 {
		originalPayloadSource = opts.OriginalRequest
	}
	originalTranslated := sdktranslator.TranslateRequest(from, to, baseModel, originalPayloadSource, true)
	body := sdktranslator.TranslateRequest(from, to, baseModel, req.Payload, true)
	body, _ = sjson.SetBytes(body, "model", baseModel)

	body, err = thinking.ApplyThinking(body, req.Model, from.String(), to.String(), e.Identifier())
	if err != nil {
		return nil, err
	}
	requestedModel := payloadRequestedModel(opts, req.Model)
	body = applyPayloadConfigWithRoot(e.cfg, baseModel, to.String(), "", body, originalTranslated, requestedModel)

	url := strings.TrimSuffix(opencodeGoBaseURL, "/") + "/messages"
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	e.applyMessagesHeaders(httpReq, auth, apiKey, true)
	recordAPIRequest(ctx, e.cfg, e.requestLog(url, httpReq, body, auth))

	httpClient := newProxyAwareHTTPClient(ctx, e.cfg, auth, 0)
	httpResp, err := httpClient.Do(httpReq)
	if err != nil {
		recordAPIResponseError(ctx, e.cfg, err)
		reporter.publishFailureWithContent(ctx, string(req.Payload), err.Error())
		return nil, err
	}
	recordAPIResponseMetadata(ctx, e.cfg, httpResp.StatusCode, httpResp.Header.Clone())
	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		b := readUpstreamErrorBody(e.Identifier(), httpResp.Body)
		appendAPIResponseChunk(ctx, e.cfg, b)
		reporter.publishFailureWithContent(ctx, string(req.Payload), string(b))
		if errClose := httpResp.Body.Close(); errClose != nil {
			log.Errorf("opencode go executor: close response body error: %v", errClose)
		}
		return nil, statusErr{code: httpResp.StatusCode, msg: string(b)}
	}

	out := make(chan cliproxyexecutor.StreamChunk)
	reporter.setInputContent(string(req.Payload))
	go func() {
		defer close(out)
		defer func() {
			if errClose := httpResp.Body.Close(); errClose != nil {
				log.Errorf("opencode go executor: close response body error: %v", errClose)
			}
		}()
		scanner := bufio.NewScanner(httpResp.Body)
		scanner.Buffer(nil, 52_428_800)
		var param any
		for scanner.Scan() {
			line := scanner.Bytes()
			appendAPIResponseChunk(ctx, e.cfg, line)
			reporter.appendOutputChunk(line)
			if detail, ok := parseClaudeStreamUsage(line); ok {
				reporter.publish(ctx, detail)
			}
			if from == to {
				cloned := append(bytes.Clone(line), '\n')
				out <- cliproxyexecutor.StreamChunk{Payload: cloned}
				continue
			}
			chunks := sdktranslator.TranslateStream(ctx, to, from, req.Model, opts.OriginalRequest, body, bytes.Clone(line), &param)
			for i := range chunks {
				out <- cliproxyexecutor.StreamChunk{Payload: []byte(chunks[i])}
			}
		}
		if errScan := scanner.Err(); errScan != nil {
			recordAPIResponseError(ctx, e.cfg, errScan)
			reporter.publishFailure(ctx)
			out <- cliproxyexecutor.StreamChunk{Err: errScan}
		}
		reporter.ensurePublished(ctx)
	}()
	return &cliproxyexecutor.StreamResult{Headers: httpResp.Header.Clone(), Chunks: out}, nil
}

func (e *OpenCodeGoExecutor) applyMessagesHeaders(req *http.Request, auth *cliproxyauth.Auth, apiKey string, stream bool) {
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Anthropic-Version", "2023-06-01")
	req.Header.Set("User-Agent", "cli-proxy-opencode-go")
	if stream {
		req.Header.Set("Accept", "text/event-stream")
		req.Header.Set("Cache-Control", "no-cache")
	} else {
		req.Header.Set("Accept", "application/json")
	}
	var attrs map[string]string
	if auth != nil {
		attrs = auth.Attributes
	}
	util.ApplyCustomHeadersFromAttrs(req, attrs)
}

func (e *OpenCodeGoExecutor) requestLog(url string, req *http.Request, body []byte, auth *cliproxyauth.Auth) upstreamRequestLog {
	var authID, authLabel, authType, authValue string
	if auth != nil {
		authID = auth.ID
		authLabel = auth.Label
		authType, authValue = auth.AccountInfo()
	}
	return upstreamRequestLog{
		URL:       url,
		Method:    http.MethodPost,
		Headers:   req.Header.Clone(),
		Body:      body,
		Provider:  e.Identifier(),
		AuthID:    authID,
		AuthLabel: authLabel,
		AuthType:  authType,
		AuthValue: authValue,
	}
}

// opencodeGoStripOrphanedToolCalls removes tool_calls from assistant messages
// that are not followed by tool messages responding to them. Strict upstream
// providers (e.g., DeepSeek) reject requests with unresolved tool_calls.

func (e *OpenCodeGoExecutor) newAnalyzer(auth *cliproxyauth.Auth) *vision.OpenCodeGoAnalyzer {
	apiKey := opencodeGoAPIKey(auth)
	model, ok := opencodeGoResolveAnalyzerModel(e.cfg, auth)
	if !ok || model == "" {
		return nil
	}
	return vision.NewOpenCodeGoAnalyzer(opencodeGoBaseURL, apiKey, model)
}

// opencodeGoResolveAnalyzerModel determines the model to use for the image
// analyzer, respecting excluded_models from both auth attributes and config.
// Returns (model, false) when no usable analyzer model is available.
func opencodeGoResolveAnalyzerModel(cfg *config.Config, auth *cliproxyauth.Auth) (string, bool) {
	model := opencodeGoVisionFallbackModel(cfg, auth)
	if model != "" {
		return model, true
	}
	// Check why the fallback model is empty:
	if opencodeGoFallbackIsConfigured(cfg, auth) {
		// Fallback was configured but excluded -- don't default to anything
		return "", false
	}
	// No fallback configured -- use default if not excluded
	defaultModel := "qwen3.5-plus"
	if opencodeGoModelIsExcluded(defaultModel, cfg, auth) {
		return "", false
	}
	return defaultModel, true
}

// opencodeGoFallbackIsConfigured returns true when a fallback model name
// exists in config or auth attributes (regardless of exclusion).
func opencodeGoFallbackIsConfigured(cfg *config.Config, auth *cliproxyauth.Auth) bool {
	if auth == nil {
		return false
	}
	if auth.Attributes != nil {
		if strings.TrimSpace(auth.Attributes["vision_fallback_model"]) != "" {
			return true
		}
	}
	if cfg == nil {
		return false
	}
	apiKey := opencodeGoAPIKey(auth)
	if apiKey == "" {
		return false
	}
	for i := range cfg.OpenCodeGoKey {
		if strings.EqualFold(strings.TrimSpace(cfg.OpenCodeGoKey[i].APIKey), apiKey) {
			return strings.TrimSpace(cfg.OpenCodeGoKey[i].VisionFallbackModel) != ""
		}
	}
	return false
}

// opencodeGoModelIsExcluded checks if a model is excluded by either auth
// attribute excluded_models or config ExcludedModels for this API key.
func opencodeGoModelIsExcluded(model string, cfg *config.Config, auth *cliproxyauth.Auth) bool {
	if auth != nil && auth.Attributes != nil {
		if opencodeGoModelExcluded(model, auth.Attributes["excluded_models"]) {
			return true
		}
	}
	apiKey := opencodeGoAPIKey(auth)
	if apiKey == "" || cfg == nil {
		return false
	}
	for i := range cfg.OpenCodeGoKey {
		if strings.EqualFold(strings.TrimSpace(cfg.OpenCodeGoKey[i].APIKey), apiKey) {
			excluded := strings.Join(cfg.OpenCodeGoKey[i].ExcludedModels, ",")
			return opencodeGoModelExcluded(model, excluded)
		}
	}
	return false
}

func opencodeGoStripOrphanedToolCalls(payload []byte) []byte {
	if !gjson.ValidBytes(payload) {
		return payload
	}

	// Determine format: "messages" (Chat Completions) or "input" (Responses API)
	arrayName := "messages"
	msgs := gjson.GetBytes(payload, "messages")
	if !msgs.Exists() || !msgs.IsArray() || len(msgs.Array()) == 0 {
		msgs = gjson.GetBytes(payload, "input")
		if !msgs.Exists() || !msgs.IsArray() || len(msgs.Array()) == 0 {
			return payload
		}
		arrayName = "input"
	}

	items := msgs.Array()
	needsStrip := make([]int, 0)

	for i, item := range items {
		if item.Get("role").String() != "assistant" {
			continue
		}
		tc := item.Get("tool_calls")
		if !tc.Exists() || !tc.IsArray() {
			continue
		}

		// Collect tool_call_ids resolved by tool messages after this message
		resolved := make(map[string]bool)
		for _, later := range items[i+1:] {
			if later.Get("role").String() == "tool" {
				if id := later.Get("tool_call_id").String(); id != "" {
					resolved[id] = true
				}
			}
		}

		allResolved := true
		for _, call := range tc.Array() {
			if id := call.Get("id").String(); id != "" {
				if !resolved[id] {
					allResolved = false
					break
				}
			}
		}
		if !allResolved {
			needsStrip = append(needsStrip, i)
		}
	}

	if len(needsStrip) == 0 {
		return payload
	}

	for i := len(needsStrip) - 1; i >= 0; i-- {
		path := fmt.Sprintf("%s.%d.tool_calls", arrayName, needsStrip[i])
		payload, _ = sjson.DeleteBytes(payload, path)
	}
	return payload
}

func opencodeGoUsesMessages(model string) bool {
	base := strings.ToLower(strings.TrimSpace(thinking.ParseSuffix(model).ModelName))
	_, ok := opencodeGoMessagesModels[base]
	return ok
}

func opencodeGoAPIKey(auth *cliproxyauth.Auth) string {
	if auth == nil || auth.Attributes == nil {
		return ""
	}
	return strings.TrimSpace(auth.Attributes["api_key"])
}

func opencodeGoAuthWithBaseURL(auth *cliproxyauth.Auth) *cliproxyauth.Auth {
	if auth == nil {
		return &cliproxyauth.Auth{Attributes: map[string]string{"base_url": opencodeGoBaseURL}}
	}
	clone := *auth
	attrs := make(map[string]string, len(auth.Attributes)+1)
	for k, v := range auth.Attributes {
		attrs[k] = v
	}
	attrs["base_url"] = strings.TrimSuffix(opencodeGoBaseURL, "/")
	clone.Attributes = attrs
	return &clone
}

// opencodeGoFixToolCallArguments repairs tool_calls where function.arguments contains
// concatenated JSON objects by splitting them into separate tool_call entries.
// This handles a Codex Desktop client bug where multiple shell_command calls get
// merged into a single tool_call's arguments field (e.g., {"c":"ls"}{"c":"cat pkg.json"}).
func opencodeGoFixToolCallArguments(payload []byte) []byte {
	if len(payload) == 0 || !json.Valid(payload) {
		return payload
	}
	var root map[string]any
	if err := json.Unmarshal(payload, &root); err != nil {
		return payload
	}
	messages, ok := root["messages"].([]any)
	if !ok {
		return payload
	}
	changed := false
	for _, rawMsg := range messages {
		msg, ok := rawMsg.(map[string]any)
		if !ok {
			continue
		}
		role, _ := msg["role"].(string)
		if !strings.EqualFold(role, "assistant") {
			continue
		}
		rawCalls, ok := msg["tool_calls"]
		if !ok {
			continue
		}
		calls, ok := rawCalls.([]any)
		if !ok {
			continue
		}
		fixed := make([]any, 0, len(calls))
		for _, rawCall := range calls {
			call, ok := rawCall.(map[string]any)
			if !ok {
				fixed = append(fixed, rawCall)
				continue
			}
			fn, ok := call["function"].(map[string]any)
			if !ok {
				fixed = append(fixed, rawCall)
				continue
			}
			args, ok := fn["arguments"].(string)
			if !ok {
				fixed = append(fixed, rawCall)
				continue
			}
			if json.Valid([]byte(args)) {
				fixed = append(fixed, rawCall)
				continue
			}
			parts := splitConcatenatedJSONObjects(args)
			if len(parts) <= 1 {
				fixed = append(fixed, rawCall)
				continue
			}
			changed = true
			// First split part replaces the original tool_call's arguments.
			fn["arguments"] = parts[0]
			fixed = append(fixed, call)
			baseID, _ := call["id"].(string)
			for i := 1; i < len(parts); i++ {
				newCall := cloneToolCallMap(call, baseID, i)
				if newFn, ok := newCall["function"].(map[string]any); ok {
					newFn["arguments"] = parts[i]
				}
				fixed = append(fixed, newCall)
			}
		}
		if changed {
			msg["tool_calls"] = fixed
		}
	}
	if !changed {
		return payload
	}
	out, err := json.Marshal(root)
	if err != nil {
		return payload
	}
	return out
}

// splitConcatenatedJSONObjects splits a string containing concatenated JSON objects
// into individual JSON strings. Uses json.Decoder to read one object at a time
// regardless of whitespace, nesting, or string content.
func splitConcatenatedJSONObjects(input string) []string {
	decoder := json.NewDecoder(strings.NewReader(input))
	var parts []string
	for {
		var raw json.RawMessage
		if err := decoder.Decode(&raw); err != nil {
			break
		}
		parts = append(parts, string(raw))
	}
	return parts
}

// cloneToolCallMap creates a shallow copy of a tool_call map with a new ID
// and a deep clone of its function sub-map.
func cloneToolCallMap(original map[string]any, baseID string, splitIdx int) map[string]any {
	clone := make(map[string]any, len(original))
	for k, v := range original {
		clone[k] = v
	}
	if baseID != "" {
		clone["id"] = fmt.Sprintf("%s_split_%d", baseID, splitIdx)
	}
	// Deep clone the function map so each split tool_call has independent state.
	if fn, ok := original["function"].(map[string]any); ok {
		fnClone := make(map[string]any, len(fn))
		for k, v := range fn {
			fnClone[k] = v
		}
		clone["function"] = fnClone
	}
	return clone
}

func opencodeGoClaudeMessageToSSE(data []byte) []byte {
	root := gjson.ParseBytes(data)
	if !root.Exists() {
		return data
	}
	var b strings.Builder
	writeData := func(raw string) {
		if strings.TrimSpace(raw) == "" {
			return
		}
		b.WriteString("data: ")
		b.WriteString(raw)
		b.WriteString("\n\n")
	}

	messageStart := `{"type":"message_start","message":{"id":"","type":"message","role":"assistant","model":"","content":[],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":0,"output_tokens":0}}}`
	messageStart, _ = sjson.Set(messageStart, "message.id", root.Get("id").String())
	messageStart, _ = sjson.Set(messageStart, "message.model", root.Get("model").String())
	if v := root.Get("usage.input_tokens"); v.Exists() {
		messageStart, _ = sjson.Set(messageStart, "message.usage.input_tokens", v.Int())
	}
	writeData(messageStart)

	index := 0
	if content := root.Get("content"); content.Exists() && content.IsArray() {
		for _, block := range content.Array() {
			blockType := block.Get("type").String()
			switch blockType {
			case "text":
				start := `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`
				start, _ = sjson.Set(start, "index", index)
				writeData(start)
				delta := `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":""}}`
				delta, _ = sjson.Set(delta, "index", index)
				delta, _ = sjson.Set(delta, "delta.text", block.Get("text").String())
				writeData(delta)
			case "thinking":
				start := `{"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}`
				start, _ = sjson.Set(start, "index", index)
				writeData(start)
				delta := `{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":""}}`
				delta, _ = sjson.Set(delta, "index", index)
				delta, _ = sjson.Set(delta, "delta.thinking", block.Get("thinking").String())
				writeData(delta)
			case "tool_use":
				start := `{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"","name":"","input":{}}}`
				start, _ = sjson.Set(start, "index", index)
				start, _ = sjson.Set(start, "content_block.id", block.Get("id").String())
				start, _ = sjson.Set(start, "content_block.name", block.Get("name").String())
				if input := block.Get("input"); input.Exists() {
					start, _ = sjson.SetRaw(start, "content_block.input", input.Raw)
				}
				writeData(start)
			default:
				index++
				continue
			}
			stop := `{"type":"content_block_stop","index":0}`
			stop, _ = sjson.Set(stop, "index", index)
			writeData(stop)
			index++
		}
	}

	messageDelta := `{"type":"message_delta","delta":{"stop_reason":null,"stop_sequence":null},"usage":{"output_tokens":0}}`
	if v := root.Get("stop_reason"); v.Exists() {
		messageDelta, _ = sjson.Set(messageDelta, "delta.stop_reason", v.String())
	}
	if v := root.Get("stop_sequence"); v.Exists() && v.Type != gjson.Null {
		messageDelta, _ = sjson.Set(messageDelta, "delta.stop_sequence", v.String())
	}
	if v := root.Get("usage.output_tokens"); v.Exists() {
		messageDelta, _ = sjson.Set(messageDelta, "usage.output_tokens", v.Int())
	}
	writeData(messageDelta)
	writeData(`{"type":"message_stop"}`)
	return []byte(b.String())
}
