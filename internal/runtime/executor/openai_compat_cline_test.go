package executor

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	_ "github.com/router-for-me/CLIProxyAPI/v6/internal/translator"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v6/sdk/translator"
	"github.com/tidwall/gjson"
)

func TestOpenAICompatExecutorClineUsesVisionFallbackForImageRequests(t *testing.T) {
	var gotPath string
	var gotBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chatcmpl_1","object":"chat.completion","created":1,"model":"cline-pass/mimo-v2.5-pro","choices":[{"index":0,"message":{"role":"assistant","content":"vision ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	}))
	defer server.Close()

	exec := NewOpenAICompatExecutor("cline", &config.Config{})
	auth := &cliproxyauth.Auth{Attributes: map[string]string{
		"base_url":              server.URL + "/api/v1",
		"api_key":               "test-key",
		"vision_fallback_model": "cline-pass/mimo-v2.5-pro",
	}}
	payload := []byte(`{"model":"cline-pass/deepseek-v4-flash","messages":[{"role":"user","content":[{"type":"text","text":"what is this?"},{"type":"image_url","image_url":{"url":"data:image/png;base64,aGVsbG8="}}]}]}`)
	resp, err := exec.Execute(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "cline-pass/deepseek-v4-flash",
		Payload: payload,
	}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAI})
	if err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}

	if gotPath != "/api/v1/chat/completions" {
		t.Fatalf("path = %q, want /api/v1/chat/completions", gotPath)
	}
	if gotModel := gjson.GetBytes(gotBody, "model").String(); gotModel != "cline-pass/mimo-v2.5-pro" {
		t.Fatalf("upstream model = %q, want cline-pass/mimo-v2.5-pro; body=%s", gotModel, string(gotBody))
	}
	if !strings.Contains(string(gotBody), `"image_url"`) {
		t.Fatalf("image should be preserved for vision fallback model; body=%s", string(gotBody))
	}
	if gotModel := gjson.GetBytes(resp.Payload, "model").String(); gotModel != "cline-pass/deepseek-v4-flash" {
		t.Fatalf("response model = %q, want cline-pass/deepseek-v4-flash; payload=%s", gotModel, string(resp.Payload))
	}
}
