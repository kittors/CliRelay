package auth

import (
	"context"
	"net/http"
	"strings"
	"time"
)

const (
	quotaProbePendingBackoff = 30 * time.Second
	quotaProbeFailureBackoff = 2 * time.Minute
	quotaProbeMinInterval    = time.Minute
	quotaProbeMaxInterval    = 15 * time.Minute
)

// QuotaProbeResult describes the latest quota status for a credential.
// When Models is empty, the result applies to every quota-blocked model state on the auth.
type QuotaProbeResult struct {
	Recovered     bool
	NextRecoverAt time.Time
	Models        map[string]QuotaProbeModelResult
}

// QuotaProbeModelResult describes the latest quota status for a specific model.
type QuotaProbeModelResult struct {
	Recovered     bool
	NextRecoverAt time.Time
}

// QuotaRecoveryProber is an optional executor capability for reconciling local quota cooldown
// state against the upstream provider's current quota view.
type QuotaRecoveryProber interface {
	ProbeQuotaRecovery(ctx context.Context, auth *Auth) (*QuotaProbeResult, error)
}

// ReconcileQuota forces an immediate quota reconciliation for the given auth entry.
func (m *Manager) ReconcileQuota(ctx context.Context, id string) (bool, error) {
	return m.probeQuotaRecovery(ctx, id, true)
}

// ClearQuotaStatus manually clears local quota/cooldown runtime state for an auth entry.
func (m *Manager) ClearQuotaStatus(ctx context.Context, id string) (bool, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	id = strings.TrimSpace(id)
	if id == "" {
		return false, nil
	}

	now := time.Now()
	var (
		updated         *Auth
		recoveredModels []string
	)

	m.mu.Lock()
	current, ok := m.auths[id]
	if !ok || current == nil {
		delete(m.quotaProbeAfter, id)
		m.mu.Unlock()
		return false, nil
	}

	changed, models := clearQuotaStatusForAuth(current, now)
	recoveredModels = models
	delete(m.quotaProbeAfter, id)
	if changed {
		current.UpdatedAt = now
		updated = current.Clone()
		m.auths[id] = current
	}
	m.mu.Unlock()

	if updated != nil {
		if errPersist := m.persist(ctx, updated); errPersist != nil {
			return true, errPersist
		}
		m.hook.OnAuthUpdated(ctx, updated.Clone())
	}
	m.resumeRecoveredQuotaModels(id, recoveredModels)
	return updated != nil, nil
}

func (m *Manager) checkQuotaRecoveries(ctx context.Context, snapshot []*Auth, now time.Time) {
	for _, auth := range snapshot {
		if !m.shouldProbeQuota(auth, now) {
			continue
		}
		go m.probeQuotaRecoveryWithLimit(ctx, auth.ID, false)
	}
}

func (m *Manager) shouldProbeQuota(auth *Auth, now time.Time) bool {
	if auth == nil || auth.Disabled || strings.TrimSpace(auth.ID) == "" {
		return false
	}
	if !authHasActiveQuotaCooldown(auth, now) {
		return false
	}
	exec := m.executorFor(auth.Provider)
	if exec == nil {
		return false
	}
	if _, ok := exec.(QuotaRecoveryProber); !ok {
		return false
	}

	m.mu.RLock()
	next := m.quotaProbeAfter[auth.ID]
	m.mu.RUnlock()
	return next.IsZero() || !next.After(now)
}

func (m *Manager) probeQuotaRecoveryWithLimit(ctx context.Context, id string, force bool) {
	if m.refreshSemaphore == nil {
		_, _ = m.probeQuotaRecovery(ctx, id, force)
		return
	}
	select {
	case m.refreshSemaphore <- struct{}{}:
		defer func() { <-m.refreshSemaphore }()
	case <-ctx.Done():
		return
	}
	_, _ = m.probeQuotaRecovery(ctx, id, force)
}

