package executor

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const (
	minimaxImageGenerationAlt = "images/generations"

	// minimaxDefaultBaseURL is the global host. The regional host
	// (https://api.minimaxi.com/v1) is selected by setting base_url on the
	// credential, which is also how an operator points at a relay.
	minimaxDefaultBaseURL = "https://api.minimax.io/v1"

	minimaxAPIVersionSegment   = "/v1"
	minimaxImageGenerationPath = "/image_generation"
)

// minimaxIsMediaAlt reports whether a request targets the image endpoint rather
// than the chat surface.
//
// Only generation is claimed here. The reference-image form of these models is a
// different upstream request shape, not a variant of this call, so an edit request
// is left to fail as an unsupported model rather than being silently answered with
// a text-to-image result.
func minimaxIsMediaAlt(alt string) bool {
	return strings.TrimSpace(alt) == minimaxImageGenerationAlt
}

// minimaxImageBaseURL returns the upstream host for image requests.
func minimaxImageBaseURL(auth *cliproxyauth.Auth) string {
	baseURL := ""
	if auth != nil && auth.Attributes != nil {
		baseURL = strings.TrimSpace(auth.Attributes["base_url"])
	}
	if baseURL == "" {
		return minimaxDefaultBaseURL
	}
	return strings.TrimSuffix(baseURL, "/")
}

// minimaxImageEndpoint resolves the full image-generation URL.
//
// Credentials configured for chat already carry a versioned base URL, so the
// version segment is appended only when the operator left it off.
func minimaxImageEndpoint(auth *cliproxyauth.Auth) string {
	base := minimaxImageBaseURL(auth)
	if strings.HasSuffix(base, minimaxAPIVersionSegment) {
		return base + minimaxImageGenerationPath
	}
	return base + minimaxAPIVersionSegment + minimaxImageGenerationPath
}

// executeImageGeneration forwards an image request to the MiniMax image endpoint.
func (e *MiniMaxExecutor) executeImageGeneration(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (resp cliproxyexecutor.Response, err error) {
	execCtx := newExecutionContext(ctx, e.Identifier(), e.cfg, auth, req, opts, ExecutionOptions{})
	reporter := execCtx.Reporter()
	defer reporter.trackFailure(execCtx.Context, &err)

	if _, apiKey := e.resolveCredentials(auth); strings.TrimSpace(apiKey) == "" {
		return resp, statusErr{code: http.StatusUnauthorized, msg: "minimax credential has no api key"}
	}

	payload := req.Payload
	if len(bytes.TrimSpace(payload)) == 0 {
		return resp, statusErr{code: http.StatusBadRequest, msg: "image request body is empty"}
	}

	payload, dropped := shapeMiniMaxImageRequest(payload)
	if len(dropped) > 0 {
		logWithRequestID(execCtx.Context).Debugf(
			"minimax image request: dropped unsupported arguments %s", strings.Join(dropped, ", "),
		)
	}

	endpoint := minimaxImageEndpoint(auth)
	httpReq, err := http.NewRequestWithContext(execCtx.Context, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return resp, err
	}
	// Reuses the credential handling every request for this provider goes through:
	// bearer authorization plus any operator-supplied custom headers.
	if err = e.PrepareRequest(httpReq, auth); err != nil {
		return resp, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "application/json")

	recorder := execCtx.Recorder()
	recorder.RecordRequest(endpoint, http.MethodPost, httpReq.Header.Clone(), payload)

	httpResp, err := execCtx.HTTPClient(0).Do(httpReq) //nolint:bodyclose // closed by the defer below.
	if err != nil {
		recorder.RecordResponseError(err)
		reporter.publishFailureWithContentBytes(execCtx.Context, req.Payload, err.Error())
		return resp, err
	}
	defer func() {
		if errClose := httpResp.Body.Close(); errClose != nil {
			log.Errorf("minimax executor: close image response body error: %v", errClose)
		}
	}()

	recorder.RecordResponseMetadata(httpResp.StatusCode, httpResp.Header.Clone())
	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		body := readUpstreamErrorBody(e.Identifier(), httpResp.Body)
		recorder.AppendResponseChunk(body)
		logWithRequestID(execCtx.Context).Debugf(
			"minimax image request error, status: %d, message: %s",
			httpResp.StatusCode, summarizeErrorBody(httpResp.Header.Get("Content-Type"), body),
		)
		reporter.publishFailureWithContentBytes(execCtx.Context, req.Payload, string(body))
		return resp, statusErr{code: httpResp.StatusCode, msg: string(body), upstreamBody: body}
	}

	data, err := readUpstreamResponseBody(e.Identifier(), httpResp.Body)
	if err != nil {
		recorder.RecordResponseError(err)
		return resp, err
	}
	recorder.AppendResponseChunk(data)

	translated, err := translateMiniMaxImageResponse(data)
	if err != nil {
		reporter.publishFailureWithContentBytes(execCtx.Context, req.Payload, err.Error())
		return resp, err
	}

	return cliproxyexecutor.Response{Payload: translated, Headers: minimaxImageResponseHeaders(httpResp.Header)}, nil
}

