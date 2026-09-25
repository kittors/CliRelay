package auth

import (
	"fmt"
	"net/http"
	"strings"
	"time"
)

// Cooldowns across processes.
//
// Cooldowns (429 backoffs, exhausted quotas, 5xx backoffs, suspensions) live
// in each process's Manager. A process that has not seen the failure itself
// keeps sending requests to an account every other process already knows is
// cooling down. The manager therefore reports every cooldown it sets or
// extends to a publisher, and applies cooldowns reported by peers through
// ApplyPeerCooldown.

// CooldownNotice describes a cooldown on an upstream account or model.
type CooldownNotice struct {
	AuthID string
	// Model is empty for an account-wide cooldown.
	Model string
	// Until is when the account or model may be tried again.
	Until time.Time
	// StatusCode is the upstream status behind the cooldown, when known.
	StatusCode int
	// Reason is the model registry suspension reason ("quota",
	// "unauthorized", ...), empty for a plain backoff.
	Reason string
	// Quota marks an exhausted quota, as opposed to a backoff.
	Quota bool
	// Origin names the process that observed the failure, for status text.
	Origin string
}

type cooldownPublisherRef struct{ publish func(CooldownNotice) }

// maxPeerCooldown caps how long a cooldown reported by a peer blocks this
// process. The peer may recover early (a probe confirms a quota is back, an
// operator clears the account) and nothing tells this process when it does,
// while it can always confirm a long cooldown itself with one request, which
// then sets its own full-length cooldown. 30 minutes is the longest backoff
// this package derives by itself; anything longer comes from an upstream
// declared window.
const maxPeerCooldown = 30 * time.Minute

// SetCooldownPublisher registers publish to receive every cooldown this
// process sets or extends. It is called after the manager's lock is released
// and must not block. nil stops publishing.
func (m *Manager) SetCooldownPublisher(publish func(CooldownNotice)) {
	if m == nil {
		return
	}
	if publish == nil {
		m.cooldownPublisher.Store(nil)
		return
	}
	m.cooldownPublisher.Store(&cooldownPublisherRef{publish: publish})
}

func (m *Manager) cooldownPublishing() bool {
	return m != nil && m.cooldownPublisher.Load() != nil
}

// cooldownBeforeLocked records the deadline a result is about to change.
// Callers must hold m.mu.
func (m *Manager) cooldownBeforeLocked(auth *Auth, result Result, now time.Time) time.Time {
	if !m.cooldownPublishing() || result.Success {
		return time.Time{}
	}
	until, _ := cooldownDeadline(auth, result.Model, now)
	return until
}

// cooldownNoticeLocked reports the cooldown a failed result set or extended,
// or nil. Callers must hold m.mu.
func (m *Manager) cooldownNoticeLocked(auth *Auth, result Result, effects resultStateEffects, before, now time.Time) *CooldownNotice {
	if !m.cooldownPublishing() || result.Success {
		return nil
	}
	until, quota := cooldownDeadline(auth, result.Model, now)
	if until.IsZero() || !until.After(before) {
		return nil
	}
	return &CooldownNotice{
		AuthID:     auth.ID,
		Model:      result.Model,
		Until:      until,
		StatusCode: statusCodeFromResult(result.Error),
		Reason:     effects.suspendReason,
		Quota:      quota,
	}
}

func (m *Manager) publishCooldown(notice *CooldownNotice) {
	if notice == nil || m == nil {
		return
	}
	if ref := m.cooldownPublisher.Load(); ref != nil {
		ref.publish(*notice)
	}
}

// cooldownDeadline is when auth, or its model when model is set, may next be
// tried, and whether the cooldown is an exhausted quota. It is zero when no
// cooldown is in force.
func cooldownDeadline(auth *Auth, model string, now time.Time) (time.Time, bool) {
	if auth == nil {
		return time.Time{}, false
	}
	var until time.Time
	var quota bool
	if model != "" {
		state := auth.ModelStates[model]
		if state == nil {
			return time.Time{}, false
		}
		if state.Unavailable {
			until = state.NextRetryAfter
		}
		if state.Quota.Exceeded {
			quota = true
			if state.Quota.NextRecoverAt.After(until) {
				until = state.Quota.NextRecoverAt
			}
		}
	} else {
		if auth.Unavailable {
			until = auth.NextRetryAfter
		}
		if auth.Quota.Exceeded {
			quota = true
			if auth.Quota.NextRecoverAt.After(until) {
				until = auth.Quota.NextRecoverAt
			}
		}
	}
	if !until.After(now) {
		return time.Time{}, false
	}
	return until, quota
}

