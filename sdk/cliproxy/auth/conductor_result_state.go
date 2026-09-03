package auth

import (
	"time"
)

func ensureModelState(auth *Auth, model string) *ModelState {
	if auth == nil || model == "" {
		return nil
	}
	if auth.ModelStates == nil {
		auth.ModelStates = make(map[string]*ModelState)
	}
	if state, ok := auth.ModelStates[model]; ok && state != nil {
		return state
	}
	state := &ModelState{Status: StatusActive}
	auth.ModelStates[model] = state
	return state
}

func resetModelState(state *ModelState, now time.Time) {
	if state == nil {
		return
	}
	state.Unavailable = false
	state.Status = StatusActive
	state.StatusMessage = ""
	state.NextRetryAfter = time.Time{}
	state.LastError = nil
	state.Quota = QuotaState{}
	state.UpdatedAt = now
}

func updateAggregatedAvailability(auth *Auth, now time.Time) {
	if auth == nil || len(auth.ModelStates) == 0 {
		return
	}
	allUnavailable := true
	earliestRetry := time.Time{}
	quotaExceeded := false
	quotaRecover := time.Time{}
	quotaWindow := ""
	quotaWindowMinutes := 0
	quotaRecoveryRequired := false
	maxBackoffLevel := 0
	for _, state := range auth.ModelStates {
		if state == nil {
			continue
		}
		stateUnavailable := false
		if state.Status == StatusDisabled {
			stateUnavailable = true
		} else if quotaNeedsConfirmedRecovery(state.Quota, now) {
			stateUnavailable = true
			state.Unavailable = true
			if state.NextRetryAfter.After(now) && (earliestRetry.IsZero() || state.NextRetryAfter.Before(earliestRetry)) {
				earliestRetry = state.NextRetryAfter
			}
		} else if state.Unavailable {
			if state.NextRetryAfter.IsZero() {
				stateUnavailable = false
			} else if state.NextRetryAfter.After(now) {
				stateUnavailable = true
				if earliestRetry.IsZero() || state.NextRetryAfter.Before(earliestRetry) {
					earliestRetry = state.NextRetryAfter
				}
			} else {
				state.Unavailable = false
				state.NextRetryAfter = time.Time{}
			}
		}
		if !stateUnavailable {
			allUnavailable = false
		}
		if state.Quota.Exceeded {
			quotaExceeded = true
			quotaRecoveryRequired = quotaRecoveryRequired || state.Quota.RecoveryRequired
			if quotaRecover.IsZero() || (!state.Quota.NextRecoverAt.IsZero() && state.Quota.NextRecoverAt.Before(quotaRecover)) {
				quotaRecover = state.Quota.NextRecoverAt
				quotaWindow = state.Quota.Window
				quotaWindowMinutes = state.Quota.WindowMinutes
			} else if quotaWindow == "" {
				quotaWindow = state.Quota.Window
				quotaWindowMinutes = state.Quota.WindowMinutes
			}
			if state.Quota.BackoffLevel > maxBackoffLevel {
				maxBackoffLevel = state.Quota.BackoffLevel
			}
		}
	}
	auth.Unavailable = allUnavailable
	if allUnavailable {
		auth.NextRetryAfter = earliestRetry
	} else {
		auth.NextRetryAfter = time.Time{}
	}

	// For Antigravity, only set auth.Quota.Exceeded = true if ALL active families are exhausted.
	// If only Gemini is exhausted while Claude is fine (or vice versa), the auth level quota
	// is NOT globally exceeded, preventing global 429 lockout and top-level error badges.
	if IsAntigravityAuth(auth) {
		famGeminiExceeded := false
		famClaudeExceeded := false
		hasGemini := false
		hasClaude := false
		for m, state := range auth.ModelStates {
			if state == nil {
				continue
			}
			fam := ModelAntigravityQuotaFamily(m)
			if fam == AntigravityFamilyGemini {
				hasGemini = true
				if state.Quota.Exceeded && activeModelQuotaCooldown(state, now) {
					famGeminiExceeded = true
				}
			} else if fam == AntigravityFamilyClaude {
				hasClaude = true
				if state.Quota.Exceeded && activeModelQuotaCooldown(state, now) {
					famClaudeExceeded = true
				}
			}
		}
		// If both exist, both must be exceeded; if only one exists, that one must be exceeded
		allFamiliesExceeded := false
		if hasGemini && hasClaude {
			allFamiliesExceeded = famGeminiExceeded && famClaudeExceeded
		} else if hasGemini {
			allFamiliesExceeded = famGeminiExceeded
		} else if hasClaude {
			allFamiliesExceeded = famClaudeExceeded
		}
		quotaExceeded = allFamiliesExceeded
	}

	if quotaExceeded {
		auth.Quota.Exceeded = true
		auth.Quota.RecoveryRequired = quotaRecoveryRequired
		auth.Quota.Reason = "quota"
		auth.Quota.Window = quotaWindow
		auth.Quota.WindowMinutes = quotaWindowMinutes
		auth.Quota.NextRecoverAt = quotaRecover
		auth.Quota.BackoffLevel = maxBackoffLevel
	} else {
		auth.Quota.Exceeded = false
		auth.Quota.RecoveryRequired = false
		auth.Quota.Reason = ""
		auth.Quota.Window = ""
		auth.Quota.WindowMinutes = 0
		auth.Quota.NextRecoverAt = time.Time{}
		auth.Quota.BackoffLevel = 0
	}
}

