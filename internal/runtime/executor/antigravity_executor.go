// Package executor provides runtime execution capabilities for various AI service providers.
// This file implements the Antigravity executor that proxies requests to the antigravity
// upstream using OAuth credentials.
package executor

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	antigravityauth "github.com/router-for-me/CLIProxyAPI/v6/internal/auth/antigravity"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v6/sdk/translator"
	log "github.com/sirupsen/logrus"
)

const (
	antigravityBaseURLDaily        = "https://daily-cloudcode-pa.googleapis.com"
	antigravitySandboxBaseURLDaily = "https://daily-cloudcode-pa.sandbox.googleapis.com"
	antigravityBaseURLProd         = "https://cloudcode-pa.googleapis.com"
	antigravityCountTokensPath     = "/v1internal:countTokens"
	antigravityStreamPath          = "/v1internal:streamGenerateContent"
	antigravityGeneratePath        = "/v1internal:generateContent"
	antigravityModelsPath          = "/v1internal:fetchAvailableModels"
	// Shared with the account status probe. The two had drifted to different
	// versions, so CloudCode saw one account as two different clients and
	// served the older one a reduced model set.
	defaultAntigravityAgent = antigravityauth.ClientUserAgent
	antigravityAuthType     = "antigravity"
	refreshSkew             = 3000 * time.Second
	systemInstruction       = "You are Antigravity, a powerful agentic AI coding assistant designed by the Google Deepmind team working on Advanced Agentic Coding.You are pair programming with a USER to solve their coding task. The task may require creating a new codebase, modifying or debugging an existing codebase, or simply answering a question.**Absolute paths only****Proactiveness**"
)

// DefaultAntigravitySensitiveWords returns the built-in list of sensitive words/phrases
// to obfuscate with zero-width characters in Antigravity system instructions.
// This mitigates upstream keyword-triggered 429 RESOURCE_EXHAUSTED errors.
func DefaultAntigravitySensitiveWords() []string {
	return []string{
		"Claude Agent SDK",
		"Hermes Agent",
		"Nous Research",
	}
}

// AntigravityExecutor proxies requests to the antigravity upstream.
type AntigravityExecutor struct {
	cfg *config.Config
}

// NewAntigravityExecutor creates a new Antigravity executor instance.
func NewAntigravityExecutor(cfg *config.Config) *AntigravityExecutor {
	return &AntigravityExecutor{cfg: cfg}
}

// Identifier returns the executor identifier.
func (e *AntigravityExecutor) Identifier() string { return antigravityAuthType }

// PrepareRequest injects Antigravity credentials into the outgoing HTTP request.
func (e *AntigravityExecutor) PrepareRequest(req *http.Request, auth *cliproxyauth.Auth) error {
	if req == nil {
		return nil
	}
	token, _, errToken := e.ensureAccessToken(req.Context(), auth)
	if errToken != nil {
		return errToken
	}
	if strings.TrimSpace(token) == "" {
		return statusErr{code: http.StatusUnauthorized, msg: "missing access token"}
	}
	req.Header.Set("Authorization", "Bearer "+token)
	return nil
}

