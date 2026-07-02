// Package executor provides runtime execution capabilities for various AI service providers.
// This file implements the Gemini CLI executor that talks to Cloud Code Assist endpoints
// using OAuth credentials from auth metadata.
package executor

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/thinking"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v6/sdk/translator"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
	"net/http"
	"strings"
)

const (
	codeAssistEndpoint = "https://cloudcode-pa.googleapis.com"
	codeAssistVersion  = "v1internal"
)

// GeminiCLIExecutor talks to the Cloud Code Assist endpoint using OAuth credentials from auth metadata.
type GeminiCLIExecutor struct {
	cfg *config.Config
}

// NewGeminiCLIExecutor creates a new Gemini CLI executor instance.
//
// Parameters:
//   - cfg: The application configuration
//
// Returns:
//   - *GeminiCLIExecutor: A new Gemini CLI executor instance
func NewGeminiCLIExecutor(cfg *config.Config) *GeminiCLIExecutor {
	return &GeminiCLIExecutor{cfg: cfg}
}

// Identifier returns the executor identifier.
func (e *GeminiCLIExecutor) Identifier() string { return "gemini-cli" }

// PrepareRequest injects Gemini CLI credentials into the outgoing HTTP request.
func (e *GeminiCLIExecutor) PrepareRequest(req *http.Request, auth *cliproxyauth.Auth) error {
	if req == nil {
		return nil
	}
	tokenSource, _, errSource := prepareGeminiCLITokenSource(req.Context(), e.cfg, auth)
	if errSource != nil {
		return errSource
	}
	tok, errTok := tokenSource.Token()
	if errTok != nil {
		return errTok
	}
	if strings.TrimSpace(tok.AccessToken) == "" {
		return statusErr{code: http.StatusUnauthorized, msg: "missing access token"}
	}
	req.Header.Set("Authorization", "Bearer "+tok.AccessToken)
	applyGeminiCLIHeaders(req, e.cfg, auth)
	return nil
}

func (e *GeminiCLIExecutor) ProbeQuotaRecovery(ctx context.Context, auth *cliproxyauth.Auth) (*cliproxyauth.QuotaProbeResult, error) {
	if auth == nil {
		return nil, fmt.Errorf("gemini cli executor: auth is nil")
	}
	projectID := geminiCLIProjectID(auth)
	if projectID == "" {
		return nil, fmt.Errorf("gemini cli executor: missing project_id")
	}

	payload, err := json.Marshal(map[string]string{"project": projectID})
	if err != nil {
		return nil, err
	}
	reqBody := bytes.NewReader(payload)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, codeAssistEndpoint+"/"+codeAssistVersion+":retrieveUserQuota", reqBody)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if errPrepare := e.PrepareRequest(req, auth); errPrepare != nil {
		return nil, errPrepare
	}

	httpClient := newProxyAwareHTTPClient(ctx, e.cfg, auth, 0)
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() {
		if errClose := resp.Body.Close(); errClose != nil {
			log.Errorf("gemini cli executor: close quota probe body error: %v", errClose)
		}
	}()

	body, err := readUpstreamResponseBody(e.Identifier(), resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, newGeminiStatusErr(resp.StatusCode, body)
	}
	return parseGeminiCLIQuotaProbe(auth, body), nil
}

// HttpRequest injects Gemini CLI credentials into the request and executes it.
func (e *GeminiCLIExecutor) HttpRequest(ctx context.Context, auth *cliproxyauth.Auth, req *http.Request) (*http.Response, error) {
	if req == nil {
		return nil, fmt.Errorf("gemini-cli executor: request is nil")
	}
	if ctx == nil {
		ctx = req.Context()
	}
	httpReq := req.WithContext(ctx)
	if err := e.PrepareRequest(httpReq, auth); err != nil {
		return nil, err
	}
	httpClient := newHTTPClient(ctx, e.cfg, auth, 0)
	return httpClient.Do(httpReq)
}