func (m *Manager) probeQuotaRecovery(ctx context.Context, id string, force bool) (bool, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	id = strings.TrimSpace(id)
	if id == "" {
		return false, nil
	}

	now := time.Now()
	m.mu.Lock()
	auth, ok := m.auths[id]
	if !ok || auth == nil {
		delete(m.quotaProbeAfter, id)
		m.mu.Unlock()
		return false, nil
	}
	if !force {
		if next := m.quotaProbeAfter[id]; !next.IsZero() && next.After(now) {
			m.mu.Unlock()
			return false, nil
		}
	}
	m.quotaProbeAfter[id] = now.Add(quotaProbePendingBackoff)
	cloned := auth.Clone()
	exec := m.executors[auth.Provider]
	m.mu.Unlock()

	prober, ok := exec.(QuotaRecoveryProber)
	if !ok || prober == nil {
		m.mu.Lock()
		delete(m.quotaProbeAfter, id)
		m.mu.Unlock()
		return false, nil
	}

	result, err := prober.ProbeQuotaRecovery(ctx, cloned)
	if err != nil {
		if ctx.Err() != nil {
			return false, ctx.Err()
		}
		m.mu.Lock()
		m.quotaProbeAfter[id] = time.Now().Add(quotaProbeFailureBackoff)
		m.mu.Unlock()
		return false, err
	}
	if result == nil {
		m.mu.Lock()
		m.quotaProbeAfter[id] = time.Now().Add(quotaProbeFailureBackoff)
		m.mu.Unlock()
		return false, nil
	}

	now = time.Now()
	var (
		updated         *Auth
		recoveredModels []string
	)

	m.mu.Lock()
	current, ok := m.auths[id]
	if !ok || current == nil {
		delete(m.quotaProbeAfter, id)
		m.mu.Unlock()
		return false, nil
	}

	changed, models := applyQuotaProbeResult(current, result, now)
	recoveredModels = models
	if authHasActiveQuotaCooldown(current, now) {
		m.quotaProbeAfter[id] = nextQuotaProbeTime(current, now)
	} else {
		delete(m.quotaProbeAfter, id)
	}
	if changed {
		current.UpdatedAt = now
		updated = current.Clone()
		m.auths[id] = current
	}
	m.mu.Unlock()

	if updated != nil {
		if errPersist := m.persist(ctx, updated); errPersist != nil {
			return true, errPersist
		}
		m.hook.OnAuthUpdated(ctx, updated.Clone())
	}
	m.resumeRecoveredQuotaModels(id, recoveredModels)
	return updated != nil, nil
}

func applyQuotaProbeResult(auth *Auth, result *QuotaProbeResult, now time.Time) (bool, []string) {
	if auth == nil || result == nil {
		return false, nil
	}

	var changed bool
	recoveredModels := make([]string, 0)
	modelResults := normalizeQuotaProbeModels(result.Models)
	authWide := QuotaProbeModelResult{Recovered: result.Recovered, NextRecoverAt: result.NextRecoverAt}

	if len(auth.ModelStates) > 0 {
		beforeUnavailable := auth.Unavailable
		beforeNextRetry := auth.NextRetryAfter
		beforeQuota := auth.Quota
		beforeStatus := auth.Status
		beforeStatusMessage := auth.StatusMessage
		beforeLastError := cloneError(auth.LastError)

		for modelID, state := range auth.ModelStates {
			if state == nil || !state.Quota.Exceeded {
				continue
			}

			outcome, ok := modelResults[canonicalModelKey(modelID)]
			if !ok {
				if len(modelResults) > 0 {
					continue
				}
				outcome = authWide
			}

			if outcome.Recovered {
				if clearQuotaModelState(state, now) {
					changed = true
				}
				recoveredModels = append(recoveredModels, modelID)
				continue
			}
			if updateQuotaModelRecoverAt(state, outcome.NextRecoverAt, now) {
				changed = true
			}
		}

		updateAggregatedAvailability(auth, now)
		if auth.Unavailable != beforeUnavailable || !auth.NextRetryAfter.Equal(beforeNextRetry) || auth.Quota != beforeQuota {
			changed = true
		}
		if !hasModelError(auth, now) {
			auth.Status = StatusActive
			auth.StatusMessage = ""
			auth.LastError = nil
		}
		if auth.Status != beforeStatus || auth.StatusMessage != beforeStatusMessage || !errorsEqual(auth.LastError, beforeLastError) {
			changed = true
		}
		return changed, recoveredModels
	}

	if !auth.Quota.Exceeded {
		return false, nil
	}
	if authWide.Recovered {
		if clearQuotaAuthState(auth, now) {
			changed = true
		}
		return changed, nil
	}
	if updateQuotaAuthRecoverAt(auth, authWide.NextRecoverAt, now) {
		changed = true
	}
	return changed, nil
}