// ApplyPeerCooldown applies a cooldown another process reported. It only ever
// extends: a deadline no later than the one already in force changes nothing,
// so notices may arrive late, twice or out of order. Nothing is persisted;
// the reporting process persists its own state. It reports whether anything
// changed.
func (m *Manager) ApplyPeerCooldown(n CooldownNotice) bool {
	if m == nil || strings.TrimSpace(n.AuthID) == "" {
		return false
	}
	now := time.Now()
	until := n.Until
	if limit := now.Add(maxPeerCooldown); until.After(limit) {
		until = limit
	}
	if !until.After(now) {
		return false
	}

	m.mu.Lock()
	auth := m.auths[n.AuthID]
	changed := false
	if auth != nil && !auth.Disabled && auth.Status != StatusDisabled && !peerCooldownSuppressed(auth, n) {
		if n.Model != "" {
			changed = extendModelCooldownLocked(auth, n, until, now)
		} else {
			changed = extendAuthCooldownLocked(auth, n, until, now)
		}
	}
	registryRef := m.modelRegistry
	m.mu.Unlock()

	if changed && n.Model != "" && registryRef != nil {
		if n.Quota {
			registryRef.SetModelQuotaExceeded(n.AuthID, n.Model)
		}
		if n.Reason != "" {
			registryRef.SuspendClientModel(n.AuthID, n.Model, n.Reason)
		}
	}
	return changed
}

// peerCooldownSuppressed honours disable-cooling on the receiving side too:
// it switches off quota and backoff cooldowns, but not suspensions such as a
// 401, exactly as it does for failures seen locally.
func peerCooldownSuppressed(auth *Auth, n CooldownNotice) bool {
	if !quotaCooldownDisabledForAuth(auth) {
		return false
	}
	if n.Quota {
		return true
	}
	switch n.StatusCode {
	case http.StatusTooManyRequests, http.StatusRequestTimeout, http.StatusInternalServerError,
		http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true
	}
	return false
}

func extendModelCooldownLocked(auth *Auth, n CooldownNotice, until, now time.Time) bool {
	state := ensureModelState(auth, n.Model)
	if state == nil || state.Status == StatusDisabled {
		return false
	}
	if current, _ := cooldownDeadline(auth, n.Model, now); !until.After(current) {
		return false
	}
	message := peerCooldownMessage(n)
	state.Unavailable = true
	state.NextRetryAfter = until
	state.Status = StatusError
	state.StatusMessage = message
	state.UpdatedAt = now
	if state.LastError == nil {
		state.LastError = peerCooldownError(n, message)
	}
	if n.Quota {
		state.Quota.Exceeded = true
		state.Quota.Reason = "quota"
		if until.After(state.Quota.NextRecoverAt) {
			state.Quota.NextRecoverAt = until
		}
	}
	auth.Status = StatusError
	auth.UpdatedAt = now
	updateAggregatedAvailability(auth, now)
	return true
}

func extendAuthCooldownLocked(auth *Auth, n CooldownNotice, until, now time.Time) bool {
	if current, _ := cooldownDeadline(auth, "", now); !until.After(current) {
		return false
	}
	message := peerCooldownMessage(n)
	auth.Unavailable = true
	auth.NextRetryAfter = until
	auth.Status = StatusError
	auth.StatusMessage = message
	if auth.LastError == nil {
		auth.LastError = peerCooldownError(n, message)
	}
	if n.Quota {
		auth.Quota.Exceeded = true
		auth.Quota.Reason = "quota"
		if until.After(auth.Quota.NextRecoverAt) {
			auth.Quota.NextRecoverAt = until
		}
	}
	auth.UpdatedAt = now
	return true
}

func peerCooldownMessage(n CooldownNotice) string {
	cause := n.Reason
	if cause == "" && n.StatusCode > 0 {
		cause = fmt.Sprintf("upstream status %d", n.StatusCode)
	}
	if cause == "" {
		cause = "upstream failure"
	}
	if n.Origin != "" {
		return fmt.Sprintf("cooling down: %s (reported by node %s)", cause, n.Origin)
	}
	return "cooling down: " + cause + " (reported by another node)"
}

func peerCooldownError(n CooldownNotice, message string) *Error {
	return &Error{Code: "peer_cooldown", Message: message, HTTPStatus: n.StatusCode, Retryable: true}
}
