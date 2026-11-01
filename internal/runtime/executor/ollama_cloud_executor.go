package executor

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
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

type OllamaCloudExecutor struct {
	cfg *config.Config
}

const (
	ollamaCloudCapabilityCacheTTL  = 10 * time.Minute
	ollamaCloudImagePlaceholderKey = "ollama_cloud_image_placeholder"
)

type ollamaCloudCapabilityCacheEntry struct {
	supportsVision bool
	expiresAt      time.Time
}

var ollamaCloudCapabilityCache sync.Map

func NewOllamaCloudExecutor(cfg *config.Config) *OllamaCloudExecutor {
	return &OllamaCloudExecutor{cfg: cfg}
}

func (e *OllamaCloudExecutor) Identifier() string { return "ollama-cloud" }

func (e *OllamaCloudExecutor) PrepareRequest(req *http.Request, auth *cliproxyauth.Auth) error {
	if req == nil {
		return nil
	}
	_, apiKey := e.resolveCredentials(auth)
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	var attrs map[string]string
	if auth != nil {
		attrs = auth.Attributes
	}
	util.ApplyCustomHeadersFromAttrs(req, attrs)
	return nil
}

func (e *OllamaCloudExecutor) HttpRequest(ctx context.Context, auth *cliproxyauth.Auth, req *http.Request) (*http.Response, error) {
	if req == nil {
		return nil, fmt.Errorf("ollama cloud executor: request is nil")
	}
	if ctx == nil {
		ctx = req.Context()
	}
	httpReq := req.WithContext(ctx)
	if err := e.PrepareRequest(httpReq, auth); err != nil {
		return nil, err
	}
	return newProxyAwareHTTPClient(ctx, e.cfg, auth, 0).Do(httpReq)
}

func (e *OllamaCloudExecutor) Execute(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	preparedCtx, prepared, err := e.prepareImageInput(ctx, auth, req, opts)
	if err != nil {
		return cliproxyexecutor.Response{}, err
	}
	ctx = withOllamaPromptCacheEstimator(preparedCtx, auth, prepared.Request, opts)
	req = prepared.Request

	var resp cliproxyexecutor.Response
	if opts.Alt != "responses/compact" && (opts.SourceFormat == sdktranslator.FormatOpenAIResponse || opts.SourceFormat == sdktranslator.FormatOpenAI) {
		resp, err = e.executeNativeChat(ctx, auth, req, opts)
	} else if opts.Alt != "responses/compact" && opts.SourceFormat == sdktranslator.FormatClaude {
		resp, err = e.executeDirect(ctx, auth, req, opts, sdktranslator.FormatClaude, "/messages", parseClaudeUsage)
	} else {
		resp, err = e.openAIExecutor().Execute(ctx, e.openAIAuth(auth), req, opts)
	}
	if err != nil || !prepared.Applied {
		return resp, err
	}
	resp.Payload = opencodeGoRewriteFallbackResponseModel(resp.Payload, prepared.OriginalModel)
	return resp, nil
}

func (e *OllamaCloudExecutor) ExecuteStream(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	preparedCtx, prepared, err := e.prepareImageInput(ctx, auth, req, opts)
	if err != nil {
		return nil, err
	}
	ctx = withOllamaPromptCacheEstimator(preparedCtx, auth, prepared.Request, opts)
	req = prepared.Request

	var result *cliproxyexecutor.StreamResult
	if opts.Alt != "responses/compact" && (opts.SourceFormat == sdktranslator.FormatOpenAIResponse || opts.SourceFormat == sdktranslator.FormatOpenAI) {
		result, err = e.executeNativeChatStream(ctx, auth, req, opts)
	} else if opts.Alt != "responses/compact" && opts.SourceFormat == sdktranslator.FormatClaude {
		result, err = e.executeDirectStream(ctx, auth, req, opts, sdktranslator.FormatClaude, "/messages", (&claudeStreamUsage{}).observe)
	} else {
		result, err = e.openAIExecutor().ExecuteStream(ctx, e.openAIAuth(auth), req, opts)
	}
	if err != nil || !prepared.Applied {
		return result, err
	}
	return opencodeGoRewriteFallbackStreamResult(result, prepared.OriginalModel), nil
}

func (e *OllamaCloudExecutor) CountTokens(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return e.openAIExecutor().CountTokens(ctx, e.openAIAuth(auth), req, opts)
}

func (e *OllamaCloudExecutor) Refresh(ctx context.Context, auth *cliproxyauth.Auth) (*cliproxyauth.Auth, error) {
	_ = ctx
	return auth, nil
}

func (e *OllamaCloudExecutor) openAIExecutor() *OpenAICompatExecutor {
	return NewOpenAICompatExecutor(e.Identifier(), e.cfg)
}

