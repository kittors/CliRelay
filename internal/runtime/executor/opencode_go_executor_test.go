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

func TestOpenCodeGoExecutorRoutesOpenAIModelsToChatCompletions(t *testing.T) {
	var gotPath, gotAuth string
	var gotBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chatcmpl_1","object":"chat.completion","created":1,"model":"deepseek-v4-flash","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	}))
	defer server.Close()

	oldBaseURL := opencodeGoBaseURL
	opencodeGoBaseURL = server.URL + "/zen/go/v1"
	t.Cleanup(func() { opencodeGoBaseURL = oldBaseURL })

	exec := NewOpenCodeGoExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{Attributes: map[string]string{"api_key": "test-key"}}
	payload := []byte(`{"model":"deepseek-v4-flash","messages":[{"role":"user","content":"hi"}]}`)
	resp, err := exec.Execute(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "deepseek-v4-flash",
		Payload: payload,
	}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAI})
	if err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}

	if gotPath != "/zen/go/v1/chat/completions" {
		t.Fatalf("path = %q, want /zen/go/v1/chat/completions", gotPath)
	}
	if gotAuth != "Bearer test-key" {
		t.Fatalf("Authorization = %q, want bearer key", gotAuth)
	}
	if gotModel := gjson.GetBytes(gotBody, "model").String(); gotModel != "deepseek-v4-flash" {
		t.Fatalf("upstream model = %q, want deepseek-v4-flash", gotModel)
	}
	if gotText := gjson.GetBytes(resp.Payload, "choices.0.message.content").String(); gotText != "ok" {
		t.Fatalf("response text = %q, want ok; payload=%s", gotText, string(resp.Payload))
	}
}

func TestOpenCodeGoExecutorUsesVisionFallbackForImageRequests(t *testing.T) {
	var gotPath string
	var gotBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		// Vision fallback execution — image sent directly to qwen3.5-plus
		gotPath = r.URL.Path
		gotBody = body
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chatcmpl_vision","object":"chat.completion","created":1,"model":"qwen3.5-plus","choices":[{"index":0,"message":{"role":"assistant","content":"vision ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":2,"completion_tokens":1,"total_tokens":3}}`))
	}))
	defer server.Close()

	oldBaseURL := opencodeGoBaseURL
	opencodeGoBaseURL = server.URL + "/zen/go/v1"
	t.Cleanup(func() { opencodeGoBaseURL = oldBaseURL })

	exec := NewOpenCodeGoExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{
		Attributes: map[string]string{
			"api_key":               "test-key",
			"vision_fallback_model": "qwen3.5-plus",
		},
	}
	payload := []byte(`{"model":"deepseek-v4-flash","messages":[{"role":"user","content":[{"type":"text","text":"what is this?"},{"type":"image_url","image_url":{"url":"data:image/png;base64,aGVsbG8="}}]}]}`)
	resp, err := exec.Execute(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "deepseek-v4-flash",
		Payload: payload,
	}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAI})
	if err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}

	if gotPath != "/zen/go/v1/chat/completions" {
		t.Fatalf("path = %q, want /zen/go/v1/chat/completions", gotPath)
	}
	if gotModel := gjson.GetBytes(gotBody, "model").String(); gotModel != "qwen3.5-plus" {
		t.Fatalf("upstream model = %q, want qwen3.5-plus; body=%s", gotModel, string(gotBody))
	}
	if !strings.Contains(string(gotBody), `"image_url"`) {
		t.Fatalf("image should be preserved for vision fallback model; body=%s", string(gotBody))
	}
	if gotModel := gjson.GetBytes(resp.Payload, "model").String(); gotModel != "deepseek-v4-flash" {
		t.Fatalf("response model = %q, want deepseek-v4-flash; payload=%s", gotModel, string(resp.Payload))
	}
	if !gjson.GetBytes(gotBody, "enable_thinking").Exists() || gjson.GetBytes(gotBody, "enable_thinking").Bool() {
		t.Fatalf("enable_thinking should be false for qwen vision fallback; body=%s", string(gotBody))
	}
	if gotText := gjson.GetBytes(resp.Payload, "choices.0.message.content").String(); gotText != "vision ok" {
		t.Fatalf("response text = %q, want vision ok; payload=%s", gotText, string(resp.Payload))
	}
}

