package api

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	proxyconfig "github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	runtimeexecutor "github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor"
	_ "github.com/router-for-me/CLIProxyAPI/v8/internal/translator"
	sdkaccess "github.com/router-for-me/CLIProxyAPI/v8/sdk/access"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
)

func TestModelStreamingHotPathEndToEndReusesUpstreamConnection(t *testing.T) {
	var newConnections atomic.Int32
	spoolDir := t.TempDir()
	t.Setenv("TMPDIR", spoolDir)

	upstream := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("upstream path = %q", r.URL.Path)
		}
		if _, err := io.Copy(io.Discard, r.Body); err != nil {
			t.Errorf("read upstream request: %v", err)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		largeChunk := strings.Repeat("x", 1024)
		for i := range 300 {
			_, _ = fmt.Fprintf(w, "data: {\"id\":\"chatcmpl-test\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"test-model\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"chunk-%d-%s\"},\"finish_reason\":null}]}\n\n", i, largeChunk)
			if flusher != nil {
				flusher.Flush()
			}
		}
		_, _ = io.WriteString(w, "data: {\"id\":\"chatcmpl-test\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"test-model\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":4,\"total_tokens\":5}}\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	upstream.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateNew {
			newConnections.Add(1)
		}
	}
	upstream.Start()
	defer upstream.Close()

	cfg := &proxyconfig.Config{
		SDKConfig: sdkconfig.SDKConfig{
			APIKeys: []string{"test-key"},
		},
		Port:                   0,
		AuthDir:                t.TempDir(),
		Debug:                  true,
		LoggingToFile:          false,
		UsageStatisticsEnabled: false,
	}
	cfg.RequestLogStorage.StoreContent = true

	authManager := auth.NewManager(nil, nil, nil)
	executor := runtimeexecutor.NewOpenAICompatExecutor("mock-openai", cfg)
	authManager.RegisterExecutor(executor)
	authFile := &auth.Auth{
		ID:       "hotpath-auth",
		Provider: executor.Identifier(),
		Status:   auth.StatusActive,
		Attributes: map[string]string{
			"base_url": upstream.URL + "/v1",
			"api_key":  "upstream-test-key",
		},
	}
	if _, err := authManager.Register(context.Background(), authFile); err != nil {
		t.Fatalf("register auth: %v", err)
	}
	registry.GetGlobalRegistry().RegisterClient(authFile.ID, authFile.Provider, []*registry.ModelInfo{{ID: "test-model"}})
	t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(authFile.ID) })

	server := NewServer(
		cfg,
		authManager,
		sdkaccess.NewManager(),
		filepath.Join(t.TempDir(), "config.yaml"),
		WithRequestLoggerFactory(nil),
	)

	for requestIndex := range 2 {
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"test-model","messages":[{"role":"user","content":"hello"}],"stream":true}`))
		req.Header.Set("Authorization", "Bearer test-key")
		req.Header.Set("Content-Type", "application/json")
		recorder := httptest.NewRecorder()
		server.engine.ServeHTTP(recorder, req)
		if recorder.Code != http.StatusOK {
			t.Fatalf("request %d status = %d, body=%s", requestIndex, recorder.Code, recorder.Body.String())
		}
		body := recorder.Body.String()
		if !strings.Contains(body, "chunk-0-") || !strings.Contains(body, "chunk-299-") || !strings.Contains(body, "[DONE]") {
			t.Fatalf("request %d incomplete stream body (len=%d)", requestIndex, len(body))
		}
		spools, errGlob := filepath.Glob(filepath.Join(spoolDir, "clirelay-api-exchange-*"))
		if errGlob != nil {
			t.Fatalf("glob API exchange spools: %v", errGlob)
		}
		if len(spools) != 0 {
			t.Fatalf("request %d leaked API exchange spools: %v", requestIndex, spools)
		}
		deadline := time.Now().Add(2 * time.Second)
		for {
			usageSpools, errUsageGlob := filepath.Glob(filepath.Join(spoolDir, "cliproxy-usage-*"))
			if errUsageGlob != nil {
				t.Fatalf("glob usage spools: %v", errUsageGlob)
			}
			if len(usageSpools) == 0 {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("request %d leaked usage spools: %v", requestIndex, usageSpools)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}

	if got := newConnections.Load(); got != 1 {
		t.Fatalf("upstream new connections = %d, want 1 reused keep-alive connection", got)
	}
}
