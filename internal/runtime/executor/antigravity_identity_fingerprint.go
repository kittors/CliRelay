package executor

import (
	"context"
	"net/http"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/identityfingerprint"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

func antigravityIdentityFingerprint(cfg *config.Config, auth *cliproxyauth.Auth, ctx context.Context) (config.AntigravityIdentityFingerprintConfig, *identityfingerprint.LearnedRecord, bool) {
	if cfg == nil || !cfg.IdentityFingerprint.Antigravity.Enabled {
		return config.DefaultAntigravityIdentityFingerprint(), nil, false
	}
	learned := observeRuntimeIdentityFingerprint(identityfingerprint.ProviderAntigravity, auth, ctx)
	resolved, _ := identityfingerprint.ResolveAntigravity(cfg.IdentityFingerprint.Antigravity, learned)
	return resolved, learned, true
}

func applyAntigravityIdentityFingerprintHeaders(headers http.Header, fp config.AntigravityIdentityFingerprintConfig) {
	if headers == nil || !fp.Enabled {
		return
	}
	if ua := strings.TrimSpace(fp.UserAgent); ua != "" {
		headers.Set("User-Agent", ua)
	}
	for key, value := range fp.CustomHeaders {
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)
		if key != "" && value != "" {
			headers.Set(key, value)
		}
	}
}
