package executor

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/openaicompat"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/thinking"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/util"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
	coreusage "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/usage"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v6/sdk/translator"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// OpenAICompatExecutor implements a stateless executor for OpenAI-compatible providers.
// It performs request/response translation and executes against the provider base URL
// using per-auth credentials (API key) and per-auth HTTP transport (proxy) from context.
type OpenAICompatExecutor struct {
	provider string
	cfg      *config.Config
}

// NewOpenAICompatExecutor creates an executor bound to a provider key (e.g., "openrouter").
func NewOpenAICompatExecutor(provider string, cfg *config.Config) *OpenAICompatExecutor {
	return &OpenAICompatExecutor{provider: provider, cfg: cfg}
}

// Identifier implements cliproxyauth.ProviderExecutor.
func (e *OpenAICompatExecutor) Identifier() string { return e.provider }

// PrepareRequest injects OpenAI-compatible credentials into the outgoing HTTP request.
func (e *OpenAICompatExecutor) PrepareRequest(req *http.Request, auth *cliproxyauth.Auth) error {
	if req == nil {
		return nil
	}
	_, apiKey := e.resolveCredentials(auth)
	if strings.TrimSpace(apiKey) != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	var attrs map[string]string
	if auth != nil {
		attrs = auth.Attributes
	}
	util.ApplyCustomHeadersFromAttrs(req, attrs)
	return nil
}

