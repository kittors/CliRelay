package executor

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"hash/crc32"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v6/sdk/translator"
	"github.com/tidwall/gjson"
)

func TestResolveBedrockModelID_UsesDefaultMappingAndRegionPrefix(t *testing.T) {
	auth := &cliproxyauth.Auth{
		Attributes: map[string]string{
			"region": "eu-west-1",
		},
	}

	got, ok := resolveBedrockModelID(auth, "claude-sonnet-4-5")

	if !ok {
		t.Fatal("expected model mapping to resolve")
	}
	want := "eu.anthropic.claude-sonnet-4-5-20250929-v1:0"
	if got != want {
		t.Fatalf("resolveBedrockModelID() = %q, want %q", got, want)
	}
}

func TestResolveBedrockModelID_ForceGlobal(t *testing.T) {
	auth := &cliproxyauth.Auth{
		Attributes: map[string]string{
			"region":       "us-east-1",
			"force_global": "true",
		},
	}

	got, ok := resolveBedrockModelID(auth, "claude-opus-4-5-thinking")

	if !ok {
		t.Fatal("expected model mapping to resolve")
	}
	want := "global.anthropic.claude-opus-4-5-20251101-v1:0"
	if got != want {
		t.Fatalf("resolveBedrockModelID() = %q, want %q", got, want)
	}
}

func TestBuildBedrockURL_EncodesModelIDAndChoosesEndpoint(t *testing.T) {
	got := buildBedrockURL("", "us-east-1", "us.anthropic.claude-sonnet-4-5-20250929-v1:0", true)
	want := "https://bedrock-runtime.us-east-1.amazonaws.com/model/us.anthropic.claude-sonnet-4-5-20250929-v1%3A0/invoke-with-response-stream"
	if got != want {
		t.Fatalf("buildBedrockURL(stream) = %q, want %q", got, want)
	}

	got = buildBedrockURL("https://bedrock.test/base", "us-east-1", "anthropic.claude-3-5-haiku", false)
	want = "https://bedrock.test/base/model/anthropic.claude-3-5-haiku/invoke"
	if got != want {
		t.Fatalf("buildBedrockURL(base-url) = %q, want %q", got, want)
	}
}

func TestPrepareBedrockRequestBody_AdaptsClaudePayload(t *testing.T) {
	input := []byte(`{
		"model": "claude-sonnet-4-5",
		"stream": true,
		"system": [{"type":"text","text":"hello","cache_control":{"type":"ephemeral","ttl":"5m","scope":"global"}}],
		"tools": [{"name":"Read","custom":{"defer_loading":true},"input_schema":{"type":"object"}}],
		"messages": [{"role":"user","content":[{"type":"text","text":"hi","cache_control":{"type":"ephemeral","scope":"global"}}]}],
		"output_config": {"enabled": true}
	}`)

	got, err := prepareBedrockRequestBody(input, "us.anthropic.claude-sonnet-4-5-20250929-v1:0", []string{"fine-grained-tool-streaming-2025-05-14"})
	if err != nil {
		t.Fatalf("prepareBedrockRequestBody() error = %v", err)
	}

	if v := gjson.GetBytes(got, "anthropic_version").String(); v != "bedrock-2023-05-31" {
		t.Fatalf("anthropic_version = %q", v)
	}
	if !gjson.GetBytes(got, "anthropic_beta.0").Exists() {
		t.Fatal("expected anthropic_beta to be injected")
	}
	for _, path := range []string{
		"model",
		"stream",
		"output_config",
		"tools.0.custom",
		"system.0.cache_control.ttl",
		"system.0.cache_control.scope",
		"messages.0.content.0.cache_control.scope",
	} {
		if gjson.GetBytes(got, path).Exists() {
			t.Fatalf("expected %s to be removed from body: %s", path, got)
		}
	}
}

func TestTransformBedrockInvocationMetrics(t *testing.T) {
	input := []byte(`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"amazon-bedrock-invocationMetrics":{"inputTokenCount":150,"outputTokenCount":42}}`)

	got := transformBedrockInvocationMetrics(input)

	if gjson.GetBytes(got, "amazon-bedrock-invocationMetrics").Exists() {
		t.Fatalf("amazon-bedrock-invocationMetrics should be removed: %s", got)
	}
	if inputTokens := gjson.GetBytes(got, "usage.input_tokens").Int(); inputTokens != 150 {
		t.Fatalf("usage.input_tokens = %d, want 150", inputTokens)
	}
	if outputTokens := gjson.GetBytes(got, "usage.output_tokens").Int(); outputTokens != 42 {
		t.Fatalf("usage.output_tokens = %d, want 42", outputTokens)
	}
}

func TestBedrockEventStreamDecoder_DecodesChunkPayload(t *testing.T) {
	payload := []byte(`{"bytes":"` + base64.StdEncoding.EncodeToString([]byte(`{"type":"message_start"}`)) + `"}`)
	frame := buildBedrockTestEventStreamFrame(t, ":event-type", "chunk", payload)

	decoded, err := newBedrockEventStreamDecoder(strings.NewReader(string(frame))).Decode()
	if err != nil {
		t.Fatalf("Decode() error = %v", err)
	}
	if string(decoded) != string(payload) {
		t.Fatalf("Decode() = %s, want %s", decoded, payload)
	}
}

