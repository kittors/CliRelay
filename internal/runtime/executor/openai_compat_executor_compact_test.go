package executor

import (
	"context"
	"encoding/json"
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

func TestOpenAICompatExecutorStreamAddsKimiReasoningForAssistantToolCalls(t *testing.T) {
	var gotBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var err error
		gotBody, err = io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read body: %v", err)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer server.Close()

	executor := NewOpenAICompatExecutor("opencode-go", &config.Config{})
	auth := &cliproxyauth.Auth{Attributes: map[string]string{
		"base_url": server.URL + "/v1",
		"api_key":  "test",
	}}
	payload := []byte(`{
		"model":"kimi-k2.6",
		"max_tokens":1024,
		"messages":[
			{"role":"user","content":[{"type":"text","text":"hi"}]},
			{"role":"assistant","content":[
				{"type":"tool_use","id":"Bash:3","name":"Bash","input":{"cmd":"pwd"}},
				{"type":"tool_use","id":"Read:2","name":"Read","input":{"file_path":"README.md"}}
			]}
		],
		"tools":[
			{"name":"Bash","description":"Run command","input_schema":{"type":"object"}},
			{"name":"Read","description":"Read file","input_schema":{"type":"object"}}
		]
	}`)

	result, err := executor.ExecuteStream(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "kimi-k2.6",
		Payload: payload,
	}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("claude"),
		Stream:       true,
	})
	if err != nil {
		t.Fatalf("ExecuteStream error: %v", err)
	}
	for range result.Chunks {
	}

	reasoning := gjson.GetBytes(gotBody, "messages.1.reasoning_content")
	if !reasoning.Exists() {
		t.Fatalf("messages.1.reasoning_content should exist in upstream body: %s", string(gotBody))
	}
	if reasoning.String() == "" {
		t.Fatalf("messages.1.reasoning_content should not be empty")
	}
}

