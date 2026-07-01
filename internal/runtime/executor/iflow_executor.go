package executor

import (
	"bufio"
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	iflowauth "github.com/router-for-me/CLIProxyAPI/v6/internal/auth/iflow"
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

const (
	iflowDefaultEndpoint = "/chat/completions"
	iflowUserAgent       = "iFlow-Cli"
)

// IFlowExecutor executes OpenAI-compatible chat completions against the iFlow API using API keys derived from OAuth.
type IFlowExecutor struct {
	cfg *config.Config
}

// NewIFlowExecutor constructs a new executor instance.
func NewIFlowExecutor(cfg *config.Config) *IFlowExecutor { return &IFlowExecutor{cfg: cfg} }

// Identifier returns the provider key.
func (e *IFlowExecutor) Identifier() string { return "iflow" }

// PrepareRequest injects iFlow credentials into the outgoing HTTP request.
func (e *IFlowExecutor) PrepareRequest(req *http.Request, auth *cliproxyauth.Auth) error {
	if req == nil {
		return nil
	}
	apiKey, _ := iflowCreds(auth)
	if strings.TrimSpace(apiKey) != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	return nil
}

// HttpRequest injects iFlow credentials into the request and executes it.
func (e *IFlowExecutor) HttpRequest(ctx context.Context, auth *cliproxyauth.Auth, req *http.Request) (*http.Response, error) {
	if req == nil {
		return nil, fmt.Errorf("iflow executor: request is nil")
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

// Execute performs a non-streaming chat completion request.
func (e *IFlowExecutor) Execute(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (resp cliproxyexecutor.Response, err error) {
	if opts.Alt == "responses/compact" {
		return resp, statusErr{code: http.StatusNotImplemented, msg: "/responses/compact not supported"}
	}
	execCtx := newExecutionContext(ctx, e.Identifier(), e.cfg, auth, req, opts, ExecutionOptions{
		TargetFormat:      sdktranslator.FromString("openai"),
		TranslateAsStream: false,
	})

	apiKey, baseURL := iflowCreds(auth)
	if strings.TrimSpace(apiKey) == "" {
		err = fmt.Errorf("iflow executor: missing api key")
		return resp, err
	}
	if baseURL == "" {
		baseURL = iflowauth.DefaultAPIBaseURL
	}

	reporter := execCtx.Reporter()
	defer reporter.trackFailure(execCtx.Context, &err)

	translated, originalTranslated := execCtx.TranslateRequestPair(req.Payload)
	translated, _ = sjson.SetBytes(translated, "model", execCtx.BaseModel)

	translated, err = thinking.ApplyThinking(
		translated,
		req.Model,
		execCtx.SourceFormat.String(),
		"iflow",
		e.Identifier(),
	)
	if err != nil {
		return resp, err
	}

	translated = preserveReasoningContentInMessages(translated)
	translated = execCtx.ApplyPayloadConfig(translated, originalTranslated)

	endpoint := strings.TrimSuffix(baseURL, "/") + iflowDefaultEndpoint

	httpReq, err := http.NewRequestWithContext(execCtx.Context, http.MethodPost, endpoint, bytes.NewReader(translated))
	if err != nil {
		return resp, err
	}
	applyIFlowHeaders(httpReq, apiKey, false)
	recorder := execCtx.Recorder()
	recorder.RecordRequest(endpoint, http.MethodPost, httpReq.Header.Clone(), translated)

	httpClient := execCtx.HTTPClient(0)
	//nolint:bodyclose // body is closed by the defer below.
	httpResp, err := httpClient.Do(httpReq)
	if err != nil {
		recorder.RecordResponseError(err)
		reporter.publishFailureWithContent(execCtx.Context, string(req.Payload), err.Error())
		return resp, err
	}
	defer func() {
		if errClose := httpResp.Body.Close(); errClose != nil {
			log.Errorf("iflow executor: close response body error: %v", errClose)
		}
	}()
	recorder.RecordResponseMetadata(httpResp.StatusCode, httpResp.Header.Clone())

	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		b := readUpstreamErrorBody(e.Identifier(), httpResp.Body)
		recorder.AppendResponseChunk(b)
		logWithRequestID(execCtx.Context).Debugf("request error, error status: %d error message: %s", httpResp.StatusCode, summarizeErrorBody(httpResp.Header.Get("Content-Type"), b))
		reporter.publishFailureWithContent(execCtx.Context, string(req.Payload), string(b))
		err = statusErr{code: httpResp.StatusCode, msg: string(b)}
		return resp, err
	}

	data, err := readUpstreamResponseBody(e.Identifier(), httpResp.Body)
	if err != nil {
		recorder.RecordResponseError(err)
		return resp, err
	}
	recorder.AppendResponseChunk(data)
	reporter.publishWithContent(execCtx.Context, parseOpenAIUsage(data), string(req.Payload), string(data))
	// Ensure usage is recorded even if upstream omits usage metadata.
	reporter.ensurePublished(execCtx.Context)

	var param any
	// Note: TranslateNonStream uses req.Model (original with suffix) to preserve
	// the original model name in the response for client compatibility.
	out := sdktranslator.TranslateNonStream(
		execCtx.Context,
		execCtx.Execution.TargetFormat,
		execCtx.SourceFormat,
		req.Model,
		opts.OriginalRequest,
		translated,
		data,
		&param,
	)
	resp = cliproxyexecutor.Response{Payload: []byte(out), Headers: httpResp.Header.Clone()}
	return resp, nil
}

// ExecuteStream performs a streaming chat completion request.
func (e *IFlowExecutor) ExecuteStream(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (_ *cliproxyexecutor.StreamResult, err error) {
	if opts.Alt == "responses/compact" {
		return nil, statusErr{code: http.StatusNotImplemented, msg: "/responses/compact not supported"}
	}
	execCtx := newExecutionContext(ctx, e.Identifier(), e.cfg, auth, req, opts, ExecutionOptions{
		TargetFormat:      sdktranslator.FromString("openai"),
		TranslateAsStream: true,
	})

	apiKey, baseURL := iflowCreds(auth)
	if strings.TrimSpace(apiKey) == "" {
		err = fmt.Errorf("iflow executor: missing api key")
		return nil, err
	}
	if baseURL == "" {
		baseURL = iflowauth.DefaultAPIBaseURL
	}

	reporter := execCtx.Reporter()
	defer reporter.trackFailure(execCtx.Context, &err)

	translated, originalTranslated := execCtx.TranslateRequestPair(req.Payload)
	translated, _ = sjson.SetBytes(translated, "model", execCtx.BaseModel)

	translated, err = thinking.ApplyThinking(
		translated,
		req.Model,
		execCtx.SourceFormat.String(),
		"iflow",
		e.Identifier(),
	)
	if err != nil {
		return nil, err
	}

	translated = preserveReasoningContentInMessages(translated)
	// Ensure tools array exists to avoid provider quirks similar to Qwen's behaviour.
	toolsResult := gjson.GetBytes(translated, "tools")
	if toolsResult.Exists() && toolsResult.IsArray() && len(toolsResult.Array()) == 0 {
		translated = ensureToolsArray(translated)
	}
	translated = execCtx.ApplyPayloadConfig(translated, originalTranslated)

	endpoint := strings.TrimSuffix(baseURL, "/") + iflowDefaultEndpoint

	httpReq, err := http.NewRequestWithContext(execCtx.Context, http.MethodPost, endpoint, bytes.NewReader(translated))
	if err != nil {
		return nil, err
	}
	applyIFlowHeaders(httpReq, apiKey, true)
	recorder := execCtx.Recorder()
	recorder.RecordRequest(endpoint, http.MethodPost, httpReq.Header.Clone(), translated)

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
		data := readUpstreamErrorBody(e.Identifier(), httpResp.Body)
		if errClose := httpResp.Body.Close(); errClose != nil {
			log.Errorf("iflow executor: close response body error: %v", errClose)
		}
		recorder.AppendResponseChunk(data)
		logWithRequestID(execCtx.Context).Debugf("request error, error status: %d error message: %s", httpResp.StatusCode, summarizeErrorBody(httpResp.Header.Get("Content-Type"), data))
		reporter.publishFailureWithContent(execCtx.Context, string(req.Payload), string(data))
		err = statusErr{code: httpResp.StatusCode, msg: string(data)}
		return nil, err
	}

	out := make(chan cliproxyexecutor.StreamChunk)
	reporter.setInputContent(string(req.Payload))
	go func() {
		defer close(out)
		defer func() {
			if errClose := httpResp.Body.Close(); errClose != nil {
				log.Errorf("iflow executor: close response body error: %v", errClose)
			}
		}()

		scanner := bufio.NewScanner(httpResp.Body)
		scanner.Buffer(nil, 52_428_800) // 50MB
		var param any
		for scanner.Scan() {
			line := scanner.Bytes()
			recorder.AppendResponseChunk(line)
			reporter.appendOutputChunk(line)
			if detail, ok := parseOpenAIStreamUsage(line); ok {
				reporter.publish(execCtx.Context, detail)
			}
			chunks := sdktranslator.TranslateStream(
				execCtx.Context,
				execCtx.Execution.TargetFormat,
				execCtx.SourceFormat,
				req.Model,
				opts.OriginalRequest,
				translated,
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
		// Guarantee a usage record exists even if the stream never emitted usage data.
		reporter.ensurePublished(execCtx.Context)
	}()

	return &cliproxyexecutor.StreamResult{Headers: httpResp.Header.Clone(), Chunks: out}, nil
}

func (e *IFlowExecutor) CountTokens(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	execCtx := newExecutionContext(ctx, e.Identifier(), e.cfg, auth, req, opts, ExecutionOptions{
		TargetFormat:      sdktranslator.FromString("openai"),
		TranslateAsStream: false,
	})
	body, _ := execCtx.TranslateRequestPair(req.Payload)

	enc, err := tokenizerForModel(execCtx.BaseModel)
	if err != nil {
		return cliproxyexecutor.Response{}, fmt.Errorf("iflow executor: tokenizer init failed: %w", err)
	}

	count, err := countOpenAIChatTokens(enc, body)
	if err != nil {
		return cliproxyexecutor.Response{}, fmt.Errorf("iflow executor: token counting failed: %w", err)
	}

	usageJSON := buildOpenAIUsageJSON(count)
	translated := sdktranslator.TranslateTokenCount(
		execCtx.Context,
		execCtx.Execution.TargetFormat,
		execCtx.SourceFormat,
		count,
		usageJSON,
	)
	return cliproxyexecutor.Response{Payload: []byte(translated)}, nil
}

// Refresh refreshes OAuth tokens or cookie-based API keys and updates the stored API key.
func (e *IFlowExecutor) Refresh(ctx context.Context, auth *cliproxyauth.Auth) (*cliproxyauth.Auth, error) {
	log.Debugf("iflow executor: refresh called")
	if auth == nil {
		return nil, fmt.Errorf("iflow executor: auth is nil")
	}

	// Check if this is cookie-based authentication
	var cookie string
	var email string
	if auth.Metadata != nil {
		if v, ok := auth.Metadata["cookie"].(string); ok {
			cookie = strings.TrimSpace(v)
		}
		if v, ok := auth.Metadata["email"].(string); ok {
			email = strings.TrimSpace(v)
		}
	}

	// If cookie is present, use cookie-based refresh
	if cookie != "" && email != "" {
		return e.refreshCookieBased(ctx, auth, cookie, email)
	}

	// Otherwise, use OAuth-based refresh
	return e.refreshOAuthBased(ctx, auth)
}

// refreshCookieBased refreshes API key using browser cookie
func (e *IFlowExecutor) refreshCookieBased(ctx context.Context, auth *cliproxyauth.Auth, cookie, email string) (*cliproxyauth.Auth, error) {
	maskedEmail := util.HideAPIKey(email)
	log.Debugf("iflow executor: checking refresh need for cookie-based API key for user: %s", maskedEmail)

	// Get current expiry time from metadata
	var currentExpire string
	if auth.Metadata != nil {
		if v, ok := auth.Metadata["expired"].(string); ok {
			currentExpire = strings.TrimSpace(v)
		}
	}

	// Check if refresh is needed
	needsRefresh, _, err := iflowauth.ShouldRefreshAPIKey(currentExpire)
	if err != nil {
		log.Warnf("iflow executor: failed to check refresh need: %v", err)
		// If we can't check, continue with refresh anyway as a safety measure
	} else if !needsRefresh {
		log.Debugf("iflow executor: no refresh needed for user: %s", maskedEmail)
		return auth, nil
	}

	log.Infof("iflow executor: refreshing cookie-based API key for user: %s", maskedEmail)

	svc := iflowauth.NewIFlowAuth(e.cfg)
	keyData, err := svc.RefreshAPIKey(ctx, cookie, email)
	if err != nil {
		log.Errorf("iflow executor: cookie-based API key refresh failed: %v", err)
		return nil, err
	}

	if auth.Metadata == nil {
		auth.Metadata = make(map[string]any)
	}
	auth.Metadata["api_key"] = keyData.APIKey
	auth.Metadata["expired"] = keyData.ExpireTime
	auth.Metadata["type"] = "iflow"
	auth.Metadata["last_refresh"] = time.Now().Format(time.RFC3339)
	auth.Metadata["cookie"] = cookie
	auth.Metadata["email"] = email

	log.Infof("iflow executor: cookie-based API key refreshed successfully, new expiry: %s", keyData.ExpireTime)

	if auth.Attributes == nil {
		auth.Attributes = make(map[string]string)
	}
	auth.Attributes["api_key"] = keyData.APIKey

	return auth, nil
}

// refreshOAuthBased refreshes tokens using OAuth refresh token
func (e *IFlowExecutor) refreshOAuthBased(ctx context.Context, auth *cliproxyauth.Auth) (*cliproxyauth.Auth, error) {
	refreshToken := ""
	oldAccessToken := ""
	if auth.Metadata != nil {
		if v, ok := auth.Metadata["refresh_token"].(string); ok {
			refreshToken = strings.TrimSpace(v)
		}
		if v, ok := auth.Metadata["access_token"].(string); ok {
			oldAccessToken = strings.TrimSpace(v)
		}
	}
	if refreshToken == "" {
		return auth, nil
	}

	// Log the old access token (masked) before refresh
	if oldAccessToken != "" {
		log.Debugf("iflow executor: refreshing access token, old: %s", util.HideAPIKey(oldAccessToken))
	}

	svc := iflowauth.NewIFlowAuth(e.cfg)
	tokenData, err := svc.RefreshTokens(ctx, refreshToken)
	if err != nil {
		log.Errorf("iflow executor: token refresh failed: %v", err)
		return nil, err
	}

	if auth.Metadata == nil {
		auth.Metadata = make(map[string]any)
	}
	auth.Metadata["access_token"] = tokenData.AccessToken
	if tokenData.RefreshToken != "" {
		auth.Metadata["refresh_token"] = tokenData.RefreshToken
	}
	if tokenData.APIKey != "" {
		auth.Metadata["api_key"] = tokenData.APIKey
	}
	auth.Metadata["expired"] = tokenData.Expire
	auth.Metadata["type"] = "iflow"
	auth.Metadata["last_refresh"] = time.Now().Format(time.RFC3339)

	// Log the new access token (masked) after successful refresh
	log.Debugf("iflow executor: token refresh successful, new: %s", util.HideAPIKey(tokenData.AccessToken))

	if auth.Attributes == nil {
		auth.Attributes = make(map[string]string)
	}
	if tokenData.APIKey != "" {
		auth.Attributes["api_key"] = tokenData.APIKey
	}

	return auth, nil
}

func applyIFlowHeaders(r *http.Request, apiKey string, stream bool) {
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Authorization", "Bearer "+apiKey)
	r.Header.Set("User-Agent", iflowUserAgent)

	// Generate session-id
	sessionID := "session-" + generateUUID()
	r.Header.Set("session-id", sessionID)

	// Generate timestamp and signature
	timestamp := time.Now().UnixMilli()
	r.Header.Set("x-iflow-timestamp", fmt.Sprintf("%d", timestamp))

	signature := createIFlowSignature(iflowUserAgent, sessionID, timestamp, apiKey)
	if signature != "" {
		r.Header.Set("x-iflow-signature", signature)
	}

	if stream {
		r.Header.Set("Accept", "text/event-stream")
	} else {
		r.Header.Set("Accept", "application/json")
	}
}

// createIFlowSignature generates HMAC-SHA256 signature for iFlow API requests.
// The signature payload format is: userAgent:sessionId:timestamp
func createIFlowSignature(userAgent, sessionID string, timestamp int64, apiKey string) string {
	if apiKey == "" {
		return ""
	}
	payload := fmt.Sprintf("%s:%s:%d", userAgent, sessionID, timestamp)
	h := hmac.New(sha256.New, []byte(apiKey))
	h.Write([]byte(payload))
	return hex.EncodeToString(h.Sum(nil))
}

// generateUUID generates a random UUID v4 string.
func generateUUID() string {
	return uuid.New().String()
}

func iflowCreds(a *cliproxyauth.Auth) (apiKey, baseURL string) {
	if a == nil {
		return "", ""
	}
	if a.Attributes != nil {
		if v := strings.TrimSpace(a.Attributes["api_key"]); v != "" {
			apiKey = v
		}
		if v := strings.TrimSpace(a.Attributes["base_url"]); v != "" {
			baseURL = v
		}
	}
	if apiKey == "" && a.Metadata != nil {
		if v, ok := a.Metadata["api_key"].(string); ok {
			apiKey = strings.TrimSpace(v)
		}
	}
	if baseURL == "" && a.Metadata != nil {
		if v, ok := a.Metadata["base_url"].(string); ok {
			baseURL = strings.TrimSpace(v)
		}
	}
	return apiKey, baseURL
}

func ensureToolsArray(body []byte) []byte {
	placeholder := `[{"type":"function","function":{"name":"noop","description":"Placeholder tool to stabilise streaming","parameters":{"type":"object"}}}]`
	updated, err := sjson.SetRawBytes(body, "tools", []byte(placeholder))
	if err != nil {
		return body
	}
	return updated
}

// preserveReasoningContentInMessages checks if reasoning_content from assistant messages
// is preserved in conversation history for iFlow models that support thinking.
// This is helpful for multi-turn conversations where the model may benefit from seeing
// its previous reasoning to maintain coherent thought chains.
//
// For GLM-4.6/4.7 and MiniMax M2/M2.1, it is recommended to include the full assistant
// response (including reasoning_content) in message history for better context continuity.
func preserveReasoningContentInMessages(body []byte) []byte {
	model := strings.ToLower(gjson.GetBytes(body, "model").String())

	// Only apply to models that support thinking with history preservation
	needsPreservation := strings.HasPrefix(model, "glm-4") || strings.HasPrefix(model, "minimax-m2")

	if !needsPreservation {
		return body
	}

	messages := gjson.GetBytes(body, "messages")
	if !messages.Exists() || !messages.IsArray() {
		return body
	}

	// Check if any assistant message already has reasoning_content preserved
	hasReasoningContent := false
	messages.ForEach(func(_, msg gjson.Result) bool {
		role := msg.Get("role").String()
		if role == "assistant" {
			rc := msg.Get("reasoning_content")
			if rc.Exists() && rc.String() != "" {
				hasReasoningContent = true
				return false // stop iteration
			}
		}
		return true
	})

	// If reasoning content is already present, the messages are properly formatted
	// No need to modify - the client has correctly preserved reasoning in history
	if hasReasoningContent {
		log.Debugf("iflow executor: reasoning_content found in message history for %s", model)
	}

	return body
}
