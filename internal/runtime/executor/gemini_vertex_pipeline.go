package executor

import (
	"bufio"
	"bytes"
	"context"
	"net/http"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/thinking"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v6/sdk/translator"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

func (e *GeminiVertexExecutor) newVertexExecutionContext(
	ctx context.Context,
	auth *cliproxyauth.Auth,
	req cliproxyexecutor.Request,
	opts cliproxyexecutor.Options,
	stream bool,
) *ExecutionContext {
	return newExecutionContext(ctx, e.Identifier(), e.cfg, auth, req, opts, ExecutionOptions{
		TargetFormat:      sdktranslator.FromString("gemini"),
		TranslateAsStream: stream,
	})
}

func (e *GeminiVertexExecutor) buildVertexPayload(execCtx *ExecutionContext) ([]byte, error) {
	if execCtx == nil {
		return nil, nil
	}
	if isImagenModel(execCtx.BaseModel) {
		return convertToImagenRequest(execCtx.Request.Payload)
	}

	body, originalTranslated := execCtx.TranslateRequestPair(execCtx.Request.Payload)
	body, err := thinking.ApplyThinking(body, execCtx.Request.Model, execCtx.SourceFormat.String(), execCtx.Execution.TargetFormat.String(), e.Identifier())
	if err != nil {
		return nil, err
	}

	body = fixGeminiImageAspectRatio(execCtx.BaseModel, body)
	body = execCtx.ApplyPayloadConfig(body, originalTranslated)
	body, _ = sjson.SetBytes(body, "model", execCtx.BaseModel)
	return body, nil
}

func (e *GeminiVertexExecutor) buildVertexCountTokensPayload(execCtx *ExecutionContext) ([]byte, error) {
	if execCtx == nil {
		return nil, nil
	}

	body, _ := execCtx.TranslateRequestPair(execCtx.Request.Payload)
	body, err := thinking.ApplyThinking(body, execCtx.Request.Model, execCtx.SourceFormat.String(), execCtx.Execution.TargetFormat.String(), e.Identifier())
	if err != nil {
		return nil, err
	}

	body = fixGeminiImageAspectRatio(execCtx.BaseModel, body)
	body, _ = sjson.SetBytes(body, "model", execCtx.BaseModel)
	body, _ = sjson.DeleteBytes(body, "tools")
	body, _ = sjson.DeleteBytes(body, "generationConfig")
	body, _ = sjson.DeleteBytes(body, "safetySettings")
	return body, nil
}

func (e *GeminiVertexExecutor) newVertexHTTPRequest(
	ctx context.Context,
	auth *cliproxyauth.Auth,
	url string,
	body []byte,
	apiKey string,
	saJSON []byte,
) (*http.Request, error) {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if apiKey != "" {
		httpReq.Header.Set("x-goog-api-key", apiKey)
	} else if token, errTok := vertexAccessToken(ctx, e.cfg, auth, saJSON); errTok == nil && token != "" {
		httpReq.Header.Set("Authorization", "Bearer "+token)
	} else if errTok != nil {
		log.Errorf("vertex executor: access token error: %v", errTok)
		return nil, statusErr{code: 500, msg: "internal server error"}
	}
	applyGeminiHeaders(httpReq, auth)
	return httpReq, nil
}

func (e *GeminiVertexExecutor) executeVertexNonStream(
	execCtx *ExecutionContext,
	auth *cliproxyauth.Auth,
	body []byte,
	url string,
	apiKey string,
	saJSON []byte,
) (resp cliproxyexecutor.Response, err error) {
	reporter := execCtx.Reporter()
	defer reporter.trackFailure(execCtx.Context, &err)

	httpReq, err := e.newVertexHTTPRequest(execCtx.Context, auth, url, body, apiKey, saJSON)
	if err != nil {
		return resp, err
	}

	recorder := execCtx.Recorder()
	recorder.RecordRequest(url, http.MethodPost, httpReq.Header.Clone(), body)

	//nolint:bodyclose // body is closed by the defer below.
	httpResp, err := execCtx.HTTPClient(0).Do(httpReq)
	if err != nil {
		recorder.RecordResponseError(err)
		return resp, err
	}
	defer func() {
		if errClose := httpResp.Body.Close(); errClose != nil {
			log.Errorf("vertex executor: close response body error: %v", errClose)
		}
	}()

	recorder.RecordResponseMetadata(httpResp.StatusCode, httpResp.Header.Clone())
	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		b := readUpstreamErrorBody(e.Identifier(), httpResp.Body)
		recorder.AppendResponseChunk(b)
		logWithRequestID(execCtx.Context).Debugf("request error, error status: %d, error message: %s", httpResp.StatusCode, summarizeErrorBody(httpResp.Header.Get("Content-Type"), b))
		reporter.publishFailureWithContent(execCtx.Context, string(execCtx.Request.Payload), string(b))
		err = statusErr{code: httpResp.StatusCode, msg: string(b)}
		return resp, err
	}

	data, err := readUpstreamResponseBody(e.Identifier(), httpResp.Body)
	if err != nil {
		recorder.RecordResponseError(err)
		return resp, err
	}
	recorder.AppendResponseChunk(data)
	reporter.publish(execCtx.Context, parseGeminiUsage(data))

	if isImagenModel(execCtx.BaseModel) {
		data = convertImagenToGeminiResponse(data, execCtx.BaseModel)
	}

	var param any
	out := sdktranslator.TranslateNonStream(execCtx.Context, execCtx.Execution.TargetFormat, execCtx.SourceFormat, execCtx.Request.Model, execCtx.OriginalPayload, body, data, &param)
	resp = cliproxyexecutor.Response{Payload: []byte(out), Headers: httpResp.Header.Clone()}
	return resp, nil
}

