package warmup_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/management/warmup"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

func TestAntigravityDriverRealPipeline(t *testing.T) {
	var requestedPath string
	var requestedBody string
	var authHeader string

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestedPath = r.URL.Path
		authHeader = r.Header.Get("Authorization")
		b, _ := io.ReadAll(r.Body)
		requestedBody = string(b)

		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"response\":{\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"pong\"}]}}]}}\n\n"))
	}))
	defer server.Close()

	cfg := &config.Config{}
	driver := warmup.NewAntigravityDriver(cfg)

	auth := &coreauth.Auth{
		ID:       "ag-pipeline-test",
		Provider: "antigravity",
		Metadata: map[string]any{
			"type":         "antigravity",
			"access_token": "ya29.real-test-token",
			"expired":      time.Now().Add(2 * time.Hour).Format(time.RFC3339),
			"base_url":     server.URL,
		},
	}

	targets := driver.GetTargets(auth)
	res, err := driver.ExecuteWarmup(context.Background(), auth, targets[0])
	if err != nil {
		t.Fatalf("ExecuteWarmup() err = %v", err)
	}

	if !res.Success {
		t.Fatalf("expected success, got error: %s", res.ErrorMessage)
	}
	if !strings.HasPrefix(authHeader, "Bearer ya29.") {
		t.Fatalf("missing or invalid Authorization header: %s", authHeader)
	}
	if !strings.Contains(requestedPath, "streamGenerateContent") {
		t.Fatalf("unexpected upstream path: %s", requestedPath)
	}
	if !strings.Contains(requestedBody, "project") {
		t.Fatalf("expected wrapped antigravity envelope in payload, got: %s", requestedBody)
	}
}
