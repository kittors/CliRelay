package auth

import (
	"net/http"
	"testing"
	"time"
)

// legacyIsAuthBlockedForModel is isAuthBlockedForModel as it was before
// authBlockForModel took its body, kept to pin that the routing simulator's
// refactor changed no selection decision.
func legacyIsAuthBlockedForModel(auth *Auth, model string, now time.Time) (bool, blockReason, time.Time) {
	if auth == nil {
		return true, blockReasonOther, time.Time{}
	}
	if auth.Disabled || auth.Status == StatusDisabled {
		return true, blockReasonDisabled, time.Time{}
	}
	if auth.Quota.Exceeded && (!IsAntigravityAuth(auth) || isAntigravityExhaustedForModel(auth, model, now)) {
		next := auth.Quota.NextRecoverAt
		if quotaNeedsConfirmedRecovery(auth.Quota, now) || (!next.IsZero() && next.After(now)) {
			if auth.NextRetryAfter.After(now) && (next.IsZero() || auth.NextRetryAfter.Before(next)) {
				next = auth.NextRetryAfter
			}
			if !next.After(now) {
				next = time.Time{}
			}
			return true, blockReasonCooldown, next
		}
	}
	if model != "" {
		if len(auth.ModelStates) > 0 {
			state, ok := auth.ModelStates[model]
			if (!ok || state == nil) && model != "" {
				baseModel := canonicalModelKey(model)
				if baseModel != "" && baseModel != model {
					state, ok = auth.ModelStates[baseModel]
				}
			}
			if ok && state != nil {
				if state.Status == StatusDisabled {
					return true, blockReasonDisabled, time.Time{}
				}
				if quotaNeedsConfirmedRecovery(state.Quota, now) {
					next := state.Quota.NextRecoverAt
					if state.NextRetryAfter.After(now) && (next.IsZero() || state.NextRetryAfter.Before(next)) {
						next = state.NextRetryAfter
					}
					if !next.After(now) {
						next = time.Time{}
					}
					return true, blockReasonCooldown, next
				}
				if state.Unavailable {
					if state.NextRetryAfter.IsZero() {
						return false, blockReasonNone, time.Time{}
					}
					if state.NextRetryAfter.After(now) {
						next := state.NextRetryAfter
						if !state.Quota.NextRecoverAt.IsZero() && state.Quota.NextRecoverAt.After(now) {
							next = state.Quota.NextRecoverAt
						}
						if next.Before(now) {
							next = now
						}
						if state.Quota.Exceeded {
							return true, blockReasonCooldown, next
						}
						return true, blockReasonOther, next
					}
				}
				return false, blockReasonNone, time.Time{}
			}
			if IsAntigravityAuth(auth) {
				fam := ModelAntigravityQuotaFamily(model)
				for m, s := range auth.ModelStates {
					if s == nil || !s.Quota.Exceeded {
						continue
					}
					if ModelAntigravityQuotaFamily(m) == fam && activeModelQuotaCooldown(s, now) {
						next := s.Quota.NextRecoverAt
						if s.NextRetryAfter.After(now) && (next.IsZero() || s.NextRetryAfter.Before(next)) {
							next = s.NextRetryAfter
						}
						return true, blockReasonCooldown, next
					}
				}
			}
		}
		return false, blockReasonNone, time.Time{}
	}
	if auth.Unavailable && auth.NextRetryAfter.After(now) {
		next := auth.NextRetryAfter
		if !auth.Quota.NextRecoverAt.IsZero() && auth.Quota.NextRecoverAt.After(now) {
			next = auth.Quota.NextRecoverAt
		}
		if next.Before(now) {
			next = now
		}
		if auth.Quota.Exceeded {
			return true, blockReasonCooldown, next
		}
		return true, blockReasonOther, next
	}
	return false, blockReasonNone, time.Time{}
}

