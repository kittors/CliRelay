package auth

import (
	"net/http"
	"strings"
	"time"

	"github.com/tidwall/gjson"
)

// Codex OAuth health, recorded the way Claude OAuth health is: a metadata entry
// the management API shows and the scheduler reads.
//
// A Codex 401 means one of two very different things, and both used to park the
// account for 30 minutes:
//
//   - token_expired: the access token ran out (or the upstream invalidated it
//     early) but the refresh token is fine. One refresh fixes it; waiting half an
//     hour for nothing is the cost.
//   - token_revoked, invalid_grant, refresh_token_reused, account_deactivated: the
//     credential is dead. Refreshing cannot help and the operator has to sign the
//     account in again, which the panel can only say if the scheduler knows.
//
// Only these codes are recognised. A 401 with any other body keeps the existing
// handling.

const (
	CodexOAuthHealthMetadataKey = "codex_oauth_health"

	codexOAuthHealthStatusActive         = "active"
	codexOAuthHealthStatusRefreshPending = "refresh_pending"
	codexOAuthHealthStatusReauthRequired = "reauth_required"

	codexOAuthRefreshPendingMessage = "codex_oauth_refresh_pending"
	codexOAuthReasonTokenExpired    = "token_expired"
	// codexOAuthReasonExpiredNoRefresh is how an expired token with no refresh
	// token behind it is recorded: nothing can renew it, so it is a dead
	// credential like a revoked one. It is not a code the upstream sends.
	codexOAuthReasonExpiredNoRefresh = "token_expired_no_refresh"

	// codexOAuthRefreshCooldown is how long the model rests while the refresh
	// loop (which wakes every few seconds) renews the token.
	codexOAuthRefreshCooldown = 30 * time.Second
	// codexOAuthReauthCooldown matches the cooldown any 401 starts: a dead
	// credential is probed again after this long, in case it was replaced.
	codexOAuthReauthCooldown = 30 * time.Minute
)

type codexUnauthorizedKind int

const (
	codexUnauthorizedUnknown codexUnauthorizedKind = iota
	codexUnauthorizedExpired
	codexUnauthorizedReauth
)

var codexReauthCodes = map[string]struct{}{
	"token_revoked":        {},
	"invalid_grant":        {},
	"refresh_token_reused": {},
	"account_deactivated":  {},
}

// codexUnauthorizedCode reads the error code of a Codex 401. ChatGPT puts it at
// error.code; OAuth-style bodies put it at code, or in error when that is a
// string. Anything that is not a known code reads as "".
func codexUnauthorizedCode(err *Error) string {
	if err == nil {
		return ""
	}
	candidates := []string{err.Code}
	if message := strings.TrimSpace(err.Message); message != "" && gjson.Valid(message) {
		root := gjson.Parse(message)
		candidates = append(candidates, root.Get("error.code").String(), root.Get("code").String())
		if inner := root.Get("error"); inner.Type == gjson.String {
			candidates = append(candidates, inner.String())
		}
	}
	for _, candidate := range candidates {
		candidate = strings.ToLower(strings.TrimSpace(candidate))
		if candidate == codexOAuthReasonTokenExpired {
			return candidate
		}
		if _, ok := codexReauthCodes[candidate]; ok {
			return candidate
		}
	}
	return ""
}

func codexUnauthorizedKindOf(code string) codexUnauthorizedKind {
	switch {
	case code == codexOAuthReasonTokenExpired:
		return codexUnauthorizedExpired
	case code != "":
		return codexUnauthorizedReauth
	default:
		return codexUnauthorizedUnknown
	}
}

// isCodexOAuthAuth reports whether auth is a Codex account signed in with OAuth,
// as opposed to one that holds an API key.
func isCodexOAuthAuth(auth *Auth) bool {
	if auth == nil || !strings.EqualFold(strings.TrimSpace(auth.Provider), "codex") {
		return false
	}
	if auth.Attributes != nil && strings.TrimSpace(auth.Attributes["api_key"]) != "" {
		return false
	}
	return authTokenValue(auth, "access_token", "accessToken") != "" || authRefreshToken(auth) != ""
}