// HttpRequest injects OpenAI-compatible credentials into the request and executes it.
func (e *OpenAICompatExecutor) HttpRequest(ctx context.Context, auth *cliproxyauth.Auth, req *http.Request) (*http.Response, error) {
	if req == nil {
		return nil, fmt.Errorf("openai compat executor: request is nil")
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

func (e *OpenAICompatExecutor) Execute(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (resp cliproxyexecutor.Response, err error) {
	fallback := opencodeGoVisionFallbackResult{Request: req}
	if openAICompatSupportsVisionFallback(e.provider) {
		originalRequestedModel := payloadRequestedModel(opts, req.Model)
		originalUpstreamModel := thinking.ParseSuffix(req.Model).ModelName
		fallback = applyVisionFallback(req, opts, openAICompatVisionFallbackModel(e.cfg, auth, e.provider))
		if fallback.Applied {
			ctx = contextWithVisionFallbackLog(ctx, originalRequestedModel, originalUpstreamModel, fallback.FallbackModel)
		}
		req = fallback.Request
	}

	to := sdktranslator.FromString("openai")
	endpoint := "/chat/completions"
	if opts.Alt == "responses/compact" {
		to = sdktranslator.FromString("openai-response")
		endpoint = "/responses/compact"
	}
	execCtx := newExecutionContext(ctx, e.Identifier(), e.cfg, auth, req, opts, ExecutionOptions{
		TargetFormat:      to,
		TranslateAsStream: false,
	})
	reporter := execCtx.Reporter()
	defer reporter.trackFailure(execCtx.Context, &err)

	baseURL, apiKey := e.resolveCredentials(auth)
	if baseURL == "" {
		err = statusErr{code: http.StatusUnauthorized, msg: "missing provider baseURL"}
		return
	}

	translated, originalTranslated := execCtx.TranslateRequestPair(req.Payload)
	translated = execCtx.ApplyPayloadConfig(translated, originalTranslated)
	if opts.Alt == "responses/compact" {
		if updated, errDelete := sjson.DeleteBytes(translated, "stream"); errDelete == nil {
			translated = updated
		}
	}

	translated, err = thinking.ApplyThinking(translated, req.Model, execCtx.SourceFormat.String(), to.String(), e.Identifier())
	if err != nil {
		return resp, err
	}
	if shouldNormalizeKimiCompatPayload(execCtx.BaseModel) {
		translated, err = normalizeKimiToolMessageLinks(translated)
		if err != nil {
			return resp, err
		}
	}
	if e.provider == openCodeGoProvider && execCtx.SourceFormat == sdktranslator.FormatClaude && endpoint == "/chat/completions" {
		translated = opencodeGoPreserveClaudeCacheControl(translated, req.Payload)
	}

	// OpenCode-Go: the translator drops reasoning_content when converting
	// from Responses API. Inject it here into translated payload so it survives.
	if e.provider == openCodeGoProvider && opencodeGoNeedsReasoningInjection(execCtx.BaseModel) {
		sessionID := opencodeGoSessionID(opts, auth)
		if sessionID != "" {
			translated = opencodeGoInjectMessagesArray(translated, req.Model, sessionID)
		}
	}

	// Expand Codex MCP namespace tools into concrete function tools for
	// OpenAI-compatible upstreams that cannot consume Codex namespaces directly.
	translated = opencodeGoInjectCodexToolBridgeTools(translated)

	// Fix concatenated JSON in function.arguments - some upstreams (e.g., Alibaba/Qwen)
	// reject malformed tool_call arguments generated by the Codex Desktop client.
	translated = opencodeGoFixToolCallArguments(translated)
	if endpoint == "/chat/completions" {
		translated = normalizeOpenAIChatToolCallMessages(translated)
	}

	url := strings.TrimSuffix(baseURL, "/") + endpoint
	httpReq, err := http.NewRequestWithContext(execCtx.Context, http.MethodPost, url, bytes.NewReader(translated))
	if err != nil {
		return resp, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if apiKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+apiKey)
	}
	httpReq.Header.Set("User-Agent", "cli-proxy-openai-compat")
	var attrs map[string]string
	if auth != nil {
		attrs = auth.Attributes
	}
	util.ApplyCustomHeadersFromAttrs(httpReq, attrs)
	recorder := execCtx.Recorder()
	recorder.RecordRequest(url, http.MethodPost, httpReq.Header.Clone(), translated)

	httpClient := execCtx.HTTPClient(0)
	//nolint:bodyclose // success body is consumed and closed by the stream goroutine below.
	httpResp, err := httpClient.Do(httpReq)
	if err != nil {
		recorder.RecordResponseError(err)
		reporter.publishFailureWithContent(execCtx.Context, string(req.Payload), err.Error())
		return resp, err
	}
	defer func() {
		if errClose := httpResp.Body.Close(); errClose != nil {
			log.Errorf("openai compat executor: close response body error: %v", errClose)
		}
	}()
	recorder.RecordResponseMetadata(httpResp.StatusCode, httpResp.Header.Clone())
	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		b := readUpstreamErrorBody(e.Identifier(), httpResp.Body)
		recorder.AppendResponseChunk(b)
		logWithRequestID(execCtx.Context).Debugf("request error, error status: %d, error message: %s", httpResp.StatusCode, summarizeErrorBody(httpResp.Header.Get("Content-Type"), b))
		reporter.publishFailureWithContent(execCtx.Context, string(req.Payload), string(b))
		err = statusErr{code: httpResp.StatusCode, msg: string(b), headers: httpResp.Header.Clone()}
		return resp, err
	}
	body, err := readUpstreamResponseBody(e.Identifier(), httpResp.Body)
	if err != nil {
		recorder.RecordResponseError(err)
		return resp, err
	}
	recorder.AppendResponseChunk(body)
	responseBody := unwrapOpenAICompatProviderEnvelope(body)
	// Preserve the clean, request-time model (ec.BaseModel) for the usage record.
	// The upstream response's "model" field echoes a provider-internal path such as
	// "accounts/fireworks/models/glm-5p2", which is not a valid model name for
	// display, pricing lookup (CalculateCostV2) or filtering. All other executors
	// (codex/claude/gemini/...) never override the reporter's model, so this keeps
	// the OpenAI-compat path consistent with them.
	reporter.publishWithContent(execCtx.Context, parseOpenAIUsage(responseBody), string(req.Payload), string(responseBody))
	// Ensure we at least record the request even if upstream doesn't return usage
	reporter.ensurePublished(execCtx.Context)
	// Translate response back to source format when needed
	var param any
	out := sdktranslator.TranslateNonStream(execCtx.Context, to, execCtx.SourceFormat, req.Model, opts.OriginalRequest, translated, responseBody, &param)
	resp = cliproxyexecutor.Response{Payload: []byte(out), Headers: httpResp.Header.Clone()}
	if fallback.Applied {
		resp.Payload = opencodeGoRewriteFallbackResponseModel(resp.Payload, fallback.OriginalModel)
	}
	return resp, nil
}

func (e *OpenAICompatExecutor) ExecuteStream(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (_ *cliproxyexecutor.StreamResult, err error) {
	fallback := opencodeGoVisionFallbackResult{Request: req}
	if openAICompatSupportsVisionFallback(e.provider) {
		originalRequestedModel := payloadRequestedModel(opts, req.Model)
		originalUpstreamModel := thinking.ParseSuffix(req.Model).ModelName
		fallback = applyVisionFallback(req, opts, openAICompatVisionFallbackModel(e.cfg, auth, e.provider))
		if fallback.Applied {
			ctx = contextWithVisionFallbackLog(ctx, originalRequestedModel, originalUpstreamModel, fallback.FallbackModel)
		}
		req = fallback.Request
	}

	to := sdktranslator.FromString("openai")
	execCtx := newExecutionContext(ctx, e.Identifier(), e.cfg, auth, req, opts, ExecutionOptions{
		TargetFormat:      to,
		TranslateAsStream: true,
	})
	reporter := execCtx.Reporter()
	defer reporter.trackFailure(execCtx.Context, &err)

	baseURL, apiKey := e.resolveCredentials(auth)
	if baseURL == "" {
		err = statusErr{code: http.StatusUnauthorized, msg: "missing provider baseURL"}
		return nil, err
	}

	translated, originalTranslated := execCtx.TranslateRequestPair(req.Payload)
	translated = execCtx.ApplyPayloadConfig(translated, originalTranslated)

	translated, err = thinking.ApplyThinking(translated, req.Model, execCtx.SourceFormat.String(), to.String(), e.Identifier())
	if err != nil {
		return nil, err
	}
	if shouldNormalizeKimiCompatPayload(execCtx.BaseModel) {
		translated, err = normalizeKimiToolMessageLinks(translated)
		if err != nil {
			return nil, err
		}
	}
	if e.provider == openCodeGoProvider && execCtx.SourceFormat == sdktranslator.FormatClaude {
		translated = opencodeGoPreserveClaudeCacheControl(translated, req.Payload)
	}

	// Inject reasoning_content for OpenCode-Go after Responses API translation.
	if e.provider == openCodeGoProvider && opencodeGoNeedsReasoningInjection(execCtx.BaseModel) {
		sessionID := opencodeGoSessionID(opts, auth)
		if sessionID != "" {
			translated = opencodeGoInjectMessagesArray(translated, req.Model, sessionID)
		}
	}

	// Expand Codex MCP namespace tools into concrete function tools for
	// OpenAI-compatible upstreams that cannot consume Codex namespaces directly.
	translated = opencodeGoInjectCodexToolBridgeTools(translated)

	// Fix concatenated JSON in function.arguments - some upstreams (e.g., Alibaba/Qwen)
	// reject malformed tool_call arguments generated by the Codex Desktop client.
	translated = opencodeGoFixToolCallArguments(translated)
	translated = normalizeOpenAIChatToolCallMessages(translated)

	url := strings.TrimSuffix(baseURL, "/") + "/chat/completions"
	httpReq, err := http.NewRequestWithContext(execCtx.Context, http.MethodPost, url, bytes.NewReader(translated))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if apiKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+apiKey)
	}
	httpReq.Header.Set("User-Agent", "cli-proxy-openai-compat")
	var attrs map[string]string
	if auth != nil {
		attrs = auth.Attributes
	}
	util.ApplyCustomHeadersFromAttrs(httpReq, attrs)
	httpReq.Header.Set("Accept", "text/event-stream")
	httpReq.Header.Set("Cache-Control", "no-cache")
	recorder := execCtx.Recorder()
	recorder.RecordRequest(url, http.MethodPost, httpReq.Header.Clone(), translated)

	httpClient := execCtx.HTTPClient(0)
	//nolint:bodyclose // success body is consumed and closed by the stream goroutine below.
	httpResp, err := httpClient.Do(httpReq)
	if err != nil {
		recorder.RecordResponseError(err)
		reporter.publishFailureWithContent(execCtx.Context, string(req.Payload), err.Error())
		return nil, err
	}
	recorder.RecordResponseMetadata(httpResp.StatusCode, httpResp.Header.Clone())
	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		b := readUpstreamErrorBody(e.Identifier(), httpResp.Body)
		recorder.AppendResponseChunk(b)
		logWithRequestID(execCtx.Context).Debugf("request error, error status: %d, error message: %s", httpResp.StatusCode, summarizeErrorBody(httpResp.Header.Get("Content-Type"), b))
		reporter.publishFailureWithContent(execCtx.Context, string(req.Payload), string(b))
		if errClose := httpResp.Body.Close(); errClose != nil {
			log.Errorf("openai compat executor: close response body error: %v", errClose)
		}
		err = statusErr{code: httpResp.StatusCode, msg: string(b), headers: httpResp.Header.Clone()}
		return nil, err
	}
	out := make(chan cliproxyexecutor.StreamChunk)
	reporter.setInputContent(string(req.Payload))
	go func() {
		defer close(out)
		defer func() {
			if errClose := httpResp.Body.Close(); errClose != nil {
				log.Errorf("openai compat executor: close response body error: %v", errClose)
			}
		}()
		scanner := bufio.NewScanner(httpResp.Body)
		scanner.Buffer(nil, 52_428_800) // 50MB
		var param any
		var lastUsage coreusage.Detail
		hasUsage := false
		var pendingResponsesCompleted []byte
		for scanner.Scan() {
			line := scanner.Bytes()
			recorder.AppendResponseChunk(line)
			reporter.appendOutputChunk(line)
			// Intentionally do NOT call reporter.setModel(parseOpenAIStreamModel(line)).
			// See the non-stream branch above: the upstream "model" echo is a
			// provider-internal path (e.g. accounts/fireworks/models/glm-5p2) that
			// must not override the clean request-time model used for logging/cost.
			lineForTranslation := unwrapOpenAICompatProviderEnvelopeSSELine(line)
			if detail, ok := parseOpenAIStreamUsage(lineForTranslation); ok {
				lastUsage = detail
				hasUsage = true
				if len(pendingResponsesCompleted) > 0 && isOpenAIStreamUsageOnly(lineForTranslation) {
					out <- cliproxyexecutor.StreamChunk{Payload: patchResponsesCompletedUsage(pendingResponsesCompleted, lastUsage)}
					pendingResponsesCompleted = nil
				}
			}
			if len(line) == 0 {
				continue
			}

			if !bytes.HasPrefix(line, []byte("data:")) {
				continue
			}

			// OpenAI-compatible streams are SSE: lines typically prefixed with "data: ".
			// Pass through translator; it yields one or more chunks for the target schema.
			chunks := sdktranslator.TranslateStream(execCtx.Context, to, execCtx.SourceFormat, req.Model, opts.OriginalRequest, translated, bytes.Clone(lineForTranslation), &param)
			for i := range chunks {
				if shouldHoldResponsesCompleted(execCtx.SourceFormat, lineForTranslation, []byte(chunks[i])) {
					pendingResponsesCompleted = []byte(chunks[i])
					continue
				}
				out <- cliproxyexecutor.StreamChunk{Payload: []byte(chunks[i])}
			}
		}
		if len(pendingResponsesCompleted) > 0 {
			if hasUsage {
				pendingResponsesCompleted = patchResponsesCompletedUsage(pendingResponsesCompleted, lastUsage)
			}
			out <- cliproxyexecutor.StreamChunk{Payload: pendingResponsesCompleted}
		}
		if errScan := scanner.Err(); errScan != nil {
			recorder.RecordResponseError(errScan)
			reporter.publishFailure(execCtx.Context)
			out <- cliproxyexecutor.StreamChunk{Err: errScan}
		}
		if hasUsage {
			reporter.publish(execCtx.Context, lastUsage)
		}
		// Ensure we record the request if no usage chunk was ever seen
		reporter.ensurePublished(execCtx.Context)
	}()
	result := &cliproxyexecutor.StreamResult{Headers: httpResp.Header.Clone(), Chunks: out}
	if fallback.Applied {
		result = opencodeGoRewriteFallbackStreamResult(result, fallback.OriginalModel)
	}
	return result, nil
}

