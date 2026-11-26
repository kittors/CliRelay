package auth

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strings"
	"time"
)

func preserveRuntimeFields(dst *Auth, src *Auth) {
	preserveRuntimeFieldsForUpdate(dst, src, false)
}

func preserveRuntimeFieldsForUpdate(dst *Auth, src *Auth, credentialCreate bool) {
	if dst == nil || src == nil {
		return
	}
	if dst.Runtime == nil {
		dst.Runtime = src.Runtime
	}
	// An explicit create-or-replace (re-login / upload / import) replaces the
	// underlying credential object; do not carry an older TokenStorage over when
	// the new record is metadata-only.
	if dst.Storage == nil && !credentialCreate {
		dst.Storage = src.Storage
	}
	if dst.Index == "" {
		dst.Index = src.Index
		dst.indexAssigned = src.indexAssigned
	}
	if dst.FileName == "" {
		dst.FileName = src.FileName
	}
	if !credentialCreate && !CredentialRefreshTokenChanged(dst, src) && dst.permanentRefreshFailedDigest == "" {
		dst.permanentRefreshFailedDigest = src.permanentRefreshFailedDigest
	}
}

func refreshTokenDigest(auth *Auth) string {
	token := authRefreshToken(auth)
	if token == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func isPermanentRefreshFailedToken(auth *Auth) bool {
	if auth == nil || auth.permanentRefreshFailedDigest == "" {
		return false
	}
	digest := refreshTokenDigest(auth)
	return digest != "" && digest == auth.permanentRefreshFailedDigest
}

// CredentialRefreshTokenChanged reports whether dst carries a different refresh
// token from src (or installs one where src had none).
func CredentialRefreshTokenChanged(dst, src *Auth) bool {
	if dst == nil || src == nil {
		return false
	}
	dstRefresh := authRefreshToken(dst)
	srcRefresh := authRefreshToken(src)
	return dstRefresh != "" && dstRefresh != srcRefresh
}

// authCredentialMaterialChanged reports whether dst installs a newer access or
// refresh token than src. Token replacement invalidates stale 401 auth errors
// without touching account-level 429 quota cooldowns.
func authCredentialMaterialChanged(dst, src *Auth) bool {
	if dst == nil || src == nil {
		return false
	}
	if CredentialRefreshTokenChanged(dst, src) {
		return true
	}
	dstAccess := authTokenValue(dst, "access_token", "accessToken")
	srcAccess := authTokenValue(src, "access_token", "accessToken")
	return dstAccess != "" && dstAccess != srcAccess
}

func preserveAvailabilityRuntimeForUpdate(dst *Auth, src *Auth, now time.Time) {
	preserveAvailabilityRuntimeForUpdateWithOptions(dst, src, now, false)
}

func preserveAvailabilityRuntimeForUpdateWithOptions(dst *Auth, src *Auth, now time.Time, dropAuthFailures bool) []string {
	if dst == nil || src == nil {
		return nil
	}
	if dst.Disabled || dst.Status == StatusDisabled {
		return nil
	}

	var releasedAuthFailureModels []string
	preservedModelState := false
	if len(src.ModelStates) > 0 {
		if dst.ModelStates == nil {
			dst.ModelStates = make(map[string]*ModelState, len(src.ModelStates))
		}
		for model, srcState := range src.ModelStates {
			if !shouldPreserveModelRuntimeState(dst.ModelStates[model], srcState, now) {
				continue
			}
			if shouldDropOllamaCloudNotFoundRuntimeState(dst, srcState) {
				continue
			}
			// A fresh login, import, or rotated token supersedes 401 auth
			// cooldowns from the old token while keeping real 429 quota limits.
			if dropAuthFailures && isAuthFailureModelState(srcState) {
				releasedAuthFailureModels = append(releasedAuthFailureModels, model)
				continue
			}
			dst.ModelStates[model] = srcState.Clone()
			preservedModelState = true
		}
		if len(dst.ModelStates) == 0 {
			dst.ModelStates = nil
		}
	}

	if preservedModelState {
		updateAggregatedAvailability(dst, now)
		if !dropAuthFailures || !isAuthFailureError(src.LastError, src.StatusMessage) {
			if dst.Status == "" || dst.Status == StatusUnknown || dst.Status == StatusActive {
				dst.Status = src.Status
			}
			if dst.StatusMessage == "" {
				dst.StatusMessage = src.StatusMessage
			}
			if dst.LastError == nil {
				dst.LastError = cloneError(src.LastError)
			}
		}
	}

	if shouldDropOllamaCloudNotFoundAuthRuntimeState(dst, src) ||
		(dropAuthFailures && isAuthFailureAuthState(src)) ||
		!shouldPreserveAuthRuntimeState(dst, src, now) {
		return releasedAuthFailureModels
	}
	dst.Unavailable = src.Unavailable
	dst.Status = src.Status
	dst.StatusMessage = src.StatusMessage
	dst.LastError = cloneError(src.LastError)
	dst.Quota = src.Quota
	dst.NextRetryAfter = src.NextRetryAfter
	return releasedAuthFailureModels
}

func isAuthFailureModelState(state *ModelState) bool {
	if state == nil || state.Quota.Exceeded {
		return false
	}
	return isAuthFailureError(state.LastError, state.StatusMessage)
}

func isAuthFailureAuthState(auth *Auth) bool {
	if auth == nil || auth.Quota.Exceeded {
		return false
	}
	if isAuthFailureError(auth.LastError, auth.StatusMessage) {
		return true
	}
	return auth.Status == StatusError && auth.LastError != nil && auth.LastError.HTTPStatus == 0
}

func isAuthFailureError(err *Error, statusMessage string) bool {
	if err != nil {
		if err.HTTPStatus == http.StatusUnauthorized {
			return true
		}
		switch CooldownReasonOf(err, QuotaState{}) {
		case CooldownReasonUnauthorized, CooldownReasonTokenRefreshing, CooldownReasonReauthRequired:
			return true
		}
	}
	msg := strings.ToLower(strings.TrimSpace(statusMessage))
	if msg == "" {
		return false
	}
	return msg == codexOAuthRefreshPendingMessage ||
		msg == claudeOAuthRefreshPendingMessage ||
		strings.HasPrefix(msg, "re-login required:")
}

func clearStaleOAuthHealthOnCredentialCreate(auth *Auth) {
	if auth == nil || auth.Metadata == nil {
		return
	}
	delete(auth.Metadata, CodexOAuthHealthMetadataKey)
	delete(auth.Metadata, ClaudeOAuthHealthMetadataKey)
}

func shouldDropOllamaCloudNotFoundRuntimeState(auth *Auth, state *ModelState) bool {
	if auth == nil || state == nil || state.LastError == nil {
		return false
	}
	if !strings.EqualFold(strings.TrimSpace(auth.Provider), "ollama-cloud") {
		return false
	}
	// Older builds misrouted Ollama Cloud through OpenAI-compatible chat and
	// persisted false 404 cooldowns; a real 404 will be recorded again.
	return state.LastError.HTTPStatus == http.StatusNotFound
}

func shouldDropOllamaCloudNotFoundAuthRuntimeState(dst *Auth, src *Auth) bool {
	if dst == nil || src == nil || src.LastError == nil {
		return false
	}
	if !strings.EqualFold(strings.TrimSpace(dst.Provider), "ollama-cloud") {
		return false
	}
	return src.LastError.HTTPStatus == http.StatusNotFound
}
