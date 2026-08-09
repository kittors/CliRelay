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
	if errAdmission := enforceCodexClientAdmissionForRequest(ctx, e.cfg, auth, httpReq); errAdmission != nil {
		return nil, errAdmission
	}
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
	req, opts = maybeStripCodexHistoryDataURLImagesOnRequest(req, opts)
	execCtx := newExecutionContext(ctx, e.Identifier(), e.cfg, auth, req, opts, ExecutionOptions{
		TargetFormat: sdktranslator.FromString("codex"),
	})
	reporter := execCtx.Reporter()
	defer reporter.trackFailure(execCtx.Context, &err)

	if errAdmission := enforceCodexClientAdmission(execCtx.Context, e.cfg, auth); errAdmission != nil {
		return resp, errAdmission
	}

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
	sets := map[string][]byte{"stream": util.JSONBool(true)}
	if !gjson.GetBytes(body, "instructions").Exists() {
		// Extract system messages from original OpenAI-format payload to use as instructions.
		// This preserves system prompts injected by SystemPromptMiddleware.
		sets["instructions"] = util.JSONString(extractSystemMessagesAsInstructions(execCtx.Request.Payload))
	}
	body = util.MutateTopLevelObject(body, sets, []string{
		"previous_response_id",
		"prompt_cache_retention",
		"safety_identifier",
	})
	body = maybeEnsureCodexImageGenerationTool(body, auth, execCtx.BaseModel, codexAdmissionHeadersFromContext(execCtx.Context))

	url := strings.TrimSuffix(baseURL, "/") + "/responses"
	httpReq, err := e.cacheHelper(execCtx.Context, auth, execCtx.SourceFormat, url, req, body)
	if err != nil {
		return resp, err
	}
	applyCodexHeaders(httpReq, e.cfg, auth, apiKey, true)
	recorder := execCtx.Recorder()
	recorder.RecordRequest(url, http.MethodPost, httpReq.Header.Clone(), body)
	httpClient := execCtx.HTTPClient(0)
	httpResp, err := httpClient.Do(httpReq) //nolint:bodyclose // body is closed by the defer below.
	if err != nil {
		recorder.RecordResponseError(err)
		reporter.publishFailureWithContentBytes(execCtx.Context, req.Payload, err.Error())
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
		reporter.publishFailureWithContentBytes(execCtx.Context, req.Payload, string(b))
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
	var streamErr error
	for _, line := range lines {
		if !bytes.HasPrefix(line, dataTag) {
			continue
		}

		line = bytes.TrimSpace(line[5:])
		switch gjson.GetBytes(line, "type").String() {
		case "response.failed", "error":
			streamErr = codexResponsesFailedStatusErr(line)
			continue
		}
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
		// Non-stream: still rewrite /mnt/data markdown when hosted image results exist.
		imageStream := newCodexImageStreamNormalizer()
		for _, item := range pendingOutputItems {
			imageStream.observe(item)
		}
		imageStream.observe(line)
		line = normalizeCodexImageGenerationCallStatus(line)
		line = imageStream.rewriteAssistantMarkdown(line)

		if detail, ok := parseCodexUsage(line); ok {
			reporter.publishWithContentBytes(execCtx.Context, detail, req.Payload, string(data))
		} else {
			reporter.setInputContentBytes(req.Payload)
			reporter.appendOutputChunk(data)
			reporter.ensurePublished(execCtx.Context)
		}

		var param any
		out := sdktranslator.TranslateNonStream(execCtx.Context, to, execCtx.SourceFormat, req.Model, execCtx.OriginalPayload, body, line, &param)
		resp = cliproxyexecutor.Response{Payload: []byte(out), Headers: httpResp.Header.Clone()}
		return resp, nil
	}
	if streamErr != nil {
		reporter.publishFailureWithContentBytes(execCtx.Context, req.Payload, streamErr.Error())
		return resp, streamErr
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

	if errAdmission := enforceCodexClientAdmission(execCtx.Context, e.cfg, auth); errAdmission != nil {
		return resp, errAdmission
	}

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
	httpReq, err := e.cacheHelper(execCtx.Context, auth, execCtx.SourceFormat, url, req, body)
	if err != nil {
		return resp, err
	}
	applyCodexHeaders(httpReq, e.cfg, auth, apiKey, false)
	recorder := execCtx.Recorder()
	recorder.RecordRequest(url, http.MethodPost, httpReq.Header.Clone(), body)
	httpClient := execCtx.HTTPClient(0)
	//nolint:bodyclose // body is closed by the defer below.
	httpResp, err := httpClient.Do(httpReq)
	if err != nil {
		recorder.RecordResponseError(err)
		reporter.publishFailureWithContentBytes(execCtx.Context, req.Payload, err.Error())
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
		reporter.publishFailureWithContentBytes(execCtx.Context, req.Payload, string(b))
		err = newCodexStatusErr(httpResp.StatusCode, b, httpResp.Header)
		return resp, err
	}
	data, err := readUpstreamResponseBody(e.Identifier(), httpResp.Body)
	if err != nil {
		recorder.RecordResponseError(err)
		return resp, err
	}
	recorder.AppendResponseChunk(data)
	reporter.publishWithContentBytes(execCtx.Context, parseOpenAIUsage(data), req.Payload, string(data))
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
	// Shrink multi-MB Desktop history data URLs before translation/sanitize so later
	// body-level passes never see the full base64 history.
	req, opts = maybeStripCodexHistoryDataURLImagesOnRequest(req, opts)
	execCtx := newExecutionContext(ctx, e.Identifier(), e.cfg, auth, req, opts, ExecutionOptions{
		TargetFormat:      sdktranslator.FromString("codex"),
		TranslateAsStream: true,
	})
	reporter := execCtx.Reporter()
	defer reporter.trackFailure(execCtx.Context, &err)

	if errAdmission := enforceCodexClientAdmission(execCtx.Context, e.cfg, auth); errAdmission != nil {
		return nil, errAdmission
	}

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
	sets := map[string][]byte{}
	if !gjson.GetBytes(body, "instructions").Exists() {
		sets["instructions"] = util.JSONString(extractSystemMessagesAsInstructions(execCtx.Request.Payload))
	}
	body = util.MutateTopLevelObject(body, sets, []string{
		"previous_response_id",
		"prompt_cache_retention",
		"safety_identifier",
	})
	body = ensureTranslatedCodexModel(body, execCtx.BaseModel)
	body = maybeEnsureCodexImageGenerationTool(body, auth, execCtx.BaseModel, codexAdmissionHeadersFromContext(execCtx.Context))

	url := strings.TrimSuffix(baseURL, "/") + "/responses"
	httpReq, err := e.cacheHelper(execCtx.Context, auth, execCtx.SourceFormat, url, req, body)
	if err != nil {
		return nil, err
	}
	applyCodexHeaders(httpReq, e.cfg, auth, apiKey, true)
	recorder := execCtx.Recorder()
	recorder.RecordRequest(url, http.MethodPost, httpReq.Header.Clone(), body)
	httpClient := execCtx.HTTPClient(0)
	//nolint:bodyclose // success body is consumed and closed by the stream goroutine below.
	httpResp, err := httpClient.Do(httpReq)
	if err != nil {
		recorder.RecordResponseError(err)
		reporter.publishFailureWithContentBytes(execCtx.Context, req.Payload, err.Error())
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
		reporter.publishFailureWithContentBytes(execCtx.Context, req.Payload, string(data))
		err = newCodexStatusErr(httpResp.StatusCode, data, httpResp.Header)
		return nil, err
	}
	out := make(chan cliproxyexecutor.StreamChunk)
	reporter.setInputContentBytes(req.Payload)
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
		completed := false
		imageStream := newCodexImageStreamNormalizer()
		for scanner.Scan() {
			line := scanner.Bytes()
			normalizedEvents := normalizeCodexImageGenerationOutboundEventWithState(imageStream, bytes.Clone(line))
			if len(normalizedEvents) == 0 {
				normalizedEvents = [][]byte{bytes.Clone(line)}
			}

			var terminalErr error
			for _, eventLine := range normalizedEvents {
				recorder.AppendResponseChunk(eventLine)
				reporter.appendOutputChunk(eventLine)

				if bytes.HasPrefix(eventLine, dataTag) {
					data := bytes.TrimSpace(eventLine[len(dataTag):])
					switch gjson.GetBytes(data, "type").String() {
					case "response.completed", "response.done":
						completed = true
						if detail, ok := parseCodexUsage(data); ok {
							reporter.publish(execCtx.Context, detail)
						}
					case "response.failed", "error":
						terminalErr = codexResponsesFailedStatusErr(data)
					}
				}

				chunks := sdktranslator.TranslateStream(execCtx.Context, to, execCtx.SourceFormat, req.Model, execCtx.OriginalPayload, body, eventLine, &param)
				for i := range chunks {
					out <- cliproxyexecutor.StreamChunk{Payload: []byte(chunks[i])}
				}
			}
			if terminalErr != nil {
				recorder.RecordResponseError(terminalErr)
				reporter.publishFailure(execCtx.Context)
				out <- cliproxyexecutor.StreamChunk{Err: terminalErr}
				return
			}
		}
		if errScan := scanner.Err(); errScan != nil {
			recorder.RecordResponseError(errScan)
			reporter.publishFailure(execCtx.Context)
			out <- cliproxyexecutor.StreamChunk{Err: errScan}
			return
		}
		if completed {
			reporter.ensurePublished(execCtx.Context)
			return
		}
		streamErr := newCodexResponsesIncompleteError()
		recorder.RecordResponseError(streamErr)
		reporter.publishFailure(execCtx.Context)
		out <- cliproxyexecutor.StreamChunk{Err: streamErr}
	}()
	return &cliproxyexecutor.StreamResult{Headers: httpResp.Header.Clone(), Chunks: out}, nil
}