func unwrapOpenAICompatProviderEnvelope(body []byte) []byte {
	root := openaicompat.ParseResponseRoot(body)
	if root.Raw == "" || root.Raw == string(body) {
		return body
	}
	return []byte(root.Raw)
}

func unwrapOpenAICompatProviderEnvelopeSSELine(line []byte) []byte {
	if !bytes.HasPrefix(line, []byte("data:")) {
		return line
	}
	payload := bytes.TrimSpace(line[5:])
	if len(payload) == 0 || bytes.Equal(payload, []byte("[DONE]")) {
		return line
	}
	unwrapped := unwrapOpenAICompatProviderEnvelope(payload)
	if bytes.Equal(unwrapped, payload) {
		return line
	}
	out := make([]byte, 0, len("data: ")+len(unwrapped))
	out = append(out, "data: "...)
	out = append(out, unwrapped...)
	return out
}

func shouldHoldResponsesCompleted(sourceFormat sdktranslator.Format, line, chunk []byte) bool {
	return sourceFormat == sdktranslator.FormatOpenAIResponse &&
		isOpenAIStreamFinish(line) &&
		!openAIStreamHasCachedTokens(line) &&
		isResponsesCompletedChunk(chunk)
}

func isOpenAIStreamFinish(line []byte) bool {
	payload := jsonPayload(line)
	if len(payload) == 0 || !gjson.ValidBytes(payload) {
		return false
	}
	for _, choice := range gjson.GetBytes(payload, "choices").Array() {
		if strings.TrimSpace(choice.Get("finish_reason").String()) != "" {
			return true
		}
	}
	return false
}

