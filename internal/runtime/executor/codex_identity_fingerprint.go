package executor

import (
	"context"
	"net/http"
	"strings"
	"sync"

	"github.com/google/uuid"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/identityfingerprint"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/usage"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
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
	if !isCodexOAuthAdmissionAuth(auth) {
		resolved, _ := identityfingerprint.ResolveCodexSafeFallback(cfg.IdentityFingerprint.Codex)
		return resolved, true
	}

	// Learning happens before selection so the first trusted request for a new
	// client variant can immediately use its own complete identity bundle.
	_ = observeRuntimeIdentityFingerprint(identityfingerprint.ProviderCodex, auth, ctx)
	accountKey, _ := identityFingerprintAccount(auth)
	profiles, err := usage.ListIdentityFingerprintProfiles(identityfingerprint.ProviderCodex, accountKey)
	if err != nil {
		log.WithError(err).Warn("identity fingerprint: list Codex profiles")
		resolved, _ := identityfingerprint.ResolveCodexSafeFallback(cfg.IdentityFingerprint.Codex)
		return resolved, true
	}
	policy, err := usage.GetIdentityFingerprintAccountPolicy(identityfingerprint.ProviderCodex, accountKey)
	if err != nil {
		log.WithError(err).Warn("identity fingerprint: load Codex account policy")
	}
	selection := identityfingerprint.SelectCodexProfile(profiles, policy)
	if selection.Profile != nil {
		resolved, _ := identityfingerprint.ResolveCodexProfile(cfg.IdentityFingerprint.Codex, selection.Profile)
		return resolved, true
	}
	resolved, _ := identityfingerprint.ResolveCodexSafeFallback(cfg.IdentityFingerprint.Codex)
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
	// Product identity headers are an atomic bundle. Clear any values copied
	// from the inbound request or an earlier layer before applying the selected
	// profile so an absent field cannot leak in from another identity.
	for _, key := range []string{"Version", "User-Agent", "OpenAI-Beta", "X-Codex-Beta-Features"} {
		headers.Del(key)
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