func TestOpenCodeGoExecutorUsesVisionFallbackForResponsesFunctionCallOutputImage(t *testing.T) {
	var gotBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chatcmpl_tool_vision","object":"chat.completion","created":1,"model":"qwen3.5-plus","choices":[{"index":0,"message":{"role":"assistant","content":"tool vision ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":2,"completion_tokens":1,"total_tokens":3}}`))
	}))
	defer server.Close()

	oldBaseURL := opencodeGoBaseURL
	opencodeGoBaseURL = server.URL + "/zen/go/v1"
	t.Cleanup(func() { opencodeGoBaseURL = oldBaseURL })

	exec := NewOpenCodeGoExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{
		Attributes: map[string]string{
			"api_key":               "test-key",
			"vision_fallback_model": "qwen3.5-plus",
		},
	}
	payload := []byte(`{"model":"deepseek-v4-flash","input":[{"role":"user","content":[{"type":"input_text","text":"inspect this screenshot"}]},{"type":"function_call","call_id":"call_1","name":"get_app_state","arguments":"{}"},{"type":"function_call_output","call_id":"call_1","output":[{"type":"input_image","image_url":"data:image/png;base64,aGVsbG8="}]}]}`)
	resp, err := exec.Execute(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "deepseek-v4-flash",
		Payload: payload,
	}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAIResponse})
	if err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}

	if gotModel := gjson.GetBytes(gotBody, "model").String(); gotModel != "qwen3.5-plus" {
		t.Fatalf("upstream model = %q, want qwen3.5-plus; body=%s", gotModel, string(gotBody))
	}
	if gotModel := gjson.GetBytes(resp.Payload, "model").String(); gotModel != "deepseek-v4-flash" {
		t.Fatalf("response model = %q, want deepseek-v4-flash; payload=%s", gotModel, string(resp.Payload))
	}
	if gotText := gjson.GetBytes(resp.Payload, "output.0.content.0.text").String(); gotText != "tool vision ok" {
		t.Fatalf("response text = %q, want tool vision ok; payload=%s", gotText, string(resp.Payload))
	}
}

func TestOpenCodeGoExecutorUsesConfiguredNonQwenVisionFallback(t *testing.T) {
	var gotBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		reqModel := gjson.GetBytes(body, "model").String()
		if reqModel == "qwen3.5-plus" {
			// Vision preprocessing call to describe the image
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"a test image description"}}]}`))
			return
		}
		// Actual execution call — stays on original model
		gotBody = body
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chatcmpl_vision","object":"chat.completion","created":1,"model":"deepseek-v4-flash","choices":[{"index":0,"message":{"role":"assistant","content":"vision ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":2,"completion_tokens":1,"total_tokens":3}}`))
	}))
	defer server.Close()

	oldBaseURL := opencodeGoBaseURL
	opencodeGoBaseURL = server.URL + "/zen/go/v1"
	t.Cleanup(func() { opencodeGoBaseURL = oldBaseURL })

	exec := NewOpenCodeGoExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{
		Attributes: map[string]string{
			"api_key":               "test-key",
			"vision_fallback_model": "mimo-v2-omni",
		},
	}
	payload := []byte(`{"model":"deepseek-v4-flash","messages":[{"role":"user","content":[{"type":"text","text":"what is this?"},{"type":"image_url","image_url":{"url":"data:image/png;base64,aGVsbG8="}}]}]}`)
	resp, err := exec.Execute(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "deepseek-v4-flash",
		Payload: payload,
	}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAI})
	if err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}

	if gotModel := gjson.GetBytes(gotBody, "model").String(); gotModel != "mimo-v2-omni" {
		t.Fatalf("upstream model = %q, want mimo-v2-omni; body=%s", gotModel, string(gotBody))
	}
	if !strings.Contains(string(gotBody), `"image_url"`) {
		t.Fatalf("image should be preserved for vision fallback model; body=%s", string(gotBody))
	}
	if gjson.GetBytes(gotBody, "enable_thinking").Exists() {
		t.Fatalf("enable_thinking should not be added for non-qwen fallback; body=%s", string(gotBody))
	}
	if gotModel := gjson.GetBytes(resp.Payload, "model").String(); gotModel != "deepseek-v4-flash" {
		t.Fatalf("response model = %q, want deepseek-v4-flash; payload=%s", gotModel, string(resp.Payload))
	}
}

