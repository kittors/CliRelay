package executor

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/util"
)

func TestApplyCodexHeadersIdentityFingerprintOverridesClientHeaders(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ginCtx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ginCtx.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	ginCtx.Request.Header.Set("User-Agent", "curl/8.14.1")
	ginCtx.Request.Header.Set("Version", "0.80.0")
	ginCtx.Request.Header.Set("Session_id", "client-session")

	req := httptest.NewRequest(http.MethodPost, "https://chatgpt.com/backend-api/codex/responses", nil)
	req = req.WithContext(context.WithValue(req.Context(), util.ContextKeyGin, ginCtx))
	cfg := &config.Config{
		IdentityFingerprint: config.IdentityFingerprintConfig{
			Codex: config.CodexIdentityFingerprintConfig{
				Enabled:       true,
				UserAgent:     "codex_cli_rs/test",
				Version:       "9.9.9",
				Originator:    "codex_cli_rs",
				WebsocketBeta: "responses_websockets=test",
				SessionMode:   "fixed",
				SessionID:     "server-session",
			},
		},
	}

	applyCodexHeaders(req, cfg, nil, "token", true)

	if got := req.Header.Get("User-Agent"); got != "codex_cli_rs/test" {
		t.Fatalf("User-Agent = %q, want fingerprint value", got)
	}
	if got := req.Header.Get("Version"); got != "9.9.9" {
		t.Fatalf("Version = %q, want fingerprint value", got)
	}
	if got := req.Header.Get("Session_id"); got != "server-session" {
		t.Fatalf("Session_id = %q, want fingerprint value", got)
	}
}

func TestApplyCodexHeadersIdentityFingerprintPreservesPromptCacheSession(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "https://chatgpt.com/backend-api/codex/responses", nil)
	req.Header.Set("Session_id", "cache-session")
	cfg := &config.Config{
		IdentityFingerprint: config.IdentityFingerprintConfig{
			Codex: config.CodexIdentityFingerprintConfig{
				Enabled:     true,
				SessionMode: "fixed",
				SessionID:   "server-session",
			},
		},
	}

	applyCodexHeaders(req, cfg, nil, "token", false)

	if got := req.Header.Get("Session_id"); got != "cache-session" {
		t.Fatalf("Session_id = %q, want prompt cache session", got)
	}
}

func TestApplyCodexWebsocketHeadersIdentityFingerprintOverridesClientHeaders(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ginCtx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ginCtx.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	ginCtx.Request.Header.Set("User-Agent", "Terminal/1")
	ginCtx.Request.Header.Set("OpenAI-Beta", "client-beta")
	ginCtx.Request.Header.Set("x-codex-turn-state", "client-turn")

	ctx := context.WithValue(context.Background(), util.ContextKeyGin, ginCtx)
	headers := http.Header{}
	cfg := &config.Config{
		IdentityFingerprint: config.IdentityFingerprintConfig{
			Codex: config.CodexIdentityFingerprintConfig{
				Enabled:       true,
				UserAgent:     "codex_cli_rs/test",
				Version:       "9.9.9",
				Originator:    "codex_cli_rs",
				WebsocketBeta: "responses_websockets=test",
				SessionMode:   "fixed",
				SessionID:     "server-session",
				CustomHeaders: map[string]string{
					"x-codex-turn-state": "server-turn",
				},
			},
		},
	}

	got := applyCodexWebsocketHeaders(ctx, headers, cfg, nil, "token")

	if ua := got.Get("User-Agent"); ua != "" {
		t.Fatalf("User-Agent = %q, want empty (upstream does not forward UA over websocket)", ua)
	}
	if beta := got.Get("OpenAI-Beta"); beta != "responses_websockets=test" {
		t.Fatalf("OpenAI-Beta = %q, want fingerprint value", beta)
	}
	if turn := got.Get("x-codex-turn-state"); turn != "server-turn" {
		t.Fatalf("x-codex-turn-state = %q, want custom fingerprint value", turn)
	}
}
