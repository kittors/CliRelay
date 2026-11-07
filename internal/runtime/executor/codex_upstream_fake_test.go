package executor

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/klauspost/compress/zstd"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	_ "github.com/router-for-me/CLIProxyAPI/v6/internal/translator"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/util"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v6/sdk/translator"
)

const codexCompletedSSE = "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"object\":\"response\",\"created_at\":1710000002,\"status\":\"completed\",\"output\":[],\"usage\":{\"input_tokens\":1,\"output_tokens\":1,\"total_tokens\":2}}}\n\n"

// codexWireCall is one request exactly as it reached the (fake) upstream.
type codexWireCall struct {
	URL    string
	Header http.Header
	// Wire is the body as received, still carrying any Content-Encoding.
	Wire          []byte
	ContentLength int64
}

// Plain returns the request body with any zstd encoding undone.
func (c codexWireCall) Plain(t *testing.T) []byte {
	t.Helper()
	if !strings.EqualFold(c.Header.Get("Content-Encoding"), "zstd") {
		return c.Wire
	}
	dec, err := zstd.NewReader(nil)
	if err != nil {
		t.Fatalf("zstd reader: %v", err)
	}
	defer dec.Close()
	plain, err := dec.DecodeAll(c.Wire, nil)
	if err != nil {
		t.Fatalf("wire body is not a valid zstd frame: %v", err)
	}
	return plain
}

// fakeCodexUpstream stands in for chatgpt.com. It is installed as the request
// transport, so the executor can be pointed at the real chatgpt.com URL (the
// host is part of what decides compression) without any network.
type fakeCodexUpstream struct {
	mu    sync.Mutex
	calls []codexWireCall
	// respond builds the answer to call n (zero based). nil answers every call
	// with a completed response.
	respond func(n int, call codexWireCall) *http.Response
}

func (f *fakeCodexUpstream) RoundTrip(req *http.Request) (*http.Response, error) {
	var wire []byte
	if req.Body != nil {
		wire, _ = io.ReadAll(req.Body)
		_ = req.Body.Close()
	}
	call := codexWireCall{URL: req.URL.String(), Header: req.Header.Clone(), Wire: wire, ContentLength: req.ContentLength}
	f.mu.Lock()
	n := len(f.calls)
	f.calls = append(f.calls, call)
	f.mu.Unlock()
	if f.respond != nil {
		if resp := f.respond(n, call); resp != nil {
			resp.Request = req
			return resp, nil
		}
	}
	return codexSSEResponse(req), nil
}

func (f *fakeCodexUpstream) Calls() []codexWireCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]codexWireCall(nil), f.calls...)
}

func codexSSEResponse(req *http.Request) *http.Response {
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       io.NopCloser(strings.NewReader(codexCompletedSSE)),
		Request:    req,
	}
}

func codexErrorResponse(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

// codexOAuthAuth is a ChatGPT OAuth account on the default (chatgpt.com) backend.
func codexOAuthAuth() *cliproxyauth.Auth {
	return &cliproxyauth.Auth{
		ID:       "codex-oauth",
		Provider: "codex",
		Status:   cliproxyauth.StatusActive,
		Metadata: map[string]any{"access_token": "token", "account_id": "account-1", "email": "a@example.com"},
	}
}

// codexAPIKeyAuth is an API-key account on the same chatgpt.com URL.
func codexAPIKeyAuth() *cliproxyauth.Auth {
	return &cliproxyauth.Auth{
		ID:         "codex-key",
		Provider:   "codex",
		Status:     cliproxyauth.StatusActive,
		Attributes: map[string]string{"api_key": "sk-test"},
		Metadata:   map[string]any{"account_id": "account-2", "email": "b@example.com"},
	}
}

// codexExecEnv bundles what a wire-level executor test needs.
type codexExecEnv struct {
	Ctx      context.Context
	Gin      *gin.Context
	Upstream *fakeCodexUpstream
	Exec     *CodexExecutor
}

func newCodexExecEnv(t *testing.T, cfg *config.Config) *codexExecEnv {
	t.Helper()
	gin.SetMode(gin.TestMode)
	ginCtx, _ := gin.CreateTestContext(httptest.NewRecorder())
	upstream := &fakeCodexUpstream{}
	ctx := context.WithValue(context.Background(), util.ContextKeyGin, ginCtx)
	ctx = cliproxyexecutor.WithRoundTripper(ctx, upstream)
	if cfg == nil {
		cfg = &config.Config{}
	}
	return &codexExecEnv{Ctx: ctx, Gin: ginCtx, Upstream: upstream, Exec: NewCodexExecutor(cfg)}
}

// codexCall describes one downstream request to run through the executor.
type codexCall struct {
	Auth    *cliproxyauth.Auth
	From    string
	Payload string
	// Alt selects a sub-endpoint such as "responses/compact".
	Alt    string
	Stream bool
	// APIKey is the downstream caller's key and Tenant its tenant; both feed the
	// derived Chat cache key.
	APIKey string
	Tenant string
}

// Do runs the call and returns what the downstream client would receive.
func (e *codexExecEnv) Do(t *testing.T, c codexCall) ([]byte, error) {
	t.Helper()
	req := cliproxyexecutor.Request{
		Model:   "gpt-5.4",
		Payload: []byte(c.Payload),
		Format:  sdktranslator.FromString(c.From),
	}
	opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString(c.From), Alt: c.Alt, Stream: c.Stream}
	if c.Tenant != "" {
		opts.Metadata = map[string]any{cliproxyexecutor.TenantMetadataKey: c.Tenant}
	}
	ctx := e.Ctx
	if c.APIKey != "" {
		ctx = context.WithValue(ctx, util.ContextKeyAPIKey, c.APIKey)
	}
	if !c.Stream {
		resp, err := e.Exec.Execute(ctx, c.Auth, req, opts)
		return resp.Payload, err
	}
	result, err := e.Exec.ExecuteStream(ctx, c.Auth, req, opts)
	if err != nil {
		return nil, err
	}
	var out []byte
	var streamErr error
	for chunk := range result.Chunks {
		if chunk.Err != nil {
			streamErr = chunk.Err
		}
		out = append(out, chunk.Payload...)
	}
	return out, streamErr
}

// RequestLog is the upstream request log as the panel would store it.
func (e *codexExecEnv) RequestLog(t *testing.T) string {
	t.Helper()
	raw, ok := e.Gin.Get(apiRequestKey)
	if !ok {
		t.Fatal("no upstream request log was captured")
	}
	logBytes, ok := raw.([]byte)
	if !ok {
		t.Fatalf("request log type = %T", raw)
	}
	return string(logBytes)
}

// ResponseLog is the upstream response log, one block per attempt.
func (e *codexExecEnv) ResponseLog(t *testing.T) string {
	t.Helper()
	return string(ensureAPIExchangeCapture(e.Gin).APIResponseSnapshot())
}

func codexLoggingConfig() *config.Config {
	return &config.Config{SDKConfig: config.SDKConfig{RequestLog: true}}
}

func bytesContainsFold(haystack []byte, needle string) bool {
	return bytes.Contains(bytes.ToLower(haystack), []byte(strings.ToLower(needle)))
}