func newCodexResponsesIncompleteError() *cliproxyauth.Error {
	return &cliproxyauth.Error{
		Code:       "response_stream_incomplete",
		Message:    "upstream responses stream closed before response.completed",
		Retryable:  true,
		HTTPStatus: http.StatusBadGateway,
	}
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
	sets := map[string][]byte{"stream": util.JSONBool(false)}
	if !gjson.GetBytes(body, "instructions").Exists() {
		sets["instructions"] = util.JSONString(extractSystemMessagesAsInstructions(execCtx.Request.Payload))
	}
	body = util.MutateTopLevelObject(body, sets, []string{
		"previous_response_id",
		"prompt_cache_retention",
		"safety_identifier",
	})

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

// maybeStripCodexHistoryDataURLImagesOnRequest shrinks multi-MB Desktop history
// data:image payloads before translation. Only openai-response payloads carry
// that history shape; other source formats are left untouched.
func maybeStripCodexHistoryDataURLImagesOnRequest(req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Request, cliproxyexecutor.Options) {
	if opts.SourceFormat != sdktranslator.FormatOpenAIResponse {
		return req, opts
	}
	if len(req.Payload) == 0 {
		return req, opts
	}
	stripped := stripCodexHistoryDataURLImages(req.Payload)
	if samePayloadStorage(stripped, req.Payload) {
		return req, opts
	}
	// Keep Payload and OriginalRequest in lockstep when they shared storage.
	if samePayloadStorage(opts.OriginalRequest, req.Payload) || len(opts.OriginalRequest) == 0 {
		opts.OriginalRequest = stripped
	}
	req.Payload = stripped
	return req, opts
}

func (e *CodexExecutor) cacheHelper(ctx context.Context, auth *cliproxyauth.Auth, from sdktranslator.Format, url string, req cliproxyexecutor.Request, rawJSON []byte) (*http.Request, error) {
	var cache codexCache
	if from == "claude" {
		userIDResult := gjson.GetBytes(req.Payload, "metadata.user_id")
		if userIDResult.Exists() {
			key := codexPromptCacheMapKey(auth, req.Model, userIDResult.String())
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
			cache.ID = codexAccountScopedExplicitSessionID(auth, promptCacheKey.String())
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