func openAIStreamHasCachedTokens(line []byte) bool {
	payload := jsonPayload(line)
	return len(payload) > 0 && gjson.GetBytes(payload, "usage.prompt_tokens_details.cached_tokens").Exists()
}

func isOpenAIStreamUsageOnly(line []byte) bool {
	payload := jsonPayload(line)
	if len(payload) == 0 || !gjson.GetBytes(payload, "usage").Exists() {
		return false
	}
	choices := gjson.GetBytes(payload, "choices")
	return !choices.Exists() || len(choices.Array()) == 0
}

func isResponsesCompletedChunk(chunk []byte) bool {
	payload := responsesSSEData(chunk)
	return len(payload) > 0 && gjson.GetBytes(payload, "type").String() == "response.completed"
}

func patchResponsesCompletedUsage(chunk []byte, detail coreusage.Detail) []byte {
	payload := responsesSSEData(chunk)
	if len(payload) == 0 || !gjson.ValidBytes(payload) {
		return chunk
	}
	updated, err := sjson.SetBytes(payload, "response.usage.input_tokens", detail.InputTokens)
	if err != nil {
		return chunk
	}
	cachedTokens := detail.CacheReadTokens
	if cachedTokens == 0 {
		cachedTokens = detail.CachedTokens
	}
	updated, _ = sjson.SetBytes(updated, "response.usage.input_tokens_details.cached_tokens", cachedTokens)
	updated, _ = sjson.SetBytes(updated, "response.usage.output_tokens", detail.OutputTokens)
	if detail.ReasoningTokens > 0 {
		updated, _ = sjson.SetBytes(updated, "response.usage.output_tokens_details.reasoning_tokens", detail.ReasoningTokens)
	}
	totalTokens := detail.TotalTokens
	if totalTokens == 0 {
		totalTokens = detail.InputTokens + detail.OutputTokens
	}
	updated, _ = sjson.SetBytes(updated, "response.usage.total_tokens", totalTokens)

	lines := strings.Split(string(chunk), "\n")
	for i, line := range lines {
		if strings.HasPrefix(strings.TrimSpace(line), "data:") {
			lines[i] = "data: " + string(updated)
			break
		}
	}
	return []byte(strings.Join(lines, "\n"))
}