func activeModelQuotaCooldown(state *ModelState, now time.Time) bool {
	if state == nil {
		return false
	}
	return activeQuotaCooldown(state.Quota, state.NextRetryAfter, now)
}

func activeAuthQuotaCooldown(auth *Auth, now time.Time) bool {
	if auth == nil {
		return false
	}
	return activeQuotaCooldown(auth.Quota, auth.NextRetryAfter, now)
}

func activeQuotaCooldown(quota QuotaState, retryAfter time.Time, now time.Time) bool {
	if !quota.Exceeded {
		return false
	}
	if quotaNeedsConfirmedRecovery(quota, now) {
		return true
	}
	if !quota.NextRecoverAt.IsZero() {
		return quota.NextRecoverAt.After(now)
	}
	return !retryAfter.IsZero() && retryAfter.After(now)
}

// quotaNeedsConfirmedRecovery reports whether the credential must stay blocked
// until a recovery probe confirms quota is back.
//
// The gate is always bounded by NextRecoverAt. Without that ceiling a credential
// whose probe can never succeed — API-key mode xAI auths reject the Grok Build
// billing probe outright, and the billing endpoint can simply be down — would be
// blacklisted forever, because the selector never picks it again and only a
// successful request could clear the flag. A zero or elapsed NextRecoverAt
// therefore lets one request through; if quota really is still exhausted the
// upstream 402 re-arms the gate with a fresh window.
func quotaNeedsConfirmedRecovery(quota QuotaState, now time.Time) bool {
	return quota.Exceeded && quota.RecoveryRequired && quota.NextRecoverAt.After(now)
}

func activeModelRuntimeState(state *ModelState, now time.Time) bool {
	if state == nil {
		return false
	}
	if activeModelQuotaCooldown(state, now) {
		return true
	}
	if state.Unavailable {
		return state.NextRetryAfter.IsZero() || state.NextRetryAfter.After(now)
	}
	if state.Status == StatusError && state.LastError != nil {
		return state.NextRetryAfter.IsZero() || state.NextRetryAfter.After(now)
	}
	return false
}

func activeAuthRuntimeState(auth *Auth, now time.Time) bool {
	if auth == nil {
		return false
	}
	if activeAuthQuotaCooldown(auth, now) {
		return true
	}
	if auth.Unavailable {
		return auth.NextRetryAfter.IsZero() || auth.NextRetryAfter.After(now)
	}
	if auth.Status == StatusError && auth.LastError != nil {
		return auth.NextRetryAfter.IsZero() || auth.NextRetryAfter.After(now)
	}
	return false
}

func hasModelError(auth *Auth, now time.Time) bool {
	if auth == nil || len(auth.ModelStates) == 0 {
		return false
	}
	for _, state := range auth.ModelStates {
		if state == nil {
			continue
		}
		if state.LastError != nil {
			return true
		}
		if state.Status == StatusError {
			if quotaNeedsConfirmedRecovery(state.Quota, now) || (state.Unavailable && (state.NextRetryAfter.IsZero() || state.NextRetryAfter.After(now))) {
				return true
			}
		}
	}
	return false
}

func clearAuthStateOnSuccess(auth *Auth, now time.Time) {
	if auth == nil {
		return
	}
	auth.Unavailable = false
	auth.Status = StatusActive
	auth.StatusMessage = ""
	auth.Quota.Exceeded = false
	auth.Quota.RecoveryRequired = false
	auth.Quota.Reason = ""
	auth.Quota.Window = ""
	auth.Quota.WindowMinutes = 0
	auth.Quota.NextRecoverAt = time.Time{}
	auth.Quota.BackoffLevel = 0
	auth.LastError = nil
	auth.NextRetryAfter = time.Time{}
	auth.UpdatedAt = now
}