func TestBedrockEventStreamDecoder_ReturnsExceptionPayload(t *testing.T) {
	payload := []byte(`{"message":"model stream failed"}`)
	frame := buildBedrockTestEventStreamFrameWithHeaders(t, map[string]string{
		":message-type":   "exception",
		":exception-type": "modelStreamErrorException",
	}, payload)

	_, err := newBedrockEventStreamDecoder(strings.NewReader(string(frame))).Decode()

	if err == nil {
		t.Fatal("expected exception frame to return an error")
	}
	if !strings.Contains(err.Error(), "modelStreamErrorException") || !strings.Contains(err.Error(), "model stream failed") {
		t.Fatalf("exception error = %q, want type and message", err.Error())
	}
}

func TestBedrockPrepareRequest_SignsSigV4AndAppliesCustomHeaders(t *testing.T) {
	exec := NewBedrockExecutor(&config.Config{})
	req, err := http.NewRequest(http.MethodPost, "https://bedrock-runtime.us-east-1.amazonaws.com/model/test/invoke", strings.NewReader(`{"ok":true}`))
	if err != nil {
		t.Fatalf("NewRequest() error = %v", err)
	}
	auth := &cliproxyauth.Auth{
		Provider: "bedrock",
		Attributes: map[string]string{
			"auth_mode":         "sigv4",
			"access_key_id":     "AKIATEST",
			"secret_access_key": "SECRET",
			"session_token":     "SESSION",
			"region":            "us-east-1",
			"header:X-Test":     "yes",
		},
	}

	if err := exec.PrepareRequest(req, auth); err != nil {
		t.Fatalf("PrepareRequest() error = %v", err)
	}

	if got := req.Header.Get("X-Test"); got != "yes" {
		t.Fatalf("custom header = %q, want yes", got)
	}
	if got := req.Header.Get("X-Amz-Security-Token"); got != "SESSION" {
		t.Fatalf("session token header = %q, want SESSION", got)
	}
	if got := req.Header.Get("Authorization"); !strings.HasPrefix(got, "AWS4-HMAC-SHA256 ") {
		t.Fatalf("Authorization = %q, want SigV4 header", got)
	}
	body, _ := io.ReadAll(req.Body)
	if string(body) != `{"ok":true}` {
		t.Fatalf("request body after signing = %q, want original body", body)
	}
}

func TestBedrockExecutorExecute_APIKeyModeSendsInvokeRequest(t *testing.T) {
	var gotPath string
	var gotAuth string
	var gotBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.EscapedPath()
		gotAuth = r.Header.Get("Authorization")
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("x-amzn-requestid", "aws-req-1")
		_, _ = w.Write([]byte(`{"id":"msg_1","type":"message","role":"assistant","model":"claude-sonnet-4-5","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	defer server.Close()

	exec := NewBedrockExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{
		ID:       "bedrock-api-key",
		Provider: "bedrock",
		Label:    "bedrock",
		Attributes: map[string]string{
			"auth_mode": "api-key",
			"api_key":   "bedrock-test-key",
			"region":    "us-east-1",
			"base_url":  server.URL,
		},
	}
	req := cliproxyexecutor.Request{
		Model:   "claude-sonnet-4-5",
		Payload: []byte(`{"model":"claude-sonnet-4-5","messages":[{"role":"user","content":"hi"}],"stream":false}`),
	}

	resp, err := exec.Execute(context.Background(), auth, req, cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("claude")})
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}

	if gotAuth != "Bearer bedrock-test-key" {
		t.Fatalf("Authorization = %q, want bearer API key", gotAuth)
	}
	wantPath := "/model/us.anthropic.claude-sonnet-4-5-20250929-v1%3A0/invoke"
	if gotPath != wantPath {
		t.Fatalf("path = %q, want %q", gotPath, wantPath)
	}
	if gjson.GetBytes(gotBody, "model").Exists() || gjson.GetBytes(gotBody, "stream").Exists() {
		t.Fatalf("Bedrock body should not contain model/stream: %s", gotBody)
	}
	if v := gjson.GetBytes(gotBody, "anthropic_version").String(); v != "bedrock-2023-05-31" {
		t.Fatalf("anthropic_version = %q", v)
	}
	if reqID := resp.Headers.Get("x-amzn-requestid"); reqID != "aws-req-1" {
		t.Fatalf("response request id header = %q", reqID)
	}
}

func buildBedrockTestEventStreamFrame(t *testing.T, headerName, headerValue string, payload []byte) []byte {
	t.Helper()
	return buildBedrockTestEventStreamFrameWithHeaders(t, map[string]string{headerName: headerValue}, payload)
}

func buildBedrockTestEventStreamFrameWithHeaders(t *testing.T, headerValues map[string]string, payload []byte) []byte {
	t.Helper()
	headers := make([]byte, 0)
	for headerName, headerValue := range headerValues {
		headers = append(headers, byte(len(headerName)))
		headers = append(headers, []byte(headerName)...)
		headers = append(headers, 7) // string header value
		valueLen := make([]byte, 2)
		binary.BigEndian.PutUint16(valueLen, uint16(len(headerValue)))
		headers = append(headers, valueLen...)
		headers = append(headers, []byte(headerValue)...)
	}
	totalLen := uint32(12 + len(headers) + len(payload) + 4)
	prelude := make([]byte, 12)
	binary.BigEndian.PutUint32(prelude[0:4], totalLen)
	binary.BigEndian.PutUint32(prelude[4:8], uint32(len(headers)))
	binary.BigEndian.PutUint32(prelude[8:12], crc32.ChecksumIEEE(prelude[0:8]))

	frame := append(prelude, headers...)
	frame = append(frame, payload...)
	messageCRC := make([]byte, 4)
	binary.BigEndian.PutUint32(messageCRC, crc32.ChecksumIEEE(frame))
	frame = append(frame, messageCRC...)
	return frame
}
