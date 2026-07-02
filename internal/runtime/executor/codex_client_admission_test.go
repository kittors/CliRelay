package executor

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/codexadmission"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/util"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

func TestEnforceCodexClientAdmission(t *testing.T) {
	gin.SetMode(gin.TestMode)

	tests := []struct {
		name       string
		auth       *cliproxyauth.Auth
		headers    http.Header
		cfg        *config.Config
		wantDenied bool
	}{
		{
			name: "disabled does not block generic client",
			auth: &cliproxyauth.Auth{
				Provider: "codex",
				Metadata: map[string]any{
					"codex_cli_only": false,
				},
			},
			headers: http.Header{
				"User-Agent": []string{"curl/8.0"},
			},
		},
		{
			name: "codex api key auth is not subject to oauth admission",
			auth: &cliproxyauth.Auth{
				Provider: "codex",
				Attributes: map[string]string{
					"api_key": "sk-test",
				},
				Metadata: map[string]any{
					"codex_cli_only": true,
				},
			},
			headers: http.Header{
				"User-Agent": []string{"curl/8.0"},
			},
		},
		{
			name: "official codex user agent allowed",
			auth: codexOAuthAdmissionTestAuth(true, nil),
			headers: http.Header{
				"User-Agent": []string{"codex_cli_rs/0.130.0"},
			},
		},
		{
			name: "official codex originator allowed",
			auth: codexOAuthAdmissionTestAuth(true, nil),
			headers: http.Header{
				"User-Agent": []string{"curl/8.0"},
				"Originator": []string{"codex_vscode"},
			},
		},
		{
			name: "claude code preset allowed only with both headers",
			auth: codexOAuthAdmissionTestAuth(true, []string{codexadmission.AllowedClientClaudeCode}),
			headers: http.Header{
				"User-Agent": []string{"Claude Code/0.5.0 (Macos 15.5; arm64) iTerm2.app (Claude Code; 1.0.4)"},
				"Originator": []string{"Claude Code"},
			},
		},
		{
			name: "global claude code preset allowed only after account enables admission",
			auth: codexOAuthAdmissionTestAuth(true, nil),
			headers: http.Header{
				"User-Agent": []string{"Claude Code/0.5.0 (Macos 15.5; arm64) iTerm2.app (Claude Code; 1.0.4)"},
				"Originator": []string{"Claude Code"},
			},
			cfg: &config.Config{
				CodexOAuthAdmission: config.CodexOAuthAdmissionConfig{
					AllowedClientPresets: []string{codexadmission.AllowedClientClaudeCode},
				},
			},
		},
		{
			name: "claude code user agent without originator denied",
			auth: codexOAuthAdmissionTestAuth(true, []string{codexadmission.AllowedClientClaudeCode}),
			headers: http.Header{
				"User-Agent": []string{"Claude Code/0.5.0 (Macos 15.5; arm64)"},
			},
			wantDenied: true,
		},
		{
			name: "claude code originator without user agent marker denied",
			auth: codexOAuthAdmissionTestAuth(true, []string{codexadmission.AllowedClientClaudeCode}),
			headers: http.Header{
				"User-Agent": []string{"curl/8.0"},
				"Originator": []string{"Claude Code"},
			},
			wantDenied: true,
		},
		{
			name: "unknown stored preset is ignored and does not allow bypass",
			auth: codexOAuthAdmissionTestAuth(true, []string{"unknown_client"}),
			headers: http.Header{
				"User-Agent": []string{"Claude Code/0.5.0 (Macos 15.5; arm64)"},
				"Originator": []string{"Claude Code"},
			},
			wantDenied: true,
		},
		{
			name: "generic client denied",
			auth: codexOAuthAdmissionTestAuth(true, nil),
			headers: http.Header{
				"User-Agent": []string{"curl/8.0"},
			},
			wantDenied: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := enforceCodexClientAdmission(contextWithCodexAdmissionHeaders(tt.headers), tt.cfg, tt.auth)
			if tt.wantDenied {
				if err == nil {
					t.Fatal("enforceCodexClientAdmission() error = nil, want denial")
				}
				status, ok := err.(statusErr)
				if !ok {
					t.Fatalf("error type = %T, want statusErr", err)
				}
				if status.StatusCode() != http.StatusForbidden {
					t.Fatalf("status = %d, want 403", status.StatusCode())
				}
				return
			}
			if err != nil {
				t.Fatalf("enforceCodexClientAdmission() error = %v", err)
			}
		})
	}
}

func TestCodexClientAdmissionConfigFiltersKnownPresets(t *testing.T) {
	auth := codexOAuthAdmissionTestAuth(true, []string{"unknown", "claude_code", "CLAUDE_CODE"})

	cfg := codexClientAdmissionConfig(nil, auth)

	if !cfg.Enabled {
		t.Fatal("Enabled = false, want true")
	}
	if len(cfg.AllowedClientPresets) != 1 || cfg.AllowedClientPresets[0] != codexadmission.AllowedClientClaudeCode {
		t.Fatalf("AllowedClientPresets = %#v, want [claude_code]", cfg.AllowedClientPresets)
	}
}

