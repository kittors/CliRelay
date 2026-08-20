package executor

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
	cliproxyusage "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/usage"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v6/sdk/translator"
)

func TestGeminiExecuteStreamPublishesUsageRecordWithoutUsageMetadata(t *testing.T) {
	const model = "gemini-log-fallback-test"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1beta/models/"+model+":streamGenerateContent" {
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"hello\"}]}}]}\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer server.Close()

	usagePlugin := &usageCapturePlugin{records: make(chan cliproxyusage.Record, 8)}
	cliproxyusage.RegisterPlugin(usagePlugin)

	auth := &cliproxyauth.Auth{
		ID:       "gemini-log-fallback-auth",
		Provider: "gemini",
		Status:   cliproxyauth.StatusActive,
		Attributes: map[string]string{
			"api_key":  "test-key",
			"base_url": server.URL,
		},
	}

	stream, err := NewGeminiExecutor(&config.Config{}).ExecuteStream(context.Background(), auth, cliproxyexecutor.Request{
		Model:   model,
		Payload: []byte(`{"contents":[{"role":"user","parts":[{"text":"hello"}]}],"stream":true}`),
		Format:  sdktranslator.FromString("gemini"),
	}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("gemini")})
	if err != nil {
		t.Fatalf("ExecuteStream() error = %v", err)
	}
	for chunk := range stream.Chunks {
		if chunk.Err != nil {
			t.Fatalf("stream chunk error = %v", chunk.Err)
		}
	}

	timer := time.NewTimer(2 * time.Second)
	defer timer.Stop()
	for {
		select {
		case record := <-usagePlugin.records:
			if record.Provider != "gemini" || record.Model != model {
				continue
			}
			if record.Failed {
				t.Fatal("completed stream without usage metadata should not be marked failed")
			}
			if !record.Streaming {
				t.Fatal("completed stream usage record should be marked streaming")
			}
			return
		case <-timer.C:
			t.Fatal("timed out waiting for Gemini stream fallback usage record")
		}
	}
}

func TestGeminiExecuteStreamPublishesUsageFromFilteredMetadataChunk(t *testing.T) {
	const model = "gemini-filtered-usage-log-test"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1beta/models/"+model+":streamGenerateContent" {
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"hello\"}]}}]}\n\n")
		_, _ = io.WriteString(w, "data: {\"usageMetadata\":{\"promptTokenCount\":2,\"candidatesTokenCount\":3,\"totalTokenCount\":5}}\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer server.Close()

	usagePlugin := &usageCapturePlugin{records: make(chan cliproxyusage.Record, 8)}
	cliproxyusage.RegisterPlugin(usagePlugin)

	auth := &cliproxyauth.Auth{
		ID:       "gemini-filtered-usage-auth",
		Provider: "gemini",
		Status:   cliproxyauth.StatusActive,
		Attributes: map[string]string{
			"api_key":  "test-key",
			"base_url": server.URL,
		},
	}

	stream, err := NewGeminiExecutor(&config.Config{}).ExecuteStream(context.Background(), auth, cliproxyexecutor.Request{
		Model:   model,
		Payload: []byte(`{"contents":[{"role":"user","parts":[{"text":"hello"}]}],"stream":true}`),
		Format:  sdktranslator.FromString("gemini"),
	}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("gemini")})
	if err != nil {
		t.Fatalf("ExecuteStream() error = %v", err)
	}
	for chunk := range stream.Chunks {
		if chunk.Err != nil {
			t.Fatalf("stream chunk error = %v", chunk.Err)
		}
	}

	timer := time.NewTimer(2 * time.Second)
	defer timer.Stop()
	for {
		select {
		case record := <-usagePlugin.records:
			if record.Provider != "gemini" || record.Model != model {
				continue
			}
			if record.Failed {
				t.Fatal("filtered usage metadata stream should not be marked failed")
			}
			if record.Detail.InputTokens != 2 || record.Detail.OutputTokens != 3 || record.Detail.TotalTokens != 5 {
				t.Fatalf("usage = %d/%d/%d, want 2/3/5", record.Detail.InputTokens, record.Detail.OutputTokens, record.Detail.TotalTokens)
			}
			return
		case <-timer.C:
			t.Fatal("timed out waiting for Gemini filtered usage metadata record")
		}
	}
}
