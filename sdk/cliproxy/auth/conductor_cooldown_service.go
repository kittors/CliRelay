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
	if isContentPolicyViolation(err) {
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
	if !result.Success && result.RetryAfter == nil && isXAIWeekBalanceExhaustedError(result.Error) {
		probeCtx := context.Background()
		if ctx != nil {
			probeCtx = context.WithoutCancel(ctx)
		}
		go s.manager.probeQuotaRecoveryWithLimit(probeCtx, result.AuthID, false)
	}
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
		if activeModelQuotaCooldown(state, now) && !quotaNeedsConfirmedRecovery(state.Quota, now) {
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
	if activeAuthQuotaCooldown(auth, now) && !quotaNeedsConfirmedRecovery(auth.Quota, now) {
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
	// Client request errors (400 invalid payload/image/etc.) are not auth health
	// signals. Do not mark the credential unavailable or start any cooldown;
	// failover is already suppressed by isRequestInvalidError.
	if result.Error != nil && isRequestInvalidError(result.Error) {
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
	if isResponseStreamIncompleteError(result.Error) {
		state.Unavailable = false
		state.NextRetryAfter = time.Time{}
		auth.Status = StatusError
		auth.UpdatedAt = now
		updateAggregatedAvailability(auth, now)
		return
	}

	statusCode := statusCodeFromResult(result.Error)
	// xAI Grok Build returns HTTP 402 for weekly usage balance exhaustion.
	// Route those as quota cooldowns (window/retry metadata from executor),
	// not the generic 30m payment_required probe loop.
	if statusCode == 402 && isQuotaExhaustionError(result.Error, result.RetryAfter) {
		applyModelQuotaFailureLocked(auth, state, result, now, effects)
	} else {
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
			// A 429 is not automatically an exhausted quota. Google's CloudCode
			// answers "Resource has been exhausted" for ordinary per-minute
			// throttling, and treating that as quota exhaustion suspends the
			// model on an account that still has its full allowance.
			//
			// Observed in production: an antigravity account reporting 100%
			// remaining cycled suspend → resume → suspend within five seconds,
			// repeatedly, because the first request after each recovery drew
			// another throttling 429 and was again read as exhaustion.
			if isTransientRateLimitError(result.Error) {
				applyTransientRateLimitLocked(auth, state, result, now, effects)
				break
			}
			applyModelQuotaFailureLocked(auth, state, result, now, effects)
		case 408, 500, 502, 503, 504:
			// An upstream load-shed says the provider is busy right now, not that
			// this credential is unhealthy. Parking the model for a full minute
			// turns a sub-second blip into an outage once every account in a small
			// pool has been shed, so those back off briefly instead.
			if quotaCooldownDisabledForAuth(auth) {
				state.NextRetryAfter = time.Time{}
			} else if isTransientUpstreamOverloadError(result.Error) {
				state.NextRetryAfter = now.Add(transientUpstreamOverloadCooldown)
			} else {
				state.NextRetryAfter = now.Add(1 * time.Minute)
			}
		default:
			state.NextRetryAfter = time.Time{}
		}
	}

	auth.Status = StatusError
	auth.UpdatedAt = now
	updateAggregatedAvailability(auth, now)
}

func isResponseStreamIncompleteError(err *Error) bool {
	return err != nil && strings.TrimSpace(err.Code) == "response_stream_incomplete"
}

// applyModelQuotaFailureLocked records a provider quota exhaustion against a model.
// Used for classic 429s and for quota-like 402s (e.g. xAI weekly balance exhausted).
func applyModelQuotaFailureLocked(auth *Auth, state *ModelState, result Result, now time.Time, effects *resultStateEffects) {
	if state == nil {
		return
	}
	backoffLevel := state.Quota.BackoffLevel
	cooldown, nextLevel := quotaFailureCooldown(result.Error, result.RetryAfter, backoffLevel, quotaCooldownDisabledForAuth(auth))
	var next time.Time
	if cooldown > 0 {
		next = now.Add(cooldown)
	}
	backoffLevel = nextLevel
	var window string
	var windowMinutes int
	if result.Error != nil {
		window = result.Error.QuotaWindow
		windowMinutes = result.Error.QuotaWindowMinutes
	}
	state.NextRetryAfter = next
	state.Quota = QuotaState{
		Exceeded:         true,
		RecoveryRequired: isXAIWeekBalanceExhaustedError(result.Error),
		Reason:           "quota",
		Window:           window,
		WindowMinutes:    windowMinutes,
		NextRecoverAt:    next,
		BackoffLevel:     backoffLevel,
	}
	if result.Error != nil && result.Error.Message != "" {
		state.StatusMessage = result.Error.Message
	} else {
		state.StatusMessage = "quota exhausted"
	}
	effects.suspendReason = "quota"
	effects.shouldSuspendModel = true
	effects.setModelQuota = true
}

// isQuotaExhaustionError reports whether a non-429 failure should still be treated
// as quota exhaustion (window metadata and/or balance-exhausted body).
func isQuotaExhaustionError(err *Error, retryAfter *time.Duration) bool {
	if err == nil {
		return false
	}
	if err.QuotaWindow != "" || err.QuotaWindowMinutes > 0 {
		return true
	}
	if retryAfter != nil && *retryAfter > 0 && isUsageBalanceExhaustedMessage(err.Message) {
		return true
	}
	return isUsageBalanceExhaustedMessage(err.Message)
}

// isTransientRateLimitError reports whether a 429 describes short-term
// throttling rather than an exhausted allowance.
//
// The distinction is in the body: providers name an exhausted quota explicitly
// ("quota_exhausted", a quotaResetDelay, a daily quota), whereas a bare
// "resource has been exhausted" is the generic throttle Google returns for
// per-minute limits. Anything not recognised as throttling keeps the previous
// behaviour and is treated as exhaustion, so this can only narrow the cases
// that suspend a model, never widen them.
func isTransientRateLimitError(err *Error) bool {
	if err == nil {
		return false
	}
	body := strings.ToLower(err.Message)
	if body == "" {
		return false
	}
	for _, explicit := range []string{
		"quota_exhausted", "quotaresetdelay", "quota reset",
		"quota limit", "daily quota", "usage balance exhausted", "balance exhausted",
	} {
		if strings.Contains(body, explicit) {
			return false
		}
	}
	return strings.Contains(body, "resource has been exhausted") ||
		strings.Contains(body, "resource_exhausted") ||
		strings.Contains(body, "rate limit") ||
		strings.Contains(body, "too many requests")
}

// applyTransientRateLimitLocked backs off briefly without declaring the model
// out of quota: no quota state, no suspension, so the next scheduling pass can
// use the account again instead of parking it behind a quota cooldown.
func applyTransientRateLimitLocked(auth *Auth, state *ModelState, result Result, now time.Time, effects *resultStateEffects) {
	if state == nil {
		return
	}
	cooldown := transientRateLimitCooldown
	if result.RetryAfter != nil && *result.RetryAfter > 0 {
		cooldown = *result.RetryAfter
	}
	if quotaCooldownDisabledForAuth(auth) {
		cooldown = 0
	}
	if cooldown > 0 {
		state.NextRetryAfter = now.Add(cooldown)
	} else {
		state.NextRetryAfter = time.Time{}
	}
	// Explicitly clear any quota flag a previous misclassification may have set,
	// so one throttled request cannot leave the model parked indefinitely.
	state.Quota = QuotaState{}
	effects.clearModelQuota = true
	auth.Status = StatusError
	auth.UpdatedAt = now
	updateAggregatedAvailability(auth, now)
}

// transientRateLimitCooldown matches the shortest window a provider realistically
// throttles on; a per-minute limit clears within it.
const transientRateLimitCooldown = time.Minute

// transientUpstreamOverloadCooldown is the back-off for a provider load-shed.
// Long enough to let the current spike pass, short enough that a small pool
// recovers before a retrying client can notice it was ever empty.
const transientUpstreamOverloadCooldown = 5 * time.Second

// isTransientUpstreamOverloadError reports whether a 5xx describes the provider
// shedding load rather than a fault attributable to this credential.
//
// ChatGPT answers `server_is_overloaded` / `service_unavailable_error` inside a
// 200 SSE stream when it is busy; the executor surfaces that as a 502 because no
// other mapping fits. Any account would have drawn the same answer, so the model
// must not be parked on the assumption this one is broken.
//
// Only phrases that name overload count. Anything unrecognised keeps the
// existing one-minute cooldown, so this can only shorten the cases it matches,
// never widen them.
func isTransientUpstreamOverloadError(err *Error) bool {
	if err == nil {
		return false
	}
	haystack := strings.ToLower(err.Code + " " + err.Message)
	if strings.TrimSpace(haystack) == "" {
		return false
	}
	for _, signal := range []string{
		"server_is_overloaded",
		"service_unavailable_error",
		"overloaded",
	} {
		if strings.Contains(haystack, signal) {
			return true
		}
	}
	return false
}

func isUsageBalanceExhaustedMessage(message string) bool {
	lower := strings.ToLower(strings.TrimSpace(message))
	if lower == "" {
		return false
	}
	return strings.Contains(lower, "usage balance exhausted") ||
		strings.Contains(lower, "balance exhausted")
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
