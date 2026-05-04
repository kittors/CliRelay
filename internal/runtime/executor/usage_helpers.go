package executor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/util"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/usage"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const usageReporterOutputMemoryLimit = 256 * 1024

type usageReporter struct {
	provider    string
	model       string
	authID      string
	authIndex   string
	apiKey      string
	source      string
	channelName string
	requestedAt time.Time
	once        sync.Once
	contentMu   sync.Mutex

	// Content captured for log detail viewer
	inputContent  string
	outputContent string
	outputBuilder strings.Builder
	outputFile    *os.File
	outputPath    string
}

func newUsageReporter(ctx context.Context, provider, model string, auth *cliproxyauth.Auth) *usageReporter {
	apiKey := apiKeyFromContext(ctx)
	reporter := &usageReporter{
		provider:    provider,
		model:       model,
		requestedAt: time.Now(),
		apiKey:      apiKey,
		source:      resolveUsageSource(auth, apiKey),
	}
	if auth != nil {
		reporter.authID = auth.ID
		reporter.authIndex = auth.EnsureIndex()
		reporter.channelName = strings.TrimSpace(auth.ChannelName())
	}
	return reporter
}

func (r *usageReporter) publish(ctx context.Context, detail usage.Detail) {
	r.publishWithOutcome(ctx, detail, false)
}

func (r *usageReporter) publishWithContent(ctx context.Context, detail usage.Detail, inputContent, outputContent string) {
	r.inputContent = inputContent
	r.outputContent = outputContent
	r.publishWithOutcome(ctx, detail, false)
}

// setInputContent stores the request payload for inclusion in usage records.
// Call before starting the streaming goroutine.
func (r *usageReporter) setInputContent(content string) {
	if r == nil {
		return
	}
	r.contentMu.Lock()
	defer r.contentMu.Unlock()
	r.inputContent = content
}

// appendOutputChunk accumulates a streaming response line for inclusion in usage records.
func (r *usageReporter) appendOutputChunk(chunk []byte) {
	if r == nil || len(chunk) == 0 {
		return
	}
	r.contentMu.Lock()
	defer r.contentMu.Unlock()

	if r.outputFile == nil && r.outputBuilder.Len()+len(chunk)+1 > usageReporterOutputMemoryLimit {
		if err := r.spillOutputBuilderToFileLocked(); err != nil {
			log.Errorf("usage: spill streaming output to temp file: %v", err)
		}
	}

	if r.outputFile != nil {
		if _, err := r.outputFile.Write(chunk); err != nil {
			log.Errorf("usage: write streaming output chunk to temp file: %v", err)
			r.outputBuilder.Write(chunk)
			r.outputBuilder.WriteByte('\n')
			return
		}
		if _, err := r.outputFile.Write([]byte{'\n'}); err != nil {
			log.Errorf("usage: write streaming output newline to temp file: %v", err)
		}
		return
	}

	r.outputBuilder.Write(chunk)
	r.outputBuilder.WriteByte('\n')
}

func (r *usageReporter) publishFailure(ctx context.Context) {
	r.publishWithOutcome(ctx, usage.Detail{}, true)
}

// publishFailureWithContent records a failed request together with the
// request payload and the upstream error response body so that the error
// is visible in the management UI error-detail modal.
func (r *usageReporter) publishFailureWithContent(ctx context.Context, inputContent, outputContent string) {
	if r == nil {
		return
	}
	if shouldSuppressUsageFailure(nil, outputContent) {
		return
	}
	r.contentMu.Lock()
	r.inputContent = inputContent
	r.outputContent = outputContent
	r.contentMu.Unlock()
	r.publishWithOutcome(ctx, usage.Detail{}, true)
}

func (r *usageReporter) trackFailure(ctx context.Context, errPtr *error) {
	if r == nil || errPtr == nil {
		return
	}
	if *errPtr != nil {
		if shouldSuppressUsageFailure(*errPtr, "") {
			return
		}
		r.contentMu.Lock()
		if r.outputContent == "" && r.outputBuilder.Len() == 0 && r.outputFile == nil {
			r.outputContent = structuredUpstreamErrorJSON(*errPtr)
		}
		r.contentMu.Unlock()
		r.publishFailure(ctx)
	}
}