// codexOAuthRefreshPending reports whether a 401 told us the token needs renewing
// and the renewal has not happened yet.
func codexOAuthRefreshPending(auth *Auth) bool {
	if !isCodexOAuthAuth(auth) || authRefreshToken(auth) == "" {
		return false
	}
	health := cloneCodexOAuthHealth(auth)
	if len(health) == 0 {
		return false
	}
	status, _ := health["status"].(string)
	return strings.EqualFold(strings.TrimSpace(status), codexOAuthHealthStatusRefreshPending)
}

func markCodexOAuthHealthSuccessLocked(auth *Auth, now time.Time) {
	if !isCodexOAuthAuth(auth) {
		return
	}
	// A credential that never failed has nothing to correct; do not write a
	// health entry for every healthy account.
	if _, ok := auth.Metadata[CodexOAuthHealthMetadataKey]; !ok {
		return
	}
	health := ensureCodexOAuthHealth(auth, now)
	health["status"] = codexOAuthHealthStatusActive
	health["last_runtime_at"] = formatClaudeOAuthHealthTime(now)
	delete(health, "temporary_unschedulable_until")
	delete(health, "temporary_unschedulable_reason")
	delete(health, "reauth_reason")
	auth.Metadata[CodexOAuthHealthMetadataKey] = health
}

func markCodexOAuthHealthRefreshSuccessLocked(auth *Auth, now time.Time) {
	if !isCodexOAuthAuth(auth) {
		return
	}
	if _, ok := auth.Metadata[CodexOAuthHealthMetadataKey]; !ok {
		return
	}
	health := ensureCodexOAuthHealth(auth, now)
	health["status"] = codexOAuthHealthStatusActive
	health["last_refresh_at"] = formatClaudeOAuthHealthTime(now)
	delete(health, "temporary_unschedulable_until")
	delete(health, "temporary_unschedulable_reason")
	delete(health, "reauth_reason")
	auth.Metadata[CodexOAuthHealthMetadataKey] = health
}

// clearCodexOAuthRuntimeRestrictionLocked removes only the scheduling part of the
// health entry, for an operator reset: the diagnostics stay, but a stale
// refresh_pending or reauth_required can no longer describe an account the
// operator just cleared.
func clearCodexOAuthRuntimeRestrictionLocked(auth *Auth, now time.Time) bool {
	if auth == nil || auth.Metadata == nil {
		return false
	}
	if _, ok := auth.Metadata[CodexOAuthHealthMetadataKey]; !ok {
		return false
	}
	health := cloneCodexOAuthHealth(auth)
	changed := false
	if status, _ := health["status"].(string); !strings.EqualFold(strings.TrimSpace(status), codexOAuthHealthStatusActive) {
		health["status"] = codexOAuthHealthStatusActive
		changed = true
	}
	for _, key := range []string{"temporary_unschedulable_until", "temporary_unschedulable_reason", "reauth_reason"} {
		if _, ok := health[key]; ok {
			delete(health, key)
			changed = true
		}
	}
	if !changed {
		return false
	}
	health["updated_at"] = formatClaudeOAuthHealthTime(now)
	auth.Metadata[CodexOAuthHealthMetadataKey] = health
	return true
}