func (e *OllamaCloudExecutor) executeDirect(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options, target sdktranslator.Format, endpoint string, parseUsage func([]byte) coreusage.Detail) (resp cliproxyexecutor.Response, err error) {
	execCtx, body, err := e.prepareDirect(ctx, auth, req, opts, target, false)
	if err != nil {
		return resp, err
	}
	reporter := execCtx.Reporter()
	defer reporter.trackFailure(execCtx.Context, &err)

	httpResp, err := e.doJSON(execCtx, auth, endpoint, body, false)
	if err != nil {
		reporter.publishFailureWithContent(execCtx.Context, string(req.Payload), err.Error())
		return resp, err
	}
	defer func() {
		if errClose := httpResp.Body.Close(); errClose != nil {
			log.Errorf("ollama cloud executor: close response body error: %v", errClose)
		}
	}()
	recorder := execCtx.Recorder()
	recorder.RecordResponseMetadata(httpResp.StatusCode, httpResp.Header.Clone())
	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		b := readUpstreamErrorBody(e.Identifier(), httpResp.Body)
		recorder.AppendResponseChunk(b)
		reporter.publishFailureWithContent(execCtx.Context, string(req.Payload), string(b))
		err = statusErr{code: httpResp.StatusCode, msg: string(b), headers: httpResp.Header.Clone()}
		return resp, err
	}
	data, err := readUpstreamResponseBody(e.Identifier(), httpResp.Body)
	if err != nil {
		recorder.RecordResponseError(err)
		return resp, err
	}
	recorder.AppendResponseChunk(data)
	reporter.publishWithContent(execCtx.Context, parseUsage(data), string(req.Payload), string(data))
	reporter.ensurePublished(execCtx.Context)

	var param any
	out := sdktranslator.TranslateNonStream(execCtx.Context, target, execCtx.SourceFormat, req.Model, opts.OriginalRequest, body, data, &param)
	return cliproxyexecutor.Response{Payload: []byte(out), Headers: httpResp.Header.Clone()}, nil
}

func (e *OllamaCloudExecutor) executeDirectStream(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options, target sdktranslator.Format, endpoint string, parseUsage func([]byte) (coreusage.Detail, bool)) (_ *cliproxyexecutor.StreamResult, err error) {
	execCtx, body, err := e.prepareDirect(ctx, auth, req, opts, target, true)
	if err != nil {
		return nil, err
	}
	reporter := execCtx.Reporter()
	defer reporter.trackFailure(execCtx.Context, &err)

	//nolint:bodyclose // success body is consumed and closed by the stream goroutine below.
	httpResp, err := e.doJSON(execCtx, auth, endpoint, body, true)
	if err != nil {
		reporter.publishFailureWithContent(execCtx.Context, string(req.Payload), err.Error())
		return nil, err
	}
	recorder := execCtx.Recorder()
	recorder.RecordResponseMetadata(httpResp.StatusCode, httpResp.Header.Clone())
	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		b := readUpstreamErrorBody(e.Identifier(), httpResp.Body)
		recorder.AppendResponseChunk(b)
		reporter.publishFailureWithContent(execCtx.Context, string(req.Payload), string(b))
		if errClose := httpResp.Body.Close(); errClose != nil {
			log.Errorf("ollama cloud executor: close response body error: %v", errClose)
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
				log.Errorf("ollama cloud executor: close response body error: %v", errClose)
			}
		}()
		scanner := bufio.NewScanner(httpResp.Body)
		scanner.Buffer(nil, 52_428_800)
		var param any
		for scanner.Scan() {
			line := scanner.Bytes()
			recorder.AppendResponseChunk(line)
			reporter.appendOutputChunk(line)
			if detail, ok := parseUsage(line); ok {
				reporter.publish(execCtx.Context, detail)
			}
			if execCtx.SourceFormat == target {
				out <- cliproxyexecutor.StreamChunk{Payload: append(bytes.Clone(line), '\n')}
				continue
			}
			for _, chunk := range sdktranslator.TranslateStream(execCtx.Context, target, execCtx.SourceFormat, req.Model, opts.OriginalRequest, body, bytes.Clone(line), &param) {
				out <- cliproxyexecutor.StreamChunk{Payload: []byte(chunk)}
			}
		}
		if errScan := scanner.Err(); errScan != nil {
			recorder.RecordResponseError(errScan)
			reporter.publishFailure(execCtx.Context)
			out <- cliproxyexecutor.StreamChunk{Err: errScan}
			return
		}
		reporter.ensurePublished(execCtx.Context)
	}()
	return &cliproxyexecutor.StreamResult{Headers: httpResp.Header.Clone(), Chunks: out}, nil
}