func shouldSuppressUsageFailure(err error, outputContent string) bool {
	if errors.Is(err, context.Canceled) {
		return true
	}
	return strings.Contains(strings.ToLower(outputContent), "context canceled")
}

type upstreamBodyError interface {
	UpstreamErrorBody() []byte
}

func structuredUpstreamErrorJSON(err error) string {
	msg := ""
	if err != nil {
		msg = strings.TrimSpace(err.Error())
	}
	if msg == "" {
		msg = "Upstream request failed."
	}
	errorBody := map[string]any{
		"message": msg,
		"type":    "upstream_error",
	}
	if upstreamErr, ok := err.(upstreamBodyError); ok {
		upstreamBody := strings.TrimSpace(string(upstreamErr.UpstreamErrorBody()))
		if upstreamBody != "" {
			errorBody["upstream"] = parseStructuredUpstreamBody(upstreamBody)
		}
	}
	body := map[string]any{
		"error": errorBody,
	}
	data, err := json.Marshal(body)
	if err != nil {
		return `{"error":{"message":"Upstream request failed.","type":"upstream_error"}}`
	}
	return string(data)
}

func parseStructuredUpstreamBody(body string) any {
	var decoded any
	if err := json.Unmarshal([]byte(body), &decoded); err == nil {
		return decoded
	}
	return body
}

func (r *usageReporter) publishWithOutcome(ctx context.Context, detail usage.Detail, failed bool) {
	if r == nil {
		return
	}
	if detail.TotalTokens == 0 {
		total := detail.InputTokens + detail.OutputTokens + detail.ReasoningTokens
		if total > 0 {
			detail.TotalTokens = total
		}
	}
	if detail.InputTokens == 0 && detail.OutputTokens == 0 && detail.ReasoningTokens == 0 && detail.CachedTokens == 0 && detail.TotalTokens == 0 && !failed {
		return
	}
	r.once.Do(func() {
		inputContent, outputContent := r.finalizeContent()
		latencyMs := time.Since(r.requestedAt).Milliseconds()
		if latencyMs < 0 {
			latencyMs = 0
		}
		firstTokenMs := firstTokenLatencyMsFromContext(ctx, r.requestedAt)
		usage.PublishRecord(ctx, usage.Record{
			Provider:      r.provider,
			Model:         r.model,
			Source:        r.source,
			ChannelName:   r.channelName,
			APIKey:        r.apiKey,
			AuthID:        r.authID,
			AuthIndex:     r.authIndex,
			RequestedAt:   r.requestedAt,
			LatencyMs:     latencyMs,
			FirstTokenMs:  firstTokenMs,
			Failed:        failed,
			Detail:        detail,
			InputContent:  inputContent,
			OutputContent: outputContent,
			DetailContent: buildRequestDetailContent(ctx),
		})
	})
}

// ensurePublished guarantees that a usage record is emitted exactly once.
// It is safe to call multiple times; only the first call wins due to once.Do.
// This is used to ensure request counting even when upstream responses do not
// include any usage fields (tokens), especially for streaming paths.
func (r *usageReporter) ensurePublished(ctx context.Context) {
	if r == nil {
		return
	}
	r.once.Do(func() {
		inputContent, outputContent := r.finalizeContent()
		latencyMs := time.Since(r.requestedAt).Milliseconds()
		if latencyMs < 0 {
			latencyMs = 0
		}
		firstTokenMs := firstTokenLatencyMsFromContext(ctx, r.requestedAt)
		usage.PublishRecord(ctx, usage.Record{
			Provider:      r.provider,
			Model:         r.model,
			Source:        r.source,
			ChannelName:   r.channelName,
			APIKey:        r.apiKey,
			AuthID:        r.authID,
			AuthIndex:     r.authIndex,
			RequestedAt:   r.requestedAt,
			LatencyMs:     latencyMs,
			FirstTokenMs:  firstTokenMs,
			Failed:        false,
			Detail:        usage.Detail{},
			InputContent:  inputContent,
			OutputContent: outputContent,
			DetailContent: buildRequestDetailContent(ctx),
		})
	})
}

