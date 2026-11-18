package auth

import (
	"strings"
	"time"
)

func applyRecoveredRefreshState(auth *Auth, now time.Time, refreshErr error) {
	if auth == nil {
		return
	}
	if canKeepRefreshFailureActive(auth, now) {
		auth.NextRefreshAfter = time.Time{}
		auth.LastError = nil
		auth.Status = StatusActive
		auth.StatusMessage = ""
		return
	}
	if auth.Disabled || auth.Status == StatusDisabled || auth.Unavailable || auth.Quota.Exceeded {
		return
	}
	auth.NextRefreshAfter = now.Add(refreshFailureBackoff)
	auth.LastError = &Error{Message: refreshErrorMessage(refreshErr)}
	auth.Status = StatusError
	auth.StatusMessage = auth.LastError.Message
}

func refreshErrorMessage(err error) string {
	if err == nil {
		return "refresh token recovered but access token is not usable"
	}
	return err.Error()
}

func supportsRefreshTokenRaceRecovery(auth *Auth) bool {
	if auth == nil {
		return false
	}
	provider := strings.ToLower(strings.TrimSpace(auth.Provider))
	if providerSupportsRefreshTokenRaceRecovery(provider) {
		return true
	}
	if auth.Metadata != nil {
		if typ, _ := auth.Metadata["type"].(string); providerSupportsRefreshTokenRaceRecovery(typ) {
			return true
		}
	}
	return false
}

func providerSupportsRefreshTokenRaceRecovery(provider string) bool {
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case "codex", "claude":
		return true
	default:
		return false
	}
}

func canKeepRefreshFailureActive(auth *Auth, now time.Time) bool {
	if auth == nil || auth.Disabled || auth.Status == StatusDisabled {
		return false
	}
	if auth.Unavailable || auth.Quota.Exceeded {
		return false
	}
	if !authAccessTokenUsable(auth, now) {
		return false
	}
	return auth.Status == "" || auth.Status == StatusUnknown || auth.Status == StatusActive || auth.Status == StatusError
}

func authAccessTokenUsable(auth *Auth, now time.Time) bool {
	if authTokenValue(auth, "access_token", "accessToken") == "" {
		return false
	}
	expiry, hasExpiry := auth.ExpirationTime()
	return !hasExpiry || expiry.After(now)
}

func authTokenValue(auth *Auth, keys ...string) string {
	if auth == nil {
		return ""
	}
	if val := tokenValueFromMap(auth.Metadata, keys...); val != "" {
		return val
	}
	for _, nestedKey := range []string{"token", "Token"} {
		nested, ok := auth.Metadata[nestedKey]
		if !ok {
			continue
		}
		switch typed := nested.(type) {
		case map[string]any:
			if val := tokenValueFromMap(typed, keys...); val != "" {
				return val
			}
		case map[string]string:
			for _, key := range keys {
				if val := strings.TrimSpace(typed[key]); val != "" {
					return val
				}
			}
		}
	}
	return ""
}

func tokenValueFromMap(meta map[string]any, keys ...string) string {
	if len(meta) == 0 {
		return ""
	}
	for _, key := range keys {
		switch raw := meta[key].(type) {
		case string:
			if val := strings.TrimSpace(raw); val != "" {
				return val
			}
		case []byte:
			if val := strings.TrimSpace(string(raw)); val != "" {
				return val
			}
		}
	}
	return ""
}