func (e *OllamaCloudExecutor) prepareDirect(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options, target sdktranslator.Format, stream bool) (*ExecutionContext, []byte, error) {
	execCtx := newExecutionContext(ctx, e.Identifier(), e.cfg, auth, req, opts, ExecutionOptions{
		TargetFormat:      target,
		TranslateAsStream: stream,
	})
	translated, originalTranslated := execCtx.TranslateRequestPair(req.Payload)
	translated, _ = sjson.SetBytes(translated, "model", execCtx.BaseModel)
	translated = execCtx.ApplyPayloadConfig(translated, originalTranslated)
	updated, err := thinking.ApplyThinking(translated, req.Model, execCtx.SourceFormat.String(), target.String(), e.Identifier())
	if err != nil {
		return nil, nil, err
	}
	updated = applyOllamaCloudImagePolicy(req, updated)
	updated = applyProviderPromptCaching(updated, req.Payload, auth, e.Identifier(), execCtx.BaseModel, target, opts)
	return execCtx, updated, nil
}

func (e *OllamaCloudExecutor) prepareImageInput(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (context.Context, opencodeGoVisionFallbackResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	result := opencodeGoVisionFallbackResult{
		Request:       req,
		OriginalModel: payloadRequestedModel(opts, req.Model),
	}
	if !opencodeGoPayloadHasImage(req.Payload) {
		return ctx, result, nil
	}
	baseModel := thinking.ParseSuffix(req.Model).ModelName

	supportsVision, capabilityErr := e.modelSupportsVision(ctx, auth, baseModel)
	if capabilityErr == nil && supportsVision {
		return ctx, result, nil
	}

	fallbackModel := openAICompatVisionFallbackModel(e.cfg, auth, e.Identifier())
	if fallbackModel != "" && !strings.EqualFold(baseModel, fallbackModel) {
		fallbackSupportsVision, fallbackErr := e.modelSupportsVision(ctx, auth, fallbackModel)
		if fallbackErr == nil && fallbackSupportsVision {
			req.Model = fallbackModel
			req.Payload, _ = sjson.SetBytes(req.Payload, "model", fallbackModel)
			result.Request = req
			result.FallbackModel = fallbackModel
			result.Applied = true
			ctx = contextWithVisionFallbackLog(ctx, result.OriginalModel, baseModel, fallbackModel)
			return ctx, result, nil
		}
	}

	placeholder := "[Image omitted: the selected Ollama model does not support image input. Switch to a vision-capable model to analyze it.]"
	if capabilityErr != nil {
		log.Warnf("ollama cloud executor: could not verify vision capability for %s: %v", baseModel, capabilityErr)
		placeholder = "[Image omitted: CliRelay could not verify that the selected Ollama model supports image input. Switch to a verified vision-capable model to analyze it.]"
	}
	result.Request = withOllamaCloudImagePlaceholder(req, placeholder)
	return ctx, result, nil
}

func (e *OllamaCloudExecutor) modelSupportsVision(ctx context.Context, auth *cliproxyauth.Auth, model string) (bool, error) {
	model = strings.TrimSpace(model)
	if model == "" {
		return false, fmt.Errorf("model is empty")
	}
	baseURL, _ := e.resolveCredentials(auth)
	cacheKey := strings.ToLower(ollamaCloudNativeBaseURL(baseURL) + "\x00" + model)
	if cached, ok := ollamaCloudCapabilityCache.Load(cacheKey); ok {
		entry, _ := cached.(ollamaCloudCapabilityCacheEntry)
		if time.Now().Before(entry.expiresAt) {
			return entry.supportsVision, nil
		}
		ollamaCloudCapabilityCache.Delete(cacheKey)
	}

	body := []byte(`{"model":"","verbose":false}`)
	body, _ = sjson.SetBytes(body, "model", model)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, ollamaCloudNativeBaseURL(baseURL)+"/api/show", bytes.NewReader(body))
	if err != nil {
		return false, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "application/json")
	httpReq.Header.Set("User-Agent", "cli-proxy-ollama-cloud")
	if err = e.PrepareRequest(httpReq, auth); err != nil {
		return false, err
	}

	httpResp, err := newProxyAwareHTTPClient(ctx, e.cfg, auth, 0).Do(httpReq)
	if err != nil {
		return false, err
	}
	defer httpResp.Body.Close()
	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		return false, fmt.Errorf("capability request returned status %d: %s", httpResp.StatusCode, strings.TrimSpace(string(readUpstreamErrorBody(e.Identifier(), httpResp.Body))))
	}
	data, err := readUpstreamResponseBody(e.Identifier(), httpResp.Body)
	if err != nil {
		return false, err
	}
	capabilities := gjson.GetBytes(data, "capabilities")
	if !capabilities.IsArray() {
		return false, fmt.Errorf("capability response has no capabilities array")
	}
	supportsVision := false
	for _, capability := range capabilities.Array() {
		if strings.EqualFold(strings.TrimSpace(capability.String()), "vision") {
			supportsVision = true
			break
		}
	}
	ollamaCloudCapabilityCache.Store(cacheKey, ollamaCloudCapabilityCacheEntry{
		supportsVision: supportsVision,
		expiresAt:      time.Now().Add(ollamaCloudCapabilityCacheTTL),
	})
	return supportsVision, nil
}

