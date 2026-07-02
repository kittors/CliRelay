package executor

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"

	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

func codexAccountScopedExplicitSessionID(auth *cliproxyauth.Auth, raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	scope := codexSessionIsolationScope(auth)
	if scope == "" {
		return raw
	}
	sum := sha256.Sum256([]byte(scope + "\x00" + raw))
	return raw + "-" + hex.EncodeToString(sum[:8])
}

func codexSessionIsolationScope(auth *cliproxyauth.Auth) string {
	accountKey, authSubjectID := identityFingerprintAccount(auth)
	if strings.TrimSpace(accountKey) != "" {
		if strings.TrimSpace(authSubjectID) != "" {
			return "account:" + strings.TrimSpace(accountKey) + ":" + strings.TrimSpace(authSubjectID)
		}
		return "account:" + strings.TrimSpace(accountKey)
	}
	if auth == nil {
		return ""
	}
	if strings.TrimSpace(auth.ID) != "" {
		return "auth:" + strings.TrimSpace(auth.ID)
	}
	if idx := strings.TrimSpace(auth.EnsureIndex()); idx != "" {
		return "index:" + idx
	}
	if auth.Metadata != nil {
		for _, key := range []string{"account_id", "email", "access_token", "refresh_token"} {
			if value, ok := auth.Metadata[key].(string); ok && strings.TrimSpace(value) != "" {
				sum := sha256.Sum256([]byte(key + "\x00" + strings.TrimSpace(value)))
				return "metadata:" + key + ":" + hex.EncodeToString(sum[:8])
			}
		}
	}
	return ""
}

func codexPromptCacheMapKey(auth *cliproxyauth.Auth, model, userID string) string {
	scope := codexSessionIsolationScope(auth)
	model = strings.TrimSpace(model)
	userID = strings.TrimSpace(userID)
	if scope == "" {
		return model + "-" + userID
	}
	return scope + ":" + model + "-" + userID
}
