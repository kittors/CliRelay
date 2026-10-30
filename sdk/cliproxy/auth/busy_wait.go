package auth

import (
	"context"
	"math/rand/v2"
	"net/http"
	"strings"
	"sync"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

// Busy waiting: when every account that may serve a request is cooling down
// after a recoverable failure (408, 5xx, a short 429), the request waits for
// the soonest one to recover instead of failing at once, up to the governing
// channel group's busy-wait-max-seconds (stream_guards.go).
//
// It is deliberately narrower than the request-retry wait that predates it:
//   - Only accounts this request may use count (tenant, groups, channels,
//     model), and accounts an earlier attempt of the same request already
//     failed on are left out; the older wait looks at every account of the
//     provider.
//   - A quota that ran out, a 401/403 or a 404 is not waited for: none of them
//     recovers within minutes, so waiting would only delay the error.
//   - If any usable account is not cooling at all, the failure was not "the
//     pool is busy" and nothing is waited for. That also keeps accounts with
//     cooling disabled from turning this into a hot retry loop.
//   - The budget is per request and shared by every retry of it, and never
//     reaches past the request's own deadline.
//
// When it gives up, the request fails exactly as it did before, including the
// Retry-After the scheduler derives from the cooldowns.

// maxConcurrentBusyWaiters caps how many requests may wait at once. In an
// outage longer than the waits, every incoming request would otherwise park
// here; past the cap requests fail at once, as they did before.
const maxConcurrentBusyWaiters = 256

// RetryWaiter performs a wait the scheduler decided on before trying a request
// again. A streaming handler installs one to keep its client informed while
// nothing has been sent yet; without one the scheduler simply sleeps. It must
// return as soon as ctx ends.
type RetryWaiter func(ctx context.Context, wait time.Duration) error

type retryWaiterKey struct{}

// WithRetryWaiter makes the scheduler wait through waiter for this request.
func WithRetryWaiter(ctx context.Context, waiter RetryWaiter) context.Context {
	if waiter == nil {
		return ctx
	}
	return context.WithValue(ctx, retryWaiterKey{}, waiter)
}

func waitBeforeRetry(ctx context.Context, wait time.Duration) error {
	if waiter, ok := ctx.Value(retryWaiterKey{}).(RetryWaiter); ok && waiter != nil {
		return waiter(ctx, wait)
	}
	return waitForCooldown(ctx, wait)
}

// retryWaitBudget is the busy waiting one request has done so far.
type retryWaitBudget struct {
	mu    sync.Mutex
	spent time.Duration
}

type retryWaitBudgetKey struct{}

// WithRetryWaitBudget shares one busy-wait budget between every scheduling
// pass of a request, including the ones a handler starts when it retries a
// stream on another account. Without it each pass would get the full budget.
func WithRetryWaitBudget(ctx context.Context) context.Context {
	if _, ok := ctx.Value(retryWaitBudgetKey{}).(*retryWaitBudget); ok {
		return ctx
	}
	return context.WithValue(ctx, retryWaitBudgetKey{}, &retryWaitBudget{})
}

func retryWaitBudgetFor(ctx context.Context) *retryWaitBudget {
	if budget, ok := ctx.Value(retryWaitBudgetKey{}).(*retryWaitBudget); ok && budget != nil {
		return budget
	}
	return &retryWaitBudget{}
}

func (b *retryWaitBudget) remaining(limit time.Duration) time.Duration {
	b.mu.Lock()
	defer b.mu.Unlock()
	return limit - b.spent
}

func (b *retryWaitBudget) charge(waited time.Duration) {
	b.mu.Lock()
	b.spent += waited
	b.mu.Unlock()
}

// retryPlan is a decided wait. A busy wait holds a waiter slot until it ends.
type retryPlan struct {
	wait    time.Duration
	budget  *retryWaitBudget
	release func()
}

func (p retryPlan) run(ctx context.Context) error {
	if p.release != nil {
		defer p.release()
	}
	started := time.Now()
	err := waitBeforeRetry(ctx, p.wait)
	if p.budget != nil {
		p.budget.charge(time.Since(started))
	}
	return err
}

// nextRetryPlan decides whether a failed scheduling pass is tried again and
// after how long. The request-retry wait keeps its old behaviour and is
// consulted first; busy waiting applies when it declines.
func (m *Manager) nextRetryPlan(
	ctx context.Context,
	err error,
	attempt int,
	providers []string,
	model string,
	maxWait time.Duration,
	opts cliproxyexecutor.Options,
	budget *retryWaitBudget,
	busyWait bool,
) (retryPlan, bool) {
	// A retry that excludes accounts must not wait for one of them, and the
	// request-retry wait cannot tell them apart.
	if _, excluding := retryExclusionsFromMetadata(opts.Metadata); !excluding {
		if wait, ok := m.shouldRetryAfterError(err, attempt, providers, model, maxWait, opts.Metadata); ok {
			return retryPlan{wait: wait}, true
		}
	}
	if !busyWait {
		return retryPlan{}, false
	}
	wait, ok := m.busyWaitFor(ctx, err, providers, model, opts, budget)
	if !ok {
		return retryPlan{}, false
	}
	if m.busyWaiters.Add(1) > maxConcurrentBusyWaiters {
		m.busyWaiters.Add(-1)
		return retryPlan{}, false
	}
	return retryPlan{wait: wait, budget: budget, release: func() { m.busyWaiters.Add(-1) }}, true
}

// busyWaitFor returns how long to wait for the soonest usable account to come
// back, when every usable account is cooling and one recovers within the
// request's remaining budget.
func (m *Manager) busyWaitFor(ctx context.Context, err error, providers []string, model string, opts cliproxyexecutor.Options, budget *retryWaitBudget) (time.Duration, bool) {
	if err == nil || ctx.Err() != nil || isSinglePickRouteRequest(opts.Metadata) {
		return 0, false
	}
	if statusCodeFromError(err) == http.StatusOK || isRequestInvalidError(err) || isContentPolicyViolation(err) {
		return 0, false
	}
	limit := m.streamGuardsFor(opts.Metadata).busyWaitMax
	if limit <= 0 {
		return 0, false
	}
	remaining := budget.remaining(limit)
	if deadline, ok := ctx.Deadline(); ok {
		// Waiting past the request's own deadline would only turn the
		// upstream's error, and its Retry-After, into a bare timeout.
		remaining = min(remaining, time.Until(deadline))
	}
	if remaining <= 0 {
		return 0, false
	}
	now := time.Now()
	next, ok := m.soonestRecoveryLocked(providers, model, opts.Metadata, now)
	if !ok {
		return 0, false
	}
	wait := next.Sub(now)
	if wait < 0 {
		wait = 0
	}
	if wait > remaining {
		return 0, false
	}
	return spreadBusyWait(wait, remaining), true
}

// spreadBusyWait adds up to a fifth of the wait, within the budget. Requests
// waiting for the same account would otherwise all retry in the instant it
// recovers and could knock it straight back into its cooldown.
func spreadBusyWait(wait, remaining time.Duration) time.Duration {
	spread := wait / 5
	if room := remaining - wait; spread > room {
		spread = room
	}
	if spread <= 0 {
		return wait
	}
	return wait + rand.N(spread)
}

// soonestRecoveryLocked reports when the first account this request may use
// recovers, provided every such account is cooling and at least one of them
// for a recoverable reason.
func (m *Manager) soonestRecoveryLocked(providers []string, model string, meta map[string]any, now time.Time) (time.Time, bool) {
	scope := newSelectionScope(m.currentRuntimeConfigForTenant(tenantIDFromMetadata(meta)), model, meta)
	scope.grants = m.sharedGrantIndex()
	excluded := make(map[string]struct{})
	if exclusions, ok := retryExclusionsFromMetadata(meta); ok {
		for _, id := range exclusions.authIDs {
			excluded[id] = struct{}{}
		}
	}
	providerSet := make(map[string]struct{}, len(providers))
	for _, provider := range providers {
		if key := strings.TrimSpace(strings.ToLower(provider)); key != "" {
			providerSet[key] = struct{}{}
		}
	}

	m.mu.RLock()
	defer m.mu.RUnlock()
	var soonest time.Time
	for _, routeGroup := range scope.routeGroupsToTry() {
		for _, candidate := range m.auths {
			if !scope.allowsCandidate(candidate, routeGroup, excluded, m.modelRegistry) || !authParticipatesInSelection(candidate) {
				continue
			}
			providerKey := strings.TrimSpace(strings.ToLower(candidate.Provider))
			if _, ok := providerSet[providerKey]; !ok || m.executorForAuthLocked(candidate, providerKey) == nil {
				continue
			}
			// The same model key the selector checks availability with.
			blocked, reason, next := isAuthBlockedForModel(candidate, scope.model, now)
			if !blocked {
				// Someone could serve right now, so the pool is not busy: the
				// request failed for another reason and waiting would not help.
				return time.Time{}, false
			}
			if reason == blockReasonDisabled || next.IsZero() || !recoverableCooldown(candidate, scope.model) {
				continue
			}
			if soonest.IsZero() || next.Before(soonest) {
				soonest = next
			}
		}
	}
	return soonest, !soonest.IsZero()
}

// recoverableCooldown reports whether an account is parked for a reason that
// passes on its own within minutes: rate limiting or an upstream fault.
func recoverableCooldown(auth *Auth, model string) bool {
	lastErr, quota := auth.LastError, auth.Quota
	if state := modelStateFor(auth, model); state != nil {
		lastErr, quota = state.LastError, state.Quota
	}
	if quota.Exceeded || auth.Quota.Exceeded {
		return false
	}
	switch CooldownReasonOf(lastErr, quota) {
	case CooldownReasonRateLimited, CooldownReasonUpstreamError:
		return true
	default:
		return false
	}
}

func modelStateFor(auth *Auth, model string) *ModelState {
	if auth == nil || model == "" || len(auth.ModelStates) == 0 {
		return nil
	}
	if state, ok := auth.ModelStates[model]; ok && state != nil {
		return state
	}
	if base := canonicalModelKey(model); base != "" && base != model {
		return auth.ModelStates[base]
	}
	return nil
}