// HttpRequest injects Antigravity credentials into the request and executes it.
func (e *AntigravityExecutor) HttpRequest(ctx context.Context, auth *cliproxyauth.Auth, req *http.Request) (*http.Response, error) {
	if req == nil {
		return nil, fmt.Errorf("antigravity executor: request is nil")
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

// Execute performs a non-streaming request to the Antigravity API.
func (e *AntigravityExecutor) Execute(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (resp cliproxyexecutor.Response, err error) {
	if opts.Alt == "responses/compact" {
		return resp, statusErr{code: http.StatusNotImplemented, msg: "/responses/compact not supported"}
	}
	if ctx == nil {
		ctx = context.Background()
	}
	// Every non-streaming request is served by asking the streaming endpoint and
	// assembling the result.
	//
	// The upstream's :generateContent does not work for the whole model range.
	// Production data over a week: 41 non-streaming calls to gemini-3.7-flash-*
	// failed without exception while 38 streaming calls to the same model on the
	// same account all succeeded; gemini-2.5-flash, meanwhile, worked either way.
	// So the endpoint's coverage varies per model and cannot be predicted from
	// the name.
	//
	// This detour already existed but was gated on `claude` or `gemini-3-pro`
	// appearing in the model id — a list that silently excluded every model
	// added since, including the gemini-3.x line. Rather than extending the list
	// on each incident, every model now takes the path that is known to work for
	// all of them: :streamGenerateContent is what ordinary streaming requests
	// use, so it is exercised continuously by real traffic.
	return e.executeViaStreamEndpoint(ctx, auth, req, opts)
}

// ExecuteStream performs a streaming request to the Antigravity API.
func (e *AntigravityExecutor) ExecuteStream(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (_ *cliproxyexecutor.StreamResult, err error) {
	if opts.Alt == "responses/compact" {
		return nil, statusErr{code: http.StatusNotImplemented, msg: "/responses/compact not supported"}
	}
	if ctx == nil {
		ctx = context.Background()
	}
	ctx = context.WithValue(ctx, "alt", "")

	token, updatedAuth, errToken := e.ensureAccessToken(ctx, auth)
	if errToken != nil {
		return nil, errToken
	}
	if updatedAuth != nil {
		auth = updatedAuth
	}

	execCtx := e.newAntigravityExecutionContext(ctx, auth, req, opts, true)
	reporter := execCtx.Reporter()
	defer reporter.trackFailure(execCtx.Context, &err)
	translated, err := e.buildAntigravityPayload(execCtx)
	if err != nil {
		return nil, err
	}

	baseURLs := antigravityBaseURLFallbackOrder(auth)
	httpClient := execCtx.HTTPClient(0)
	recorder := execCtx.Recorder()

	attempts := antigravityRetryAttempts(auth, e.cfg)

attemptLoop:
	for attempt := 0; attempt < attempts; attempt++ {
		var lastStatus int
		var lastBody []byte
		var lastErr error

		for idx, baseURL := range baseURLs {
			httpReq, requestBody, errReq := e.buildRequest(execCtx.Context, auth, token, execCtx.BaseModel, translated, true, opts.Alt, baseURL)
			if errReq != nil {
				err = errReq
				return nil, err
			}
			recorder.RecordRequest(httpReq.URL.String(), httpReq.Method, httpReq.Header.Clone(), requestBody)
			httpResp, errDo := httpClient.Do(httpReq)
			if errDo != nil {
				recorder.RecordResponseError(errDo)
				if errors.Is(errDo, context.Canceled) || errors.Is(errDo, context.DeadlineExceeded) {
					return nil, errDo
				}
				lastStatus = 0
				lastBody = nil
				lastErr = errDo
				if idx+1 < len(baseURLs) {
					log.Debugf("antigravity executor: request error on base url %s, retrying with fallback base url: %s", baseURL, baseURLs[idx+1])
					continue
				}
				err = errDo
				return nil, err
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
						return nil, err
					}
					if errCtx := execCtx.Context.Err(); errCtx != nil {
						err = errCtx
						return nil, err
					}
					lastStatus = 0
					lastBody = nil
					lastErr = errRead
					if idx+1 < len(baseURLs) {
						log.Debugf("antigravity executor: read error on base url %s, retrying with fallback base url: %s", baseURL, baseURLs[idx+1])
						continue
					}
					err = errRead
					return nil, err
				}
				recorder.AppendResponseChunk(bodyBytes)
				lastStatus = httpResp.StatusCode
				lastBody = append([]byte(nil), bodyBytes...)
				lastErr = nil
				// 429 is an account-level RESOURCE_EXHAUSTED signal, not a
				// host-specific hiccup: production traces show the same auth
				// getting 429 on both sandbox and daily back to back, so
				// falling back to the other host here only burns a second
				// attempt on an account that's already known to be limited,
				// leaving the outer selector fewer retries to reach a
				// different, healthy account.
				if antigravityShouldRetryNoCapacity(httpResp.StatusCode, bodyBytes) {
					if idx+1 < len(baseURLs) {
						log.Debugf("antigravity executor: no capacity on base url %s, retrying with fallback base url: %s", baseURL, baseURLs[idx+1])
						continue
					}
					if attempt+1 < attempts {
						delay := antigravityNoCapacityRetryDelay(attempt)
						log.Debugf("antigravity executor: no capacity for model %s, retrying in %s (attempt %d/%d)", execCtx.BaseModel, delay, attempt+1, attempts)
						if errWait := antigravityWait(ctx, delay); errWait != nil {
							return nil, errWait
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
				return nil, err
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
				var param any
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

					chunks := sdktranslator.TranslateStream(execCtx.Context, execCtx.Execution.TargetFormat, execCtx.SourceFormat, req.Model, execCtx.OriginalPayload, translated, bytes.Clone(payload), &param)
					for i := range chunks {
						out <- cliproxyexecutor.StreamChunk{Payload: []byte(chunks[i])}
					}
				}
				tail := sdktranslator.TranslateStream(execCtx.Context, execCtx.Execution.TargetFormat, execCtx.SourceFormat, req.Model, execCtx.OriginalPayload, translated, []byte("[DONE]"), &param)
				for i := range tail {
					out <- cliproxyexecutor.StreamChunk{Payload: []byte(tail[i])}
				}
				if errScan := scanner.Err(); errScan != nil {
					recorder.RecordResponseError(errScan)
					reporter.publishFailure(execCtx.Context)
					out <- cliproxyexecutor.StreamChunk{Err: errScan}
				} else {
					reporter.ensurePublished(execCtx.Context)
				}
			}(httpResp)
			return &cliproxyexecutor.StreamResult{Headers: httpResp.Header.Clone(), Chunks: out}, nil
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
		return nil, err
	}

	return nil, err
}

// Refresh refreshes the authentication credentials using the refresh token.
func (e *AntigravityExecutor) Refresh(ctx context.Context, auth *cliproxyauth.Auth) (*cliproxyauth.Auth, error) {
	if auth == nil {
		return auth, nil
	}
	updated, errRefresh := e.refreshToken(ctx, auth.Clone())
	if errRefresh != nil {
		return nil, errRefresh
	}
	return updated, nil
}
