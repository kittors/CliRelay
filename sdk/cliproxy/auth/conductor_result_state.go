package auth

import (
	"time"
)

// quotaOnlyModelUnavailability reports whether every unavailable model state on
// the auth is held down by a quota cooldown rather than by a failure that applies
// to the credential as a whole (401, disabled, ...). Only the former is safe to
// clear at the auth level when another quota pool is still serving.
func quotaOnlyModelUnavailability(auth *Auth, now time.Time) bool {
	if auth == nil {
		return false
	}
	for _, state := range auth.ModelStates {
		if state == nil {
			continue
		}
		if state.Status == StatusDisabled {
			return false
		}
		if state.Unavailable && !activeModelQuotaCooldown(state, now) {
			return false
		}
	}
	return true
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
