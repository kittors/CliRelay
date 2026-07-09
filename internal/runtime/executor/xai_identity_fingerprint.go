package executor

import (
	"context"
	"net/http"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/identityfingerprint"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

func xaiIdentityFingerprint(cfg *config.Config, auth *cliproxyauth.Auth, ctx context.Context) (config.XAIIdentityFingerprintConfig, bool) {
	if cfg == nil || !cfg.IdentityFingerprint.XAI.Enabled {
		return config.XAIIdentityFingerprintConfig{}, false
	}
	learned := observeRuntimeIdentityFingerprint(identityfingerprint.ProviderXAI, auth, ctx)
	resolved, _ := identityfingerprint.ResolveXAI(cfg.IdentityFingerprint.XAI, learned)
	return resolved, true
}

func applyXAIIdentityFingerprintHeaders(headers http.Header, fp config.XAIIdentityFingerprintConfig) {
	if headers == nil {
		return
	}
	if strings.TrimSpace(fp.UserAgent) != "" {
		headers.Set("User-Agent", fp.UserAgent)
	}
	if strings.TrimSpace(fp.GrokConversationID) != "" {
		headers.Set("X-Grok-Conv-Id", fp.GrokConversationID)
	}
	for key, value := range fp.CustomHeaders {
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)
		if key == "" || value == "" || isXAIFingerprintRuntimeBlockedHeader(key) {
			continue
		}
		headers.Set(key, value)
	}
}

func isXAIFingerprintRuntimeBlockedHeader(key string) bool {
	switch strings.ToLower(strings.TrimSpace(key)) {
	case "authorization", "content-type", "accept", "connection", "user-agent", "x-grok-conv-id":
		return true
	default:
		return false
	}
}
