package executor

import (
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
	originalRequestedModel := payloadRequestedModel(opts, req.Model)
	originalUpstreamModel := thinking.ParseSuffix(req.Model).ModelName
	fallback := e.applyVisionFallback(auth, req, opts)
	if fallback.Applied {
		ctx = contextWithVisionFallbackLog(ctx, originalRequestedModel, originalUpstreamModel, fallback.FallbackModel)
	}
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
	// Expand Codex MCP namespace tools into concrete function tools for
	// OpenAI-compatible upstreams that cannot consume Codex namespaces directly.
	req.Payload = opencodeGoInjectCodexToolBridgeTools(req.Payload)
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
	originalRequestedModel := payloadRequestedModel(opts, req.Model)
	originalUpstreamModel := thinking.ParseSuffix(req.Model).ModelName
	fallback := e.applyVisionFallback(auth, req, opts)
	if fallback.Applied {
		ctx = contextWithVisionFallbackLog(ctx, originalRequestedModel, originalUpstreamModel, fallback.FallbackModel)
	}
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
	// Expand Codex MCP namespace tools into concrete function tools for
	// OpenAI-compatible upstreams that cannot consume Codex namespaces directly.
	req.Payload = opencodeGoInjectCodexToolBridgeTools(req.Payload)
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
	return applyVisionFallback(req, opts, opencodeGoVisionFallbackModel(e.cfg, auth))
}

func applyVisionFallback(req cliproxyexecutor.Request, opts cliproxyexecutor.Options, fallback string) opencodeGoVisionFallbackResult {
	result := opencodeGoVisionFallbackResult{
		Request:       req,
		OriginalModel: payloadRequestedModel(opts, req.Model),
	}
	fallback = strings.TrimSpace(fallback)
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

func clineVisionFallbackModel(cfg *config.Config, auth *cliproxyauth.Auth) string {
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
	for i := range cfg.ClineKey {
		entry := &cfg.ClineKey[i]
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
	return vision.CurrentTurnHasImages(payload)
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
	for _, candidate := range opencodeGoVisionModelCandidates(baseModel) {
		if _, ok := opencodeGoKnownNativeVisionModels[candidate]; ok {
			return true
		}
		if opencodeGoModelNameImpliesVision(candidate) {
			return true
		}
	}
	return false
}

func opencodeGoVisionModelCandidates(model string) []string {
	model = strings.TrimSpace(model)
	if model == "" {
		return nil
	}
	if idx := strings.LastIndex(model, "/"); idx >= 0 && idx+1 < len(model) {
		return []string{model, model[idx+1:]}
	}
	return []string{model}
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
	for _, candidate := range opencodeGoVisionModelCandidates(baseModel) {
		if strings.HasPrefix(candidate, "qwen") {
			return true
		}
	}
	return false
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