func TestOpenCodeGoExecutorIgnoresConfiguredTextOnlyFallbackModel(t *testing.T) {
	var gotBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		reqModel := gjson.GetBytes(body, "model").String()
		if reqModel == "qwen3.5-plus" {
			// Vision preprocessing call to describe the image
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"a description"}}]}`))
			return
		}
		// Actual execution call — text-only model, stays on original model
		gotBody = body
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chatcmpl_text_only_fallback","object":"chat.completion","created":1,"model":"deepseek-v4-flash","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	}))
	defer server.Close()

	oldBaseURL := opencodeGoBaseURL
	opencodeGoBaseURL = server.URL + "/zen/go/v1"
	t.Cleanup(func() { opencodeGoBaseURL = oldBaseURL })

	exec := NewOpenCodeGoExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{
		Attributes: map[string]string{
			"api_key":               "test-key",
			"vision_fallback_model": "deepseek-v4-pro",
		},
	}
	payload := []byte(`{"model":"deepseek-v4-flash","messages":[{"role":"user","content":[{"type":"text","text":"what is this?"},{"type":"image_url","image_url":{"url":"data:image/png;base64,aGVsbG8="}}]}]}`)
	if _, err := exec.Execute(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "deepseek-v4-flash",
		Payload: payload,
	}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAI}); err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}

	if gotModel := gjson.GetBytes(gotBody, "model").String(); gotModel != "deepseek-v4-flash" {
		t.Fatalf("upstream model = %q, want deepseek-v4-flash; body=%s", gotModel, string(gotBody))
	}
}

func TestOpenCodeGoExecutorLeavesTextRequestsOnRequestedModel(t *testing.T) {
	var gotBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chatcmpl_text","object":"chat.completion","created":1,"model":"deepseek-v4-flash","choices":[{"index":0,"message":{"role":"assistant","content":"text ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	}))
	defer server.Close()

	oldBaseURL := opencodeGoBaseURL
	opencodeGoBaseURL = server.URL + "/zen/go/v1"
	t.Cleanup(func() { opencodeGoBaseURL = oldBaseURL })

	exec := NewOpenCodeGoExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{
		Attributes: map[string]string{
			"api_key":               "test-key",
			"vision_fallback_model": "deepseek-v4-flash",
		},
	}
	payload := []byte(`{"model":"deepseek-v4-flash","messages":[{"role":"user","content":"hi"}]}`)
	if _, err := exec.Execute(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "deepseek-v4-flash",
		Payload: payload,
	}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAI}); err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}

	if gotModel := gjson.GetBytes(gotBody, "model").String(); gotModel != "deepseek-v4-flash" {
		t.Fatalf("upstream model = %q, want deepseek-v4-flash; body=%s", gotModel, string(gotBody))
	}
	if gjson.GetBytes(gotBody, "enable_thinking").Exists() {
		t.Fatalf("enable_thinking should not be added for text requests; body=%s", string(gotBody))
	}
}

func TestOpenCodeGoExecutorLeavesTextFollowUpWithHistoricalImageOnRequestedModel(t *testing.T) {
	var gotBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chatcmpl_followup","object":"chat.completion","created":1,"model":"deepseek-v4-flash","choices":[{"index":0,"message":{"role":"assistant","content":"text follow-up ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":2,"completion_tokens":1,"total_tokens":3}}`))
	}))
	defer server.Close()

	oldBaseURL := opencodeGoBaseURL
	opencodeGoBaseURL = server.URL + "/zen/go/v1"
	t.Cleanup(func() { opencodeGoBaseURL = oldBaseURL })

	exec := NewOpenCodeGoExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{
		Attributes: map[string]string{
			"api_key":               "test-key",
			"vision_fallback_model": "deepseek-v4-flash",
		},
	}
	payload := []byte(`{"model":"deepseek-v4-flash","messages":[{"role":"user","content":[{"type":"text","text":"what is this?"},{"type":"image_url","image_url":{"url":"data:image/png;base64,aGVsbG8="}}]},{"role":"assistant","content":"vision ok"},{"role":"user","content":"now answer a normal text question"}]}`)
	if _, err := exec.Execute(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "deepseek-v4-flash",
		Payload: payload,
	}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAI}); err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}

	if gotModel := gjson.GetBytes(gotBody, "model").String(); gotModel != "deepseek-v4-flash" {
		t.Fatalf("upstream model = %q, want deepseek-v4-flash; body=%s", gotModel, string(gotBody))
	}
	if strings.Contains(string(gotBody), `"image_url"`) {
		t.Fatalf("historical image_url should be sanitized for text follow-up; body=%s", string(gotBody))
	}
	if gjson.GetBytes(gotBody, "enable_thinking").Exists() {
		t.Fatalf("enable_thinking should not be added for text follow-up; body=%s", string(gotBody))
	}
}

