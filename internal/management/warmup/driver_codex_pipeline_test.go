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

func TestCodexDriverRealPipeline(t *testing.T) {
	var requestedPath string
	var requestedBody string
	var authHeader string

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestedPath = r.URL.Path
		authHeader = r.Header.Get("Authorization")
		b, _ := io.ReadAll(r.Body)
		requestedBody = string(b)

		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"type\":\"response.completed\"}\n\ndata: [DONE]\n\n"))
	}))
	defer server.Close()

	cfg := &config.Config{}
	driver := warmup.NewCodexDriver(cfg)

	auth := &coreauth.Auth{
		ID:       "codex-pipeline-test",
		Provider: "codex",
		Attributes: map[string]string{
			"api_key":  "eyJ-codex-test-token",
			"base_url": server.URL,
		},
		Metadata: map[string]any{
			"type":         "codex",
			"access_token": "eyJ-codex-test-token",
			"expired":      time.Now().Add(2 * time.Hour).Format(time.RFC3339),
		},
	}

	targets := driver.GetTargets(auth)
	if len(targets) == 0 {
		t.Fatal("expected codex targets")
	}

	res, err := driver.ExecuteWarmup(context.Background(), auth, targets[0])
	if err != nil {
		t.Fatalf("ExecuteWarmup() err = %v", err)
	}

	if !res.Success {
		t.Fatalf("expected success, got error: %s", res.ErrorMessage)
	}
	if !strings.HasPrefix(authHeader, "Bearer eyJ") {
		t.Fatalf("missing or invalid Authorization header: %s", authHeader)
	}
	if !strings.Contains(requestedBody, "gpt-5.3-codex-spark") {
		t.Fatalf("expected spark model in request, got: %s", requestedBody)
	}
	_ = requestedPath
}
