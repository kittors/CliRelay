package auth

import (
	"context"
	"math/rand/v2"
	"net/http"
	"strings"
	"sync"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v6/sdk/config"
)

// Busy waiting: when every account that may serve a request is cooling down
// after a recoverable failure (408, a 5xx, a short 429), a request governed by
// a channel group that turned it on (busy-wait-max-seconds, off unless set)
// waits for the soonest account to recover instead of failing at once.
//
// It is deliberately narrower than the request-retry wait that predates it:
//   - Only accounts this request may use count (tenant, groups, channels,
//     model), and accounts an earlier attempt of the same request already
//     failed on are left out; the older wait looks at every account of the
//     provider.
//   - A quota that ran out, a 401/403 or a 404 is not waited for: none of them
//     recovers within minutes, so waiting would only delay the error.
//   - A brief back-off is not waited for either (briefBackoff): the few seconds
//     a provider load-shed or a first-token timeout sets mean "the provider is
//     struggling right now", and every request that wakes up to try again adds
//     to the load that caused it. Such a pool fails at once, as it did before,
//     with the Retry-After for the client to act on.
//   - If any usable account is not cooling at all, the failure was not "the
//     pool is busy" and nothing is waited for. That also keeps accounts with
//     cooling disabled from turning this into a hot retry loop.
//   - It starts only when the selector has nothing to offer: a pool whose
//     soonest block ends within optimisticRetryWindow is not empty to the
//     selector, which tries that account at once, so a wait is for recoveries
//     beyond that, or for the pass after one in which every account was tried.
//   - The budget is per request and shared by every retry of it, and never
//     reaches past the request's own deadline. A request also waits at most
//     maxBusyWaitRounds times: each wait ends in a new pass over the pool, so
//     the rounds bound what one request can cost the upstream (at most
//     rounds+1 attempts per account) however the cooldowns line up.
//
// When it gives up, the request fails exactly as it did before, including the
// Retry-After the scheduler derives from the cooldowns.

const (
	// maxBusyWaitRounds is how many times one request may wait. The first wait
	// is the feature itself: every account was freshly parked and the soonest
	// is back within the budget. A second covers a pool that recovered and was
	// knocked down again by the first probes. A third would mean the upstream
	// is down rather than busy, and the client is better served by the error
	// and its Retry-After than by one more pass over the pool.
	maxBusyWaitRounds = 2

	// busyWaitJitter is the window after the moment the soonest account
	// recovers over which the requests waiting for it resume, each at a
	// uniformly random point. It is a fixed length on purpose: a spread
	// proportional to the wait gives requests that start waiting late (a
	// second before the recovery) almost none, and under steady arrival those
	// late requests are exactly the herd that wakes together and knocks the
	// account straight back into its cooldown.
	//
	// 3s because the waiter cap bounds how many wait at once (64 by default),
	// so a stalled pool resumes at about 20 requests a second at worst, the
	// order of an account's ordinary traffic instead of a spike, while it
	// stays short next to the cooldowns worth waiting for (longer than the
	// 5-second tier). The window may carry a wait past the group's budget by
	// up to this much, but never past the request's own deadline.
	busyWaitJitter = 3 * time.Second
)

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
	mu     sync.Mutex
	spent  time.Duration
	rounds int
}

type retryWaitBudgetKey struct{}

// WithRetryWaitBudget shares one busy-wait budget between every scheduling
// pass of a request, including the ones a handler starts when it retries a
// stream on another account. Without it each pass would get the full budget,
// and its own two rounds.
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

// remaining is how much more the request may busy-wait under limit; zero once
// its rounds are used up, whatever time is left.
func (b *retryWaitBudget) remaining(limit time.Duration) time.Duration {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.rounds >= maxBusyWaitRounds {
		return 0
	}
	return limit - b.spent
}

// startRound counts one more wait, reporting false when the request has had
// all of them.
func (b *retryWaitBudget) startRound() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.rounds >= maxBusyWaitRounds {
		return false
	}
	b.rounds++
	return true
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
	release, ok := m.acquireBusyWaiter()
	if !ok {
		return retryPlan{}, false
	}
	if !budget.startRound() {
		release()
		return retryPlan{}, false
	}
	return retryPlan{wait: wait, budget: budget, release: release}, true
}

// acquireBusyWaiter takes one of the process-wide waiter slots, if any is
// left. In an outage longer than the waits every incoming request would
// otherwise park here, each holding a client connection that counts against
// the reverse proxy's limit; past the cap requests fail at once, as they did
// before.
func (m *Manager) acquireBusyWaiter() (release func(), ok bool) {
	if m.busyWaiters.Add(1) > int32(m.BusyWaitMaxWaiters()) {
		m.busyWaiters.Add(-1)
		return nil, false
	}
	return func() { m.busyWaiters.Add(-1) }, true
}

// BusyWaitMaxWaiters is the cap on requests waiting at once for a cooling
// account, as SetBusyWaitConfig last set it. A Manager that never got that call
// (the host sets it on every start and reload) takes the default.
func (m *Manager) BusyWaitMaxWaiters() int {
	if m == nil {
		return 0
	}
	if limit := int(m.maxBusyWaiters.Load()); limit > 0 {
		return limit
	}
	return sdkconfig.StreamingConfig{}.BusyWaiters()
}

