package executor

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	xaiauth "github.com/router-for-me/CLIProxyAPI/v6/internal/auth/xai"
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

// XAIExecutor executes Grok requests through xAI's Responses API.
type XAIExecutor struct {
	cfg *config.Config
}

func NewXAIExecutor(cfg *config.Config) *XAIExecutor { return &XAIExecutor{cfg: cfg} }

func (e *XAIExecutor) Identifier() string { return "xai" }

func (e *XAIExecutor) PrepareRequest(req *http.Request, auth *cliproxyauth.Auth) error {
	if req == nil {
		return nil
	}
	token, _ := xaiCreds(auth)
	applyXAIHeaders(req, e.cfg, auth, token, false)
	return nil
}

func (e *XAIExecutor) HttpRequest(ctx context.Context, auth *cliproxyauth.Auth, req *http.Request) (*http.Response, error) {
	if req == nil {
		return nil, fmt.Errorf("xai executor: request is nil")
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

func (e *XAIExecutor) Execute(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (resp cliproxyexecutor.Response, err error) {
	if opts.Alt == "responses/compact" {
		return resp, statusErr{code: http.StatusNotImplemented, msg: "/responses/compact not supported"}
	}
	execCtx, body, _, err := e.prepareResponsesRequest(ctx, auth, req, opts, true)
	if err != nil {
		return resp, err
	}
	reporter := execCtx.Reporter()
	defer reporter.trackFailure(execCtx.Context, &err)

	token, baseURL := xaiCreds(auth)
	if baseURL == "" {
		baseURL = xaiauth.DefaultAPIBaseURL
	}
	endpoint := strings.TrimSuffix(baseURL, "/") + "/responses"
	httpReq, err := http.NewRequestWithContext(execCtx.Context, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return resp, err
	}
	applyXAIHeaders(httpReq, e.cfg, auth, token, true)
	recorder := execCtx.Recorder()
	recorder.RecordRequest(endpoint, http.MethodPost, httpReq.Header.Clone(), body)

	httpClient := execCtx.HTTPClient(0)
	httpResp, err := httpClient.Do(httpReq) //nolint:bodyclose // body is closed by the defer below.
	if err != nil {
		recorder.RecordResponseError(err)
		reporter.publishFailureWithContent(execCtx.Context, string(req.Payload), err.Error())
		return resp, err
	}
	defer func() {
		if errClose := httpResp.Body.Close(); errClose != nil {
			log.Errorf("xai executor: close response body error: %v", errClose)
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
	data, err := readUpstreamResponseBody(e.Identifier(), httpResp.Body)
	if err != nil {
		recorder.RecordResponseError(err)
		return resp, err
	}
	recorder.AppendResponseChunk(data)

	pendingOutputItems := make([][]byte, 0, 1)
	pendingOutputKeys := make([]string, 0, 1)
	pendingSeen := make(map[string]struct{})
	var streamErr error
	for _, line := range bytes.Split(data, []byte("\n")) {
		if !bytes.HasPrefix(line, dataTag) {
			continue
		}
		payload := bytes.TrimSpace(line[len(dataTag):])
		switch gjson.GetBytes(payload, "type").String() {
		case "response.failed", "error":
			streamErr = codexResponsesFailedStatusErr(payload)
			continue
		}
		if item, key, ok := extractCodexResponsesOutputItemDone(payload); ok {
			if _, exists := pendingSeen[key]; !exists {
				pendingSeen[key] = struct{}{}
				pendingOutputItems = append(pendingOutputItems, item)
				pendingOutputKeys = append(pendingOutputKeys, key)
			}
			continue
		}
		if gjson.GetBytes(payload, "type").String() != "response.completed" {
			continue
		}
		payload = mergeCodexResponsesCompletedOutput(payload, pendingOutputItems, pendingOutputKeys)
		if detail, ok := parseCodexUsage(payload); ok {
			reporter.publishWithContent(execCtx.Context, detail, string(req.Payload), string(data))
		}
		var param any
		out := sdktranslator.TranslateNonStream(execCtx.Context, execCtx.Execution.TargetFormat, execCtx.SourceFormat, req.Model, execCtx.OriginalPayload, body, payload, &param)
		return cliproxyexecutor.Response{Payload: []byte(out), Headers: httpResp.Header.Clone()}, nil
	}
	if streamErr != nil {
		reporter.publishFailureWithContent(execCtx.Context, string(req.Payload), streamErr.Error())
		return resp, streamErr
	}
	err = statusErr{code: http.StatusRequestTimeout, msg: "xai stream error: stream disconnected before response.completed"}
	return resp, err
}

func (e *XAIExecutor) ExecuteStream(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (_ *cliproxyexecutor.StreamResult, err error) {
	if opts.Alt == "responses/compact" {
		return nil, statusErr{code: http.StatusBadRequest, msg: "streaming not supported for /responses/compact"}
	}
	execCtx, body, _, err := e.prepareResponsesRequest(ctx, auth, req, opts, true)
	if err != nil {
		return nil, err
	}
	reporter := execCtx.Reporter()
	defer reporter.trackFailure(execCtx.Context, &err)

	token, baseURL := xaiCreds(auth)
	if baseURL == "" {
		baseURL = xaiauth.DefaultAPIBaseURL
	}
	endpoint := strings.TrimSuffix(baseURL, "/") + "/responses"
	httpReq, err := http.NewRequestWithContext(execCtx.Context, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	applyXAIHeaders(httpReq, e.cfg, auth, token, true)
	recorder := execCtx.Recorder()
	recorder.RecordRequest(endpoint, http.MethodPost, httpReq.Header.Clone(), body)

	httpClient := execCtx.HTTPClient(0)
	httpResp, err := httpClient.Do(httpReq) //nolint:bodyclose // success body is consumed and closed by the stream goroutine below.
	if err != nil {
		recorder.RecordResponseError(err)
		reporter.publishFailureWithContent(execCtx.Context, string(req.Payload), err.Error())
		return nil, err
	}
	recorder.RecordResponseMetadata(httpResp.StatusCode, httpResp.Header.Clone())
	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		data := readUpstreamErrorBody(e.Identifier(), httpResp.Body)
		if errClose := httpResp.Body.Close(); errClose != nil {
			log.Errorf("xai executor: close response body error: %v", errClose)
		}
		recorder.AppendResponseChunk(data)
		logWithRequestID(execCtx.Context).Debugf("request error, error status: %d, error message: %s", httpResp.StatusCode, summarizeErrorBody(httpResp.Header.Get("Content-Type"), data))
		reporter.publishFailureWithContent(execCtx.Context, string(req.Payload), string(data))
		err = statusErr{code: httpResp.StatusCode, msg: string(data), headers: httpResp.Header.Clone()}
		return nil, err
	}

	out := make(chan cliproxyexecutor.StreamChunk)
	reporter.setInputContent(string(req.Payload))
	go func() {
		defer close(out)
		defer func() {
			if errClose := httpResp.Body.Close(); errClose != nil {
				log.Errorf("xai executor: close response body error: %v", errClose)
			}
		}()
		scanner := bufio.NewScanner(httpResp.Body)
		scanner.Buffer(nil, streamScannerBuffer)
		var param any
		completed := false
		for scanner.Scan() {
			line := scanner.Bytes()
			recorder.AppendResponseChunk(line)
			reporter.appendOutputChunk(line)

			var terminalErr error
			if bytes.HasPrefix(line, dataTag) {
				payload := bytes.TrimSpace(line[len(dataTag):])
				switch gjson.GetBytes(payload, "type").String() {
				case "response.completed":
					completed = true
					if detail, ok := parseCodexUsage(payload); ok {
						reporter.publish(execCtx.Context, detail)
					}
				case "response.failed", "error":
					terminalErr = codexResponsesFailedStatusErr(payload)
				}
			}

			chunks := sdktranslator.TranslateStream(execCtx.Context, execCtx.Execution.TargetFormat, execCtx.SourceFormat, req.Model, execCtx.OriginalPayload, body, bytes.Clone(line), &param)
			for i := range chunks {
				out <- cliproxyexecutor.StreamChunk{Payload: []byte(chunks[i])}
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

func (e *XAIExecutor) CountTokens(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	execCtx, body, _, err := e.prepareResponsesRequest(ctx, auth, req, opts, false)
	if err != nil {
		return cliproxyexecutor.Response{}, err
	}
	enc, err := tokenizerForCodexModel(execCtx.BaseModel)
	if err != nil {
		return cliproxyexecutor.Response{}, fmt.Errorf("xai executor: tokenizer init failed: %w", err)
	}
	count, err := countCodexInputTokens(enc, body)
	if err != nil {
		return cliproxyexecutor.Response{}, fmt.Errorf("xai executor: token counting failed: %w", err)
	}
	usageJSON := fmt.Sprintf(`{"response":{"usage":{"input_tokens":%d,"output_tokens":0,"total_tokens":%d}}}`, count, count)
	translated := sdktranslator.TranslateTokenCount(execCtx.Context, execCtx.Execution.TargetFormat, execCtx.SourceFormat, count, []byte(usageJSON))
	return cliproxyexecutor.Response{Payload: []byte(translated)}, nil
}

func (e *XAIExecutor) Refresh(ctx context.Context, auth *cliproxyauth.Auth) (*cliproxyauth.Auth, error) {
	log.Debugf("xai executor: refresh called")
	if auth == nil {
		return nil, fmt.Errorf("xai executor: auth is nil")
	}
	refreshToken := xaiMetadataString(auth.Metadata, "refresh_token")
	if refreshToken == "" {
		return auth, nil
	}
	tokenEndpoint := xaiMetadataString(auth.Metadata, "token_endpoint")
	svc := xaiauth.NewXAIAuthWithProxyURL(e.cfg, auth.ProxyURL)
	td, err := svc.RefreshTokens(ctx, refreshToken, tokenEndpoint)
	if err != nil {
		return nil, err
	}
	if auth.Metadata == nil {
		auth.Metadata = make(map[string]any)
	}
	auth.Metadata["type"] = "xai"
	auth.Metadata["auth_kind"] = "oauth"
	auth.Metadata["access_token"] = td.AccessToken
	if td.RefreshToken != "" {
		auth.Metadata["refresh_token"] = td.RefreshToken
	}
	if td.IDToken != "" {
		auth.Metadata["id_token"] = td.IDToken
	}
	if td.TokenType != "" {
		auth.Metadata["token_type"] = td.TokenType
	}
	if td.ExpiresIn > 0 {
		auth.Metadata["expires_in"] = td.ExpiresIn
	}
	if td.Expire != "" {
		auth.Metadata["expired"] = td.Expire
	}
	if td.Email != "" {
		auth.Metadata["email"] = td.Email
	}
	if td.Subject != "" {
		auth.Metadata["sub"] = td.Subject
	}
	if tokenEndpoint != "" {
		auth.Metadata["token_endpoint"] = tokenEndpoint
	}
	if xaiMetadataString(auth.Metadata, "base_url") == "" {
		auth.Metadata["base_url"] = xaiauth.DefaultAPIBaseURL
	}
	auth.Metadata["last_refresh"] = time.Now().UTC().Format(time.RFC3339)
	if auth.Attributes == nil {
		auth.Attributes = make(map[string]string)
	}
	auth.Attributes["auth_kind"] = "oauth"
	if strings.TrimSpace(auth.Attributes["base_url"]) == "" {
		auth.Attributes["base_url"] = xaiauth.DefaultAPIBaseURL
	}
	return auth, nil
}

func (e *XAIExecutor) prepareResponsesRequest(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options, stream bool) (*ExecutionContext, []byte, []byte, error) {
	execCtx := newExecutionContext(ctx, e.Identifier(), e.cfg, auth, req, opts, ExecutionOptions{
		TargetFormat:      sdktranslator.FromString("codex"),
		TranslateAsStream: stream,
	})
	body, originalTranslated := execCtx.TranslateRequestPair(req.Payload)

	var err error
	body, err = thinking.ApplyThinking(body, req.Model, execCtx.SourceFormat.String(), e.Identifier(), e.Identifier())
	if err != nil {
		return nil, nil, nil, err
	}
	body = execCtx.ApplyPayloadConfig(body, originalTranslated)
	body = ensureTranslatedCodexModel(body, execCtx.BaseModel)
	body = sanitizeCodexResponsesRequest(body)
	body, _ = sjson.SetBytes(body, "stream", stream)
	body, _ = sjson.DeleteBytes(body, "previous_response_id")
	body, _ = sjson.DeleteBytes(body, "prompt_cache_retention")
	body, _ = sjson.DeleteBytes(body, "safety_identifier")
	body, _ = sjson.DeleteBytes(body, "stream_options")
	if !gjson.GetBytes(body, "instructions").Exists() {
		sysContent := extractSystemMessagesAsInstructions(execCtx.Request.Payload)
		body, _ = sjson.SetBytes(body, "instructions", sysContent)
	}
	return execCtx, body, originalTranslated, nil
}

func xaiCreds(auth *cliproxyauth.Auth) (token, baseURL string) {
	if auth == nil {
		return "", ""
	}
	if auth.Attributes != nil {
		token = strings.TrimSpace(auth.Attributes["api_key"])
		baseURL = strings.TrimSpace(auth.Attributes["base_url"])
	}
	if auth.Metadata != nil {
		if token == "" {
			token = xaiMetadataString(auth.Metadata, "access_token")
		}
		if baseURL == "" {
			baseURL = xaiMetadataString(auth.Metadata, "base_url")
		}
	}
	return token, baseURL
}

func applyXAIHeaders(r *http.Request, cfg *config.Config, auth *cliproxyauth.Auth, token string, stream bool) {
	r.Header.Set("Content-Type", "application/json")
	if strings.TrimSpace(token) != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	if stream {
		r.Header.Set("Accept", "text/event-stream")
	} else {
		r.Header.Set("Accept", "application/json")
	}
	r.Header.Set("Connection", "Keep-Alive")
	var attrs map[string]string
	if auth != nil {
		attrs = auth.Attributes
	}
	util.ApplyCustomHeadersFromAttrs(r, attrs)
	applyXAIPassthroughHeaders(r.Header, identityFingerprintHeadersFromContext(r.Context()))
	if fp, ok := xaiIdentityFingerprint(cfg, auth, r.Context()); ok {
		applyXAIIdentityFingerprintHeaders(r.Header, fp)
	}
}

func xaiMetadataString(meta map[string]any, key string) string {
	if meta == nil {
		return ""
	}
	switch v := meta[key].(type) {
	case string:
		return strings.TrimSpace(v)
	default:
		return ""
	}
}