func (r *usageReporter) spillOutputBuilderToFileLocked() error {
	if r.outputFile != nil {
		return nil
	}
	file, err := os.CreateTemp("", "cliproxy-usage-output-*")
	if err != nil {
		return err
	}
	if r.outputBuilder.Len() > 0 {
		if _, err := file.WriteString(r.outputBuilder.String()); err != nil {
			_ = file.Close()
			_ = os.Remove(file.Name())
			return err
		}
		r.outputBuilder.Reset()
	}
	r.outputFile = file
	r.outputPath = file.Name()
	return nil
}

func (r *usageReporter) finalizeContent() (string, string) {
	if r == nil {
		return "", ""
	}
	r.contentMu.Lock()
	defer r.contentMu.Unlock()

	output := r.outputContent
	if r.outputBuilder.Len() > 0 {
		output += r.outputBuilder.String()
		r.outputBuilder.Reset()
	}
	if r.outputFile != nil {
		path := r.outputPath
		if err := r.outputFile.Close(); err != nil {
			log.Errorf("usage: close streaming output temp file: %v", err)
		}
		r.outputFile = nil
		r.outputPath = ""
		if data, err := os.ReadFile(path); err != nil {
			log.Errorf("usage: read streaming output temp file: %v", err)
		} else {
			output += string(data)
		}
		if path != "" {
			if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
				log.Warnf("usage: remove streaming output temp file: %v", err)
			}
		}
	}
	r.outputContent = output
	return r.inputContent, r.outputContent
}

func apiKeyFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	if value := strings.TrimSpace(contextStringValue(ctx, util.ContextKeyAPIKey)); value != "" {
		return value
	}
	ginCtx, ok := ctx.Value(util.ContextKeyGin).(*gin.Context)
	if !ok || ginCtx == nil {
		return ""
	}
	if v, exists := ginCtx.Get("apiKey"); exists {
		switch value := v.(type) {
		case string:
			return value
		case fmt.Stringer:
			return value.String()
		default:
			return fmt.Sprintf("%v", value)
		}
	}
	return ""
}

func buildRequestDetailContent(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	ginCtx, ok := ctx.Value(util.ContextKeyGin).(*gin.Context)
	if !ok || ginCtx == nil || ginCtx.Request == nil {
		return ""
	}

	req := ginCtx.Request
	apiRequest, _ := ginCtx.Get(apiRequestKey)
	apiResponse, _ := ginCtx.Get(apiResponseKey)

	detail := map[string]any{
		"client": map[string]any{
			"ip":                  ginCtx.ClientIP(),
			"remote_addr":         req.RemoteAddr,
			"method":              req.Method,
			"url":                 req.URL.String(),
			"path":                req.URL.Path,
			"query":               req.URL.Query(),
			"host":                req.Host,
			"content_length":      req.ContentLength,
			"headers":             cloneHeaderValues(req.Header),
			"fingerprint_headers": extractFingerprintHeaders(req.Header),
		},
		"upstream": map[string]any{
			"request_log": bytesToString(apiRequest),
		},
		"response": map[string]any{
			"upstream_log": bytesToString(apiResponse),
		},
	}

	data, err := json.Marshal(detail)
	if err != nil {
		return ""
	}
	return string(data)
}

func bytesToString(value any) string {
	data, ok := value.([]byte)
	if !ok || len(data) == 0 {
		return ""
	}
	return string(data)
}

func cloneHeaderValues(headers http.Header) map[string][]string {
	if len(headers) == 0 {
		return map[string][]string{}
	}
	out := make(map[string][]string, len(headers))
	for key, values := range headers {
		copied := make([]string, len(values))
		copy(copied, values)
		out[key] = copied
	}
	return out
}

func extractFingerprintHeaders(headers http.Header) map[string][]string {
	out := make(map[string][]string)
	for key, values := range headers {
		normalized := strings.ToLower(strings.TrimSpace(key))
		if normalized == "" {
			continue
		}
		if normalized == "user-agent" ||
			strings.Contains(normalized, "session") ||
			strings.Contains(normalized, "version") ||
			strings.Contains(normalized, "originator") ||
			strings.Contains(normalized, "codex") ||
			strings.Contains(normalized, "claude") ||
			strings.Contains(normalized, "gemini") ||
			strings.HasPrefix(normalized, "x-") {
			copied := make([]string, len(values))
			copy(copied, values)
			out[key] = copied
		}
	}
	return out
}

