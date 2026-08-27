package executor

import (
	"context"
	"net/http"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/identityfingerprint"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

// kimiIdentityFingerprint resolves the outbound kimi-cli identity for an account
// and returns the learned record alongside it, because the device id is carried
// only by the observation (see ResolveKimi).
//
// Scope note: this covers the OpenAI-compatible path. A Claude-format request to
// a Kimi credential is served by ClaudeExecutor against the same gateway, where
// the caller really is a Claude client, so it keeps the Claude fingerprint.
func kimiIdentityFingerprint(cfg *config.Config, auth *cliproxyauth.Auth, ctx context.Context) (config.KimiIdentityFingerprintConfig, *identityfingerprint.LearnedRecord, bool) {
	if cfg == nil || !cfg.IdentityFingerprint.Kimi.Enabled {
		return config.KimiIdentityFingerprintConfig{}, nil, false
	}
	learned := observeRuntimeIdentityFingerprint(identityfingerprint.ProviderKimi, auth, ctx)
	preset := config.WithKimiHeaderDefaults(cfg.IdentityFingerprint.Kimi, cfg.KimiHeaderDefaults)
	resolved, _ := identityfingerprint.ResolveKimi(preset, learned)
	return resolved, learned, true
}

// applyKimiIdentityFingerprintHeaders overwrites the host-derived defaults with
// the resolved identity. Empty fields are left alone so the caller's fallback
// (hostname, GOOS/GOARCH) survives when nothing has been learned or configured.
func applyKimiIdentityFingerprintHeaders(headers http.Header, fp config.KimiIdentityFingerprintConfig) {
	if headers == nil {
		return
	}
	for header, value := range map[string]string{
		"User-Agent":         fp.UserAgent,
		"X-Msh-Platform":     fp.Platform,
		"X-Msh-Version":      fp.Version,
		"X-Msh-Device-Name":  fp.DeviceName,
		"X-Msh-Device-Model": fp.DeviceModel,
	} {
		if value = strings.TrimSpace(value); value != "" {
			headers.Set(header, value)
		}
	}
	for key, value := range fp.CustomHeaders {
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)
		if key == "" || value == "" || isKimiFingerprintRuntimeBlockedHeader(key) {
			continue
		}
		headers.Set(key, value)
	}
}

// resolveKimiOutboundDeviceID picks the device id sent upstream.
//
// The credential's own id wins: it is the device Moonshot associated with the
// account at login, and replacing it would present a known account from an
// unknown machine. A learned id is next — it is the real client's device and is
// scoped to this account, unlike the host fallback that every credential without
// an id would otherwise share.
func resolveKimiOutboundDeviceID(auth *cliproxyauth.Auth, learned *identityfingerprint.LearnedRecord) string {
	if deviceID := resolveKimiDeviceID(auth); deviceID != "" {
		return deviceID
	}
	return identityfingerprint.KimiLearnedDeviceID(learned)
}

func isKimiFingerprintRuntimeBlockedHeader(key string) bool {
	switch strings.ToLower(strings.TrimSpace(key)) {
	case "authorization", "content-type", "accept", "connection", "user-agent",
		"x-msh-platform", "x-msh-version", "x-msh-device-name", "x-msh-device-model",
		"x-msh-device-id":
		return true
	default:
		return false
	}
}
