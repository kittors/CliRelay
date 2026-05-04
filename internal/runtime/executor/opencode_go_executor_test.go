package executor

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
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
