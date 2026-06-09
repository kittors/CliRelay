package executor

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	codexauth "github.com/router-for-me/CLIProxyAPI/v6/internal/auth/codex"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/thinking"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/util"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v6/sdk/translator"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

var dataTag = []byte("data:")

// CodexExecutor is a stateless executor for Codex (OpenAI Responses API entrypoint).
// If api_key is unavailable on auth, it falls back to legacy via ClientAdapter.
type CodexExecutor struct {
	cfg *config.Config
}

func NewCodexExecutor(cfg *config.Config) *CodexExecutor { return &CodexExecutor{cfg: cfg} }

func (e *CodexExecutor) Identifier() string { return "codex" }

// PrepareRequest injects Codex credentials into the outgoing HTTP request.
func (e *CodexExecutor) PrepareRequest(req *http.Request, auth *cliproxyauth.Auth) error {
	if req == nil {
		return nil
	}
	apiKey, _ := codexCreds(auth)
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

// HttpRequest injects Codex credentials into the request and executes it.
func (e *CodexExecutor) HttpRequest(ctx context.Context, auth *cliproxyauth.Auth, req *http.Request) (*http.Response, error) {
	if req == nil {
		return nil, fmt.Errorf("codex executor: request is nil")
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

func (e *CodexExecutor) ProbeQuotaRecovery(ctx context.Context, auth *cliproxyauth.Auth) (*cliproxyauth.QuotaProbeResult, error) {
	if auth == nil {
		return nil, fmt.Errorf("codex executor: auth is nil")
	}
	accountID := ""
	if auth.Metadata != nil {
		if v, ok := auth.Metadata["account_id"].(string); ok {
			accountID = strings.TrimSpace(v)
		}
	}
	if accountID == "" {
		return nil, fmt.Errorf("codex executor: missing account_id")
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://chatgpt.com/backend-api/wham/usage", nil)
	if err != nil {
		return nil, err
	}
	apiKey, _ := codexCreds(auth)
	applyCodexHeaders(req, e.cfg, auth, apiKey, false)

	httpClient := newProxyAwareHTTPClient(ctx, e.cfg, auth, 0)
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() {
		if errClose := resp.Body.Close(); errClose != nil {
			log.Errorf("codex executor: close quota probe body error: %v", errClose)
		}
	}()

	body, err := readUpstreamResponseBody(e.Identifier(), resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, newCodexStatusErr(resp.StatusCode, body, resp.Header)
	}
	return parseCodexQuotaProbe(body), nil
}

func (e *CodexExecutor) Execute(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (resp cliproxyexecutor.Response, err error) {
	if opts.Alt == codexImageGenerationAlt || opts.Alt == codexImageEditsAlt {
		return e.executeImageGeneration(ctx, auth, req, opts)
	}
	if opts.Alt == "responses/compact" {
		return e.executeCompact(ctx, auth, req, opts)
	}
	execCtx := newExecutionContext(ctx, e.Identifier(), e.cfg, auth, req, opts, ExecutionOptions{
		TargetFormat: sdktranslator.FromString("codex"),
	})
	reporter := execCtx.Reporter()
	defer reporter.trackFailure(execCtx.Context, &err)

	apiKey, baseURL := codexCreds(auth)
	if baseURL == "" {
		baseURL = "https://chatgpt.com/backend-api/codex"
	}

	to := execCtx.Execution.TargetFormat
	body, originalTranslated := execCtx.TranslateRequestPair(req.Payload)

	body, err = thinking.ApplyThinking(body, req.Model, execCtx.SourceFormat.String(), to.String(), e.Identifier())
	if err != nil {
		return resp, err
	}

	body = execCtx.ApplyPayloadConfig(body, originalTranslated)
	body = ensureTranslatedCodexModel(body, execCtx.BaseModel)
	body = sanitizeCodexResponsesRequest(body)
	body, _ = sjson.SetBytes(body, "stream", true)
	body, _ = sjson.DeleteBytes(body, "previous_response_id")
	body, _ = sjson.DeleteBytes(body, "prompt_cache_retention")
	body, _ = sjson.DeleteBytes(body, "safety_identifier")
	if !gjson.GetBytes(body, "instructions").Exists() {
		// Extract system messages from original OpenAI-format payload to use as instructions.
		// This preserves system prompts injected by SystemPromptMiddleware.
		sysContent := extractSystemMessagesAsInstructions(execCtx.Request.Payload)
		body, _ = sjson.SetBytes(body, "instructions", sysContent)
	}

	url := strings.TrimSuffix(baseURL, "/") + "/responses"
	httpReq, err := e.cacheHelper(execCtx.Context, execCtx.SourceFormat, url, req, body)
	if err != nil {
		return resp, err
	}
	applyCodexHeaders(httpReq, e.cfg, auth, apiKey, true)
	recorder := execCtx.Recorder()
	recorder.RecordRequest(url, http.MethodPost, httpReq.Header.Clone(), body)
	httpClient := execCtx.HTTPClient(0)
	httpResp, err := httpClient.Do(httpReq)
	if err != nil {
		recorder.RecordResponseError(err)
		reporter.publishFailureWithContent(execCtx.Context, string(req.Payload), err.Error())
		return resp, err
	}
	defer func() {
		if errClose := httpResp.Body.Close(); errClose != nil {
			log.Errorf("codex executor: close response body error: %v", errClose)
		}
	}()
	recorder.RecordResponseMetadata(httpResp.StatusCode, httpResp.Header.Clone())
	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		b := readUpstreamErrorBody(e.Identifier(), httpResp.Body)
		recorder.AppendResponseChunk(b)
		logWithRequestID(execCtx.Context).Debugf("request error, error status: %d, error message: %s", httpResp.StatusCode, summarizeErrorBody(httpResp.Header.Get("Content-Type"), b))
		reporter.publishFailureWithContent(execCtx.Context, string(req.Payload), string(b))
		err = newCodexStatusErr(httpResp.StatusCode, b, httpResp.Header)
		return resp, err
	}
	data, err := readUpstreamResponseBody(e.Identifier(), httpResp.Body)
	if err != nil {
		recorder.RecordResponseError(err)
		return resp, err
	}
	recorder.AppendResponseChunk(data)

	lines := bytes.Split(data, []byte("\n"))
	pendingOutputItems := make([][]byte, 0, 1)
	pendingOutputKeys := make([]string, 0, 1)
	pendingSeen := make(map[string]struct{})
	for _, line := range lines {
		if !bytes.HasPrefix(line, dataTag) {
			continue
		}

		line = bytes.TrimSpace(line[5:])
		if item, key, ok := extractCodexResponsesOutputItemDone(line); ok {
			if _, exists := pendingSeen[key]; !exists {
				pendingSeen[key] = struct{}{}
				pendingOutputItems = append(pendingOutputItems, item)
				pendingOutputKeys = append(pendingOutputKeys, key)
			}
			continue
		}
		if gjson.GetBytes(line, "type").String() != "response.completed" {
			continue
		}
		line = mergeCodexResponsesCompletedOutput(line, pendingOutputItems, pendingOutputKeys)

		if detail, ok := parseCodexUsage(line); ok {
			reporter.publishWithContent(execCtx.Context, detail, string(req.Payload), string(data))
		}

		var param any
		out := sdktranslator.TranslateNonStream(execCtx.Context, to, execCtx.SourceFormat, req.Model, execCtx.OriginalPayload, body, line, &param)
		resp = cliproxyexecutor.Response{Payload: []byte(out), Headers: httpResp.Header.Clone()}
		return resp, nil
	}
	err = statusErr{code: 408, msg: "stream error: stream disconnected before completion: stream closed before response.completed"}
	return resp, err
}

func (e *CodexExecutor) executeCompact(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (resp cliproxyexecutor.Response, err error) {
	execCtx := newExecutionContext(ctx, e.Identifier(), e.cfg, auth, req, opts, ExecutionOptions{
		TargetFormat: sdktranslator.FromString("openai-response"),
	})
	reporter := execCtx.Reporter()
	defer reporter.trackFailure(execCtx.Context, &err)

	apiKey, baseURL := codexCreds(auth)
	if baseURL == "" {
		baseURL = "https://chatgpt.com/backend-api/codex"
	}

	to := execCtx.Execution.TargetFormat
	body, originalTranslated := execCtx.TranslateRequestPair(req.Payload)

	body, err = thinking.ApplyThinking(body, req.Model, execCtx.SourceFormat.String(), to.String(), e.Identifier())
	if err != nil {
		return resp, err
	}

	body = execCtx.ApplyPayloadConfig(body, originalTranslated)
	body = ensureTranslatedCodexModel(body, execCtx.BaseModel)
	body = sanitizeCodexResponsesRequest(body)
	body, _ = sjson.DeleteBytes(body, "stream")

	url := strings.TrimSuffix(baseURL, "/") + "/responses/compact"
	httpReq, err := e.cacheHelper(execCtx.Context, execCtx.SourceFormat, url, req, body)
	if err != nil {
		return resp, err
	}
	applyCodexHeaders(httpReq, e.cfg, auth, apiKey, false)
	recorder := execCtx.Recorder()
	recorder.RecordRequest(url, http.MethodPost, httpReq.Header.Clone(), body)
	httpClient := execCtx.HTTPClient(0)
	httpResp, err := httpClient.Do(httpReq)
	if err != nil {
		recorder.RecordResponseError(err)
		reporter.publishFailureWithContent(execCtx.Context, string(req.Payload), err.Error())
		return resp, err
	}
	defer func() {
		if errClose := httpResp.Body.Close(); errClose != nil {
			log.Errorf("codex executor: close response body error: %v", errClose)
		}
	}()
	recorder.RecordResponseMetadata(httpResp.StatusCode, httpResp.Header.Clone())
	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		b := readUpstreamErrorBody(e.Identifier(), httpResp.Body)
		recorder.AppendResponseChunk(b)
		logWithRequestID(execCtx.Context).Debugf("request error, error status: %d, error message: %s", httpResp.StatusCode, summarizeErrorBody(httpResp.Header.Get("Content-Type"), b))
		reporter.publishFailureWithContent(execCtx.Context, string(req.Payload), string(b))
		err = newCodexStatusErr(httpResp.StatusCode, b, httpResp.Header)
		return resp, err
	}
	data, err := readUpstreamResponseBody(e.Identifier(), httpResp.Body)
	if err != nil {
		recorder.RecordResponseError(err)
		return resp, err
	}
	recorder.AppendResponseChunk(data)
	reporter.publishWithContent(execCtx.Context, parseOpenAIUsage(data), string(req.Payload), string(data))
	reporter.ensurePublished(execCtx.Context)
	var param any
	out := sdktranslator.TranslateNonStream(execCtx.Context, to, execCtx.SourceFormat, req.Model, execCtx.OriginalPayload, body, data, &param)
	resp = cliproxyexecutor.Response{Payload: []byte(out), Headers: httpResp.Header.Clone()}
	return resp, nil
}

func (e *CodexExecutor) ExecuteStream(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (_ *cliproxyexecutor.StreamResult, err error) {
	if opts.Alt == "responses/compact" {
		return nil, statusErr{code: http.StatusBadRequest, msg: "streaming not supported for /responses/compact"}
	}
	execCtx := newExecutionContext(ctx, e.Identifier(), e.cfg, auth, req, opts, ExecutionOptions{
		TargetFormat:      sdktranslator.FromString("codex"),
		TranslateAsStream: true,
	})
	reporter := execCtx.Reporter()
	defer reporter.trackFailure(execCtx.Context, &err)

	apiKey, baseURL := codexCreds(auth)
	if baseURL == "" {
		baseURL = "https://chatgpt.com/backend-api/codex"
	}

	to := execCtx.Execution.TargetFormat
	body, originalTranslated := execCtx.TranslateRequestPair(req.Payload)

	body, err = thinking.ApplyThinking(body, req.Model, execCtx.SourceFormat.String(), to.String(), e.Identifier())
	if err != nil {
		return nil, err
	}

	body = execCtx.ApplyPayloadConfig(body, originalTranslated)
	body = sanitizeCodexResponsesRequest(body)
	body, _ = sjson.DeleteBytes(body, "previous_response_id")
	body, _ = sjson.DeleteBytes(body, "prompt_cache_retention")
	body, _ = sjson.DeleteBytes(body, "safety_identifier")
	body = ensureTranslatedCodexModel(body, execCtx.BaseModel)
	if !gjson.GetBytes(body, "instructions").Exists() {
		sysContent := extractSystemMessagesAsInstructions(execCtx.Request.Payload)
		body, _ = sjson.SetBytes(body, "instructions", sysContent)
	}

	url := strings.TrimSuffix(baseURL, "/") + "/responses"
	httpReq, err := e.cacheHelper(execCtx.Context, execCtx.SourceFormat, url, req, body)
	if err != nil {
		return nil, err
	}
	applyCodexHeaders(httpReq, e.cfg, auth, apiKey, true)
	recorder := execCtx.Recorder()
	recorder.RecordRequest(url, http.MethodPost, httpReq.Header.Clone(), body)
	httpClient := execCtx.HTTPClient(0)
	httpResp, err := httpClient.Do(httpReq)
	if err != nil {
		recorder.RecordResponseError(err)
		reporter.publishFailureWithContent(execCtx.Context, string(req.Payload), err.Error())
		return nil, err
	}
	recorder.RecordResponseMetadata(httpResp.StatusCode, httpResp.Header.Clone())
	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		data, readErr := readUpstreamResponseBody(e.Identifier(), httpResp.Body)
		if errClose := httpResp.Body.Close(); errClose != nil {
			log.Errorf("codex executor: close response body error: %v", errClose)
		}
		if readErr != nil {
			recorder.RecordResponseError(readErr)
			return nil, readErr
		}
		recorder.AppendResponseChunk(data)
		logWithRequestID(execCtx.Context).Debugf("request error, error status: %d, error message: %s", httpResp.StatusCode, summarizeErrorBody(httpResp.Header.Get("Content-Type"), data))
		reporter.publishFailureWithContent(execCtx.Context, string(req.Payload), string(data))
		err = newCodexStatusErr(httpResp.StatusCode, data, httpResp.Header)
		return nil, err
	}
	out := make(chan cliproxyexecutor.StreamChunk)
	reporter.setInputContent(string(req.Payload))
	go func() {
		defer close(out)
		defer func() {
			if errClose := httpResp.Body.Close(); errClose != nil {
				log.Errorf("codex executor: close response body error: %v", errClose)
			}
		}()
		scanner := bufio.NewScanner(httpResp.Body)
		scanner.Buffer(nil, 52_428_800) // 50MB
		var param any
		for scanner.Scan() {
			line := scanner.Bytes()
			recorder.AppendResponseChunk(line)
			reporter.appendOutputChunk(line)

			if bytes.HasPrefix(line, dataTag) {
				data := bytes.TrimSpace(line[5:])
				if gjson.GetBytes(data, "type").String() == "response.completed" {
					if detail, ok := parseCodexUsage(data); ok {
						reporter.publish(execCtx.Context, detail)
					}
				}
			}

			chunks := sdktranslator.TranslateStream(execCtx.Context, to, execCtx.SourceFormat, req.Model, execCtx.OriginalPayload, body, bytes.Clone(line), &param)
			for i := range chunks {
				out <- cliproxyexecutor.StreamChunk{Payload: []byte(chunks[i])}
			}
		}
		if errScan := scanner.Err(); errScan != nil {
			if shouldSuppressUsageFailure(errScan, "") {
				out <- cliproxyexecutor.StreamChunk{Err: errScan}
				return
			}
			recorder.RecordResponseError(errScan)
			reporter.publishFailure(execCtx.Context)
			out <- cliproxyexecutor.StreamChunk{Err: errScan}
		}
	}()
	return &cliproxyexecutor.StreamResult{Headers: httpResp.Header.Clone(), Chunks: out}, nil
}

func (e *CodexExecutor) CountTokens(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	execCtx := newExecutionContext(ctx, e.Identifier(), e.cfg, auth, req, opts, ExecutionOptions{
		TargetFormat: sdktranslator.FromString("codex"),
	})
	to := execCtx.Execution.TargetFormat
	body, _ := execCtx.TranslateRequestPair(req.Payload)

	body, err := thinking.ApplyThinking(body, req.Model, execCtx.SourceFormat.String(), to.String(), e.Identifier())
	if err != nil {
		return cliproxyexecutor.Response{}, err
	}

	body = ensureTranslatedCodexModel(body, execCtx.BaseModel)
	body = sanitizeCodexResponsesRequest(body)
	body, _ = sjson.DeleteBytes(body, "previous_response_id")
	body, _ = sjson.DeleteBytes(body, "prompt_cache_retention")
	body, _ = sjson.DeleteBytes(body, "safety_identifier")
	body, _ = sjson.SetBytes(body, "stream", false)
	if !gjson.GetBytes(body, "instructions").Exists() {
		sysContent := extractSystemMessagesAsInstructions(execCtx.Request.Payload)
		body, _ = sjson.SetBytes(body, "instructions", sysContent)
	}

	enc, err := tokenizerForCodexModel(execCtx.BaseModel)
	if err != nil {
		return cliproxyexecutor.Response{}, fmt.Errorf("codex executor: tokenizer init failed: %w", err)
	}

	count, err := countCodexInputTokens(enc, body)
	if err != nil {
		return cliproxyexecutor.Response{}, fmt.Errorf("codex executor: token counting failed: %w", err)
	}

	usageJSON := fmt.Sprintf(`{"response":{"usage":{"input_tokens":%d,"output_tokens":0,"total_tokens":%d}}}`, count, count)
	translated := sdktranslator.TranslateTokenCount(execCtx.Context, to, execCtx.SourceFormat, count, []byte(usageJSON))
	return cliproxyexecutor.Response{Payload: []byte(translated)}, nil
}

func (e *CodexExecutor) Refresh(ctx context.Context, auth *cliproxyauth.Auth) (*cliproxyauth.Auth, error) {
	log.Debugf("codex executor: refresh called")
	if auth == nil {
		return nil, statusErr{code: 500, msg: "codex executor: auth is nil"}
	}
	var refreshToken string
	if auth.Metadata != nil {
		if v, ok := auth.Metadata["refresh_token"].(string); ok && v != "" {
			refreshToken = v
		}
	}
	if refreshToken == "" {
		return auth, nil
	}
	svc := codexauth.NewCodexAuth(e.cfg)
	td, err := svc.RefreshTokensWithRetry(ctx, refreshToken, 3)
	if err != nil {
		return nil, err
	}
	if auth.Metadata == nil {
		auth.Metadata = make(map[string]any)
	}
	auth.Metadata["id_token"] = td.IDToken
	auth.Metadata["access_token"] = td.AccessToken
	if td.RefreshToken != "" {
		auth.Metadata["refresh_token"] = td.RefreshToken
	}
	if td.AccountID != "" {
		auth.Metadata["account_id"] = td.AccountID
	}
	auth.Metadata["email"] = td.Email
	// Use unified key in files
	auth.Metadata["expired"] = td.Expire
	auth.Metadata["type"] = "codex"
	now := time.Now().Format(time.RFC3339)
	auth.Metadata["last_refresh"] = now
	return auth, nil
}

func (e *CodexExecutor) cacheHelper(ctx context.Context, from sdktranslator.Format, url string, req cliproxyexecutor.Request, rawJSON []byte) (*http.Request, error) {
	var cache codexCache
	if from == "claude" {
		userIDResult := gjson.GetBytes(req.Payload, "metadata.user_id")
		if userIDResult.Exists() {
			key := fmt.Sprintf("%s-%s", req.Model, userIDResult.String())
			var ok bool
			if cache, ok = getCodexCache(key); !ok {
				cache = codexCache{
					ID:     uuid.New().String(),
					Expire: time.Now().Add(1 * time.Hour),
				}
				setCodexCache(key, cache)
			}
		}
	} else if from == "openai-response" {
		promptCacheKey := gjson.GetBytes(req.Payload, "prompt_cache_key")
		if promptCacheKey.Exists() {
			cache.ID = promptCacheKey.String()
		}
	}

	if cache.ID != "" {
		rawJSON, _ = sjson.SetBytes(rawJSON, "prompt_cache_key", cache.ID)
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(rawJSON))
	if err != nil {
		return nil, err
	}
	if cache.ID != "" {
		httpReq.Header.Set("Conversation_id", cache.ID)
		httpReq.Header.Set("Session_id", cache.ID)
	}
	return httpReq, nil
}