func contextStringValue(ctx context.Context, key any) string {
	if ctx == nil {
		return ""
	}
	switch value := ctx.Value(key).(type) {
	case string:
		return value
	case fmt.Stringer:
		return value.String()
	case nil:
		return ""
	default:
		return fmt.Sprintf("%v", value)
	}
}

func firstTokenLatencyMsFromContext(ctx context.Context, requestedAt time.Time) int64 {
	if ctx == nil || requestedAt.IsZero() {
		return 0
	}
	ginCtx, ok := ctx.Value(util.ContextKeyGin).(*gin.Context)
	if !ok || ginCtx == nil {
		return 0
	}
	value, exists := ginCtx.Get(util.GinKeyFirstResponseAt)
	if !exists {
		return 0
	}
	firstResponseAt, ok := value.(time.Time)
	if !ok || firstResponseAt.IsZero() {
		return 0
	}
	latencyMs := firstResponseAt.Sub(requestedAt).Milliseconds()
	if latencyMs < 0 {
		return 0
	}
	return latencyMs
}

func resolveUsageSource(auth *cliproxyauth.Auth, ctxAPIKey string) string {
	if auth != nil {
		provider := strings.TrimSpace(auth.Provider)
		if strings.EqualFold(provider, "gemini-cli") {
			if id := strings.TrimSpace(auth.ID); id != "" {
				return id
			}
		}
		if strings.EqualFold(provider, "vertex") {
			if auth.Metadata != nil {
				if projectID, ok := auth.Metadata["project_id"].(string); ok {
					if trimmed := strings.TrimSpace(projectID); trimmed != "" {
						return trimmed
					}
				}
				if project, ok := auth.Metadata["project"].(string); ok {
					if trimmed := strings.TrimSpace(project); trimmed != "" {
						return trimmed
					}
				}
			}
		}
		if _, value := auth.AccountInfo(); value != "" {
			return strings.TrimSpace(value)
		}
		if auth.Metadata != nil {
			if email, ok := auth.Metadata["email"].(string); ok {
				if trimmed := strings.TrimSpace(email); trimmed != "" {
					return trimmed
				}
			}
		}
		if auth.Attributes != nil {
			if key := strings.TrimSpace(auth.Attributes["api_key"]); key != "" {
				return key
			}
		}
	}
	if trimmed := strings.TrimSpace(ctxAPIKey); trimmed != "" {
		return trimmed
	}
	return ""
}

func parseCodexUsage(data []byte) (usage.Detail, bool) {
	usageNode := gjson.ParseBytes(data).Get("response.usage")
	if !usageNode.Exists() {
		return usage.Detail{}, false
	}
	detail := usage.Detail{
		InputTokens:  usageNode.Get("input_tokens").Int(),
		OutputTokens: usageNode.Get("output_tokens").Int(),
		TotalTokens:  usageNode.Get("total_tokens").Int(),
	}
	if cached := usageNode.Get("input_tokens_details.cached_tokens"); cached.Exists() {
		detail.CachedTokens = cached.Int()
	}
	if reasoning := usageNode.Get("output_tokens_details.reasoning_tokens"); reasoning.Exists() {
		detail.ReasoningTokens = reasoning.Int()
	}
	return detail, true
}

func parseOpenAIUsage(data []byte) usage.Detail {
	usageNode := gjson.ParseBytes(data).Get("usage")
	if !usageNode.Exists() {
		return usage.Detail{}
	}
	inputNode := usageNode.Get("prompt_tokens")
	if !inputNode.Exists() {
		inputNode = usageNode.Get("input_tokens")
	}
	outputNode := usageNode.Get("completion_tokens")
	if !outputNode.Exists() {
		outputNode = usageNode.Get("output_tokens")
	}
	detail := usage.Detail{
		InputTokens:  inputNode.Int(),
		OutputTokens: outputNode.Int(),
		TotalTokens:  usageNode.Get("total_tokens").Int(),
	}
	cached := usageNode.Get("prompt_tokens_details.cached_tokens")
	if !cached.Exists() {
		cached = usageNode.Get("input_tokens_details.cached_tokens")
	}
	if cached.Exists() {
		detail.CachedTokens = cached.Int()
	}
	reasoning := usageNode.Get("completion_tokens_details.reasoning_tokens")
	if !reasoning.Exists() {
		reasoning = usageNode.Get("output_tokens_details.reasoning_tokens")
	}
	if reasoning.Exists() {
		detail.ReasoningTokens = reasoning.Int()
	}
	return detail
}