func (e *GeminiVertexExecutor) executeVertexStream(
	execCtx *ExecutionContext,
	auth *cliproxyauth.Auth,
	body []byte,
	url string,
	apiKey string,
	saJSON []byte,
) (_ *cliproxyexecutor.StreamResult, err error) {
	reporter := execCtx.Reporter()
	defer reporter.trackFailure(execCtx.Context, &err)

	httpReq, err := e.newVertexHTTPRequest(execCtx.Context, auth, url, body, apiKey, saJSON)
	if err != nil {
		return nil, err
	}

	recorder := execCtx.Recorder()
	recorder.RecordRequest(url, http.MethodPost, httpReq.Header.Clone(), body)

	//nolint:bodyclose // success body is consumed and closed by the stream goroutine below.
	httpResp, err := execCtx.HTTPClient(0).Do(httpReq)
	if err != nil {
		recorder.RecordResponseError(err)
		return nil, err
	}
	recorder.RecordResponseMetadata(httpResp.StatusCode, httpResp.Header.Clone())
	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		b := readUpstreamErrorBody(e.Identifier(), httpResp.Body)
		recorder.AppendResponseChunk(b)
		logWithRequestID(execCtx.Context).Debugf("request error, error status: %d, error message: %s", httpResp.StatusCode, summarizeErrorBody(httpResp.Header.Get("Content-Type"), b))
		reporter.publishFailureWithContent(execCtx.Context, string(execCtx.Request.Payload), string(b))
		if errClose := httpResp.Body.Close(); errClose != nil {
			log.Errorf("vertex executor: close response body error: %v", errClose)
		}
		return nil, statusErr{code: httpResp.StatusCode, msg: string(b)}
	}

	out := make(chan cliproxyexecutor.StreamChunk)
	go func() {
		defer close(out)
		defer func() {
			if errClose := httpResp.Body.Close(); errClose != nil {
				log.Errorf("vertex executor: close response body error: %v", errClose)
			}
		}()

		scanner := bufio.NewScanner(httpResp.Body)
		scanner.Buffer(nil, streamScannerBuffer)
		var param any
		for scanner.Scan() {
			line := scanner.Bytes()
			recorder.AppendResponseChunk(line)
			if detail, ok := parseGeminiStreamUsage(line); ok {
				reporter.publish(execCtx.Context, detail)
			}
			lines := sdktranslator.TranslateStream(execCtx.Context, execCtx.Execution.TargetFormat, execCtx.SourceFormat, execCtx.Request.Model, execCtx.OriginalPayload, body, bytes.Clone(line), &param)
			for i := range lines {
				out <- cliproxyexecutor.StreamChunk{Payload: []byte(lines[i])}
			}
		}
		lines := sdktranslator.TranslateStream(execCtx.Context, execCtx.Execution.TargetFormat, execCtx.SourceFormat, execCtx.Request.Model, execCtx.OriginalPayload, body, []byte("[DONE]"), &param)
		for i := range lines {
			out <- cliproxyexecutor.StreamChunk{Payload: []byte(lines[i])}
		}
		if errScan := scanner.Err(); errScan != nil {
			recorder.RecordResponseError(errScan)
			reporter.publishFailure(execCtx.Context)
			out <- cliproxyexecutor.StreamChunk{Err: errScan}
		}
	}()
	return &cliproxyexecutor.StreamResult{Headers: httpResp.Header.Clone(), Chunks: out}, nil
}

func (e *GeminiVertexExecutor) executeVertexCountTokens(
	execCtx *ExecutionContext,
	auth *cliproxyauth.Auth,
	body []byte,
	url string,
	apiKey string,
	saJSON []byte,
) (cliproxyexecutor.Response, error) {
	httpReq, err := e.newVertexHTTPRequest(execCtx.Context, auth, url, body, apiKey, saJSON)
	if err != nil {
		return cliproxyexecutor.Response{}, err
	}

	recorder := execCtx.Recorder()
	recorder.RecordRequest(url, http.MethodPost, httpReq.Header.Clone(), body)

	httpResp, err := execCtx.HTTPClient(0).Do(httpReq)
	if err != nil {
		recorder.RecordResponseError(err)
		return cliproxyexecutor.Response{}, err
	}
	defer func() {
		if errClose := httpResp.Body.Close(); errClose != nil {
			log.Errorf("vertex executor: close response body error: %v", errClose)
		}
	}()

	recorder.RecordResponseMetadata(httpResp.StatusCode, httpResp.Header.Clone())
	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		b := readUpstreamErrorBody(e.Identifier(), httpResp.Body)
		recorder.AppendResponseChunk(b)
		logWithRequestID(execCtx.Context).Debugf("request error, error status: %d, error message: %s", httpResp.StatusCode, summarizeErrorBody(httpResp.Header.Get("Content-Type"), b))
		return cliproxyexecutor.Response{}, statusErr{code: httpResp.StatusCode, msg: string(b)}
	}

	data, err := readUpstreamResponseBody(e.Identifier(), httpResp.Body)
	if err != nil {
		recorder.RecordResponseError(err)
		return cliproxyexecutor.Response{}, err
	}
	recorder.AppendResponseChunk(data)

	count := gjson.GetBytes(data, "totalTokens").Int()
	respCtx := context.WithValue(execCtx.Context, "alt", execCtx.Options.Alt)
	out := sdktranslator.TranslateTokenCount(respCtx, execCtx.Execution.TargetFormat, execCtx.SourceFormat, count, data)
	return cliproxyexecutor.Response{Payload: []byte(out), Headers: httpResp.Header.Clone()}, nil
}
