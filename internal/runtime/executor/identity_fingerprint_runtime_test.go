package executor

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/identityfingerprint"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/usage"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/util"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v6/sdk/translator"
	"github.com/tidwall/gjson"
)

func initIdentityFingerprintRuntimeDB(t *testing.T) {
	t.Helper()
	usage.CloseDB()
	if err := usage.InitDB(filepath.Join(t.TempDir(), "usage.db"), config.RequestLogStorageConfig{}, time.UTC); err != nil {
		t.Fatalf("usage.InitDB returned error: %v", err)
	}
	t.Cleanup(usage.CloseDB)
}

func contextWithInboundHeaders(method, path string, headers http.Header) context.Context {
	gin.SetMode(gin.TestMode)
	ginCtx, _ := gin.CreateTestContext(httptest.NewRecorder())
	req := httptest.NewRequest(method, path, nil)
	for key, values := range headers {
		for _, value := range values {
			req.Header.Add(key, value)
		}
	}
	ginCtx.Request = req
	return context.WithValue(context.Background(), util.ContextKeyGin, ginCtx)
}

func authSubjectKey(t *testing.T, auth *cliproxyauth.Auth) string {
	t.Helper()
	identity := usage.ResolveAuthSubjectIdentity(auth)
	if identity == nil || identity.ID == "" {
		t.Fatalf("ResolveAuthSubjectIdentity(%#v) returned empty identity", auth)
	}
	return identity.ID
}

func TestClaudeExecutorLearnsAndAppliesClaudeCodeFingerprint(t *testing.T) {
	initIdentityFingerprintRuntimeDB(t)

	var gotHeaders http.Header
	var gotBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" {
			t.Fatalf("path = %q, want /v1/messages", r.URL.Path)
		}
		gotHeaders = r.Header.Clone()
		var err error
		gotBody, err = io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read request body: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"msg_1","type":"message","model":"claude-sonnet-4-5","role":"assistant","content":[{"type":"text","text":"ok"}],"usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	defer server.Close()

	cfg := &config.Config{
		IdentityFingerprint: config.IdentityFingerprintConfig{
			Claude: config.ClaudeIdentityFingerprintConfig{
				Enabled:     true,
				SessionMode: "fixed",
				SessionID:   "session-learned-claude",
			},
		},
	}
	auth := &cliproxyauth.Auth{
		ID:       "claude-auth",
		Provider: "claude",
		Attributes: map[string]string{
			"api_key":  "sk-ant-oat-test",
			"base_url": server.URL,
		},
		Metadata: map[string]any{
			"account_id":   "claude-account-id",
			"account_uuid": "claude-account-uuid",
		},
	}
	headers := http.Header{}
	headers.Set("User-Agent", "claude-cli/2.1.200 (external, cli)")
	headers.Set("X-App", "cli")
	headers.Set("Anthropic-Beta", "claude-code-20250219,oauth-2025-04-20")
	headers.Set("X-Stainless-Package-Version", "0.99.0")
	headers.Set("X-Stainless-Runtime-Version", "v24.4.0")
	headers.Set("X-Stainless-Timeout", "700")
	ctx := contextWithInboundHeaders(http.MethodPost, "/v1/messages", headers)

	executor := NewClaudeExecutor(cfg)
	payload := []byte(`{"model":"claude-sonnet-4-5","messages":[{"role":"user","content":[{"type":"text","text":"hello from learned claude"}]}]}`)
	if _, err := executor.Execute(ctx, auth, cliproxyexecutor.Request{
		Model:   "claude-sonnet-4-5",
		Payload: payload,
	}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("claude"),
	}); err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}

	if got := gotHeaders.Get("User-Agent"); got != "claude-cli/2.1.200 (external, cli)" {
		t.Fatalf("User-Agent = %q, want learned Claude Code UA", got)
	}
	if got := gotHeaders.Get("X-Stainless-Package-Version"); got != "0.99.0" {
		t.Fatalf("X-Stainless-Package-Version = %q, want learned package version", got)
	}
	if got := gotHeaders.Get("X-Stainless-Runtime-Version"); got != "v24.4.0" {
		t.Fatalf("X-Stainless-Runtime-Version = %q, want learned runtime", got)
	}
	if got := gotHeaders.Get("X-Stainless-Timeout"); got != "700" {
		t.Fatalf("X-Stainless-Timeout = %q, want learned timeout", got)
	}
	if got := gotHeaders.Get("X-Claude-Code-Session-Id"); got != "session-learned-claude" {
		t.Fatalf("X-Claude-Code-Session-Id = %q, want fixed session", got)
	}
	billing := gjson.GetBytes(gotBody, "system.0.text").String()
	if !gjson.GetBytes(gotBody, "system.1.text").Exists() || !containsAll(billing, "cc_version=2.1.200.", "cc_entrypoint=cli") {
		t.Fatalf("system blocks/billing = %s, want learned Claude Code billing", string(gotBody))
	}

	var userID struct {
		AccountUUID string `json:"account_uuid"`
		SessionID   string `json:"session_id"`
	}
	if err := json.Unmarshal([]byte(gjson.GetBytes(gotBody, "metadata.user_id").String()), &userID); err != nil {
		t.Fatalf("metadata.user_id is not JSON: %v; body=%s", err, string(gotBody))
	}
	if userID.AccountUUID != "claude-account-uuid" || userID.SessionID != "session-learned-claude" {
		t.Fatalf("metadata.user_id = %#v, want account UUID and fixed session", userID)
	}

	record, err := usage.GetIdentityFingerprint(identityfingerprint.ProviderClaude, authSubjectKey(t, auth))
	if err != nil {
		t.Fatalf("GetIdentityFingerprint returned error: %v", err)
	}
	if record == nil || record.Fields[identityfingerprint.FieldClaudeCLIVersion] != "2.1.200" {
		t.Fatalf("learned record = %#v, want Claude CLI version learned", record)
	}
}