func TestOpenAICompatExecutorCompactPassthrough(t *testing.T) {
	var gotPath string
	var gotBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		body, _ := io.ReadAll(r.Body)
		gotBody = body
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"resp_1","object":"response.compaction","usage":{"input_tokens":1,"output_tokens":2,"total_tokens":3}}`))
	}))
	defer server.Close()

	executor := NewOpenAICompatExecutor("openai-compatibility", &config.Config{})
	auth := &cliproxyauth.Auth{Attributes: map[string]string{
		"base_url": server.URL + "/v1",
		"api_key":  "test",
	}}
	payload := []byte(`{"model":"gpt-5.1-codex-max","input":[{"role":"user","content":"hi"}]}`)
	resp, err := executor.Execute(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "gpt-5.1-codex-max",
		Payload: payload,
	}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("openai-response"),
		Alt:          "responses/compact",
		Stream:       false,
	})
	if err != nil {
		t.Fatalf("Execute error: %v", err)
	}
	if gotPath != "/v1/responses/compact" {
		t.Fatalf("path = %q, want %q", gotPath, "/v1/responses/compact")
	}
	if !gjson.GetBytes(gotBody, "input").Exists() {
		t.Fatalf("expected input in body")
	}
	if gjson.GetBytes(gotBody, "messages").Exists() {
		t.Fatalf("unexpected messages in body")
	}
	if string(resp.Payload) != `{"id":"resp_1","object":"response.compaction","usage":{"input_tokens":1,"output_tokens":2,"total_tokens":3}}` {
		t.Fatalf("payload = %s", string(resp.Payload))
	}
}

func TestOpenAICompatExecutorPreservesUpstreamErrorBody(t *testing.T) {
	upstreamBody := []byte(`{"error":{"code":"1305","message":"该模型当前访问量过大，请您稍后再试"}}`)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write(upstreamBody)
	}))
	defer server.Close()

	executor := NewOpenAICompatExecutor("bigmodel-coding", &config.Config{})
	auth := &cliproxyauth.Auth{Attributes: map[string]string{
		"base_url": server.URL + "/v1",
		"api_key":  "test",
	}}
	_, err := executor.Execute(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "glm-5.1",
		Payload: []byte(`{"model":"glm-5.1","messages":[{"role":"user","content":"hi"}]}`),
	}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("openai"),
		Stream:       false,
	})
	if err == nil {
		t.Fatal("Execute error = nil, want upstream status error")
	}
	upstreamErr, ok := err.(interface{ UpstreamErrorBody() []byte })
	if !ok {
		t.Fatalf("error %T does not expose upstream body", err)
	}
	if got := string(upstreamErr.UpstreamErrorBody()); got != string(upstreamBody) {
		t.Fatalf("upstream body = %s, want %s", got, string(upstreamBody))
	}
}

func TestOpenAICompatExecutorAppliesMultimodalAdapterAfterAliasResolution(t *testing.T) {
	var extractorBody map[string]any
	extractorServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&extractorBody); err != nil {
			t.Fatalf("decode extractor body: %v", err)
		}
		_, _ = w.Write([]byte(`{"text":"Screenshot contains a panic stack trace."}`))
	}))
	defer extractorServer.Close()

	var upstreamBody []byte
	upstreamServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var err error
		upstreamBody, err = io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read upstream body: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"ok"}}]}`))
	}))
	defer upstreamServer.Close()

	enabled := true
	executor := NewOpenAICompatExecutor("bigmodel-coding", &config.Config{
		SDKConfig: config.SDKConfig{
			MultimodalAdapters: config.MultimodalAdaptersConfig{
				Enabled:       &enabled,
				DefaultAction: "extract",
				InjectAs:      "visual_context",
				Rules: []config.MultimodalAdapterRule{
					{
						Name:      "glm-5.1-codex-vision",
						Extractor: "vision",
						Match: config.MultimodalAdapterMatch{
							RequestedModels:   []string{"gpt-5.3-codex"},
							UpstreamProviders: []string{"bigmodel-coding"},
							UpstreamModels:    []string{"glm-5.1"},
						},
					},
				},
				Extractors: []config.MultimodalExtractorConfig{{Name: "vision", Type: "http", Endpoint: extractorServer.URL}},
			},
		},
	})
	auth := &cliproxyauth.Auth{Attributes: map[string]string{
		"base_url": upstreamServer.URL + "/v1",
		"api_key":  "test",
	}}
	payload := []byte(`{"model":"gpt-5.3-codex","messages":[{"role":"user","content":[{"type":"text","text":"what is wrong?"},{"type":"image_url","image_url":{"url":"https://example.com/panic.png"}}]}]}`)
	_, err := executor.Execute(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "glm-5.1",
		Payload: payload,
	}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("openai"),
		Metadata:     map[string]any{cliproxyexecutor.RequestedModelMetadataKey: "gpt-5.3-codex"},
	})
	if err != nil {
		t.Fatalf("Execute error: %v", err)
	}
	body := string(upstreamBody)
	if strings.Contains(body, "image_url") {
		t.Fatalf("upstream body still contains media: %s", body)
	}
	if !strings.Contains(body, "visual_context") || !strings.Contains(body, "panic stack trace") {
		t.Fatalf("upstream body missing injected visual context: %s", body)
	}
	if extractorBody["upstream_provider"] != "bigmodel-coding" || extractorBody["upstream_model"] != "glm-5.1" {
		t.Fatalf("extractor route metadata = %#v", extractorBody)
	}
}

