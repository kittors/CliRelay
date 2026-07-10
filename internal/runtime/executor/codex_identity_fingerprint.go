package executor

import (
	"context"
	"net/http"
	"strings"
	"sync"

	"github.com/google/uuid"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/identityfingerprint"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

var (
	codexServerSessionOnce sync.Once
	codexServerSessionID   string
)

func codexServerStableSessionID() string {
	codexServerSessionOnce.Do(func() {
		codexServerSessionID = uuid.NewString()
	})
	return codexServerSessionID
}

func codexIdentityFingerprint(cfg *config.Config, auth *cliproxyauth.Auth, ctx context.Context) (config.CodexIdentityFingerprintConfig, bool) {
	if cfg == nil || !cfg.IdentityFingerprint.Codex.Enabled {
		return config.CodexIdentityFingerprintConfig{}, false
	}
	learned := observeRuntimeIdentityFingerprint(identityfingerprint.ProviderCodex, auth, ctx)
	resolved, _ := identityfingerprint.ResolveCodex(cfg.IdentityFingerprint.Codex, learned)
	return resolved, true
}

func codexFingerprintSessionID(fp config.CodexIdentityFingerprintConfig) string {
	switch strings.TrimSpace(strings.ToLower(fp.SessionMode)) {
	case "fixed":
		if strings.TrimSpace(fp.SessionID) != "" {
			return strings.TrimSpace(fp.SessionID)
		}
		return codexServerStableSessionID()
	case "per-request":
		return uuid.NewString()
	default:
		return codexServerStableSessionID()
	}
}

func applyCodexIdentityFingerprintHeaders(headers http.Header, fp config.CodexIdentityFingerprintConfig, websocket bool) {
	if headers == nil {
		return
	}
	// Follow upstream codex-tui behavior: only send headers when values are non-empty.
	if strings.TrimSpace(fp.Version) != "" {
		headers.Set("Version", fp.Version)
	}
	if strings.TrimSpace(fp.UserAgent) != "" {
		headers.Set("User-Agent", fp.UserAgent)
	}
	if strings.TrimSpace(headers.Get("Session_id")) == "" {
		headers.Set("Session_id", codexFingerprintSessionID(fp))
	}
	if websocket {
		if strings.TrimSpace(fp.WebsocketBeta) != "" {
			headers.Set("OpenAI-Beta", fp.WebsocketBeta)
		}
	}
	if strings.TrimSpace(fp.BetaFeatures) != "" {
		headers.Set("X-Codex-Beta-Features", fp.BetaFeatures)
	}
	for key, value := range fp.CustomHeaders {
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)
		if key == "" || value == "" || isCodexFingerprintRuntimeBlockedHeader(key) {
			continue
		}
		headers.Set(key, value)
	}
}

func isCodexFingerprintRuntimeBlockedHeader(key string) bool {
	switch strings.ToLower(strings.TrimSpace(key)) {
	case "authorization", "content-type", "accept", "connection", "chatgpt-account-id",
		"user-agent", "version", "session_id", "session-id", "originator", "openai-beta", "x-codex-beta-features":
		return true
	default:
		return false
	}
}
