package executor

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	xaiauth "github.com/router-for-me/CLIProxyAPI/v6/internal/auth/xai"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

// xaiVersionAboveDefault is a Grok CLI version newer than the builtin default,
// the floor a learned identity has to reach to be replayed.
func xaiVersionAboveDefault() string {
	parts := strings.Split(config.DefaultXAIFingerprintClientVersion, ".")
	last, _ := strconv.Atoi(parts[len(parts)-1])
	parts[len(parts)-1] = strconv.Itoa(last + 100)
	return strings.Join(parts, ".")
}

func grokBuildOAuthAuth(id, email string) *cliproxyauth.Auth {
	return &cliproxyauth.Auth{
		ID:         id,
		Provider:   "xai",
		Attributes: map[string]string{"auth_kind": "oauth"},
		Metadata:   map[string]any{"email": email, "sub": id + "-subject"},
	}
}

// TestXAIChatHeadersKeepTheIdentitysOwnVersion pins the outbound half of
// 2026-11-09: the Grok Build chat path overwrote X-Grok-Client-Version with the
// builtin constant, so a learned identity went out naming two versions. The
// learned bundle goes out whole, its mode included.
func TestXAIChatHeadersKeepTheIdentitysOwnVersion(t *testing.T) {
	initIdentityFingerprintRuntimeDB(t)

	v := xaiVersionAboveDefault()
	cfg := &config.Config{IdentityFingerprint: config.IdentityFingerprintConfig{
		XAI: config.XAIIdentityFingerprintConfig{Enabled: true},
	}}
	auth := grokBuildOAuthAuth("xai-chat-bundle", "xai-chat-bundle@example.com")
	inbound := http.Header{}
	inbound.Set("User-Agent", "grok-pager/"+v+" grok-shell/"+v+" (macos; aarch64)")
	inbound.Set("X-Grok-Client-Identifier", "grok-pager")
	inbound.Set("X-Grok-Client-Version", v)
	inbound.Set("X-Grok-Client-Mode", "interactive")
	ctx := contextWithInboundHeaders(http.MethodPost, "/v1/chat/completions", inbound)

	req := httptest.NewRequest(http.MethodPost, xaiauth.CLIChatProxyBaseURL+"/responses", nil).WithContext(ctx)
	applyXAIChatHeaders(req, cfg, auth, "xai-token", true)

	want := map[string]string{
		"User-Agent":               "grok-pager/" + v + " grok-shell/" + v + " (macos; aarch64)",
		"X-Grok-Client-Identifier": "grok-pager",
		"X-Grok-Client-Version":    v,
		"X-Grok-Client-Mode":       "interactive",
		xaiTokenAuthHeader:         xaiTokenAuthValue,
	}
	for header, value := range want {
		if got := req.Header.Get(header); got != value {
			t.Fatalf("%s = %q, want %q", header, got, value)
		}
	}
}

// TestXAIChatHeadersSendAVersionWithoutAFingerprint keeps what the overwrite
// was for: cli-chat-proxy answers a request with no client version 426, so the
// builtin one goes when no identity set it.
func TestXAIChatHeadersSendAVersionWithoutAFingerprint(t *testing.T) {
	cfg := &config.Config{IdentityFingerprint: config.IdentityFingerprintConfig{
		XAI: config.XAIIdentityFingerprintConfig{Enabled: false},
	}}
	auth := grokBuildOAuthAuth("xai-chat-plain", "xai-chat-plain@example.com")

	req := httptest.NewRequest(http.MethodPost, xaiauth.CLIChatProxyBaseURL+"/responses", nil)
	applyXAIChatHeaders(req, cfg, auth, "xai-token", true)

	if got := req.Header.Get("X-Grok-Client-Version"); got != config.DefaultXAIFingerprintClientVersion {
		t.Fatalf("X-Grok-Client-Version = %q, want %q", got, config.DefaultXAIFingerprintClientVersion)
	}
	if got := req.Header.Get("X-Grok-Client-Mode"); got != "" {
		t.Fatalf("X-Grok-Client-Mode = %q, want none without a learned bundle", got)
	}
}
