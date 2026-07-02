package executor

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	claudeauth "github.com/router-for-me/CLIProxyAPI/v6/internal/auth/claude"
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

// ClaudeExecutor is a stateless executor for Anthropic Claude over the messages API.
// If api_key is unavailable on auth, it falls back to legacy via ClientAdapter.
type ClaudeExecutor struct {
	cfg *config.Config
}

const claudeToolPrefix = "proxy_"

func NewClaudeExecutor(cfg *config.Config) *ClaudeExecutor { return &ClaudeExecutor{cfg: cfg} }

func (e *ClaudeExecutor) Identifier() string { return "claude" }

// PrepareRequest injects Claude credentials into the outgoing HTTP request.
func (e *ClaudeExecutor) PrepareRequest(req *http.Request, auth *cliproxyauth.Auth) error {
	if req == nil {
		return nil
	}
	apiKey, _ := claudeCreds(auth)
	if strings.TrimSpace(apiKey) == "" {
		return nil
	}
	useAPIKey := auth != nil && auth.Attributes != nil && strings.TrimSpace(auth.Attributes["api_key"]) != ""
	isAnthropicBase := req.URL != nil && strings.EqualFold(req.URL.Scheme, "https") && strings.EqualFold(req.URL.Host, "api.anthropic.com")
	if isAnthropicBase && useAPIKey {
		req.Header.Del("Authorization")
		req.Header.Set("x-api-key", apiKey)
	} else {
		req.Header.Del("x-api-key")
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	var attrs map[string]string
	if auth != nil {
		attrs = auth.Attributes
	}
	util.ApplyCustomHeadersFromAttrs(req, attrs)
	return nil
}

// HttpRequest injects Claude credentials into the request and executes it.
func (e *ClaudeExecutor) HttpRequest(ctx context.Context, auth *cliproxyauth.Auth, req *http.Request) (*http.Response, error) {
	if req == nil {
		return nil, fmt.Errorf("claude executor: request is nil")
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

func (e *ClaudeExecutor) Execute(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (resp cliproxyexecutor.Response, err error) {
	if opts.Alt == "responses/compact" {
		return resp, statusErr{code: http.StatusNotImplemented, msg: "/responses/compact not supported"}
	}
	apiKey, baseURL := claudeCreds(auth)
	if baseURL == "" {
		baseURL = "https://api.anthropic.com"
	}
	skipAnthropic := auth != nil && auth.Attributes != nil && auth.Attributes["skip_anthropic_processing"] == "true"

	to := sdktranslator.FromString("claude")
	// Use streaming translation to preserve function calling, except for claude.
	stream := opts.SourceFormat != to
	execCtx := newExecutionContext(ctx, e.Identifier(), e.cfg, auth, req, opts, ExecutionOptions{
		TargetFormat:      to,
		TranslateAsStream: stream,
	})
	reporter := execCtx.Reporter()
	defer reporter.trackFailure(execCtx.Context, &err)
	body, originalTranslated := execCtx.TranslateRequestPair(req.Payload)
	body, _ = sjson.SetBytes(body, "model", execCtx.BaseModel)

	body, err = thinking.ApplyThinking(body, req.Model, execCtx.SourceFormat.String(), to.String(), e.Identifier())
	if err != nil {
		return resp, err
	}

	claudeFP, claudeFPEnabled := claudeIdentityFingerprint(e.cfg, auth, execCtx.Context)
	claudeFPSessionID := ""
	// Apply cloaking (system prompt injection, fake user ID, sensitive word obfuscation)
	// based on client type and configuration.
	// Skipped when skip_anthropic_processing is enabled (third-party Claude-compatible APIs).
	if !skipAnthropic {
		if claudeFPEnabled {
			claudeFPSessionID = claudeFingerprintSessionID(claudeFP)
			body = applyClaudeIdentityFingerprintPayload(auth, body, claudeFP, claudeFPSessionID)
		} else {
			body = applyCloaking(execCtx.Context, e.cfg, auth, body, execCtx.BaseModel, apiKey)
		}
	}

	body = execCtx.ApplyPayloadConfig(body, originalTranslated)

	if !skipAnthropic {
		// Disable thinking if tool_choice forces tool use (Anthropic API constraint)
		body = disableThinkingIfToolChoiceForced(body)

		// Auto-inject cache_control if missing (optimization for ClawdBot/clients without caching support)
		if countCacheControls(body) == 0 {
			body = ensureCacheControl(body)
		}
	}

	// Extract betas from body and convert to header
	var extraBetas []string
	extraBetas, body = extractAndRemoveBetas(body)
	bodyForTranslation := body
	bodyForUpstream := body
	if !skipAnthropic && isClaudeOAuthToken(apiKey) && !auth.ToolPrefixDisabled() {
		bodyForUpstream = applyClaudeToolPrefix(body, claudeToolPrefix)
	}

	url := fmt.Sprintf("%s/v1/messages?beta=true", baseURL)
	httpReq, err := http.NewRequestWithContext(execCtx.Context, http.MethodPost, url, bytes.NewReader(bodyForUpstream))
	if err != nil {
		return resp, err
	}
	applyClaudeHeaders(httpReq, auth, apiKey, false, extraBetas, e.cfg, claudeFP, claudeFPEnabled, claudeFPSessionID)
	recorder := execCtx.Recorder()
	recorder.RecordRequest(url, http.MethodPost, httpReq.Header.Clone(), bodyForUpstream)

	httpClient := execCtx.HTTPClient(0)
	httpResp, err := httpClient.Do(httpReq)
	if err != nil {
		recorder.RecordResponseError(err)
		reporter.publishFailureWithContent(execCtx.Context, string(req.Payload), err.Error())
		return resp, err
	}
	recorder.RecordResponseMetadata(httpResp.StatusCode, httpResp.Header.Clone())
	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		b := readUpstreamErrorBody(e.Identifier(), httpResp.Body)
		recorder.AppendResponseChunk(b)
		logWithRequestID(execCtx.Context).Debugf("request error, error status: %d, error message: %s", httpResp.StatusCode, summarizeErrorBody(httpResp.Header.Get("Content-Type"), b))
		reporter.publishFailureWithContent(execCtx.Context, string(req.Payload), string(b))
		err = statusErr{code: httpResp.StatusCode, msg: string(b), headers: httpResp.Header.Clone()}
		if errClose := httpResp.Body.Close(); errClose != nil {
			log.Errorf("response body close error: %v", errClose)
		}
		return resp, err
	}
	decodedBody, err := decodeResponseBody(httpResp.Body, httpResp.Header.Get("Content-Encoding"))
	if err != nil {
		recorder.RecordResponseError(err)
		if errClose := httpResp.Body.Close(); errClose != nil {
			log.Errorf("response body close error: %v", errClose)
		}
		return resp, err
	}
	defer func() {
		if errClose := decodedBody.Close(); errClose != nil {
			log.Errorf("response body close error: %v", errClose)
		}
	}()
	data, err := readUpstreamResponseBody(e.Identifier(), decodedBody)
	if err != nil {
		recorder.RecordResponseError(err)
		return resp, err
	}
	recorder.AppendResponseChunk(data)
	if stream {
		lines := bytes.Split(data, []byte("\n"))
		for _, line := range lines {
			if detail, ok := parseClaudeStreamUsage(line); ok {
				reporter.publish(execCtx.Context, detail)
			}
		}
	} else {
		reporter.publishWithContent(execCtx.Context, parseClaudeUsage(data), string(req.Payload), string(data))
	}
	if !skipAnthropic && isClaudeOAuthToken(apiKey) && !auth.ToolPrefixDisabled() {
		data = stripClaudeToolPrefixFromResponse(data, claudeToolPrefix)
	}
	var param any
	out := sdktranslator.TranslateNonStream(
		execCtx.Context,
		to,
		execCtx.SourceFormat,
		req.Model,
		opts.OriginalRequest,
		bodyForTranslation,
		data,
		&param,
	)
	resp = cliproxyexecutor.Response{Payload: []byte(out), Headers: httpResp.Header.Clone()}
	return resp, nil
}

func (e *ClaudeExecutor) ExecuteStream(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (_ *cliproxyexecutor.StreamResult, err error) {
	if opts.Alt == "responses/compact" {
		return nil, statusErr{code: http.StatusNotImplemented, msg: "/responses/compact not supported"}
	}
	apiKey, baseURL := claudeCreds(auth)
	if baseURL == "" {
		baseURL = "https://api.anthropic.com"
	}
	skipAnthropic := auth != nil && auth.Attributes != nil && auth.Attributes["skip_anthropic_processing"] == "true"

	to := sdktranslator.FromString("claude")
	execCtx := newExecutionContext(ctx, e.Identifier(), e.cfg, auth, req, opts, ExecutionOptions{
		TargetFormat:      to,
		TranslateAsStream: true,
	})
	reporter := execCtx.Reporter()
	defer reporter.trackFailure(execCtx.Context, &err)
	body, originalTranslated := execCtx.TranslateRequestPair(req.Payload)
	body, _ = sjson.SetBytes(body, "model", execCtx.BaseModel)

	body, err = thinking.ApplyThinking(body, req.Model, execCtx.SourceFormat.String(), to.String(), e.Identifier())
	if err != nil {
		return nil, err
	}

	claudeFP, claudeFPEnabled := claudeIdentityFingerprint(e.cfg, auth, execCtx.Context)
	claudeFPSessionID := ""
	// Apply cloaking (system prompt injection, fake user ID, sensitive word obfuscation)
	// based on client type and configuration.
	// Skipped when skip_anthropic_processing is enabled (third-party Claude-compatible APIs).
	if !skipAnthropic {
		if claudeFPEnabled {
			claudeFPSessionID = claudeFingerprintSessionID(claudeFP)
			body = applyClaudeIdentityFingerprintPayload(auth, body, claudeFP, claudeFPSessionID)
		} else {
			body = applyCloaking(execCtx.Context, e.cfg, auth, body, execCtx.BaseModel, apiKey)
		}
	}

	body = execCtx.ApplyPayloadConfig(body, originalTranslated)

	if !skipAnthropic {
		// Disable thinking if tool_choice forces tool use (Anthropic API constraint)
		body = disableThinkingIfToolChoiceForced(body)

		// Auto-inject cache_control if missing (optimization for ClawdBot/clients without caching support)
		if countCacheControls(body) == 0 {
			body = ensureCacheControl(body)
		}
	}

	// Extract betas from body and convert to header
	var extraBetas []string
	extraBetas, body = extractAndRemoveBetas(body)
	bodyForTranslation := body
	bodyForUpstream := body
	if !skipAnthropic && isClaudeOAuthToken(apiKey) && !auth.ToolPrefixDisabled() {
		bodyForUpstream = applyClaudeToolPrefix(body, claudeToolPrefix)
	}

	url := fmt.Sprintf("%s/v1/messages?beta=true", baseURL)
	httpReq, err := http.NewRequestWithContext(execCtx.Context, http.MethodPost, url, bytes.NewReader(bodyForUpstream))
	if err != nil {
		return nil, err
	}
	applyClaudeHeaders(httpReq, auth, apiKey, true, extraBetas, e.cfg, claudeFP, claudeFPEnabled, claudeFPSessionID)
	recorder := execCtx.Recorder()
	recorder.RecordRequest(url, http.MethodPost, httpReq.Header.Clone(), bodyForUpstream)

	httpClient := execCtx.HTTPClient(0)
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
			log.Errorf("response body close error: %v", errClose)
		}
		err = statusErr{code: httpResp.StatusCode, msg: string(b), headers: httpResp.Header.Clone()}
		return nil, err
	}
	decodedBody, err := decodeResponseBody(httpResp.Body, httpResp.Header.Get("Content-Encoding"))
	if err != nil {
		recorder.RecordResponseError(err)
		if errClose := httpResp.Body.Close(); errClose != nil {
			log.Errorf("response body close error: %v", errClose)
		}
		return nil, err
	}
	out := make(chan cliproxyexecutor.StreamChunk)
	reporter.setInputContent(string(req.Payload))
	go func() {
		defer close(out)
		defer func() {
			if errClose := decodedBody.Close(); errClose != nil {
				log.Errorf("response body close error: %v", errClose)
			}
		}()

		// If from == to (Claude → Claude), directly forward the SSE stream without translation
		if execCtx.SourceFormat == to {
			scanner := bufio.NewScanner(decodedBody)
			scanner.Buffer(nil, 52_428_800) // 50MB
			for scanner.Scan() {
				line := scanner.Bytes()
				recorder.AppendResponseChunk(line)
				reporter.appendOutputChunk(line)
				if detail, ok := parseClaudeStreamUsage(line); ok {
					reporter.publish(execCtx.Context, detail)
				}
				if !skipAnthropic && isClaudeOAuthToken(apiKey) && !auth.ToolPrefixDisabled() {
					line = stripClaudeToolPrefixFromStreamLine(line, claudeToolPrefix)
				}
				// Forward the line as-is to preserve SSE format
				cloned := make([]byte, len(line)+1)
				copy(cloned, line)
				cloned[len(line)] = '\n'
				out <- cliproxyexecutor.StreamChunk{Payload: cloned}
			}
			if errScan := scanner.Err(); errScan != nil {
				recorder.RecordResponseError(errScan)
				reporter.publishFailure(execCtx.Context)
				out <- cliproxyexecutor.StreamChunk{Err: errScan}
			}
			return
		}

		// For other formats, use translation
		scanner := bufio.NewScanner(decodedBody)
		scanner.Buffer(nil, 52_428_800) // 50MB
		var param any
		for scanner.Scan() {
			line := scanner.Bytes()
			recorder.AppendResponseChunk(line)
			reporter.appendOutputChunk(line)
			if detail, ok := parseClaudeStreamUsage(line); ok {
				reporter.publish(execCtx.Context, detail)
			}
			if !skipAnthropic && isClaudeOAuthToken(apiKey) && !auth.ToolPrefixDisabled() {
				line = stripClaudeToolPrefixFromStreamLine(line, claudeToolPrefix)
			}
			chunks := sdktranslator.TranslateStream(
				execCtx.Context,
				to,
				execCtx.SourceFormat,
				req.Model,
				opts.OriginalRequest,
				bodyForTranslation,
				bytes.Clone(line),
				&param,
			)
			for i := range chunks {
				out <- cliproxyexecutor.StreamChunk{Payload: []byte(chunks[i])}
			}
		}
		if errScan := scanner.Err(); errScan != nil {
			recorder.RecordResponseError(errScan)
			reporter.publishFailure(execCtx.Context)
			out <- cliproxyexecutor.StreamChunk{Err: errScan}
		}
	}()
	return &cliproxyexecutor.StreamResult{Headers: httpResp.Header.Clone(), Chunks: out}, nil
}

func (e *ClaudeExecutor) CountTokens(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	apiKey, baseURL := claudeCreds(auth)
	if baseURL == "" {
		baseURL = "https://api.anthropic.com"
	}

	to := sdktranslator.FromString("claude")
	// Use streaming translation to preserve function calling, except for claude.
	stream := opts.SourceFormat != to
	execCtx := newExecutionContext(ctx, e.Identifier(), e.cfg, auth, req, opts, ExecutionOptions{
		TargetFormat:      to,
		TranslateAsStream: stream,
	})
	body, _ := execCtx.TranslateRequestPair(req.Payload)
	body, _ = sjson.SetBytes(body, "model", execCtx.BaseModel)

	if !strings.HasPrefix(execCtx.BaseModel, "claude-3-5-haiku") {
		body = checkSystemInstructions(body)
	}

	// Extract betas from body and convert to header (for count_tokens too)
	var extraBetas []string
	extraBetas, body = extractAndRemoveBetas(body)
	claudeFP, claudeFPEnabled := claudeIdentityFingerprint(e.cfg, auth, execCtx.Context)
	claudeFPSessionID := ""
	if claudeFPEnabled {
		claudeFPSessionID = claudeFingerprintSessionID(claudeFP)
		body = applyClaudeIdentityFingerprintPayload(auth, body, claudeFP, claudeFPSessionID)
	}
	if isClaudeOAuthToken(apiKey) && !auth.ToolPrefixDisabled() {
		body = applyClaudeToolPrefix(body, claudeToolPrefix)
	}

	url := fmt.Sprintf("%s/v1/messages/count_tokens?beta=true", baseURL)
	httpReq, err := http.NewRequestWithContext(execCtx.Context, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return cliproxyexecutor.Response{}, err
	}
	applyClaudeHeaders(httpReq, auth, apiKey, false, extraBetas, e.cfg, claudeFP, claudeFPEnabled, claudeFPSessionID)
	recorder := execCtx.Recorder()
	recorder.RecordRequest(url, http.MethodPost, httpReq.Header.Clone(), body)

	httpClient := execCtx.HTTPClient(0)
	resp, err := httpClient.Do(httpReq)
	if err != nil {
		recorder.RecordResponseError(err)
		return cliproxyexecutor.Response{}, err
	}
	recorder.RecordResponseMetadata(resp.StatusCode, resp.Header.Clone())
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		b := readUpstreamErrorBody(e.Identifier(), resp.Body)
		recorder.AppendResponseChunk(b)
		if errClose := resp.Body.Close(); errClose != nil {
			log.Errorf("response body close error: %v", errClose)
		}
		return cliproxyexecutor.Response{}, statusErr{code: resp.StatusCode, msg: string(b), headers: resp.Header.Clone()}
	}
	decodedBody, err := decodeResponseBody(resp.Body, resp.Header.Get("Content-Encoding"))
	if err != nil {
		recorder.RecordResponseError(err)
		if errClose := resp.Body.Close(); errClose != nil {
			log.Errorf("response body close error: %v", errClose)
		}
		return cliproxyexecutor.Response{}, err
	}
	defer func() {
		if errClose := decodedBody.Close(); errClose != nil {
			log.Errorf("response body close error: %v", errClose)
		}
	}()
	data, err := readUpstreamResponseBody(e.Identifier(), decodedBody)
	if err != nil {
		recorder.RecordResponseError(err)
		return cliproxyexecutor.Response{}, err
	}
	recorder.AppendResponseChunk(data)
	count := gjson.GetBytes(data, "input_tokens").Int()
	out := sdktranslator.TranslateTokenCount(execCtx.Context, to, execCtx.SourceFormat, count, data)
	return cliproxyexecutor.Response{Payload: []byte(out), Headers: resp.Header.Clone()}, nil
}

func (e *ClaudeExecutor) Refresh(ctx context.Context, auth *cliproxyauth.Auth) (*cliproxyauth.Auth, error) {
	log.Debugf("claude executor: refresh called")
	if auth == nil {
		return nil, fmt.Errorf("claude executor: auth is nil")
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
	svc := claudeauth.NewClaudeAuth(e.cfg)
	td, err := svc.RefreshTokens(ctx, refreshToken)
	if err != nil {
		return nil, err
	}
	if auth.Metadata == nil {
		auth.Metadata = make(map[string]any)
	}
	auth.Metadata["access_token"] = td.AccessToken
	if td.RefreshToken != "" {
		auth.Metadata["refresh_token"] = td.RefreshToken
	}
	auth.Metadata["email"] = td.Email
	auth.Metadata["expired"] = td.Expire
	auth.Metadata["type"] = "claude"
	now := time.Now().Format(time.RFC3339)
	auth.Metadata["last_refresh"] = now
	return auth, nil
}