// Execute performs a non-streaming request to the Gemini CLI API.
func (e *GeminiCLIExecutor) Execute(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (resp cliproxyexecutor.Response, err error) {
	if opts.Alt == "responses/compact" {
		return resp, statusErr{code: http.StatusNotImplemented, msg: "/responses/compact not supported"}
	}
	execCtx := newExecutionContext(ctx, e.Identifier(), e.cfg, auth, req, opts, ExecutionOptions{
		TargetFormat:        sdktranslator.FromString("gemini-cli"),
		PayloadConfigRoot:   "request",
		PayloadConfigFormat: "gemini",
	})
	tokenSource, baseTokenData, err := prepareGeminiCLITokenSource(execCtx.Context, e.cfg, auth)
	if err != nil {
		return resp, err
	}

	reporter := execCtx.Reporter()
	defer reporter.trackFailure(execCtx.Context, &err)

	to := execCtx.Execution.TargetFormat
	basePayload, originalTranslated := execCtx.TranslateRequestPair(req.Payload)
	basePayload, err = thinking.ApplyThinking(basePayload, req.Model, execCtx.SourceFormat.String(), to.String(), e.Identifier())
	if err != nil {
		return resp, err
	}

	basePayload = fixGeminiCLIImageAspectRatio(execCtx.BaseModel, basePayload)
	basePayload = execCtx.ApplyPayloadConfig(basePayload, originalTranslated)

	action := "generateContent"
	if req.Metadata != nil {
		if a, _ := req.Metadata["action"].(string); a == "countTokens" {
			action = "countTokens"
		}
	}

	projectID := resolveGeminiProjectID(auth)
	models := cliPreviewFallbackOrder(execCtx.BaseModel)
	if len(models) == 0 || models[0] != execCtx.BaseModel {
		models = append([]string{execCtx.BaseModel}, models...)
	}

	httpClient := execCtx.HTTPClient(0)
	respCtx := context.WithValue(execCtx.Context, "alt", opts.Alt)
	recorder := execCtx.Recorder()

	var lastStatus int
	var lastBody []byte

	for idx, attemptModel := range models {
		payload := append([]byte(nil), basePayload...)
		if action == "countTokens" {
			payload = deleteJSONField(payload, "project")
			payload = deleteJSONField(payload, "model")
		} else {
			payload = setJSONField(payload, "project", projectID)
			payload = setJSONField(payload, "model", attemptModel)
		}

		tok, errTok := tokenSource.Token()
		if errTok != nil {
			err = errTok
			return resp, err
		}
		updateGeminiCLITokenMetadata(auth, baseTokenData, tok)

		url := fmt.Sprintf("%s/%s:%s", codeAssistEndpoint, codeAssistVersion, action)
		if opts.Alt != "" && action != "countTokens" {
			url = url + fmt.Sprintf("?$alt=%s", opts.Alt)
		}

		reqHTTP, errReq := http.NewRequestWithContext(execCtx.Context, http.MethodPost, url, bytes.NewReader(payload))
		if errReq != nil {
			err = errReq
			return resp, err
		}
		reqHTTP.Header.Set("Content-Type", "application/json")
		reqHTTP.Header.Set("Authorization", "Bearer "+tok.AccessToken)
		applyGeminiCLIHeaders(reqHTTP, e.cfg, auth)
		reqHTTP.Header.Set("Accept", "application/json")
		recorder.RecordRequest(url, http.MethodPost, reqHTTP.Header.Clone(), payload)

		httpResp, errDo := httpClient.Do(reqHTTP)
		if errDo != nil {
			recorder.RecordResponseError(errDo)
			err = errDo
			return resp, err
		}

		data, errRead := readUpstreamResponseBody(e.Identifier(), httpResp.Body)
		if errClose := httpResp.Body.Close(); errClose != nil {
			log.Errorf("gemini cli executor: close response body error: %v", errClose)
		}
		recorder.RecordResponseMetadata(httpResp.StatusCode, httpResp.Header.Clone())
		if errRead != nil {
			recorder.RecordResponseError(errRead)
			err = errRead
			return resp, err
		}
		recorder.AppendResponseChunk(data)
		if httpResp.StatusCode >= 200 && httpResp.StatusCode < 300 {
			reporter.publish(execCtx.Context, parseGeminiCLIUsage(data))
			var param any
			out := sdktranslator.TranslateNonStream(respCtx, to, execCtx.SourceFormat, attemptModel, opts.OriginalRequest, payload, data, &param)
			resp = cliproxyexecutor.Response{Payload: []byte(out), Headers: httpResp.Header.Clone()}
			return resp, nil
		}

		lastStatus = httpResp.StatusCode
		lastBody = append([]byte(nil), data...)
		logWithRequestID(execCtx.Context).Debugf("request error, error status: %d, error message: %s", httpResp.StatusCode, summarizeErrorBody(httpResp.Header.Get("Content-Type"), data))
		if httpResp.StatusCode == 429 {
			if idx+1 < len(models) {
				log.Debugf("gemini cli executor: rate limited, retrying with next model: %s", models[idx+1])
			} else {
				log.Debug("gemini cli executor: rate limited, no additional fallback model")
			}
			continue
		}

		reporter.publishFailureWithContent(execCtx.Context, string(req.Payload), string(data))
		err = newGeminiStatusErr(httpResp.StatusCode, data)
		return resp, err
	}

	if len(lastBody) > 0 {
		recorder.AppendResponseChunk(lastBody)
	}
	if lastStatus == 0 {
		lastStatus = 429
	}
	reporter.publishFailureWithContent(execCtx.Context, string(req.Payload), string(lastBody))
	err = newGeminiStatusErr(lastStatus, lastBody)
	return resp, err
}

// ExecuteStream performs a streaming request to the Gemini CLI API.
func (e *GeminiCLIExecutor) ExecuteStream(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (_ *cliproxyexecutor.StreamResult, err error) {
	if opts.Alt == "responses/compact" {
		return nil, statusErr{code: http.StatusNotImplemented, msg: "/responses/compact not supported"}
	}
	execCtx := newExecutionContext(ctx, e.Identifier(), e.cfg, auth, req, opts, ExecutionOptions{
		TargetFormat:        sdktranslator.FromString("gemini-cli"),
		TranslateAsStream:   true,
		PayloadConfigRoot:   "request",
		PayloadConfigFormat: "gemini",
	})
	tokenSource, baseTokenData, err := prepareGeminiCLITokenSource(execCtx.Context, e.cfg, auth)
	if err != nil {
		return nil, err
	}

	reporter := execCtx.Reporter()
	defer reporter.trackFailure(execCtx.Context, &err)

	to := execCtx.Execution.TargetFormat
	basePayload, originalTranslated := execCtx.TranslateRequestPair(req.Payload)
	basePayload, err = thinking.ApplyThinking(basePayload, req.Model, execCtx.SourceFormat.String(), to.String(), e.Identifier())
	if err != nil {
		return nil, err
	}

	basePayload = fixGeminiCLIImageAspectRatio(execCtx.BaseModel, basePayload)
	basePayload = execCtx.ApplyPayloadConfig(basePayload, originalTranslated)

	projectID := resolveGeminiProjectID(auth)

	models := cliPreviewFallbackOrder(execCtx.BaseModel)
	if len(models) == 0 || models[0] != execCtx.BaseModel {
		models = append([]string{execCtx.BaseModel}, models...)
	}

	httpClient := execCtx.HTTPClient(0)
	respCtx := context.WithValue(execCtx.Context, "alt", opts.Alt)
	recorder := execCtx.Recorder()

	var lastStatus int
	var lastBody []byte

	for idx, attemptModel := range models {
		payload := append([]byte(nil), basePayload...)
		payload = setJSONField(payload, "project", projectID)
		payload = setJSONField(payload, "model", attemptModel)

		tok, errTok := tokenSource.Token()
		if errTok != nil {
			err = errTok
			return nil, err
		}
		updateGeminiCLITokenMetadata(auth, baseTokenData, tok)

		url := fmt.Sprintf("%s/%s:%s", codeAssistEndpoint, codeAssistVersion, "streamGenerateContent")
		if opts.Alt == "" {
			url = url + "?alt=sse"
		} else {
			url = url + fmt.Sprintf("?$alt=%s", opts.Alt)
		}

		reqHTTP, errReq := http.NewRequestWithContext(execCtx.Context, http.MethodPost, url, bytes.NewReader(payload))
		if errReq != nil {
			err = errReq
			return nil, err
		}
		reqHTTP.Header.Set("Content-Type", "application/json")
		reqHTTP.Header.Set("Authorization", "Bearer "+tok.AccessToken)
		applyGeminiCLIHeaders(reqHTTP, e.cfg, auth)
		reqHTTP.Header.Set("Accept", "text/event-stream")
		recorder.RecordRequest(url, http.MethodPost, reqHTTP.Header.Clone(), payload)

		httpResp, errDo := httpClient.Do(reqHTTP)
		if errDo != nil {
			recorder.RecordResponseError(errDo)
			err = errDo
			return nil, err
		}
		recorder.RecordResponseMetadata(httpResp.StatusCode, httpResp.Header.Clone())
		if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
			data, errRead := readUpstreamResponseBody(e.Identifier(), httpResp.Body)
			if errClose := httpResp.Body.Close(); errClose != nil {
				log.Errorf("gemini cli executor: close response body error: %v", errClose)
			}
			if errRead != nil {
				recorder.RecordResponseError(errRead)
				err = errRead
				return nil, err
			}
			recorder.AppendResponseChunk(data)
			lastStatus = httpResp.StatusCode
			lastBody = append([]byte(nil), data...)
			logWithRequestID(execCtx.Context).Debugf("request error, error status: %d, error message: %s", httpResp.StatusCode, summarizeErrorBody(httpResp.Header.Get("Content-Type"), data))
			if httpResp.StatusCode == 429 {
				if idx+1 < len(models) {
					log.Debugf("gemini cli executor: rate limited, retrying with next model: %s", models[idx+1])
				} else {
					log.Debug("gemini cli executor: rate limited, no additional fallback model")
				}
				continue
			}
			reporter.publishFailureWithContent(execCtx.Context, string(req.Payload), string(data))
			err = newGeminiStatusErr(httpResp.StatusCode, data)
			return nil, err
		}

		out := make(chan cliproxyexecutor.StreamChunk)
		go func(resp *http.Response, reqBody []byte, attemptModel string) {
			defer close(out)
			defer func() {
				if errClose := resp.Body.Close(); errClose != nil {
					log.Errorf("gemini cli executor: close response body error: %v", errClose)
				}
			}()
			if opts.Alt == "" {
				scanner := bufio.NewScanner(resp.Body)
				scanner.Buffer(nil, streamScannerBuffer)
				var param any
				for scanner.Scan() {
					line := scanner.Bytes()
					recorder.AppendResponseChunk(line)
					if detail, ok := parseGeminiCLIStreamUsage(line); ok {
						reporter.publish(execCtx.Context, detail)
					}
					if bytes.HasPrefix(line, dataTag) {
						segments := sdktranslator.TranslateStream(respCtx, to, execCtx.SourceFormat, attemptModel, opts.OriginalRequest, reqBody, bytes.Clone(line), &param)
						for i := range segments {
							out <- cliproxyexecutor.StreamChunk{Payload: []byte(segments[i])}
						}
					}
				}

				segments := sdktranslator.TranslateStream(respCtx, to, execCtx.SourceFormat, attemptModel, opts.OriginalRequest, reqBody, []byte("[DONE]"), &param)
				for i := range segments {
					out <- cliproxyexecutor.StreamChunk{Payload: []byte(segments[i])}
				}
				if errScan := scanner.Err(); errScan != nil {
					recorder.RecordResponseError(errScan)
					reporter.publishFailure(execCtx.Context)
					out <- cliproxyexecutor.StreamChunk{Err: errScan}
				}
				return
			}

			data, errRead := readUpstreamResponseBody(e.Identifier(), resp.Body)
			if errRead != nil {
				recorder.RecordResponseError(errRead)
				reporter.publishFailure(execCtx.Context)
				out <- cliproxyexecutor.StreamChunk{Err: errRead}
				return
			}
			recorder.AppendResponseChunk(data)
			reporter.publish(execCtx.Context, parseGeminiCLIUsage(data))
			var param any
			segments := sdktranslator.TranslateStream(respCtx, to, execCtx.SourceFormat, attemptModel, opts.OriginalRequest, reqBody, data, &param)
			for i := range segments {
				out <- cliproxyexecutor.StreamChunk{Payload: []byte(segments[i])}
			}

			segments = sdktranslator.TranslateStream(respCtx, to, execCtx.SourceFormat, attemptModel, opts.OriginalRequest, reqBody, []byte("[DONE]"), &param)
			for i := range segments {
				out <- cliproxyexecutor.StreamChunk{Payload: []byte(segments[i])}
			}
		}(httpResp, append([]byte(nil), payload...), attemptModel)

		return &cliproxyexecutor.StreamResult{Headers: httpResp.Header.Clone(), Chunks: out}, nil
	}

	if len(lastBody) > 0 {
		recorder.AppendResponseChunk(lastBody)
	}
	if lastStatus == 0 {
		lastStatus = 429
	}
	reporter.publishFailureWithContent(execCtx.Context, string(req.Payload), string(lastBody))
	err = newGeminiStatusErr(lastStatus, lastBody)
	return nil, err
}

// CountTokens counts tokens for the given request using the Gemini CLI API.
func (e *GeminiCLIExecutor) CountTokens(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	execCtx := newExecutionContext(ctx, e.Identifier(), e.cfg, auth, req, opts, ExecutionOptions{
		TargetFormat: sdktranslator.FromString("gemini-cli"),
	})
	tokenSource, baseTokenData, err := prepareGeminiCLITokenSource(execCtx.Context, e.cfg, auth)
	if err != nil {
		return cliproxyexecutor.Response{}, err
	}

	to := execCtx.Execution.TargetFormat
	basePayload, _ := execCtx.TranslateRequestPair(req.Payload)

	models := cliPreviewFallbackOrder(execCtx.BaseModel)
	if len(models) == 0 || models[0] != execCtx.BaseModel {
		models = append([]string{execCtx.BaseModel}, models...)
	}

	httpClient := execCtx.HTTPClient(0)
	respCtx := context.WithValue(execCtx.Context, "alt", opts.Alt)
	recorder := execCtx.Recorder()

	var lastStatus int
	var lastBody []byte

	// The loop variable attemptModel is only used as the concrete model id sent to the upstream
	// Gemini CLI endpoint when iterating fallback variants.
	for range models {
		payload := append([]byte(nil), basePayload...)

		payload, err = thinking.ApplyThinking(payload, req.Model, execCtx.SourceFormat.String(), to.String(), e.Identifier())
		if err != nil {
			return cliproxyexecutor.Response{}, err
		}

		payload = deleteJSONField(payload, "project")
		payload = deleteJSONField(payload, "model")
		payload = deleteJSONField(payload, "request.safetySettings")
		payload = fixGeminiCLIImageAspectRatio(execCtx.BaseModel, payload)

		tok, errTok := tokenSource.Token()
		if errTok != nil {
			return cliproxyexecutor.Response{}, errTok
		}
		updateGeminiCLITokenMetadata(auth, baseTokenData, tok)

		url := fmt.Sprintf("%s/%s:%s", codeAssistEndpoint, codeAssistVersion, "countTokens")
		if opts.Alt != "" {
			url = url + fmt.Sprintf("?$alt=%s", opts.Alt)
		}

		reqHTTP, errReq := http.NewRequestWithContext(execCtx.Context, http.MethodPost, url, bytes.NewReader(payload))
		if errReq != nil {
			return cliproxyexecutor.Response{}, errReq
		}
		reqHTTP.Header.Set("Content-Type", "application/json")
		reqHTTP.Header.Set("Authorization", "Bearer "+tok.AccessToken)
		applyGeminiCLIHeaders(reqHTTP, e.cfg, auth)
		reqHTTP.Header.Set("Accept", "application/json")
		recorder.RecordRequest(url, http.MethodPost, reqHTTP.Header.Clone(), payload)

		resp, errDo := httpClient.Do(reqHTTP)
		if errDo != nil {
			recorder.RecordResponseError(errDo)
			return cliproxyexecutor.Response{}, errDo
		}
		data, errRead := readUpstreamResponseBody(e.Identifier(), resp.Body)
		_ = resp.Body.Close()
		recorder.RecordResponseMetadata(resp.StatusCode, resp.Header.Clone())
		if errRead != nil {
			recorder.RecordResponseError(errRead)
			return cliproxyexecutor.Response{}, errRead
		}
		recorder.AppendResponseChunk(data)
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			count := gjson.GetBytes(data, "totalTokens").Int()
			translated := sdktranslator.TranslateTokenCount(respCtx, to, execCtx.SourceFormat, count, data)
			return cliproxyexecutor.Response{Payload: []byte(translated), Headers: resp.Header.Clone()}, nil
		}
		lastStatus = resp.StatusCode
		lastBody = append([]byte(nil), data...)
		if resp.StatusCode == 429 {
			log.Debugf("gemini cli executor: rate limited, retrying with next model")
			continue
		}
		break
	}

	if lastStatus == 0 {
		lastStatus = 429
	}
	return cliproxyexecutor.Response{}, newGeminiStatusErr(lastStatus, lastBody)
}

// Refresh refreshes the authentication credentials (no-op for Gemini CLI).
func (e *GeminiCLIExecutor) Refresh(_ context.Context, auth *cliproxyauth.Auth) (*cliproxyauth.Auth, error) {
	return auth, nil
}