// executeImageGenerationStream serves the streaming entry point.
//
// The image endpoint has no streaming form, so a single terminal chunk is emitted
// rather than refusing the request: callers reaching this through the shared images
// handler should not have to know which providers stream.
func (e *MiniMaxExecutor) executeImageGenerationStream(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	resp, err := e.executeImageGeneration(ctx, auth, req, opts)
	if err != nil {
		return nil, err
	}
	chunks := make(chan cliproxyexecutor.StreamChunk, 1)
	chunks <- cliproxyexecutor.StreamChunk{Payload: resp.Payload}
	close(chunks)
	return &cliproxyexecutor.StreamResult{Chunks: chunks}, nil
}

// translateMiniMaxImageResponse rewrites an image response into the public shape.
//
// Two things make a pass-through wrong here. The images are reported under
// data.image_urls or data.image_base64 rather than as a list of image objects, and
// failures arrive with HTTP 200 and a non-zero base_resp.status_code — forwarded
// untranslated, an authentication failure would reach the caller as a successful
// response carrying no images.
func translateMiniMaxImageResponse(payload []byte) ([]byte, error) {
	parsed := gjson.ParseBytes(payload)
	if !parsed.IsObject() {
		return nil, statusErr{code: http.StatusBadGateway, msg: "minimax image response is not a JSON object", upstreamBody: payload}
	}

	if status := parsed.Get("base_resp.status_code"); status.Exists() && status.Int() != 0 {
		message := strings.TrimSpace(parsed.Get("base_resp.status_msg").String())
		if message == "" {
			message = fmt.Sprintf("minimax image generation failed with status %d", status.Int())
		}
		return nil, statusErr{code: minimaxImageHTTPStatus(status.Int()), msg: message, upstreamBody: payload}
	}

	translated := []byte(`{"data":[]}`)
	count := 0
	appendEntry := func(field, value string) {
		value = strings.TrimSpace(value)
		if value == "" {
			return
		}
		next, err := sjson.SetBytes(translated, fmt.Sprintf("data.%d.%s", count, field), value)
		if err != nil {
			return
		}
		translated = next
		count++
	}
	for _, entry := range parsed.Get("data.image_urls").Array() {
		appendEntry("url", entry.String())
	}
	for _, entry := range parsed.Get("data.image_base64").Array() {
		appendEntry("b64_json", entry.String())
	}

	if count == 0 {
		// A response that generated nothing is a failure even though the call
		// succeeded; reporting it as success would render as a blank result with no
		// explanation. Content-safety rejections are the caller's request to fix,
		// so they are reported as such rather than as an upstream fault.
		if failed := parsed.Get("metadata.failed_count"); failed.Int() > 0 {
			return nil, statusErr{
				code:         http.StatusBadRequest,
				msg:          fmt.Sprintf("minimax image generation returned no images; %d were rejected by content safety", failed.Int()),
				upstreamBody: payload,
			}
		}
		return nil, statusErr{code: http.StatusBadGateway, msg: "minimax image generation returned no images", upstreamBody: payload}
	}

	if updated, err := sjson.SetBytes(translated, "created", time.Now().Unix()); err == nil {
		translated = updated
	}
	return translated, nil
}

// minimaxImageHTTPStatus maps a base_resp status code onto an HTTP status.
//
// The mapping matters beyond the message the caller sees: a balance failure has to
// surface as 402 so it lands in the same quota cooldown path as every other
// provider, and rate limiting has to surface as 429 to be retried rather than
// counted as a permanent failure.
func minimaxImageHTTPStatus(code int64) int {
	switch code {
	case 1002:
		return http.StatusTooManyRequests
	case 1004, 2049:
		return http.StatusUnauthorized
	case 1008:
		return http.StatusPaymentRequired
	case 1026, 2013:
		return http.StatusBadRequest
	default:
		return http.StatusBadGateway
	}
}

// minimaxImageResponseHeaders forwards the upstream headers that still describe the
// response after translation.
func minimaxImageResponseHeaders(src http.Header) http.Header {
	if src == nil {
		return nil
	}
	headers := src.Clone()
	// The body was rewritten, so the upstream length and encoding no longer
	// describe it; forwarding them would truncate or mis-decode the response.
	headers.Del("Content-Length")
	headers.Del("Content-Encoding")
	return headers
}