// applyCodexOAuthFailureLocked handles a Codex OAuth 401 whose code is known. It
// reports whether it recorded the failure; false leaves the generic handling to
// run, which is what every other 401 gets.
func applyCodexOAuthFailureLocked(auth *Auth, result Result, now time.Time, effects *resultStateEffects) bool {
	if !isCodexOAuthAuth(auth) || statusCodeFromResult(result.Error) != http.StatusUnauthorized {
		return false
	}
	code := codexUnauthorizedCode(result.Error)
	kind := codexUnauthorizedKindOf(code)
	if kind == codexUnauthorizedUnknown {
		return false
	}

	health := ensureCodexOAuthHealth(auth, now)
	health["last_runtime_status"] = http.StatusUnauthorized
	health["last_runtime_at"] = formatClaudeOAuthHealthTime(now)
	health["last_401_at"] = formatClaudeOAuthHealthTime(now)
	health["last_401_code"] = code
	if result.Error != nil && strings.TrimSpace(result.Error.Message) != "" {
		health["last_401_message"] = trimClaudeOAuthHealthMessage(result.Error.Message)
	}

	failure := cloneError(result.Error)
	if failure == nil {
		failure = &Error{HTTPStatus: http.StatusUnauthorized}
	}
	failure.Code = code

	// An expired access token is only worth a refresh when there is a refresh
	// token to spend. Without one nothing can renew it and the account has to be
	// signed in again, the same as a revoked one.
	if kind == codexUnauthorizedExpired && authRefreshToken(auth) != "" {
		next := now.Add(codexOAuthRefreshCooldown)
		// A refresh that is already running, or that just failed, sets when the
		// next one may start. Retrying before that only draws the same 401.
		if wait := auth.NextRefreshAfter; wait.After(next) {
			next = wait
		}
		if limit := now.Add(codexOAuthReauthCooldown); next.After(limit) {
			next = limit
		}
		health["status"] = codexOAuthHealthStatusRefreshPending
		health["temporary_unschedulable_until"] = formatClaudeOAuthHealthTime(next)
		health["temporary_unschedulable_reason"] = codexOAuthReasonTokenExpired
		delete(health, "reauth_reason")
		auth.Metadata[CodexOAuthHealthMetadataKey] = health
		applyCodexOAuthCooldownLocked(auth, result.Model, now, next, StatusActive, codexOAuthRefreshPendingMessage, failure, codexOAuthReasonTokenExpired, effects)
		return true
	}

	// Reached for a revoked or deactivated credential, and for an expired token
	// that has no refresh token to spend.
	if kind == codexUnauthorizedExpired {
		code = codexOAuthReasonExpiredNoRefresh
		failure.Code = code
	}
	health["status"] = codexOAuthHealthStatusReauthRequired
	health["reauth_reason"] = code
	delete(health, "temporary_unschedulable_until")
	delete(health, "temporary_unschedulable_reason")
	auth.Metadata[CodexOAuthHealthMetadataKey] = health
	message := "re-login required: " + code
	applyCodexOAuthCooldownLocked(auth, result.Model, now, now.Add(codexOAuthReauthCooldown), StatusError, message, failure, "unauthorized", effects)
	return true
}

// applyCodexOAuthCooldownLocked parks the model (or the whole account when the
// result names no model) until next. It is the Claude OAuth cooldown with the
// status left to the caller: a token that is being renewed is not an error, a
// dead credential is.
func applyCodexOAuthCooldownLocked(auth *Auth, model string, now, next time.Time, status Status, message string, failure *Error, suspendReason string, effects *resultStateEffects) {
	if model != "" {
		state := ensureModelState(auth, model)
		state.Unavailable = true
		state.Status = status
		state.StatusMessage = message
		state.NextRetryAfter = next
		state.LastError = cloneError(failure)
		state.Quota = QuotaState{}
		state.UpdatedAt = now
		if effects != nil {
			effects.suspendReason = suspendReason
			effects.shouldSuspendModel = true
		}
		auth.LastError = cloneError(failure)
		auth.StatusMessage = message
		auth.Status = status
		auth.UpdatedAt = now
		updateAggregatedAvailability(auth, now)
		return
	}
	auth.Unavailable = true
	auth.Status = status
	auth.StatusMessage = message
	auth.NextRetryAfter = next
	auth.LastError = cloneError(failure)
	auth.UpdatedAt = now
}

func ensureCodexOAuthHealth(auth *Auth, now time.Time) map[string]any {
	if auth.Metadata == nil {
		auth.Metadata = make(map[string]any)
	}
	health := cloneCodexOAuthHealth(auth)
	health["enabled"] = true
	health["updated_at"] = formatClaudeOAuthHealthTime(now)
	health["refresh_available"] = authRefreshToken(auth) != ""
	return health
}

func cloneCodexOAuthHealth(auth *Auth) map[string]any {
	if auth == nil || auth.Metadata == nil {
		return map[string]any{}
	}
	return cloneAnyMap(auth.Metadata[CodexOAuthHealthMetadataKey])
}