func responsesSSEData(chunk []byte) []byte {
	for _, line := range bytes.Split(chunk, []byte("\n")) {
		trimmed := bytes.TrimSpace(line)
		if bytes.HasPrefix(trimmed, []byte("data:")) {
			return bytes.TrimSpace(trimmed[len("data:"):])
		}
	}
	return nil
}

func (e *OpenAICompatExecutor) CountTokens(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	baseModel := thinking.ParseSuffix(req.Model).ModelName

	from := opts.SourceFormat
	to := sdktranslator.FromString("openai")
	translated := sdktranslator.TranslateRequest(from, to, baseModel, req.Payload, false)

	modelForCounting := baseModel

	translated, err := thinking.ApplyThinking(translated, req.Model, from.String(), to.String(), e.Identifier())
	if err != nil {
		return cliproxyexecutor.Response{}, err
	}

	enc, err := tokenizerForModel(modelForCounting)
	if err != nil {
		return cliproxyexecutor.Response{}, fmt.Errorf("openai compat executor: tokenizer init failed: %w", err)
	}

	count, err := countOpenAIChatTokens(enc, translated)
	if err != nil {
		return cliproxyexecutor.Response{}, fmt.Errorf("openai compat executor: token counting failed: %w", err)
	}

	usageJSON := buildOpenAIUsageJSON(count)
	translatedUsage := sdktranslator.TranslateTokenCount(ctx, to, from, count, usageJSON)
	return cliproxyexecutor.Response{Payload: []byte(translatedUsage)}, nil
}