func TestClaudeCountTokensLearnsAndAppliesClaudeCodeFingerprint(t *testing.T) {
	initIdentityFingerprintRuntimeDB(t)

	var gotHeaders http.Header
	var gotBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages/count_tokens" {
			t.Fatalf("path = %q, want /v1/messages/count_tokens", r.URL.Path)
		}
		gotHeaders = r.Header.Clone()
		var err error
		gotBody, err = io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read request body: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"input_tokens":7}`))
	}))
	defer server.Close()

	cfg := &config.Config{
		IdentityFingerprint: config.IdentityFingerprintConfig{
			Claude: config.ClaudeIdentityFingerprintConfig{Enabled: true},
		},
	}
	auth := &cliproxyauth.Auth{
		ID:       "claude-count-auth",
		Provider: "claude",
		Attributes: map[string]string{
			"api_key":  "sk-ant-oat-count",
			"base_url": server.URL,
		},
		Metadata: map[string]any{"account_id": "claude-count-account"},
	}
	headers := http.Header{}
	headers.Set("User-Agent", "claude-cli/2.1.201 (external, cli)")
	headers.Set("X-App", "cli")
	headers.Set("Anthropic-Beta", "claude-code-20250219,oauth-2025-04-20")
	headers.Set("X-Stainless-Package-Version", "1.0.0")
	headers.Set("X-Stainless-Runtime-Version", "v24.5.0")
	headers.Set("X-Stainless-Timeout", "750")
	ctx := contextWithInboundHeaders(http.MethodPost, "/v1/messages/count_tokens", headers)

	executor := NewClaudeExecutor(cfg)
	payload := []byte(`{"model":"claude-sonnet-4-5","messages":[{"role":"user","content":"count these tokens"}]}`)
	if _, err := executor.CountTokens(ctx, auth, cliproxyexecutor.Request{
		Model:   "claude-sonnet-4-5",
		Payload: payload,
	}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("claude"),
	}); err != nil {
		t.Fatalf("CountTokens returned error: %v", err)
	}

	if got := gotHeaders.Get("User-Agent"); got != "claude-cli/2.1.201 (external, cli)" {
		t.Fatalf("User-Agent = %q, want learned Claude CountTokens UA", got)
	}
	if got := gotHeaders.Get("X-Stainless-Runtime-Version"); got != "v24.5.0" {
		t.Fatalf("X-Stainless-Runtime-Version = %q, want learned runtime", got)
	}
	if billing := gjson.GetBytes(gotBody, "system.0.text").String(); !containsAll(billing, "cc_version=2.1.201.", "cc_entrypoint=cli") {
		t.Fatalf("billing header block = %q, want learned CountTokens fingerprint", billing)
	}
}

func TestCodexHeadersReplayLearnedFingerprintWithoutInboundHeaders(t *testing.T) {
	initIdentityFingerprintRuntimeDB(t)

	cfg := &config.Config{
		IdentityFingerprint: config.IdentityFingerprintConfig{
			Codex: config.CodexIdentityFingerprintConfig{Enabled: true},
		},
	}
	auth := &cliproxyauth.Auth{
		ID:       "codex-auth",
		Provider: "codex",
		Metadata: map[string]any{
			"access_token": "codex-token",
			"account_id":   "codex-account-id",
		},
	}
	inbound := http.Header{}
	inbound.Set("User-Agent", "codex_cli_rs/0.130.0 (Mac OS 26.3.1; arm64) iTerm.app/3.6.9")
	inbound.Set("Version", "0.130.0")
	inbound.Set("Originator", "codex_cli_rs")
	inbound.Set("X-Codex-Beta-Features", "compact_mode")
	ctx := contextWithInboundHeaders(http.MethodPost, "/v1/responses", inbound)
	req := httptest.NewRequest(http.MethodPost, "https://chatgpt.com/backend-api/codex/responses", nil)
	req = req.WithContext(ctx)

	applyCodexHeaders(req, cfg, auth, "codex-token", false)

	if got := req.Header.Get("User-Agent"); got != "codex_cli_rs/0.130.0 (Mac OS 26.3.1; arm64) iTerm.app/3.6.9" {
		t.Fatalf("first User-Agent = %q, want learned Codex UA", got)
	}
	if got := req.Header.Get("Originator"); got != "codex_cli_rs" {
		t.Fatalf("first Originator = %q, want learned Codex originator", got)
	}
	if got := req.Header.Get("X-Codex-Beta-Features"); got != "compact_mode" {
		t.Fatalf("first X-Codex-Beta-Features = %q, want learned beta features", got)
	}

	replayReq := httptest.NewRequest(http.MethodPost, "https://chatgpt.com/backend-api/codex/responses", nil)
	applyCodexHeaders(replayReq, cfg, auth, "codex-token", false)

	if got := replayReq.Header.Get("User-Agent"); got != "codex_cli_rs/0.130.0 (Mac OS 26.3.1; arm64) iTerm.app/3.6.9" {
		t.Fatalf("replayed User-Agent = %q, want stored learned Codex UA", got)
	}
	if got := replayReq.Header.Get("Version"); got != "0.130.0" {
		t.Fatalf("replayed Version = %q, want stored learned Codex version", got)
	}
	if got := replayReq.Header.Get("Originator"); got != "codex_cli_rs" {
		t.Fatalf("replayed Originator = %q, want stored learned Codex originator", got)
	}
	if got := replayReq.Header.Get("X-Codex-Beta-Features"); got != "compact_mode" {
		t.Fatalf("replayed X-Codex-Beta-Features = %q, want stored learned beta features", got)
	}
}

func TestCodexWebsocketHeadersReplayLearnedFingerprintWithoutInboundHeaders(t *testing.T) {
	initIdentityFingerprintRuntimeDB(t)

	cfg := &config.Config{
		IdentityFingerprint: config.IdentityFingerprintConfig{
			Codex: config.CodexIdentityFingerprintConfig{Enabled: true},
		},
	}
	auth := &cliproxyauth.Auth{
		ID:       "codex-ws-auth",
		Provider: "codex",
		Metadata: map[string]any{
			"access_token": "codex-token",
			"account_id":   "codex-ws-account-id",
		},
	}
	inbound := http.Header{}
	inbound.Set("User-Agent", "codex_cli_rs/0.131.0 (Mac OS 26.3.1; arm64) iTerm.app/3.6.9")
	inbound.Set("Version", "0.131.0")
	inbound.Set("Originator", "codex_cli_rs")
	inbound.Set("OpenAI-Beta", "responses_websockets=2026-02-06")
	inbound.Set("X-Codex-Beta-Features", "ws_compact_mode")
	ctx := contextWithInboundHeaders(http.MethodGet, "/v1/responses/ws", inbound)

	first := applyCodexWebsocketHeaders(ctx, http.Header{}, cfg, auth, "codex-token")
	if got := first.Get("User-Agent"); got != "" {
		t.Fatalf("websocket User-Agent = %q, want empty because UA is not forwarded over websocket", got)
	}
	if got := first.Get("OpenAI-Beta"); got != "responses_websockets=2026-02-06" {
		t.Fatalf("first OpenAI-Beta = %q, want learned websocket beta", got)
	}
	if got := first.Get("Originator"); got != "codex_cli_rs" {
		t.Fatalf("first Originator = %q, want learned originator", got)
	}

	replayed := applyCodexWebsocketHeaders(context.Background(), http.Header{}, cfg, auth, "codex-token")
	if got := replayed.Get("User-Agent"); got != "" {
		t.Fatalf("replayed websocket User-Agent = %q, want empty", got)
	}
	if got := replayed.Get("OpenAI-Beta"); got != "responses_websockets=2026-02-06" {
		t.Fatalf("replayed OpenAI-Beta = %q, want stored learned websocket beta", got)
	}
	if got := replayed.Get("Originator"); got != "codex_cli_rs" {
		t.Fatalf("replayed Originator = %q, want stored learned originator", got)
	}
	if got := replayed.Get("X-Codex-Beta-Features"); got != "ws_compact_mode" {
		t.Fatalf("replayed X-Codex-Beta-Features = %q, want stored learned beta features", got)
	}
}

func TestGeminiCLIHeadersReplayLearnedFingerprintWithoutInboundHeaders(t *testing.T) {
	initIdentityFingerprintRuntimeDB(t)

	cfg := &config.Config{
		IdentityFingerprint: config.IdentityFingerprintConfig{
			Gemini: config.GeminiIdentityFingerprintConfig{Enabled: true},
		},
	}
	auth := &cliproxyauth.Auth{
		ID:       "gemini-auth",
		Provider: "gemini",
		Metadata: map[string]any{
			"account_id": "gemini-account-id",
		},
	}
	inbound := http.Header{}
	inbound.Set("User-Agent", "google-api-nodejs-client/9.16.0")
	inbound.Set("X-Goog-Api-Client", "gl-node/24.1.0")
	inbound.Set("Client-Metadata", "ideType=IDE_UNSPECIFIED,platform=PLATFORM_UNSPECIFIED,pluginType=GEMINI")
	ctx := contextWithInboundHeaders(http.MethodPost, "/v1beta/models/gemini:generateContent", inbound)

	req := httptest.NewRequest(http.MethodPost, "https://cloudcode-pa.googleapis.com/v1internal:generateContent", nil)
	req = req.WithContext(ctx)
	applyGeminiCLIHeaders(req, cfg, auth)
	if got := req.Header.Get("User-Agent"); got != "google-api-nodejs-client/9.16.0" {
		t.Fatalf("User-Agent = %q, want learned Gemini UA", got)
	}
	if got := req.Header.Get("X-Goog-Api-Client"); got != "gl-node/24.1.0" {
		t.Fatalf("X-Goog-Api-Client = %q, want learned Gemini API client", got)
	}

	replayReq := httptest.NewRequest(http.MethodPost, "https://cloudcode-pa.googleapis.com/v1internal:generateContent", nil)
	applyGeminiCLIHeaders(replayReq, cfg, auth)
	if got := replayReq.Header.Get("User-Agent"); got != "google-api-nodejs-client/9.16.0" {
		t.Fatalf("replayed User-Agent = %q, want stored learned Gemini UA", got)
	}
	if got := replayReq.Header.Get("X-Goog-Api-Client"); got != "gl-node/24.1.0" {
		t.Fatalf("replayed X-Goog-Api-Client = %q, want stored learned Gemini API client", got)
	}
	if got := replayReq.Header.Get("Client-Metadata"); got != "ideType=IDE_UNSPECIFIED,platform=PLATFORM_UNSPECIFIED,pluginType=GEMINI" {
		t.Fatalf("replayed Client-Metadata = %q, want stored learned Gemini metadata", got)
	}
}

func containsAll(value string, needles ...string) bool {
	for _, needle := range needles {
		if !strings.Contains(value, needle) {
			return false
		}
	}
	return true
}