func TestOpenCodeGoExecutorSanitizesResponsesHistoryImagesForTextFollowUp(t *testing.T) {
	var gotBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chatcmpl_response_followup","object":"chat.completion","created":1,"model":"deepseek-v4-flash","choices":[{"index":0,"message":{"role":"assistant","content":"response follow-up ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":2,"completion_tokens":1,"total_tokens":3}}`))
	}))
	defer server.Close()

	oldBaseURL := opencodeGoBaseURL
	opencodeGoBaseURL = server.URL + "/zen/go/v1"
	t.Cleanup(func() { opencodeGoBaseURL = oldBaseURL })

	exec := NewOpenCodeGoExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{
		Attributes: map[string]string{
			"api_key":               "test-key",
			"vision_fallback_model": "deepseek-v4-flash",
		},
	}
	payload := []byte(`{"model":"deepseek-v4-flash","input":[{"role":"user","content":[{"type":"input_text","text":"what is this?"},{"type":"input_image","image_url":"data:image/png;base64,aGVsbG8="}]},{"role":"assistant","content":[{"type":"output_text","text":"vision ok"}]},{"role":"user","content":[{"type":"input_text","text":"now answer a normal text question"}]}]}`)
	if _, err := exec.Execute(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "deepseek-v4-flash",
		Payload: payload,
	}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAIResponse}); err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}

	if gotModel := gjson.GetBytes(gotBody, "model").String(); gotModel != "deepseek-v4-flash" {
		t.Fatalf("upstream model = %q, want deepseek-v4-flash; body=%s", gotModel, string(gotBody))
	}
	if strings.Contains(string(gotBody), `"image_url"`) {
		t.Fatalf("historical input_image should not translate to upstream image_url; body=%s", string(gotBody))
	}
	if !strings.Contains(string(gotBody), "now answer a normal text question") {
		t.Fatalf("current text should be preserved; body=%s", string(gotBody))
	}
}

func TestOpenCodeGoExecutorStreamRewritesVisionFallbackModel(t *testing.T) {
	var gotBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		// Vision fallback execution — image sent directly to qwen3.5-plus
		gotBody = body
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(`data: {"id":"chatcmpl_stream","object":"chat.completion.chunk","created":1,"model":"qwen3.5-plus","choices":[{"index":0,"delta":{"content":"hi"},"finish_reason":null}]}` + "\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer server.Close()

	oldBaseURL := opencodeGoBaseURL
	opencodeGoBaseURL = server.URL + "/zen/go/v1"
	t.Cleanup(func() { opencodeGoBaseURL = oldBaseURL })

	exec := NewOpenCodeGoExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{
		Attributes: map[string]string{
			"api_key":               "test-key",
			"vision_fallback_model": "qwen3.5-plus",
		},
	}
	payload := []byte(`{"model":"deepseek-v4-flash","stream":true,"messages":[{"role":"user","content":[{"type":"text","text":"what is this?"},{"type":"image_url","image_url":{"url":"data:image/png;base64,aGVsbG8="}}]}]}`)
	result, err := exec.ExecuteStream(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "deepseek-v4-flash",
		Payload: payload,
	}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAI, Stream: true})
	if err != nil {
		t.Fatalf("ExecuteStream returned error: %v", err)
	}

	var chunks strings.Builder
	for chunk := range result.Chunks {
		if chunk.Err != nil {
			t.Fatalf("stream chunk error: %v", chunk.Err)
		}
		chunks.Write(chunk.Payload)
	}
	if gotModel := gjson.GetBytes(gotBody, "model").String(); gotModel != "qwen3.5-plus" {
		t.Fatalf("upstream model = %q, want qwen3.5-plus; body=%s", gotModel, string(gotBody))
	}
	out := chunks.String()
	if strings.Contains(out, "qwen3.5-plus") {
		t.Fatalf("stream leaked fallback model qwen3.5-plus; chunks=%s", out)
	}
	if !strings.Contains(out, "deepseek-v4-flash") {
		t.Fatalf("stream chunks should expose original model; chunks=%s", out)
	}
}