func withOllamaCloudImagePlaceholder(req cliproxyexecutor.Request, placeholder string) cliproxyexecutor.Request {
	metadata := make(map[string]any, len(req.Metadata)+1)
	for key, value := range req.Metadata {
		metadata[key] = value
	}
	metadata[ollamaCloudImagePlaceholderKey] = placeholder
	req.Metadata = metadata
	return req
}

func applyOllamaCloudImagePolicy(req cliproxyexecutor.Request, payload []byte) []byte {
	placeholder, _ := req.Metadata[ollamaCloudImagePlaceholderKey].(string)
	placeholder = strings.TrimSpace(placeholder)
	if placeholder == "" {
		return payload
	}
	if updated, changed := sanitizeImageInputs(payload, placeholder, true); changed {
		return updated
	}
	return payload
}

func (e *OllamaCloudExecutor) doJSON(execCtx *ExecutionContext, auth *cliproxyauth.Auth, endpoint string, body []byte, stream bool) (*http.Response, error) {
	baseURL, apiKey := e.resolveCredentials(auth)
	if baseURL == "" {
		return nil, statusErr{code: http.StatusUnauthorized, msg: "missing provider baseURL"}
	}
	url := ollamaCloudOpenAIBaseURL(baseURL) + endpoint
	httpReq, err := http.NewRequestWithContext(execCtx.Context, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("User-Agent", "cli-proxy-ollama-cloud")
	if apiKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+apiKey)
	}
	if endpoint == "/messages" {
		httpReq.Header.Set("Anthropic-Version", "2023-06-01")
	}
	if stream {
		httpReq.Header.Set("Accept", "text/event-stream")
		httpReq.Header.Set("Cache-Control", "no-cache")
	} else {
		httpReq.Header.Set("Accept", "application/json")
	}
	var attrs map[string]string
	if auth != nil {
		attrs = auth.Attributes
	}
	util.ApplyCustomHeadersFromAttrs(httpReq, attrs)
	execCtx.Recorder().RecordRequest(url, http.MethodPost, httpReq.Header.Clone(), body)
	resp, err := execCtx.HTTPClient(0).Do(httpReq) //nolint:bodyclose // stream bodies are closed by the stream goroutine.
	if err != nil {
		execCtx.Recorder().RecordResponseError(err)
	}
	return resp, err
}

func (e *OllamaCloudExecutor) resolveCredentials(auth *cliproxyauth.Auth) (baseURL, apiKey string) {
	baseURL = config.DefaultOllamaCloudBaseURL
	if auth == nil {
		return baseURL, ""
	}
	if auth.Attributes != nil {
		if v := strings.TrimSpace(auth.Attributes["base_url"]); v != "" {
			baseURL = strings.TrimSuffix(v, "/")
		}
		apiKey = strings.TrimSpace(auth.Attributes["api_key"])
	}
	return baseURL, apiKey
}

func (e *OllamaCloudExecutor) openAIAuth(auth *cliproxyauth.Auth) *cliproxyauth.Auth {
	baseURL, _ := e.resolveCredentials(auth)
	if auth == nil {
		return &cliproxyauth.Auth{Attributes: map[string]string{"base_url": ollamaCloudOpenAIBaseURL(baseURL)}}
	}
	clone := *auth
	attrs := make(map[string]string, len(auth.Attributes)+1)
	for k, v := range auth.Attributes {
		attrs[k] = v
	}
	attrs["base_url"] = ollamaCloudOpenAIBaseURL(baseURL)
	clone.Attributes = attrs
	return &clone
}

func ollamaCloudOpenAIBaseURL(baseURL string) string {
	base := strings.TrimSuffix(strings.TrimSpace(baseURL), "/")
	if base == "" {
		base = config.DefaultOllamaCloudBaseURL
	}
	if strings.HasSuffix(strings.ToLower(base), "/v1") {
		return base
	}
	return base + "/v1"
}

func parseOpenAIResponsesStreamUsage(line []byte) (coreusage.Detail, bool) {
	payload := responsesSSEData(line)
	if len(payload) == 0 || !gjson.ValidBytes(payload) {
		return coreusage.Detail{}, false
	}
	if detail, ok := parseCodexUsage(payload); ok {
		return detail, true
	}
	usageNode := gjson.GetBytes(payload, "usage")
	if !usageNode.Exists() {
		return coreusage.Detail{}, false
	}
	return parseOpenAIUsage(payload), true
}
