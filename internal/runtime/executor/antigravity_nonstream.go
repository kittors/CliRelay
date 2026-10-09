package executor

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v6/sdk/translator"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// executeViaStreamEndpoint serves a non-streaming request by calling the
// upstream's streaming endpoint and assembling the chunks into one response.
//
// Named for what it does rather than for claude, which is all it used to serve:
// the endpoint choice is about which upstream path actually works, not about
// which model family asked.
func (e *AntigravityExecutor) executeViaStreamEndpoint(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (resp cliproxyexecutor.Response, err error) {
	if ctx == nil {
		ctx = context.Background()
	}

	token, updatedAuth, errToken := e.ensureAccessToken(ctx, auth)
	if errToken != nil {
		return resp, errToken
	}
	if updatedAuth != nil {
		auth = updatedAuth
	}

	execCtx := e.newAntigravityExecutionContext(ctx, auth, req, opts, true)
	reporter := execCtx.Reporter()
	defer reporter.trackFailure(execCtx.Context, &err)
	translated, err := e.buildAntigravityPayload(execCtx)
	if err != nil {
		return resp, err
	}

	baseURLs := antigravityBaseURLFallbackOrder(auth)
	httpClient := execCtx.HTTPClient(0)
	recorder := execCtx.Recorder()

	attempts := antigravityRetryAttempts(auth, e.cfg)
	var locationGate antigravityLocationGateBudget
	sessionGeneration := 0
	sessionBumpsLeft := antigravitySessionBumpMaxAttempts

attemptLoop:
	for attempt := 0; attempt < attempts; attempt++ {
		var lastStatus int
		var lastBody []byte
		var lastErr error

		for idx, baseURL := range baseURLs {
			httpReq, requestBody, errReq := e.buildRequest(execCtx.Context, auth, token, execCtx.BaseModel, translated, true, opts.Alt, baseURL, sessionGeneration)
			if errReq != nil {
				err = errReq
				return resp, err
			}
			recorder.RecordRequest(httpReq.URL.String(), httpReq.Method, httpReq.Header.Clone(), requestBody)

			httpResp, errDo := httpClient.Do(httpReq)
			if errDo != nil {
				recorder.RecordResponseError(errDo)
				if errors.Is(errDo, context.Canceled) || errors.Is(errDo, context.DeadlineExceeded) {
					return resp, errDo
				}
				lastStatus = 0
				lastBody = nil
				lastErr = errDo
				if idx+1 < len(baseURLs) {
					log.Debugf("antigravity executor: request error on base url %s, retrying with fallback base url: %s", baseURL, baseURLs[idx+1])
					continue
				}
				err = errDo
				return resp, err
			}
			recorder.RecordResponseMetadata(httpResp.StatusCode, httpResp.Header.Clone())
			if httpResp.StatusCode < http.StatusOK || httpResp.StatusCode >= http.StatusMultipleChoices {
				bodyBytes, errRead := func() ([]byte, error) {
					return readUpstreamErrorBody(e.Identifier(), httpResp.Body), nil
				}()
				if errClose := httpResp.Body.Close(); errClose != nil {
					log.Errorf("antigravity executor: close response body error: %v", errClose)
				}
				if errRead != nil {
					recorder.RecordResponseError(errRead)
					if errors.Is(errRead, context.Canceled) || errors.Is(errRead, context.DeadlineExceeded) {
						err = errRead
						return resp, err
					}
					if errCtx := execCtx.Context.Err(); errCtx != nil {
						err = errCtx
						return resp, err
					}
					lastStatus = 0
					lastBody = nil
					lastErr = errRead
					if idx+1 < len(baseURLs) {
						log.Debugf("antigravity executor: read error on base url %s, retrying with fallback base url: %s", baseURL, baseURLs[idx+1])
						continue
					}
					err = errRead
					return resp, err
				}
				recorder.AppendResponseChunk(bodyBytes)
				lastStatus = httpResp.StatusCode
				lastBody = append([]byte(nil), bodyBytes...)
				lastErr = nil
				// A long tool-loop eventually pushes the server-side session past
				// the 1,048,576-token ceiling, after which every request on that
				// session id fails 400. Rotate the session id and retry on a fresh
				// upstream session. This is a recovery, not a capacity retry, so it
				// keeps its own budget (sessionBumpsLeft) and does not consume an
				// attempt.
				if sessionBumpsLeft > 0 && antigravityShouldBumpSession(httpResp.StatusCode, bodyBytes) {
					sessionGeneration++
					sessionBumpsLeft--
					attempt--
					log.Debugf("antigravity executor: session input exceeds the 1M ceiling for model %s, rotating session id (generation %d) and retrying on a fresh session", execCtx.BaseModel, sessionGeneration)
					continue attemptLoop
				}
				// 429 is an account-level RESOURCE_EXHAUSTED signal, not a
				// host-specific hiccup: see antigravity_executor.go for the
				// production trace that motivated dropping this fallback.
				//
				// The location gate is the opposite: it is a per-attempt draw
				// (antigravityShouldRetryHost), so the next host and the next
				// attempt are both worth taking before failing the client —
				// within the antigravityLocationGateMaxAttempts budget, so an
				// account that is really blocked still fails fast.
				retryNoCapacity := antigravityShouldRetryNoCapacity(httpResp.StatusCode, bodyBytes)
				retryLocation := !retryNoCapacity && antigravityShouldRetryHost(httpResp.StatusCode, bodyBytes)
				if retryNoCapacity || retryLocation {
					if idx+1 < len(baseURLs) {
						log.Debugf("antigravity executor: retryable upstream status %d on base url %s, retrying with fallback base url: %s", httpResp.StatusCode, baseURL, baseURLs[idx+1])
						continue
					}
					if retryLocation && !locationGate.consume() {
						log.Debugf("antigravity executor: location gate persisted across %d attempts for model %s, failing fast", antigravityLocationGateMaxAttempts, execCtx.BaseModel)
					} else if attempt+1 < attempts {
						delay := antigravityNoCapacityRetryDelay(attempt)
						log.Debugf("antigravity executor: retryable upstream status %d for model %s, retrying in %s (attempt %d/%d)", httpResp.StatusCode, execCtx.BaseModel, delay, attempt+1, attempts)
						if errWait := antigravityWait(ctx, delay); errWait != nil {
							return resp, errWait
						}
						continue attemptLoop
					}
				}
				sErr := statusErr{code: httpResp.StatusCode, msg: string(bodyBytes)}
				if httpResp.StatusCode == http.StatusTooManyRequests {
					if retryAfter, parseErr := parseRetryDelay(bodyBytes); parseErr == nil && retryAfter != nil {
						sErr.retryAfter = retryAfter
					}
				}
				err = sErr
				return resp, err
			}

			out := make(chan cliproxyexecutor.StreamChunk)
			reporter.setInputContent(string(req.Payload))
			go func(resp *http.Response) {
				defer close(out)
				defer func() {
					if errClose := resp.Body.Close(); errClose != nil {
						log.Errorf("antigravity executor: close response body error: %v", errClose)
					}
				}()
				scanner := bufio.NewScanner(resp.Body)
				scanner.Buffer(nil, streamScannerBuffer)
				for scanner.Scan() {
					line := scanner.Bytes()
					recorder.AppendResponseChunk(line)
					reporter.appendOutputChunk(line)

					line = FilterSSEUsageMetadata(line)

					payload := jsonPayload(line)
					if payload == nil {
						continue
					}

					if detail, ok := parseAntigravityStreamUsage(payload); ok {
						reporter.publish(execCtx.Context, detail)
					}

					out <- cliproxyexecutor.StreamChunk{Payload: payload}
				}
				if errScan := scanner.Err(); errScan != nil {
					recorder.RecordResponseError(errScan)
					reporter.publishFailure(execCtx.Context)
					out <- cliproxyexecutor.StreamChunk{Err: errScan}
				} else {
					reporter.ensurePublished(execCtx.Context)
				}
			}(httpResp)

			var buffer bytes.Buffer
			for chunk := range out {
				if chunk.Err != nil {
					return resp, chunk.Err
				}
				if len(chunk.Payload) > 0 {
					_, _ = buffer.Write(chunk.Payload)
					_, _ = buffer.Write([]byte("\n"))
				}
			}
			resp = cliproxyexecutor.Response{Payload: e.convertStreamToNonStream(buffer.Bytes())}

			reporter.publishWithContent(execCtx.Context, parseAntigravityUsage(resp.Payload), string(req.Payload), string(resp.Payload))
			var param any
			converted := sdktranslator.TranslateNonStream(execCtx.Context, execCtx.Execution.TargetFormat, execCtx.SourceFormat, req.Model, execCtx.OriginalPayload, translated, resp.Payload, &param)
			resp = cliproxyexecutor.Response{Payload: []byte(converted), Headers: httpResp.Header.Clone()}
			reporter.ensurePublished(execCtx.Context)

			return resp, nil
		}

		switch {
		case lastStatus != 0:
			sErr := statusErr{code: lastStatus, msg: string(lastBody)}
			if lastStatus == http.StatusTooManyRequests {
				if retryAfter, parseErr := parseRetryDelay(lastBody); parseErr == nil && retryAfter != nil {
					sErr.retryAfter = retryAfter
				}
			}
			err = sErr
		case lastErr != nil:
			err = lastErr
		default:
			err = statusErr{code: http.StatusServiceUnavailable, msg: "antigravity executor: no base url available"}
		}
		return resp, err
	}

	return resp, err
}

func (e *AntigravityExecutor) convertStreamToNonStream(stream []byte) []byte {
	responseTemplate := ""
	var traceID string
	var finishReason string
	var modelVersion string
	var responseID string
	var role string
	var usageRaw string
	parts := make([]map[string]interface{}, 0)
	var pendingKind string
	var pendingText strings.Builder
	var pendingThoughtSig string

	flushPending := func() {
		if pendingKind == "" {
			return
		}
		text := pendingText.String()
		switch pendingKind {
		case "text":
			if strings.TrimSpace(text) == "" {
				pendingKind = ""
				pendingText.Reset()
				pendingThoughtSig = ""
				return
			}
			parts = append(parts, map[string]interface{}{"text": text})
		case "thought":
			if strings.TrimSpace(text) == "" && pendingThoughtSig == "" {
				pendingKind = ""
				pendingText.Reset()
				pendingThoughtSig = ""
				return
			}
			part := map[string]interface{}{"thought": true}
			part["text"] = text
			if pendingThoughtSig != "" {
				part["thoughtSignature"] = pendingThoughtSig
			}
			parts = append(parts, part)
		}
		pendingKind = ""
		pendingText.Reset()
		pendingThoughtSig = ""
	}

	normalizePart := func(partResult gjson.Result) map[string]interface{} {
		var m map[string]interface{}
		_ = json.Unmarshal([]byte(partResult.Raw), &m)
		if m == nil {
			m = map[string]interface{}{}
		}
		sig := partResult.Get("thoughtSignature").String()
		if sig == "" {
			sig = partResult.Get("thought_signature").String()
		}
		if sig != "" {
			m["thoughtSignature"] = sig
			delete(m, "thought_signature")
		}
		if inlineData, ok := m["inline_data"]; ok {
			m["inlineData"] = inlineData
			delete(m, "inline_data")
		}
		return m
	}

	for _, line := range bytes.Split(stream, []byte("\n")) {
		trimmed := bytes.TrimSpace(line)
		if len(trimmed) == 0 || !gjson.ValidBytes(trimmed) {
			continue
		}

		root := gjson.ParseBytes(trimmed)
		responseNode := root.Get("response")
		if !responseNode.Exists() {
			if root.Get("candidates").Exists() {
				responseNode = root
			} else {
				continue
			}
		}
		responseTemplate = responseNode.Raw

		if traceResult := root.Get("traceId"); traceResult.Exists() && traceResult.String() != "" {
			traceID = traceResult.String()
		}

		if roleResult := responseNode.Get("candidates.0.content.role"); roleResult.Exists() {
			role = roleResult.String()
		}

		if finishResult := responseNode.Get("candidates.0.finishReason"); finishResult.Exists() && finishResult.String() != "" {
			finishReason = finishResult.String()
		}

		if modelResult := responseNode.Get("modelVersion"); modelResult.Exists() && modelResult.String() != "" {
			modelVersion = modelResult.String()
		}
		if responseIDResult := responseNode.Get("responseId"); responseIDResult.Exists() && responseIDResult.String() != "" {
			responseID = responseIDResult.String()
		}
		if usageResult := responseNode.Get("usageMetadata"); usageResult.Exists() {
			usageRaw = usageResult.Raw
		} else if usageMetadataResult := root.Get("usageMetadata"); usageMetadataResult.Exists() {
			usageRaw = usageMetadataResult.Raw
		}

		if partsResult := responseNode.Get("candidates.0.content.parts"); partsResult.IsArray() {
			for _, part := range partsResult.Array() {
				hasFunctionCall := part.Get("functionCall").Exists()
				hasInlineData := part.Get("inlineData").Exists() || part.Get("inline_data").Exists()
				sig := part.Get("thoughtSignature").String()
				if sig == "" {
					sig = part.Get("thought_signature").String()
				}
				text := part.Get("text").String()
				thought := part.Get("thought").Bool()

				if hasFunctionCall || hasInlineData {
					flushPending()
					parts = append(parts, normalizePart(part))
					continue
				}

				if thought || part.Get("text").Exists() {
					kind := "text"
					if thought {
						kind = "thought"
					}
					if pendingKind != "" && pendingKind != kind {
						flushPending()
					}
					pendingKind = kind
					pendingText.WriteString(text)
					if kind == "thought" && sig != "" {
						pendingThoughtSig = sig
					}
					continue
				}

				flushPending()
				parts = append(parts, normalizePart(part))
			}
		}
	}
	flushPending()

	if responseTemplate == "" {
		responseTemplate = `{"candidates":[{"content":{"role":"model","parts":[]}}]}`
	}

	partsJSON, _ := json.Marshal(parts)
	responseTemplate, _ = sjson.SetRaw(responseTemplate, "candidates.0.content.parts", string(partsJSON))
	if role != "" {
		responseTemplate, _ = sjson.Set(responseTemplate, "candidates.0.content.role", role)
	}
	if finishReason != "" {
		responseTemplate, _ = sjson.Set(responseTemplate, "candidates.0.finishReason", finishReason)
	}
	if modelVersion != "" {
		responseTemplate, _ = sjson.Set(responseTemplate, "modelVersion", modelVersion)
	}
	if responseID != "" {
		responseTemplate, _ = sjson.Set(responseTemplate, "responseId", responseID)
	}
	if usageRaw != "" {
		responseTemplate, _ = sjson.SetRaw(responseTemplate, "usageMetadata", usageRaw)
	} else if !gjson.Get(responseTemplate, "usageMetadata").Exists() {
		responseTemplate, _ = sjson.Set(responseTemplate, "usageMetadata.promptTokenCount", 0)
		responseTemplate, _ = sjson.Set(responseTemplate, "usageMetadata.candidatesTokenCount", 0)
		responseTemplate, _ = sjson.Set(responseTemplate, "usageMetadata.totalTokenCount", 0)
	}

	output := `{"response":{},"traceId":""}`
	output, _ = sjson.SetRaw(output, "response", responseTemplate)
	if traceID != "" {
		output, _ = sjson.Set(output, "traceId", traceID)
	}
	return []byte(output)
}
