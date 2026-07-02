package auth

import (
	"context"
	"net/http"
	"strings"
	"time"
)

type cooldownService struct {
	manager *Manager
}

type resultStateEffects struct {
	shouldResumeModel  bool
	shouldSuspendModel bool
	suspendReason      string
	clearModelQuota    bool
	setModelQuota      bool
}

func newCooldownService(manager *Manager) cooldownService {
	return cooldownService{manager: manager}
}

func (m *Manager) retrySettings() (int, time.Duration) {
	if m == nil {
		return 0, 0
	}
	return int(m.requestRetry.Load()), time.Duration(m.maxRetryInterval.Load())
}

func (m *Manager) closestCooldownWait(providers []string, model string, attempt int) (time.Duration, bool) {
	return newCooldownService(m).closestWait(providers, model, attempt)
}

func (m *Manager) shouldRetryAfterError(err error, attempt int, providers []string, model string, maxWait time.Duration, meta map[string]any) (time.Duration, bool) {
	return newCooldownService(m).shouldRetryAfterError(err, attempt, providers, model, maxWait, meta)
}

func waitForCooldown(ctx context.Context, wait time.Duration) error {
	if wait <= 0 {
		return nil
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// MarkResult records an execution result and notifies hooks.
func (m *Manager) MarkResult(ctx context.Context, result Result) {
	newCooldownService(m).markResult(ctx, result)
}

func (s cooldownService) closestWait(providers []string, model string, attempt int) (time.Duration, bool) {
	if s.manager == nil || len(providers) == 0 {
		return 0, false
	}
	now := time.Now()
	defaultRetry := int(s.manager.requestRetry.Load())
	if defaultRetry < 0 {
		defaultRetry = 0
	}
	providerSet := make(map[string]struct{}, len(providers))
	for i := range providers {
		key := strings.TrimSpace(strings.ToLower(providers[i]))
		if key == "" {
			continue
		}
		providerSet[key] = struct{}{}
	}
	s.manager.mu.RLock()
	defer s.manager.mu.RUnlock()
	var (
		found   bool
		minWait time.Duration
	)
	for _, auth := range s.manager.auths {
		if auth == nil {
			continue
		}
		providerKey := strings.TrimSpace(strings.ToLower(auth.Provider))
		if _, ok := providerSet[providerKey]; !ok {
			continue
		}
		effectiveRetry := defaultRetry
		if override, ok := auth.RequestRetryOverride(); ok {
			effectiveRetry = override
		}
		if effectiveRetry < 0 {
			effectiveRetry = 0
		}
		if attempt >= effectiveRetry {
			continue
		}
		blocked, reason, next := isAuthBlockedForModel(auth, model, now)
		if !blocked || next.IsZero() || reason == blockReasonDisabled {
			continue
		}
		wait := next.Sub(now)
		if wait < 0 {
			continue
		}
		if !found || wait < minWait {
			minWait = wait
			found = true
		}
	}
	return minWait, found
}

func (s cooldownService) shouldRetryAfterError(err error, attempt int, providers []string, model string, maxWait time.Duration, meta map[string]any) (time.Duration, bool) {
	if err == nil || maxWait <= 0 {
		return 0, false
	}
	if isSinglePickRouteRequest(meta) {
		return 0, false
	}
	if status := statusCodeFromError(err); status == http.StatusOK {
		return 0, false
	}
	if isRequestInvalidError(err) {
		return 0, false
	}
	wait, found := s.closestWait(providers, model, attempt)
	if !found || wait > maxWait {
		return 0, false
	}
	return wait, true
}

func (s cooldownService) markResult(ctx context.Context, result Result) {
	if s.manager == nil || result.AuthID == "" {
		return
	}

	var effects resultStateEffects
	s.manager.mu.Lock()
	if auth, ok := s.manager.auths[result.AuthID]; ok && auth != nil {
		now := time.Now()
		effects = s.applyResultLocked(auth, result, now)
		_ = s.manager.persist(ctx, auth)
	}
	s.manager.mu.Unlock()

	s.applyRegistryEffects(result, effects)
	s.manager.hook.OnResult(ctx, result)
}

func (s cooldownService) applyResultLocked(auth *Auth, result Result, now time.Time) resultStateEffects {
	var effects resultStateEffects
	if result.Success {
		s.applySuccessLocked(auth, result, now, &effects)
		return effects
	}
	s.applyFailureLocked(auth, result, now, &effects)
	return effects
}

func (s cooldownService) applySuccessLocked(auth *Auth, result Result, now time.Time, effects *resultStateEffects) {
	if auth == nil {
		return
	}
	markClaudeOAuthHealthSuccessLocked(auth, result, now)
	if result.Model != "" {
		state := ensureModelState(auth, result.Model)
		if activeModelQuotaCooldown(state, now) {
			updateAggregatedAvailability(auth, now)
			auth.UpdatedAt = now
			return
		}
		resetModelState(state, now)
		updateAggregatedAvailability(auth, now)
		if !hasModelError(auth, now) {
			auth.LastError = nil
			auth.StatusMessage = ""
			auth.Status = StatusActive
		}
		auth.UpdatedAt = now
		effects.shouldResumeModel = true
		effects.clearModelQuota = true
		return
	}
	if activeAuthQuotaCooldown(auth, now) {
		updateAggregatedAvailability(auth, now)
		auth.UpdatedAt = now
		return
	}
	clearAuthStateOnSuccess(auth, now)
}

func (s cooldownService) applyFailureLocked(auth *Auth, result Result, now time.Time, effects *resultStateEffects) {
	if auth == nil {
		return
	}
	if applyClaudeOAuthFailureLocked(auth, result, now, effects) {
		return
	}
	if result.Model != "" {
		s.applyModelFailureLocked(auth, result, now, effects)
		return
	}
	applyAuthFailureState(auth, result.Error, result.RetryAfter, now)
}

func (s cooldownService) applyModelFailureLocked(auth *Auth, result Result, now time.Time, effects *resultStateEffects) {
	state := ensureModelState(auth, result.Model)
	state.Unavailable = true
	state.Status = StatusError
	state.UpdatedAt = now
	if result.Error != nil {
		state.LastError = cloneError(result.Error)
		state.StatusMessage = result.Error.Message
		auth.LastError = cloneError(result.Error)
		auth.StatusMessage = result.Error.Message
	}

	statusCode := statusCodeFromResult(result.Error)
	switch statusCode {
	case 401:
		next := now.Add(30 * time.Minute)
		state.NextRetryAfter = next
		effects.suspendReason = "unauthorized"
		effects.shouldSuspendModel = true
	case 402, 403:
		next := now.Add(30 * time.Minute)
		state.NextRetryAfter = next
		effects.suspendReason = "payment_required"
		effects.shouldSuspendModel = true
	case 404:
		next := now.Add(12 * time.Hour)
		state.NextRetryAfter = next
		effects.suspendReason = "not_found"
		effects.shouldSuspendModel = true
	case 429:
		var next time.Time
		backoffLevel := state.Quota.BackoffLevel
		if result.RetryAfter != nil {
			next = now.Add(*result.RetryAfter)
		} else {
			cooldown, nextLevel := nextQuotaCooldown(backoffLevel, quotaCooldownDisabledForAuth(auth))
			if cooldown > 0 {
				next = now.Add(cooldown)
			}
			backoffLevel = nextLevel
		}
		state.NextRetryAfter = next
		state.Quota = QuotaState{
			Exceeded:      true,
			Reason:        "quota",
			Window:        result.Error.QuotaWindow,
			WindowMinutes: result.Error.QuotaWindowMinutes,
			NextRecoverAt: next,
			BackoffLevel:  backoffLevel,
		}
		effects.suspendReason = "quota"
		effects.shouldSuspendModel = true
		effects.setModelQuota = true
	case 408, 500, 502, 503, 504:
		if quotaCooldownDisabledForAuth(auth) {
			state.NextRetryAfter = time.Time{}
		} else {
			state.NextRetryAfter = now.Add(1 * time.Minute)
		}
	default:
		state.NextRetryAfter = time.Time{}
	}

	auth.Status = StatusError
	auth.UpdatedAt = now
	updateAggregatedAvailability(auth, now)
}

func (s cooldownService) applyRegistryEffects(result Result, effects resultStateEffects) {
	registryRef := s.manager.modelRegistry
	if registryRef == nil {
		return
	}
	if effects.clearModelQuota && result.Model != "" {
		registryRef.ClearModelQuotaExceeded(result.AuthID, result.Model)
	}
	if effects.setModelQuota && result.Model != "" {
		registryRef.SetModelQuotaExceeded(result.AuthID, result.Model)
	}
	if effects.shouldResumeModel {
		registryRef.ResumeClientModel(result.AuthID, result.Model)
	} else if effects.shouldSuspendModel {
		registryRef.SuspendClientModel(result.AuthID, result.Model, effects.suspendReason)
	}
}