func parseOpenAIStreamUsage(line []byte) (usage.Detail, bool) {
	payload := jsonPayload(line)
	if len(payload) == 0 || !gjson.ValidBytes(payload) {
		return usage.Detail{}, false
	}
	usageNode := gjson.GetBytes(payload, "usage")
	if !usageNode.Exists() {
		return usage.Detail{}, false
	}
	detail := usage.Detail{
		InputTokens:  usageNode.Get("prompt_tokens").Int(),
		OutputTokens: usageNode.Get("completion_tokens").Int(),
		TotalTokens:  usageNode.Get("total_tokens").Int(),
	}
	if cached := usageNode.Get("prompt_tokens_details.cached_tokens"); cached.Exists() {
		detail.CachedTokens = cached.Int()
	}
	if reasoning := usageNode.Get("completion_tokens_details.reasoning_tokens"); reasoning.Exists() {
		detail.ReasoningTokens = reasoning.Int()
	}
	return detail, true
}

func parseClaudeUsage(data []byte) usage.Detail {
	usageNode := gjson.ParseBytes(data).Get("usage")
	if !usageNode.Exists() {
		return usage.Detail{}
	}
	detail := usage.Detail{
		InputTokens:  usageNode.Get("input_tokens").Int(),
		OutputTokens: usageNode.Get("output_tokens").Int(),
		CachedTokens: usageNode.Get("cache_read_input_tokens").Int(),
	}
	if detail.CachedTokens == 0 {
		// fall back to creation tokens when read tokens are absent
		detail.CachedTokens = usageNode.Get("cache_creation_input_tokens").Int()
	}
	detail.TotalTokens = detail.InputTokens + detail.OutputTokens
	return detail
}

func parseClaudeStreamUsage(line []byte) (usage.Detail, bool) {
	payload := jsonPayload(line)
	if len(payload) == 0 || !gjson.ValidBytes(payload) {
		return usage.Detail{}, false
	}
	usageNode := gjson.GetBytes(payload, "usage")
	if !usageNode.Exists() {
		return usage.Detail{}, false
	}
	detail := usage.Detail{
		InputTokens:  usageNode.Get("input_tokens").Int(),
		OutputTokens: usageNode.Get("output_tokens").Int(),
		CachedTokens: usageNode.Get("cache_read_input_tokens").Int(),
	}
	if detail.CachedTokens == 0 {
		detail.CachedTokens = usageNode.Get("cache_creation_input_tokens").Int()
	}
	detail.TotalTokens = detail.InputTokens + detail.OutputTokens
	return detail, true
}

func parseGeminiFamilyUsageDetail(node gjson.Result) usage.Detail {
	detail := usage.Detail{
		InputTokens:     node.Get("promptTokenCount").Int(),
		OutputTokens:    node.Get("candidatesTokenCount").Int(),
		ReasoningTokens: node.Get("thoughtsTokenCount").Int(),
		TotalTokens:     node.Get("totalTokenCount").Int(),
		CachedTokens:    node.Get("cachedContentTokenCount").Int(),
	}
	if detail.TotalTokens == 0 {
		detail.TotalTokens = detail.InputTokens + detail.OutputTokens + detail.ReasoningTokens
	}
	return detail
}

func parseGeminiCLIUsage(data []byte) usage.Detail {
	usageNode := gjson.ParseBytes(data)
	node := usageNode.Get("response.usageMetadata")
	if !node.Exists() {
		node = usageNode.Get("response.usage_metadata")
	}
	if !node.Exists() {
		return usage.Detail{}
	}
	return parseGeminiFamilyUsageDetail(node)
}

func parseGeminiUsage(data []byte) usage.Detail {
	usageNode := gjson.ParseBytes(data)
	node := usageNode.Get("usageMetadata")
	if !node.Exists() {
		node = usageNode.Get("usage_metadata")
	}
	if !node.Exists() {
		return usage.Detail{}
	}
	return parseGeminiFamilyUsageDetail(node)
}

