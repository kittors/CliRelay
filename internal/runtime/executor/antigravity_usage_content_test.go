package executor

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
	cliproxyusage "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/usage"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v6/sdk/translator"
)

func TestAntigravityExecutePublishesUsageContent(t *testing.T) {
	setUsageBodyCaptureForTest(t, true)
	const input = `{"request":{"contents":[{"role":"user","parts":[{"text":"ag input marker"}]}]}}`
	const output = `{"response":{"usageMetadata":{"promptTokenCount":2,"candidatesTokenCount":3,"totalTokenCount":5}}}`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != antigravityGeneratePath {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		_, _ = w.Write([]byte(output))
	}))
	defer server.Close()

	usagePlugin := &usageCapturePlugin{records: make(chan cliproxyusage.Record, 8)}
	cliproxyusage.RegisterPlugin(usagePlugin)

	resp, err := NewAntigravityExecutor(&config.Config{}).Execute(context.Background(), antigravityTestAuth(server.URL), cliproxyexecutor.Request{
		Model:   "gemini-2.5-pro",
		Payload: []byte(input),
	}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("antigravity")})
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if len(resp.Payload) == 0 {
		t.Fatal("expected response payload")
	}

	record := waitForAntigravityUsageRecord(t, usagePlugin.records, "ag input marker")
	if !strings.Contains(record.InputContent, "ag input marker") {
		t.Fatalf("InputContent = %q, want request payload", record.InputContent)
	}
	if !strings.Contains(record.OutputContent, "usageMetadata") {
		t.Fatalf("OutputContent = %q, want upstream response", record.OutputContent)
	}
}

func TestAntigravityExecuteStreamPublishesUsageContent(t *testing.T) {
	setUsageBodyCaptureForTest(t, true)
	const input = `{"request":{"contents":[{"role":"user","parts":[{"text":"ag stream marker"}]}]}}`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != antigravityStreamPath {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"response\":{\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"hello\"}]}}]}}\n\n")
		_, _ = io.WriteString(w, "data: {\"response\":{\"usageMetadata\":{\"promptTokenCount\":2,\"candidatesTokenCount\":3,\"totalTokenCount\":5}}}\n\n")
	}))
	defer server.Close()

	usagePlugin := &usageCapturePlugin{records: make(chan cliproxyusage.Record, 8)}
	cliproxyusage.RegisterPlugin(usagePlugin)

	stream, err := NewAntigravityExecutor(&config.Config{}).ExecuteStream(context.Background(), antigravityTestAuth(server.URL), cliproxyexecutor.Request{
		Model:   "gemini-2.5-pro",
		Payload: []byte(input),
	}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("antigravity")})
	if err != nil {
		t.Fatalf("ExecuteStream() error = %v", err)
	}
	for chunk := range stream.Chunks {
		if chunk.Err != nil {
			t.Fatalf("stream chunk error = %v", chunk.Err)
		}
	}

	record := waitForAntigravityUsageRecord(t, usagePlugin.records, "ag stream marker")
	if !strings.Contains(record.InputContent, "ag stream marker") {
		t.Fatalf("InputContent = %q, want request payload", record.InputContent)
	}
	if !strings.Contains(record.OutputContent, "usageMetadata") {
		t.Fatalf("OutputContent = %q, want SSE response", record.OutputContent)
	}
}

func antigravityTestAuth(baseURL string) *cliproxyauth.Auth {
	return &cliproxyauth.Auth{
		ID:       "antigravity-test-auth",
		Provider: antigravityAuthType,
		Status:   cliproxyauth.StatusActive,
		Attributes: map[string]string{
			"base_url": baseURL,
		},
		Metadata: map[string]any{
			"access_token": "test-token",
			"expired":      time.Now().Add(2 * time.Hour).UTC().Format(time.RFC3339),
		},
	}
}

func waitForAntigravityUsageRecord(t *testing.T, records <-chan cliproxyusage.Record, inputMarker string) cliproxyusage.Record {
	t.Helper()
	timer := time.NewTimer(2 * time.Second)
	defer timer.Stop()
	for {
		select {
		case record := <-records:
			if record.Provider == antigravityAuthType && strings.Contains(record.InputContent, inputMarker) {
				return record
			}
		case <-timer.C:
			t.Fatal("timed out waiting for antigravity usage record")
		}
	}
}
