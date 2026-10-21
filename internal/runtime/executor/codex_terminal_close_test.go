package executor

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	_ "github.com/router-for-me/CLIProxyAPI/v6/internal/translator"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v6/sdk/translator"
)

func codexTerminalTestAuth(baseURL string) *cliproxyauth.Auth {
	return &cliproxyauth.Auth{
		ID: "codex-terminal-test", Provider: "codex", Status: cliproxyauth.StatusActive,
		Attributes: map[string]string{"base_url": baseURL + "/backend-api/codex"},
		Metadata:   map[string]any{"access_token": "token", "account_id": "account-1"},
	}
}

func TestValidCodexResponsesCompletedRequiresStrictTerminalShape(t *testing.T) {
	tests := []struct {
		name    string
		payload string
		valid   bool
	}{
		{"valid", `{"type":"response.completed","response":{"status":"completed"}}`, true},
		{"invalid JSON", `{"type":"response.completed","response":{"status":"completed"}`, false},
		{"missing response", `{"type":"response.completed"}`, false},
		{"null response", `{"type":"response.completed","response":null}`, false},
		{"array response", `{"type":"response.completed","response":[]}`, false},
		{"incomplete status", `{"type":"response.completed","response":{"status":"incomplete"}}`, false},
		{"null status", `{"type":"response.completed","response":{"status":null}}`, false},
		{"non-string status", `{"type":"response.completed","response":{"status":true}}`, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isValidCodexResponsesCompleted([]byte(tt.payload)); got != tt.valid {
				t.Fatalf("isValidCodexResponsesCompleted() = %v, want %v", got, tt.valid)
			}
		})
	}
}

func TestCodexExecutorCompletedStreamIgnoresClientCloseAfterTerminal(t *testing.T) {
	providerReleased := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"output\":[]}}\n\n"))
		w.(http.Flusher).Flush()
		<-providerReleased
	}))
	defer server.Close()
	defer close(providerReleased)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stream, err := NewCodexExecutor(&config.Config{}).ExecuteStream(ctx, codexTerminalTestAuth(server.URL), cliproxyexecutor.Request{
		Model: "gpt-5.4", Payload: []byte(`{"model":"gpt-5.4","input":"hi","stream":true}`),
		Format: sdktranslator.FromString("openai-response"),
	}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("openai-response")})
	if err != nil {
		t.Fatalf("ExecuteStream() error = %v", err)
	}

	var sawCompleted bool
	for chunk := range stream.Chunks {
		if strings.Contains(string(chunk.Payload), "response.completed") {
			sawCompleted = true
			cancel() // downstream closes immediately after the protocol terminal
		}
		if chunk.Err != nil {
			t.Fatalf("chunk error after response.completed = %v", chunk.Err)
		}
	}
	if !sawCompleted {
		t.Fatal("response.completed was not forwarded")
	}
}

func TestCodexExecutorDoesNotSuppressTerminalErrorAfterCompletedEvent(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(
			"data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"output\":[]}}\n\n" +
				"data: {\"type\":\"error\",\"code\":\"internal_server_error\",\"message\":\"late terminal error\"}\n\n",
		))
	}))
	defer server.Close()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	stream, err := NewCodexExecutor(&config.Config{}).ExecuteStream(ctx, codexTerminalTestAuth(server.URL), cliproxyexecutor.Request{
		Model: "gpt-5.4", Payload: []byte(`{"model":"gpt-5.4","input":"hi","stream":true}`),
		Format: sdktranslator.FromString("openai-response"),
	}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("openai-response")})
	if err != nil {
		t.Fatalf("ExecuteStream() error = %v", err)
	}
	var chunkErr error
	for chunk := range stream.Chunks {
		if chunk.Err != nil {
			chunkErr = chunk.Err
		}
	}
	if chunkErr == nil || !strings.Contains(chunkErr.Error(), "late terminal error") {
		t.Fatalf("chunk error = %v, want genuine terminal error", chunkErr)
	}
}

func TestCodexExecutorCancellationBeforeCompletedRemainsError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"type\":\"response.output_text.delta\",\"delta\":\"partial\"}\n\n"))
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer server.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stream, err := NewCodexExecutor(&config.Config{}).ExecuteStream(ctx, codexTerminalTestAuth(server.URL), cliproxyexecutor.Request{
		Model: "gpt-5.4", Payload: []byte(`{"model":"gpt-5.4","input":"hi","stream":true}`),
		Format: sdktranslator.FromString("openai-response"),
	}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("openai-response")})
	if err != nil {
		t.Fatalf("ExecuteStream() error = %v", err)
	}
	var chunkErr error
	for chunk := range stream.Chunks {
		if strings.Contains(string(chunk.Payload), "partial") {
			cancel()
		}
		if chunk.Err != nil {
			chunkErr = chunk.Err
		}
	}
	if chunkErr == nil {
		t.Fatal("chunk error = nil, want cancellation before response.completed")
	}
}

func TestCodexExecutorMalformedCompletedDoesNotAuthorizePostTerminalCancellation(t *testing.T) {
	for _, payload := range []string{
		`{"type":"response.completed"}`,
		`{"type":"response.completed","response":{"status":"incomplete"}}`,
		`{"type":"response.completed","response":{"status":"completed"}`,
	} {
		t.Run(payload, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = w.Write([]byte("data: " + payload + "\n\n"))
				w.(http.Flusher).Flush()
				<-r.Context().Done()
			}))
			defer server.Close()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			stream, err := NewCodexExecutor(&config.Config{}).ExecuteStream(ctx, codexTerminalTestAuth(server.URL), cliproxyexecutor.Request{
				Model: "gpt-5.4", Payload: []byte(`{"model":"gpt-5.4","input":"hi","stream":true}`),
				Format: sdktranslator.FromString("openai-response"),
			}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("openai-response")})
			if err != nil {
				t.Fatalf("ExecuteStream() error = %v", err)
			}
			var chunkErr error
			for chunk := range stream.Chunks {
				if len(chunk.Payload) > 0 {
					cancel()
				}
				if chunk.Err != nil {
					chunkErr = chunk.Err
				}
			}
			if chunkErr == nil {
				t.Fatal("chunk error = nil, malformed completion authorized success")
			}
		})
	}
}