// busyWaitFor returns how long to wait for the soonest usable account to come
// back, when every usable account is cooling and one recovers within the
// request's remaining budget and rounds.
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
	next, ok := m.soonestRecovery(providers, model, opts, now)
	if !ok {
		return 0, false
	}
	wait := max(next.Sub(now), 0)
	if wait > remaining {
		return 0, false
	}
	return wait + m.wakeSpread(ctx, wait), true
}

// wakeSpread is how long after the soonest recovery a waiter resumes: a
// uniformly random point of busyWaitJitter, cut short so that the wake-up
// stays inside the request's own deadline.
func (m *Manager) wakeSpread(ctx context.Context, wait time.Duration) time.Duration {
	window := busyWaitJitter
	if deadline, ok := ctx.Deadline(); ok {
		window = min(window, time.Until(deadline)-wait)
	}
	if window <= 0 {
		return 0
	}
	if m.busyWaitSpread != nil {
		return min(max(m.busyWaitSpread(window), 0), window)
	}
	return rand.N(window)
}

// soonestRecovery reports when the first account this request may use
// recovers, provided every such account is cooling, none of them only briefly,
// and at least one for a reason worth waiting for. "May use" is decided with
// the selector's own candidate set: same scope gates, group weights, protocol
// capability and concurrency filter, and the same route-group fallback rule,
// so it never waits for an account the selector would not hand the request to.
// It takes m.mu itself.
func (m *Manager) soonestRecovery(providers []string, model string, opts cliproxyexecutor.Options, now time.Time) (time.Time, bool) {
	meta := opts.Metadata
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
	includeCandidate := withSelectionCapabilities(opts, func(candidate *Auth) bool {
		providerKey := strings.TrimSpace(strings.ToLower(candidate.Provider))
		_, ok := providerSet[providerKey]
		return ok && m.executorForAuthLocked(candidate, providerKey) != nil
	})
	selector := newSelectorService(m)
	for _, routeGroup := range scope.routeGroupsToTry() {
		// Reuse the selector's scope gates and effective group weights rather
		// than evaluating the raw credentials with a different eligibility rule.
		candidates := selector.buildCandidatesLocked(scope, routeGroup, excluded, m.modelRegistry, includeCandidate)
		if len(candidates) == 0 {
			continue
		}
		if m.concurrencyLimiter != nil {
			candidates = m.concurrencyLimiter.FilterAvailableCandidates(candidates)
		}
		var soonest time.Time
		for _, candidate := range candidates {
			if !authParticipatesInSelection(candidate) {
				continue
			}
			// The same model key the selector checks availability with.
			blocked, reason, next := isAuthBlockedForModel(candidate, scope.model, now)
			if !blocked {
				// Someone could serve right now, so the pool is not busy: the
				// request failed for another reason and waiting would not help.
				return time.Time{}, false
			}
			if reason == blockReasonDisabled || next.IsZero() {
				continue
			}
			switch cooldownKindOf(candidate, scope.model, next) {
			case cooldownBrief:
				// The provider is shedding load and an account is back in a few
				// seconds: that is the client's retry to make, not a reason to
				// hold the request for another account's minute.
				return time.Time{}, false
			case cooldownWorthWaiting:
				if soonest.IsZero() || next.Before(soonest) {
					soonest = next
				}
			}
		}
		// Selection only tries a fallback when this route group has no
		// candidates, not when its candidates are cooling or weighted out.
		return soonest, !soonest.IsZero()
	}
	return time.Time{}, false
}

type cooldownKind int

const (
	// cooldownOther: a quota, a revoked key, a missing model; none recovers
	// within minutes.
	cooldownOther cooldownKind = iota
	// cooldownBrief: a recoverable failure that parks the account for no longer
	// than the provider load-shed back-off.
	cooldownBrief
	// cooldownWorthWaiting: a recoverable failure with a longer back-off.
	cooldownWorthWaiting
)

// cooldownKindOf classifies why an account is parked until next. A recoverable
// reason is rate limiting or an upstream fault, which pass on their own within
// minutes. Whether the back-off is brief is read from how long the failure
// parked the account for (the gap between when it was recorded and next): the
// 5-second load-shed tier and the first-token timeout both set exactly that
// tier, and so does an upstream that asks for a few seconds, which is the same
// kind of answer.
func cooldownKindOf(auth *Auth, model string, next time.Time) cooldownKind {
	lastErr, quota, recordedAt := auth.LastError, auth.Quota, auth.UpdatedAt
	if state := modelStateFor(auth, model); state != nil {
		lastErr, quota, recordedAt = state.LastError, state.Quota, state.UpdatedAt
	}
	if quota.Exceeded || auth.Quota.Exceeded {
		return cooldownOther
	}
	switch CooldownReasonOf(lastErr, quota) {
	case CooldownReasonRateLimited, CooldownReasonUpstreamError:
	default:
		return cooldownOther
	}
	// A state without a recorded time cannot be told apart; it counts as a
	// long back-off, which is what every state was before the tier existed. A
	// later write to the record can only shorten the gap, which turns a wait
	// into the immediate failure the request would have had anyway.
	if !recordedAt.IsZero() && next.Sub(recordedAt) <= transientUpstreamOverloadCooldown {
		return cooldownBrief
	}
	return cooldownWorthWaiting
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
