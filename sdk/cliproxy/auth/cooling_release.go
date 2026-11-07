package auth

import (
	"net/http"
	"time"
)

// Turning cooling off has to take effect at once. disable-cooling only decides
// how failures recorded from then on are treated, so without the release below
// a credential parked by a 429 a moment before the switch stays out of rotation
// for the rest of its cooldown, up to 30 minutes, and the switch looks broken to
// the operator who just flipped it.

// coolingControlsStatus reports whether disable-cooling governs the cooldown a
// failure with this status starts. 401, 402, 403 and 404 park a credential
// whatever the switch says. 529 only starts a cooldown when the upstream names a
// time, and that one is governed like the other overload statuses.
func coolingControlsStatus(status int) bool {
	switch status {
	case http.StatusTooManyRequests, http.StatusRequestTimeout, http.StatusInternalServerError,
		http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout,
		statusSiteOverloaded:
		return true
	default:
		return false
	}
}

// coolingControlsQuota reports whether disable-cooling would have kept this
// quota state from being recorded. A window the upstream declared (a Claude
// OAuth rate-limit window, xAI's weekly balance) and a probe-confirmed recovery
// gate both carry a reset the upstream announced, which the switch never
// overrides, so they are left in place.
func coolingControlsQuota(quota QuotaState) bool {
	return quota.Window == "" && quota.WindowMinutes == 0 && !quotaRecoveryGateArmed(quota)
}

func coolingReleasableModelState(state *ModelState) bool {
	if state == nil || state.Status == StatusDisabled || !coolingControlsQuota(state.Quota) {
		return false
	}
	return state.Quota.Exceeded || coolingControlsStatus(statusCodeFromResult(state.LastError))
}

func coolingReleasableAuthState(auth *Auth) bool {
	if auth == nil || auth.Disabled || auth.Status == StatusDisabled || !coolingControlsQuota(auth.Quota) {
		return false
	}
	return auth.Quota.Exceeded || coolingControlsStatus(statusCodeFromResult(auth.LastError))
}

// releaseCoolingRestrictions clears the cooldowns of auth that disable-cooling
// governs and returns the models it released. Everything else a failure left
// behind stays: an unauthorized key, a disabled model, a reset the upstream
// announced. If the upstream still refuses, the next request parks the
// credential again under whatever the switch now says.
func releaseCoolingRestrictions(auth *Auth, now time.Time) (bool, []string) {
	if auth == nil {
		return false, nil
	}
	var (
		changed  bool
		released []string
	)
	for model, state := range auth.ModelStates {
		if !coolingReleasableModelState(state) {
			continue
		}
		if clearManualModelRuntimeState(state, now) {
			changed = true
			released = append(released, model)
		}
	}
	authReleasable := coolingReleasableAuthState(auth)
	if authReleasable {
		auth.Unavailable = false
		auth.NextRetryAfter = time.Time{}
		auth.Quota = QuotaState{}
		changed = true
	}
	if !changed {
		return false, nil
	}
	// Recompute the account-level view from the models that are left, so a
	// model still parked for another reason keeps the account marked.
	updateAggregatedAvailability(auth, now)
	if authReleasable && !hasModelError(auth, now) {
		auth.LastError = nil
		auth.Status = StatusActive
		auth.StatusMessage = ""
	}
	auth.UpdatedAt = now
	return true, released
}

// releaseCoolingOnDisable lifts the cooldowns cooling no longer applies to when
// an update turns cooling off for one credential: an operator setting
// disable-cooling on a provider entry or on an auth file. The global switch
// changes both sides of this comparison at once and is released by
// ReleaseCoolingForDisabledAuths instead.
func releaseCoolingOnDisable(next, previous *Auth, now time.Time) []string {
	if next == nil || previous == nil || !quotaCooldownDisabledForAuth(next) || quotaCooldownDisabledForAuth(previous) {
		return nil
	}
	_, released := releaseCoolingRestrictions(next, now)
	return released
}

// ReleaseCoolingForDisabledAuths lifts the 429/5xx cooldowns of every credential
// whose cooling is now off and returns how many it released. It runs when the
// global disable-cooling switch turns on.
//
// Nothing is persisted: the states it clears are runtime-only, and the quota
// gates that are persisted are exactly the ones it leaves alone.
func (m *Manager) ReleaseCoolingForDisabledAuths() int {
	if m == nil {
		return 0
	}
	type releasedAuth struct {
		id     string
		models []string
	}
	now := time.Now()
	var released []releasedAuth
	m.mu.Lock()
	for id, current := range m.auths {
		if current == nil || !quotaCooldownDisabledForAuth(current) {
			continue
		}
		if changed, models := releaseCoolingRestrictions(current, now); changed {
			released = append(released, releasedAuth{id: id, models: models})
		}
	}
	m.mu.Unlock()
	for _, item := range released {
		m.resumeRecoveredQuotaModels(item.id, item.models)
	}
	return len(released)
}