func TestOpenCodeGoRewriteFallbackStreamPayloadRewritesSSEModelFields(t *testing.T) {
	chunk := []byte("event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"model\":\"qwen3.5-plus\"}}\n\n")
	got := string(opencodeGoRewriteFallbackStreamPayload(chunk, "deepseek-v4-flash"))
	if strings.Contains(got, "qwen3.5-plus") {
		t.Fatalf("SSE chunk leaked fallback model: %s", got)
	}
	if !strings.Contains(got, `"model":"deepseek-v4-flash"`) {
		t.Fatalf("SSE chunk should expose original model: %s", got)
	}
}

func TestOpenCodeGoExecutorIgnoresExcludedVisionFallback(t *testing.T) {
	var gotBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		reqModel := gjson.GetBytes(body, "model").String()
		if reqModel == "qwen3.5-plus" {
			// Vision preprocessing call to describe the image
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"a description"}}]}`))
			return
		}
		// Actual execution call — stays on original model
		gotBody = body
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chatcmpl_excluded","object":"chat.completion","created":1,"model":"deepseek-v4-flash","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	}))
	defer server.Close()

	oldBaseURL := opencodeGoBaseURL
	opencodeGoBaseURL = server.URL + "/zen/go/v1"
	t.Cleanup(func() { opencodeGoBaseURL = oldBaseURL })

	exec := NewOpenCodeGoExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{
		Attributes: map[string]string{
			"api_key":               "test-key",
			"vision_fallback_model": "qwen3.5-plus",
			"excluded_models":       "qwen3.5-plus",
		},
	}
	payload := []byte(`{"model":"deepseek-v4-flash","messages":[{"role":"user","content":[{"type":"text","text":"what is this?"},{"type":"image_url","image_url":{"url":"data:image/png;base64,aGVsbG8="}}]}]}`)
	if _, err := exec.Execute(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "deepseek-v4-flash",
		Payload: payload,
	}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAI}); err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}

	if gotModel := gjson.GetBytes(gotBody, "model").String(); gotModel != "deepseek-v4-flash" {
		t.Fatalf("upstream model = %q, want deepseek-v4-flash; body=%s", gotModel, string(gotBody))
	}
}

func TestOpenCodeGoExecutorRoutesMiniMaxModelsToMessages(t *testing.T) {
	var gotPath, gotAuth string
	var gotBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"msg_1","type":"message","role":"assistant","model":"minimax-m2.7","content":[{"type":"text","text":"hello"}],"stop_reason":"end_turn","stop_sequence":null,"usage":{"input_tokens":2,"output_tokens":3}}`))
	}))
	defer server.Close()

	oldBaseURL := opencodeGoBaseURL
	opencodeGoBaseURL = server.URL + "/zen/go/v1"
	t.Cleanup(func() { opencodeGoBaseURL = oldBaseURL })

	exec := NewOpenCodeGoExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{Attributes: map[string]string{"api_key": "test-key"}}
	payload := []byte(`{"model":"minimax-m2.7","messages":[{"role":"user","content":"hi"}]}`)
	resp, err := exec.Execute(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "minimax-m2.7",
		Payload: payload,
	}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAI})
	if err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}

	if gotPath != "/zen/go/v1/messages" {
		t.Fatalf("path = %q, want /zen/go/v1/messages", gotPath)
	}
	if gotAuth != "Bearer test-key" {
		t.Fatalf("Authorization = %q, want bearer key", gotAuth)
	}
	if gotModel := gjson.GetBytes(gotBody, "model").String(); gotModel != "minimax-m2.7" {
		t.Fatalf("upstream model = %q, want minimax-m2.7", gotModel)
	}
	if gotText := gjson.GetBytes(resp.Payload, "choices.0.message.content").String(); gotText != "hello" {
		t.Fatalf("response text = %q, want hello; payload=%s", gotText, string(resp.Payload))
	}
}