func clearQuotaStatusForAuth(auth *Auth, now time.Time) (bool, []string) {
	if auth == nil {
		return false, nil
	}

	authHadQuotaRuntime := hasQuotaRuntimeState(auth.StatusMessage, auth.LastError, auth.Unavailable, auth.NextRetryAfter, auth.Quota)
	var changed bool
	recoveredModels := make([]string, 0, len(auth.ModelStates))
	for modelID, state := range auth.ModelStates {
		if clearQuotaModelRuntimeState(state, now) {
			changed = true
			recoveredModels = append(recoveredModels, modelID)
		}
	}

	if len(auth.ModelStates) > 0 {
		beforeUnavailable := auth.Unavailable
		beforeNextRetry := auth.NextRetryAfter
		beforeQuota := auth.Quota
		updateAggregatedAvailability(auth, now)
		if auth.Unavailable != beforeUnavailable || !auth.NextRetryAfter.Equal(beforeNextRetry) || auth.Quota != beforeQuota {
			changed = true
		}
	}
	if clearQuotaAuthRuntimeState(auth, now, authHadQuotaRuntime) {
		changed = true
	}
	return changed, recoveredModels
}

func normalizeQuotaProbeModels(in map[string]QuotaProbeModelResult) map[string]QuotaProbeModelResult {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]QuotaProbeModelResult, len(in))
	for model, result := range in {
		key := canonicalModelKey(model)
		if key == "" {
			continue
		}
		out[key] = result
	}
	return out
}

func authHasActiveQuotaCooldown(auth *Auth, now time.Time) bool {
	if auth == nil || auth.Disabled {
		return false
	}
	if auth.Unavailable && auth.Quota.Exceeded && auth.NextRetryAfter.After(now) {
		return true
	}
	for _, state := range auth.ModelStates {
		if state == nil {
			continue
		}
		if state.Unavailable && state.Quota.Exceeded && state.NextRetryAfter.After(now) {
			return true
		}
	}
	return false
}

func nextQuotaProbeTime(auth *Auth, now time.Time) time.Time {
	nextRecover := time.Time{}
	if auth == nil {
		return now.Add(quotaProbeMinInterval)
	}
	if auth.Unavailable && auth.Quota.Exceeded && auth.NextRetryAfter.After(now) {
		nextRecover = auth.NextRetryAfter
		if !auth.Quota.NextRecoverAt.IsZero() && auth.Quota.NextRecoverAt.After(now) && auth.Quota.NextRecoverAt.Before(nextRecover) {
			nextRecover = auth.Quota.NextRecoverAt
		}
	}
	for _, state := range auth.ModelStates {
		if state == nil || !state.Unavailable || !state.Quota.Exceeded || !state.NextRetryAfter.After(now) {
			continue
		}
		candidate := state.NextRetryAfter
		if !state.Quota.NextRecoverAt.IsZero() && state.Quota.NextRecoverAt.After(now) && state.Quota.NextRecoverAt.Before(candidate) {
			candidate = state.Quota.NextRecoverAt
		}
		if nextRecover.IsZero() || candidate.Before(nextRecover) {
			nextRecover = candidate
		}
	}

	if nextRecover.IsZero() {
		return now.Add(quotaProbeMinInterval)
	}
	remaining := nextRecover.Sub(now)
	if remaining <= 0 {
		return now.Add(quotaProbeMinInterval)
	}
	interval := remaining / 4
	if interval < quotaProbeMinInterval {
		interval = quotaProbeMinInterval
	}
	if interval > quotaProbeMaxInterval {
		interval = quotaProbeMaxInterval
	}
	return now.Add(interval)
}

func clearQuotaModelState(state *ModelState, now time.Time) bool {
	if state == nil {
		return false
	}
	changed := state.Unavailable || state.Status != StatusActive || state.StatusMessage != "" || !state.NextRetryAfter.IsZero() || state.LastError != nil || state.Quota != (QuotaState{})
	state.Unavailable = false
	state.Status = StatusActive
	state.StatusMessage = ""
	state.NextRetryAfter = time.Time{}
	state.LastError = nil
	state.Quota = QuotaState{}
	state.UpdatedAt = now
	return changed
}