// Refresh is a no-op for API-key based compatibility providers.
func (e *OpenAICompatExecutor) Refresh(ctx context.Context, auth *cliproxyauth.Auth) (*cliproxyauth.Auth, error) {
	log.Debugf("openai compat executor: refresh called")
	_ = ctx
	return auth, nil
}

func (e *OpenAICompatExecutor) resolveCredentials(auth *cliproxyauth.Auth) (baseURL, apiKey string) {
	if auth == nil {
		return "", ""
	}
	if auth.Attributes != nil {
		baseURL = strings.TrimSpace(auth.Attributes["base_url"])
		apiKey = strings.TrimSpace(auth.Attributes["api_key"])
	}
	return
}

func (e *OpenAICompatExecutor) resolveCompatConfig(auth *cliproxyauth.Auth) *config.OpenAICompatibility {
	if auth == nil || e.cfg == nil {
		return nil
	}
	candidates := make([]string, 0, 3)
	if auth.Attributes != nil {
		if v := strings.TrimSpace(auth.Attributes["compat_name"]); v != "" {
			candidates = append(candidates, v)
		}
		if v := strings.TrimSpace(auth.Attributes["provider_key"]); v != "" {
			candidates = append(candidates, v)
		}
	}
	if v := strings.TrimSpace(auth.Provider); v != "" {
		candidates = append(candidates, v)
	}
	for i := range e.cfg.OpenAICompatibility {
		compat := &e.cfg.OpenAICompatibility[i]
		for _, candidate := range candidates {
			if candidate != "" && strings.EqualFold(strings.TrimSpace(candidate), compat.Name) {
				return compat
			}
		}
	}
	return nil
}

func (e *OpenAICompatExecutor) overrideModel(payload []byte, model string) []byte {
	if len(payload) == 0 || model == "" {
		return payload
	}
	payload, _ = sjson.SetBytes(payload, "model", model)
	return payload
}

func shouldNormalizeKimiCompatPayload(model string) bool {
	model = strings.ToLower(strings.TrimSpace(thinking.ParseSuffix(model).ModelName))
	return strings.HasPrefix(model, "kimi-") ||
		strings.Contains(model, "/kimi-") ||
		strings.Contains(model, "moonshot")
}

type statusErr struct {
	code               int
	msg                string
	retryAfter         *time.Duration
	upstreamBody       []byte
	quotaWindow        string
	quotaWindowMinutes int
	headers            http.Header
}

func (e statusErr) Error() string {
	if e.msg != "" {
		return e.msg
	}
	return fmt.Sprintf("status %d", e.code)
}
func (e statusErr) StatusCode() int            { return e.code }
func (e statusErr) RetryAfter() *time.Duration { return e.retryAfter }
func (e statusErr) QuotaWindow() (string, int) { return e.quotaWindow, e.quotaWindowMinutes }
func (e statusErr) Headers() http.Header {
	if e.headers == nil {
		return nil
	}
	return e.headers.Clone()
}
func (e statusErr) UpstreamErrorBody() []byte {
	if len(e.upstreamBody) == 0 {
		return nil
	}
	return append([]byte(nil), e.upstreamBody...)
}