func parseGeminiStreamUsage(line []byte) (usage.Detail, bool) {
	payload := jsonPayload(line)
	if len(payload) == 0 || !gjson.ValidBytes(payload) {
		return usage.Detail{}, false
	}
	node := gjson.GetBytes(payload, "usageMetadata")
	if !node.Exists() {
		node = gjson.GetBytes(payload, "usage_metadata")
	}
	if !node.Exists() {
		return usage.Detail{}, false
	}
	return parseGeminiFamilyUsageDetail(node), true
}

func parseGeminiCLIStreamUsage(line []byte) (usage.Detail, bool) {
	payload := jsonPayload(line)
	if len(payload) == 0 || !gjson.ValidBytes(payload) {
		return usage.Detail{}, false
	}
	node := gjson.GetBytes(payload, "response.usageMetadata")
	if !node.Exists() {
		node = gjson.GetBytes(payload, "usage_metadata")
	}
	if !node.Exists() {
		return usage.Detail{}, false
	}
	return parseGeminiFamilyUsageDetail(node), true
}

func parseAntigravityUsage(data []byte) usage.Detail {
	usageNode := gjson.ParseBytes(data)
	node := usageNode.Get("response.usageMetadata")
	if !node.Exists() {
		node = usageNode.Get("usageMetadata")
	}
	if !node.Exists() {
		node = usageNode.Get("usage_metadata")
	}
	if !node.Exists() {
		return usage.Detail{}
	}
	return parseGeminiFamilyUsageDetail(node)
}

func parseAntigravityStreamUsage(line []byte) (usage.Detail, bool) {
	payload := jsonPayload(line)
	if len(payload) == 0 || !gjson.ValidBytes(payload) {
		return usage.Detail{}, false
	}
	node := gjson.GetBytes(payload, "response.usageMetadata")
	if !node.Exists() {
		node = gjson.GetBytes(payload, "usageMetadata")
	}
	if !node.Exists() {
		node = gjson.GetBytes(payload, "usage_metadata")
	}
	if !node.Exists() {
		return usage.Detail{}, false
	}
	return parseGeminiFamilyUsageDetail(node), true
}

var stopChunkWithoutUsage sync.Map

func rememberStopWithoutUsage(traceID string) {
	stopChunkWithoutUsage.Store(traceID, struct{}{})
	time.AfterFunc(10*time.Minute, func() { stopChunkWithoutUsage.Delete(traceID) })
}

// FilterSSEUsageMetadata removes usageMetadata from SSE events that are not
// terminal (finishReason != "stop"). Stop chunks are left untouched. This
// function is shared between aistudio and antigravity executors.
func FilterSSEUsageMetadata(payload []byte) []byte {
	if len(payload) == 0 {
		return payload
	}

	lines := bytes.Split(payload, []byte("\n"))
	modified := false
	foundData := false
	for idx, line := range lines {
		trimmed := bytes.TrimSpace(line)
		if len(trimmed) == 0 || !bytes.HasPrefix(trimmed, []byte("data:")) {
			continue
		}
		foundData = true
		dataIdx := bytes.Index(line, []byte("data:"))
		if dataIdx < 0 {
			continue
		}
		rawJSON := bytes.TrimSpace(line[dataIdx+5:])
		traceID := gjson.GetBytes(rawJSON, "traceId").String()
		if isStopChunkWithoutUsage(rawJSON) && traceID != "" {
			rememberStopWithoutUsage(traceID)
			continue
		}
		if traceID != "" {
			if _, ok := stopChunkWithoutUsage.Load(traceID); ok && hasUsageMetadata(rawJSON) {
				stopChunkWithoutUsage.Delete(traceID)
				continue
			}
		}

		cleaned, changed := StripUsageMetadataFromJSON(rawJSON)
		if !changed {
			continue
		}
		var rebuilt []byte
		rebuilt = append(rebuilt, line[:dataIdx]...)
		rebuilt = append(rebuilt, []byte("data:")...)
		if len(cleaned) > 0 {
			rebuilt = append(rebuilt, ' ')
			rebuilt = append(rebuilt, cleaned...)
		}
		lines[idx] = rebuilt
		modified = true
	}
	if !modified {
		if !foundData {
			// Handle payloads that are raw JSON without SSE data: prefix.
			trimmed := bytes.TrimSpace(payload)
			cleaned, changed := StripUsageMetadataFromJSON(trimmed)
			if !changed {
				return payload
			}
			return cleaned
		}
		return payload
	}
	return bytes.Join(lines, []byte("\n"))
}