func TestOpenCodeGoExecutorSupportsResponsesAPIForOpenAIModels(t *testing.T) {
	var gotPath string
	var gotBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chatcmpl_2","object":"chat.completion","created":1,"model":"deepseek-v4-flash","choices":[{"index":0,"message":{"role":"assistant","content":"response ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":2,"total_tokens":3}}`))
	}))
	defer server.Close()

	oldBaseURL := opencodeGoBaseURL
	opencodeGoBaseURL = server.URL + "/zen/go/v1"
	t.Cleanup(func() { opencodeGoBaseURL = oldBaseURL })

	exec := NewOpenCodeGoExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{Attributes: map[string]string{"api_key": "test-key"}}
	payload := []byte(`{"model":"deepseek-v4-flash","input":[{"role":"user","content":[{"type":"input_text","text":"hi"}]}]}`)
	resp, err := exec.Execute(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "deepseek-v4-flash",
		Payload: payload,
	}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAIResponse})
	if err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}

	if gotPath != "/zen/go/v1/chat/completions" {
		t.Fatalf("path = %q, want /zen/go/v1/chat/completions", gotPath)
	}
	if !gjson.GetBytes(gotBody, "messages").Exists() || gjson.GetBytes(gotBody, "input").Exists() {
		t.Fatalf("expected upstream chat-completions body, got %s", string(gotBody))
	}
	if gotObject := gjson.GetBytes(resp.Payload, "object").String(); gotObject != "response" {
		t.Fatalf("response object = %q, want response; payload=%s", gotObject, string(resp.Payload))
	}
	if gotText := gjson.GetBytes(resp.Payload, "output.0.content.0.text").String(); gotText != "response ok" {
		t.Fatalf("response output text = %q, want response ok; payload=%s", gotText, string(resp.Payload))
	}
}

func TestOpenCodeGoExecutorSupportsResponsesAPIForMiniMaxModels(t *testing.T) {
	var gotPath string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"msg_2","type":"message","role":"assistant","model":"minimax-m2.7","content":[{"type":"text","text":"minimax response ok"}],"stop_reason":"end_turn","stop_sequence":null,"usage":{"input_tokens":2,"output_tokens":3}}`))
	}))
	defer server.Close()

	oldBaseURL := opencodeGoBaseURL
	opencodeGoBaseURL = server.URL + "/zen/go/v1"
	t.Cleanup(func() { opencodeGoBaseURL = oldBaseURL })

	exec := NewOpenCodeGoExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{Attributes: map[string]string{"api_key": "test-key"}}
	payload := []byte(`{"model":"minimax-m2.7","input":[{"role":"user","content":[{"type":"input_text","text":"hi"}]}]}`)
	resp, err := exec.Execute(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "minimax-m2.7",
		Payload: payload,
	}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAIResponse})
	if err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}

	if gotPath != "/zen/go/v1/messages" {
		t.Fatalf("path = %q, want /zen/go/v1/messages", gotPath)
	}
	if gotObject := gjson.GetBytes(resp.Payload, "object").String(); gotObject != "response" {
		t.Fatalf("response object = %q, want response; payload=%s", gotObject, string(resp.Payload))
	}
	if gotText := gjson.GetBytes(resp.Payload, "output.0.content.0.text").String(); gotText != "minimax response ok" {
		t.Fatalf("response output text = %q, want minimax response ok; payload=%s", gotText, string(resp.Payload))
	}
}

func TestOpenCodeGoExecutorSupportsAnthropicMessagesAPIForOpenAIModels(t *testing.T) {
	var gotPath string
	var gotBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chatcmpl_3","object":"chat.completion","created":1,"model":"deepseek-v4-flash","choices":[{"index":0,"message":{"role":"assistant","content":"anthropic ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":2,"total_tokens":3}}`))
	}))
	defer server.Close()

	oldBaseURL := opencodeGoBaseURL
	opencodeGoBaseURL = server.URL + "/zen/go/v1"
	t.Cleanup(func() { opencodeGoBaseURL = oldBaseURL })

	exec := NewOpenCodeGoExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{Attributes: map[string]string{"api_key": "test-key"}}
	payload := []byte(`{"model":"deepseek-v4-flash","max_tokens":32,"messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}]}`)
	resp, err := exec.Execute(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "deepseek-v4-flash",
		Payload: payload,
	}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatClaude})
	if err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}

	if gotPath != "/zen/go/v1/chat/completions" {
		t.Fatalf("path = %q, want /zen/go/v1/chat/completions", gotPath)
	}
	if gotModel := gjson.GetBytes(gotBody, "model").String(); gotModel != "deepseek-v4-flash" {
		t.Fatalf("upstream model = %q, want deepseek-v4-flash", gotModel)
	}
	if gotText := gjson.GetBytes(resp.Payload, "content.0.text").String(); gotText != "anthropic ok" {
		t.Fatalf("anthropic response text = %q, want anthropic ok; payload=%s", gotText, string(resp.Payload))
	}
}