func TestOpenAICompatExecutorConvertsImageEditsToChatMultimodal(t *testing.T) {
	var gotPath string
	var gotBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		body, _ := io.ReadAll(r.Body)
		gotBody = body
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chatcmpl_1","object":"chat.completion","choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`))
	}))
	defer server.Close()

	executor := NewOpenAICompatExecutor("grsai", &config.Config{
		OpenAICompatibility: []config.OpenAICompatibility{
			{Name: "grsai", ImageEditsMode: "chat-multimodal"},
		},
	})
	auth := &cliproxyauth.Auth{
		Provider: "grsai",
		Attributes: map[string]string{
			"base_url":    server.URL + "/v1",
			"api_key":     "test",
			"compat_name": "grsai",
		},
	}
	payload := []byte(`{
		"model":"gpt-image-2",
		"prompt":"put this logo on a t-shirt",
		"size":"1024x1024",
		"quality":"high",
		"response_format":"b64_json",
		"image_files":[{"file_name":"logo.png","content_type":"image/png","data_base64":"aGVsbG8="}]
	}`)
	_, err := executor.Execute(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "gpt-image-2",
		Payload: payload,
	}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("openai"),
		Alt:          "images/edits",
	})
	if err != nil {
		t.Fatalf("Execute error: %v", err)
	}
	if gotPath != "/v1/chat/completions" {
		t.Fatalf("path = %q, want /v1/chat/completions", gotPath)
	}
	if gjson.GetBytes(gotBody, "image_files").Exists() {
		t.Fatalf("image_files should be removed from upstream body: %s", string(gotBody))
	}
	if gjson.GetBytes(gotBody, "prompt").Exists() || gjson.GetBytes(gotBody, "size").Exists() || gjson.GetBytes(gotBody, "quality").Exists() {
		t.Fatalf("image-only fields should be removed from upstream body: %s", string(gotBody))
	}
	if got := gjson.GetBytes(gotBody, "messages.0.content.0.text").String(); got != "put this logo on a t-shirt" {
		t.Fatalf("text content = %q", got)
	}
	if got := gjson.GetBytes(gotBody, "messages.0.content.1.image_url.url").String(); got != "data:image/png;base64,aGVsbG8=" {
		t.Fatalf("image url = %q", got)
	}
}

func TestOpenAICompatExecutorAppliesCodexIdentityFingerprint(t *testing.T) {
	var got http.Header
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chatcmpl_1","object":"chat.completion","choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	}))
	defer server.Close()

	executor := NewOpenAICompatExecutor("bigmodel-coding", &config.Config{
		IdentityFingerprint: config.IdentityFingerprintConfig{
			Codex: config.CodexIdentityFingerprintConfig{
				Enabled:     true,
				UserAgent:   "codex-tui/test",
				Version:     "9.9.9",
				Originator:  "codex-tui",
				SessionMode: "fixed",
				SessionID:   "session-fixed",
			},
		},
		OpenAICompatibility: []config.OpenAICompatibility{
			{Name: "bigmodel-coding", IdentityFingerprint: "codex"},
		},
	})
	auth := &cliproxyauth.Auth{
		Provider: "bigmodel-coding",
		Attributes: map[string]string{
			"base_url":             server.URL + "/v1",
			"api_key":              "test",
			"compat_name":          "bigmodel-coding",
			"provider_key":         "bigmodel-coding",
			"identity_fingerprint": "codex",
		},
	}
	payload := []byte(`{"model":"gpt-5.3-codex","messages":[{"role":"user","content":"hi"}]}`)
	_, err := executor.Execute(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "gpt-5.3-codex",
		Payload: payload,
	}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("openai")})
	if err != nil {
		t.Fatalf("Execute error: %v", err)
	}
	if got.Get("User-Agent") != "codex-tui/test" {
		t.Fatalf("User-Agent = %q", got.Get("User-Agent"))
	}
	if got.Get("Version") != "9.9.9" {
		t.Fatalf("Version = %q", got.Get("Version"))
	}
	if got.Get("Originator") != "codex-tui" {
		t.Fatalf("Originator = %q", got.Get("Originator"))
	}
	if got.Get("Session_id") != "session-fixed" {
		t.Fatalf("Session_id = %q", got.Get("Session_id"))
	}
}