func TestAuthBlockForModelDecidesAsBefore(t *testing.T) {
	now := time.Now()
	soon, later, past := now.Add(3*time.Minute), now.Add(9*time.Minute), now.Add(-time.Minute)
	limited := &Error{HTTPStatus: http.StatusTooManyRequests, Message: "slow down"}
	cases := map[string]*Auth{
		"nil":      nil,
		"active":   {ID: "a"},
		"disabled": {ID: "a", Disabled: true},
		"status disabled": {
			ID: "a", Status: StatusDisabled,
		},
		"auth quota cooling": {
			ID: "a", Quota: QuotaState{Exceeded: true, NextRecoverAt: later}, NextRetryAfter: soon, LastError: limited,
		},
		"auth quota recovered": {
			ID: "a", Quota: QuotaState{Exceeded: true, NextRecoverAt: past},
		},
		"auth quota needs recovery": {
			ID: "a", Quota: QuotaState{Exceeded: true, RecoveryRequired: true},
		},
		"auth unavailable": {
			ID: "a", Unavailable: true, NextRetryAfter: soon, LastError: limited,
		},
		"auth unavailable, quota later": {
			ID: "a", Unavailable: true, NextRetryAfter: soon, Quota: QuotaState{Exceeded: true, NextRecoverAt: later},
		},
		"model disabled": {
			ID: "a", ModelStates: map[string]*ModelState{"m": {Status: StatusDisabled}},
		},
		"model needs recovery": {
			ID: "a", ModelStates: map[string]*ModelState{"m": {Quota: QuotaState{Exceeded: true, RecoveryRequired: true, NextRecoverAt: later}, NextRetryAfter: soon}},
		},
		"model cooling": {
			ID: "a", ModelStates: map[string]*ModelState{"m": {Unavailable: true, NextRetryAfter: soon, Quota: QuotaState{Exceeded: true, NextRecoverAt: later}, LastError: limited}},
		},
		"model unavailable, no time": {
			ID: "a", ModelStates: map[string]*ModelState{"m": {Unavailable: true}},
		},
		"model unavailable, other": {
			ID: "a", ModelStates: map[string]*ModelState{"m": {Unavailable: true, NextRetryAfter: soon, LastError: &Error{HTTPStatus: http.StatusBadGateway}}},
		},
		"model unavailable, expired": {
			ID: "a", ModelStates: map[string]*ModelState{"m": {Unavailable: true, NextRetryAfter: past}},
		},
		"thinking suffix reads base model": {
			ID: "a", ModelStates: map[string]*ModelState{"m": {Unavailable: true, NextRetryAfter: soon, Quota: QuotaState{Exceeded: true}}},
		},
		"other model cooling": {
			ID: "a", ModelStates: map[string]*ModelState{"other": {Unavailable: true, NextRetryAfter: soon, Quota: QuotaState{Exceeded: true}}},
		},
		"antigravity family cooling": {
			ID: "a", Provider: "antigravity", ModelStates: map[string]*ModelState{"gemini-2.5-pro": {Unavailable: true, NextRetryAfter: soon, Quota: QuotaState{Exceeded: true, NextRecoverAt: later}}},
		},
		"antigravity other family cooling": {
			ID: "a", Provider: "antigravity", ModelStates: map[string]*ModelState{"claude-sonnet-4-5": {Unavailable: true, NextRetryAfter: soon, Quota: QuotaState{Exceeded: true, NextRecoverAt: later}}},
		},
		"antigravity auth quota for other pool": {
			ID: "a", Provider: "antigravity", Quota: QuotaState{Exceeded: true, NextRecoverAt: later},
			ModelStates: map[string]*ModelState{"claude-sonnet-4-5": {Unavailable: true, NextRetryAfter: soon, Quota: QuotaState{Exceeded: true, NextRecoverAt: later}}},
		},
	}
	models := []string{"", "m", "m(high)", "gemini-2.5-flash", "claude-sonnet-4-5"}
	for name, auth := range cases {
		for _, model := range models {
			wantBlocked, wantReason, wantNext := legacyIsAuthBlockedForModel(auth, model, now)
			block := authBlockForModel(auth, model, now)
			if block.blocked != wantBlocked || block.reason != wantReason || !block.next.Equal(wantNext) {
				t.Errorf("%s, model %q: got (%v, %v, %v), want (%v, %v, %v)", name, model,
					block.blocked, block.reason, block.next, wantBlocked, wantReason, wantNext)
			}
			if !block.blocked && (block.lastErr != nil || block.quota != nil) {
				t.Errorf("%s, model %q: an unblocked result names a failure state", name, model)
			}
		}
	}
}
