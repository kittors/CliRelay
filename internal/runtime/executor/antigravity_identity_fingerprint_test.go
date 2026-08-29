package executor

import (
	"context"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"

	antigravityauth "github.com/router-for-me/CLIProxyAPI/v6/internal/auth/antigravity"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/util"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

// The fingerprint default is a third copy of the client identity, and it is the
// copy that overwrites the outgoing User-Agent. Pinned to the shared constant so
// bumping the Antigravity client version in one place cannot leave the proxy
// announcing two different clients for the same account.
func TestAntigravityFingerprintDefaultMatchesTheSharedClientIdentity(t *testing.T) {
	if config.DefaultAntigravityFingerprintUserAgent != antigravityauth.ClientUserAgent {
		t.Fatalf("config default UA = %q, want the shared %q", config.DefaultAntigravityFingerprintUserAgent, antigravityauth.ClientUserAgent)
	}
	if config.DefaultAntigravityFingerprintVersion != antigravityauth.ClientVersion {
		t.Fatalf("config default version = %q, want the shared %q", config.DefaultAntigravityFingerprintVersion, antigravityauth.ClientVersion)
	}
}

// Regression: the caller's own User-Agent was learned as the account's identity
// and replayed upstream, so every request from a Node client went out as "node"
// and Antigravity answered 403 "no valid license (#3501)". What the client calls
// itself must never reach Antigravity.
func TestAntigravityBuildRequestKeepsClientUserAgentOutOfUpstream(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, clientUA := range []string{"node", "curl/8.7.1", "Mozilla/5.0"} {
		ctx := contextWithClientUserAgent(clientUA)
		executor := &AntigravityExecutor{cfg: newAntigravityFingerprintConfig(true)}
		auth := &cliproxyauth.Auth{ID: "account-under-test"}

		req, _, err := executor.buildRequest(ctx, auth, "token", "gemini-3.7-flash-high", []byte(`{"request":{}}`), false, "", "https://example.com")
		if err != nil {
			t.Fatalf("buildRequest error: %v", err)
		}
		if got := req.Header.Get("User-Agent"); got != antigravityauth.ClientUserAgent {
			t.Errorf("client sent %q, upstream User-Agent = %q, want %q", clientUA, got, antigravityauth.ClientUserAgent)
		}
	}
}

// With the feature off the resolver still returns the built-in template, so the
// executor has to skip it entirely rather than apply it — otherwise an identity
// the credential carries of its own is silently overwritten and the switch does
// nothing.
func TestAntigravityBuildRequestHonoursDisabledFingerprint(t *testing.T) {
	gin.SetMode(gin.TestMode)
	const pinned = "vscode/1.98.0 (Antigravity/4.2.0)"
	ctx := contextWithClientUserAgent("node")
	executor := &AntigravityExecutor{cfg: newAntigravityFingerprintConfig(false)}
	auth := &cliproxyauth.Auth{
		ID:         "account-under-test",
		Attributes: map[string]string{"user_agent": pinned},
	}

	req, _, err := executor.buildRequest(ctx, auth, "token", "gemini-3.7-flash-high", []byte(`{"request":{}}`), false, "", "https://example.com")
	if err != nil {
		t.Fatalf("buildRequest error: %v", err)
	}
	if got := req.Header.Get("User-Agent"); got != pinned {
		t.Errorf("upstream User-Agent = %q, want the account's own %q", got, pinned)
	}
}

func newAntigravityFingerprintConfig(enabled bool) *config.Config {
	cfg := &config.Config{}
	cfg.IdentityFingerprint.Antigravity = config.NormalizeAntigravityIdentityFingerprint(config.AntigravityIdentityFingerprintConfig{})
	cfg.IdentityFingerprint.Antigravity.Enabled = enabled
	return cfg
}

func contextWithClientUserAgent(ua string) context.Context {
	ginCtx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ginCtx.Request = httptest.NewRequest("POST", "/v1/responses", nil)
	ginCtx.Request.Header.Set("User-Agent", ua)
	return context.WithValue(context.Background(), util.ContextKeyGin, ginCtx)
}