// StripUsageMetadataFromJSON drops usageMetadata unless finishReason is present (terminal).
// It handles both formats:
// - Aistudio: candidates.0.finishReason
// - Antigravity: response.candidates.0.finishReason
func StripUsageMetadataFromJSON(rawJSON []byte) ([]byte, bool) {
	jsonBytes := bytes.TrimSpace(rawJSON)
	if len(jsonBytes) == 0 || !gjson.ValidBytes(jsonBytes) {
		return rawJSON, false
	}

	// Check for finishReason in both aistudio and antigravity formats
	finishReason := gjson.GetBytes(jsonBytes, "candidates.0.finishReason")
	if !finishReason.Exists() {
		finishReason = gjson.GetBytes(jsonBytes, "response.candidates.0.finishReason")
	}
	terminalReason := finishReason.Exists() && strings.TrimSpace(finishReason.String()) != ""

	usageMetadata := gjson.GetBytes(jsonBytes, "usageMetadata")
	if !usageMetadata.Exists() {
		usageMetadata = gjson.GetBytes(jsonBytes, "response.usageMetadata")
	}

	// Terminal chunk: keep as-is.
	if terminalReason {
		return rawJSON, false
	}

	// Nothing to strip
	if !usageMetadata.Exists() {
		return rawJSON, false
	}

	// Remove usageMetadata from both possible locations
	cleaned := jsonBytes
	var changed bool

	if usageMetadata = gjson.GetBytes(cleaned, "usageMetadata"); usageMetadata.Exists() {
		// Rename usageMetadata to cpaUsageMetadata in the message_start event of Claude
		cleaned, _ = sjson.SetRawBytes(cleaned, "cpaUsageMetadata", []byte(usageMetadata.Raw))
		cleaned, _ = sjson.DeleteBytes(cleaned, "usageMetadata")
		changed = true
	}

	if usageMetadata = gjson.GetBytes(cleaned, "response.usageMetadata"); usageMetadata.Exists() {
		// Rename usageMetadata to cpaUsageMetadata in the message_start event of Claude
		cleaned, _ = sjson.SetRawBytes(cleaned, "response.cpaUsageMetadata", []byte(usageMetadata.Raw))
		cleaned, _ = sjson.DeleteBytes(cleaned, "response.usageMetadata")
		changed = true
	}

	return cleaned, changed
}

func hasUsageMetadata(jsonBytes []byte) bool {
	if len(jsonBytes) == 0 || !gjson.ValidBytes(jsonBytes) {
		return false
	}
	if gjson.GetBytes(jsonBytes, "usageMetadata").Exists() {
		return true
	}
	if gjson.GetBytes(jsonBytes, "response.usageMetadata").Exists() {
		return true
	}
	return false
}

func isStopChunkWithoutUsage(jsonBytes []byte) bool {
	if len(jsonBytes) == 0 || !gjson.ValidBytes(jsonBytes) {
		return false
	}
	finishReason := gjson.GetBytes(jsonBytes, "candidates.0.finishReason")
	if !finishReason.Exists() {
		finishReason = gjson.GetBytes(jsonBytes, "response.candidates.0.finishReason")
	}
	trimmed := strings.TrimSpace(finishReason.String())
	if !finishReason.Exists() || trimmed == "" {
		return false
	}
	return !hasUsageMetadata(jsonBytes)
}

func jsonPayload(line []byte) []byte {
	trimmed := bytes.TrimSpace(line)
	if len(trimmed) == 0 {
		return nil
	}
	if bytes.Equal(trimmed, []byte("[DONE]")) {
		return nil
	}
	if bytes.HasPrefix(trimmed, []byte("event:")) {
		return nil
	}
	if bytes.HasPrefix(trimmed, []byte("data:")) {
		trimmed = bytes.TrimSpace(trimmed[len("data:"):])
	}
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return nil
	}
	return trimmed
}