func clearQuotaModelRuntimeState(state *ModelState, now time.Time) bool {
	if state == nil || !hasQuotaRuntimeState(state.StatusMessage, state.LastError, state.Unavailable, state.NextRetryAfter, state.Quota) {
		return false
	}
	preserveDisabled := state.Status == StatusDisabled
	changed := state.Unavailable || !state.NextRetryAfter.IsZero() || state.LastError != nil || state.Quota != (QuotaState{})
	state.Unavailable = false
	state.NextRetryAfter = time.Time{}
	state.LastError = nil
	state.Quota = QuotaState{}
	if !preserveDisabled {
		if state.Status != StatusActive || state.StatusMessage != "" {
			changed = true
		}
		state.Status = StatusActive
		state.StatusMessage = ""
	}
	state.UpdatedAt = now
	return changed
}

func updateQuotaModelRecoverAt(state *ModelState, next time.Time, now time.Time) bool {
	if state == nil || next.IsZero() {
		return false
	}
	changed := !state.NextRetryAfter.Equal(next) || !state.Quota.NextRecoverAt.Equal(next) || !state.Unavailable || !state.Quota.Exceeded || state.Quota.Reason != "quota"
	state.Unavailable = true
	state.NextRetryAfter = next
	state.Quota.Exceeded = true
	state.Quota.Reason = "quota"
	state.Quota.NextRecoverAt = next
	state.UpdatedAt = now
	return changed
}

func clearQuotaAuthState(auth *Auth, now time.Time) bool {
	if auth == nil {
		return false
	}
	changed := auth.Unavailable || auth.Status != StatusActive || auth.StatusMessage != "" || !auth.NextRetryAfter.IsZero() || auth.LastError != nil || auth.Quota != (QuotaState{})
	auth.Unavailable = false
	auth.Status = StatusActive
	auth.StatusMessage = ""
	auth.NextRetryAfter = time.Time{}
	auth.LastError = nil
	auth.Quota = QuotaState{}
	auth.UpdatedAt = now
	return changed
}

func clearQuotaAuthRuntimeState(auth *Auth, now time.Time, force bool) bool {
	if auth == nil || (!force && !hasQuotaRuntimeState(auth.StatusMessage, auth.LastError, auth.Unavailable, auth.NextRetryAfter, auth.Quota)) {
		return false
	}
	preserveDisabled := auth.Status == StatusDisabled
	changed := auth.Unavailable || !auth.NextRetryAfter.IsZero() || auth.LastError != nil || auth.Quota != (QuotaState{})
	auth.Unavailable = false
	auth.NextRetryAfter = time.Time{}
	auth.LastError = nil
	auth.Quota = QuotaState{}
	if !preserveDisabled {
		if auth.Status != StatusActive || auth.StatusMessage != "" {
			changed = true
		}
		auth.Status = StatusActive
		auth.StatusMessage = ""
	}
	auth.UpdatedAt = now
	return changed
}

func hasQuotaRuntimeState(statusMessage string, lastError *Error, unavailable bool, nextRetryAfter time.Time, quota QuotaState) bool {
	if quota != (QuotaState{}) || unavailable || !nextRetryAfter.IsZero() {
		return true
	}
	if lastError != nil {
		if lastError.HTTPStatus == http.StatusTooManyRequests {
			return true
		}
		text := strings.ToLower(strings.TrimSpace(lastError.Code + " " + lastError.Message))
		if strings.Contains(text, "quota") || strings.Contains(text, "rate limit") || strings.Contains(text, "429") || strings.Contains(text, "cooldown") {
			return true
		}
	}
	text := strings.ToLower(strings.TrimSpace(statusMessage))
	return strings.Contains(text, "quota") || strings.Contains(text, "rate limit") || strings.Contains(text, "429") || strings.Contains(text, "cooldown")
}

func updateQuotaAuthRecoverAt(auth *Auth, next time.Time, now time.Time) bool {
	if auth == nil || next.IsZero() {
		return false
	}
	changed := !auth.NextRetryAfter.Equal(next) || !auth.Quota.NextRecoverAt.Equal(next) || !auth.Unavailable || !auth.Quota.Exceeded || auth.Quota.Reason != "quota"
	auth.Unavailable = true
	auth.NextRetryAfter = next
	auth.Quota.Exceeded = true
	auth.Quota.Reason = "quota"
	auth.Quota.NextRecoverAt = next
	auth.UpdatedAt = now
	return changed
}