func TestCodexClientAdmissionConfigSeparatesAccountAndGlobalPresets(t *testing.T) {
	auth := codexOAuthAdmissionTestAuth(true, []string{"claude_code"})

	cfg := codexClientAdmissionConfig(&config.Config{
		CodexOAuthAdmission: config.CodexOAuthAdmissionConfig{
			AllowedClientPresets: []string{"unknown", "CLAUDE_CODE"},
		},
	}, auth)

	if !cfg.Enabled {
		t.Fatal("Enabled = false, want true")
	}
	if len(cfg.AllowedClientPresets) != 1 || cfg.AllowedClientPresets[0] != codexadmission.AllowedClientClaudeCode {
		t.Fatalf("AllowedClientPresets = %#v, want account [claude_code]", cfg.AllowedClientPresets)
	}
	if len(cfg.GlobalAllowedClientPresets) != 1 || cfg.GlobalAllowedClientPresets[0] != codexadmission.AllowedClientClaudeCode {
		t.Fatalf("GlobalAllowedClientPresets = %#v, want global [claude_code]", cfg.GlobalAllowedClientPresets)
	}
}

func TestCodexClientAdmissionGlobalPresetsDoNotEnableDisabledAccounts(t *testing.T) {
	auth := codexOAuthAdmissionTestAuth(false, nil)

	cfg := codexClientAdmissionConfig(&config.Config{
		CodexOAuthAdmission: config.CodexOAuthAdmissionConfig{
			AllowedClientPresets: []string{codexadmission.AllowedClientClaudeCode},
		},
	}, auth)

	if cfg.Enabled {
		t.Fatalf("Enabled = true for disabled account: %#v", cfg)
	}
}

func TestCodexExecutorHttpRequestEnforcesAdmissionWithoutGinContext(t *testing.T) {
	called := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, server.URL, nil)
	if err != nil {
		t.Fatalf("NewRequestWithContext() error = %v", err)
	}
	req.Header.Set("User-Agent", "curl/8.0")

	resp, err := NewCodexExecutor(&config.Config{}).HttpRequest(context.Background(), codexOAuthAdmissionTestAuth(true, nil), req)
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
	if err == nil {
		t.Fatal("HttpRequest() error = nil, want admission denial")
	}
	status, ok := err.(statusErr)
	if !ok {
		t.Fatalf("error type = %T, want statusErr", err)
	}
	if status.StatusCode() != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", status.StatusCode())
	}
	if called {
		t.Fatal("upstream server was called after admission denial")
	}
}

func TestCodexExecutorHttpRequestAllowsOfficialHeaderWithoutGinContext(t *testing.T) {
	var gotAuth string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	}))
	defer server.Close()

	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, server.URL, nil)
	if err != nil {
		t.Fatalf("NewRequestWithContext() error = %v", err)
	}
	req.Header.Set("User-Agent", "codex_cli_rs/0.130.0")

	resp, err := NewCodexExecutor(&config.Config{}).HttpRequest(context.Background(), codexOAuthAdmissionTestAuth(true, nil), req)
	if err != nil {
		t.Fatalf("HttpRequest() error = %v", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("ReadAll() error = %v", err)
	}
	if resp.StatusCode != http.StatusOK || string(body) != "ok" {
		t.Fatalf("response = %d %q, want 200 ok", resp.StatusCode, string(body))
	}
	if gotAuth != "Bearer codex-access-token" {
		t.Fatalf("Authorization = %q, want bearer token injected", gotAuth)
	}
}

func TestCodexExecutorHttpRequestPrefersGinContextHeaders(t *testing.T) {
	called := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	ctx := contextWithCodexAdmissionHeaders(http.Header{
		"User-Agent": []string{"curl/8.0"},
	})
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, server.URL, nil)
	if err != nil {
		t.Fatalf("NewRequestWithContext() error = %v", err)
	}
	req.Header.Set("User-Agent", "codex_cli_rs/0.130.0")

	resp, err := NewCodexExecutor(&config.Config{}).HttpRequest(ctx, codexOAuthAdmissionTestAuth(true, nil), req)
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
	if err == nil {
		t.Fatal("HttpRequest() error = nil, want admission denial from Gin headers")
	}
	status, ok := err.(statusErr)
	if !ok {
		t.Fatalf("error type = %T, want statusErr", err)
	}
	if status.StatusCode() != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", status.StatusCode())
	}
	if called {
		t.Fatal("upstream server was called after Gin-context admission denial")
	}
}

func codexOAuthAdmissionTestAuth(enabled bool, allowedClients []string) *cliproxyauth.Auth {
	metadata := map[string]any{
		"codex_cli_only": enabled,
		"account_id":     "acct-test",
		"email":          "codex@example.com",
		"access_token":   "codex-access-token",
	}
	if allowedClients != nil {
		metadata["codex_cli_only_allowed_clients"] = allowedClients
	}
	return &cliproxyauth.Auth{
		ID:       "codex-oauth",
		Provider: "codex",
		Metadata: metadata,
	}
}

func contextWithCodexAdmissionHeaders(headers http.Header) context.Context {
	ginCtx, _ := gin.CreateTestContext(httptest.NewRecorder())
	req, _ := http.NewRequest(http.MethodPost, "/v1/responses", nil)
	for key, values := range headers {
		for _, value := range values {
			req.Header.Add(key, value)
		}
	}
	ginCtx.Request = req
	return context.WithValue(context.Background(), util.ContextKeyGin, ginCtx)
}
