package auth

import (
	"context"
	"errors"
	"sort"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v6/sdk/config"
)

// Routing simulation.
//
// The panel's routing simulator asks "where would this request go, and why":
// which route groups selection tries, which credentials each group considers,
// why the others are left out, which candidates are cooling down and until
// when, and the order the request would try candidates in.
//
// Nothing here re-implements a selection rule. Exclusions come from
// candidateExclusion and authBlockForModel, which selection itself reads, and
// the order comes from running the real pickLocked and the real selectors pick
// after pick, the way a request fails over. The selectors run on copies of
// their scheduling state (round-robin cursors, recent-selection pressure), so a
// simulation leaves the state live requests are placed by untouched.

// CandidateExclusion names the gate that keeps a credential out of selection.
// The panel keys its wording on these values: add new ones, never rename.
type CandidateExclusion string

const (
	// ExcludedDisabled is a credential switched off by the operator or marked
	// disabled after a failure.
	ExcludedDisabled CandidateExclusion = "disabled"
	// ExcludedTenant is a credential the request's tenant may not use.
	ExcludedTenant CandidateExclusion = "tenant"
	// ExcludedPinned is any credential but the one a pinned request names.
	ExcludedPinned CandidateExclusion = "pinned"
	// ExcludedChannel is outside the channels the API key may use.
	ExcludedChannel CandidateExclusion = "channel"
	// ExcludedAllowedGroups is outside the channel groups the API key may use.
	ExcludedAllowedGroups CandidateExclusion = "allowed-groups"
	// ExcludedRouteGroup is outside the route group being tried.
	ExcludedRouteGroup CandidateExclusion = "route-group"
	// ExcludedTried is a credential the request already tried.
	ExcludedTried CandidateExclusion = "tried"
	// ExcludedModel does not serve the model, or a channel group's model list
	// refuses it for this credential.
	ExcludedModel CandidateExclusion = "model"
)

// Why a candidate cannot take the request right now; empty when it can.
const (
	SimulatedBlockCooldown    = "cooldown"
	SimulatedBlockUnavailable = "unavailable"
	SimulatedBlockDisabled    = "disabled"
	// SimulatedBlockWeight is a candidate weighted 0 in its channel group.
	SimulatedBlockWeight = "weight"
)

// Where a reset-aware candidate stands, in the order the tiers are served.
const (
	ResetStandingRoom    = "room"
	ResetStandingUnknown = "unknown"
	ResetStandingLow     = "low"
	ResetStandingSpent   = "spent"
)

// maxSimulatedAttempts bounds the failover sequence. Selection never tries a
// credential twice, so the sequence ends on its own; the bound only guards
// against a selector that keeps returning credentials outside the candidates.
const maxSimulatedAttempts = 500

// SelectionSimulation is how the conductor would place one request.
type SelectionSimulation struct {
	// Model is the model as routed, thinking suffix included.
	Model string
	// ModelKey is the model credentials are matched against: Model without
	// its thinking suffix.
	ModelKey string
	// Providers are the providers whose credentials may serve the model.
	Providers []string
	// Groups lists the route groups selection tries, in order.
	Groups []RouteGroupSimulation
	// Attempts is the order the request would try credentials in if each one
	// failed. The first entry serves the request.
	Attempts []SimulatedAttempt
	// Error is what the request gets when no credential can take it.
	Error *SimulatedError
	// RetryWait is how long a request that gets Error waits for the soonest
	// cooldown to end before selection runs again.
	RetryWait time.Duration
	// RetryWaitKind says which wait the request takes; empty when it fails
	// with Error at once.
	RetryWaitKind string
}

// Which wait a request that finds no credential takes before selection runs
// again (RetryWaitKind).
const (
	// SimulatedRetryWaitRequestRetry is the request-retry wait (request-retry
	// and max-retry-interval in config.yaml), consulted first.
	SimulatedRetryWaitRequestRetry = "request-retry"
	// SimulatedRetryWaitBusyWait is the governing channel group's busy wait
	// (busy-wait-max-seconds). The live wait ends at a random point up to
	// busyWaitJitter after the reported one, and is taken only while one of
	// the process's waiter slots is free.
	SimulatedRetryWaitBusyWait = "busy-wait"
)

// RouteGroupSimulation is one route group selection tries.
type RouteGroupSimulation struct {
	// Name is the route group. Empty means the request is not narrowed to one
	// group: any credential its allowed channel groups reach may serve it.
	Name string
	// Fallback marks the default group a route path falls back to once its
	// own group has no candidate left.
	Fallback bool
	// Distribution is how the group spreads requests across its candidates.
	Distribution string
	// Sticky keeps a running conversation on the credential that served it;
	// the order below is the one a new conversation gets.
	Sticky              bool
	StickyMaxRequests   int
	StickyReleaseAtLoad float64
	// Credentials are the tenant's credentials of the request's providers,
	// candidates and excluded ones alike, ordered by ID.
	Credentials []CredentialSimulation
	// Error is why the group has no candidate, when it has none.
	Error *SimulatedError
}

// CredentialSimulation is one credential as one route group sees it.
type CredentialSimulation struct {
	AuthID   string
	Label    string
	Provider string
	// Shared marks a system credential shared with the request's tenant.
	Shared bool
	// Excluded is the gate that keeps the credential out of this group; empty
	// for a candidate.
	Excluded CandidateExclusion
	// ModelExcludedBy and ModelNotAllowedBy name the channel groups whose
	// excluded-models or allowed-models list refuses the model, when that is
	// why the credential is out. Both empty with Excluded == model means the
	// credential does not serve the model at all.
	ModelExcludedBy   []string
	ModelNotAllowedBy []string

	// The fields below describe candidates only.

	// Weight is the candidate's share in its group; 0 takes it out of rotation.
	Weight int
	// Blocked is why the candidate cannot take a request now (a
	// SimulatedBlock* value); empty when it can.
	Blocked string
	// BlockedUntil is when the block ends, zero when it waits for a probe or
	// names no time.
	BlockedUntil time.Time
	// BlockCause is the CooldownReason* code of the failure behind the block.
	BlockCause      string
	BlockHTTPStatus int
	// InFlight and ConcurrencyLimit are the account's running requests and the
	// most it takes at once (0 for no limit). Saturated is true at the limit:
	// the request queues for it only once no other candidate can serve.
	InFlight         int
	ConcurrencyLimit int
	Saturated        bool
	// LoadRatio is how full the account is, the larger of its concurrency use
	// and its quota use, in [0,1]. Fill-first moves on at 0.9.
	LoadRatio float64
	// Score is the least-load score: recent traffic and running requests per
	// unit of weight, raised as the account nears its quota. Lower goes first.
	Score float64
	// Reset is where a reset-aware group ranks the candidate.
	Reset *ResetStanding
	// Attempt is the candidate's 1-based place in the request's failover
	// order, 0 when the request would not reach it.
	Attempt int
}

// ResetStanding explains a candidate's place in a reset-aware group.
type ResetStanding struct {
	// Tier is a ResetStanding* value.
	Tier string
	// Used is the share spent of the most used window the request draws from.
	Used float64
	// Windows are the windows the request draws from, longest first.
	Windows []QuotaWindow
}

// SimulatedAttempt is one step of the failover order.
type SimulatedAttempt struct {
	AuthID string
	// Group is the route group the credential was picked in.
	Group string
	// UpstreamModel is the model name sent to this credential's upstream,
	// after its prefix and model aliases.
	UpstreamModel string
	// Queued is true when the account is at its concurrency limit: the request
	// waits for a slot on it instead of being sent at once.
	Queued bool
}

// SimulatedError is the error a request would get.
type SimulatedError struct {
	Code       string
	Message    string
	HTTPStatus int
	// RetryAfter is the wait the error advertises to the client.
	RetryAfter time.Duration
}

// SimulateSelection reports how the conductor would place a request for model
// (as routed, thinking suffix included) on providers. meta is the execution
// metadata a real request carries: tenant, allowed channels and channel groups,
// route group and fallback. It changes no state.
func (m *Manager) SimulateSelection(ctx context.Context, providers []string, model string, meta map[string]any) *SelectionSimulation {
	sim := &SelectionSimulation{Model: model, ModelKey: canonicalModelKey(model)}
	if m == nil {
		return sim
	}
	sim.Providers = m.normalizeProviders(providers)
	if len(sim.Providers) == 0 {
		sim.Error = &SimulatedError{Code: "provider_not_found", Message: "no provider supplied"}
		return sim
	}
	providerSet := make(map[string]struct{}, len(sim.Providers))
	for _, provider := range sim.Providers {
		providerSet[provider] = struct{}{}
	}
	scope := newSelectionScope(m.currentRuntimeConfigForTenant(tenantIDFromMetadata(meta)), model, meta)
	scope.grants = m.sharedGrantIndex()
	picked, pickErr := m.simulatePicks(ctx, sim, scope, providerSet, meta)

	if len(sim.Attempts) == 0 && pickErr != nil {
		sim.Error = simulatedErrorOf(pickErr)
		sim.RetryWait, sim.RetryWaitKind = m.simulatedRetryWait(ctx, pickErr, sim.Providers, model, meta)
	}
	// Alias lookups read configuration under the manager's own locks, so they
	// run after the read lock above is released.
	for i, selected := range picked {
		upstream := rewriteModelForAuth(model, selected)
		upstream = m.applyOAuthModelAlias(selected, upstream)
		sim.Attempts[i].UpstreamModel = m.applyAPIKeyModelAlias(selected, upstream)
	}
	sim.markAttempts()
	return sim
}

// simulatePicks describes each route group and runs selection pick after pick,
// as a request that fails on every credential would, under the manager's read
// lock. It returns copies of the picked credentials, safe to read once the lock
// is released, and the error that ended the sequence.
func (m *Manager) simulatePicks(ctx context.Context, sim *SelectionSimulation, scope selectionScope, providerSet map[string]struct{}, meta map[string]any) ([]*Auth, error) {
	service := newSelectorService(m)
	opts := cliproxyexecutor.Options{Metadata: meta}
	now := time.Now()

	m.mu.RLock()
	defer m.mu.RUnlock()
	include := service.mixedCandidateFilterLocked(providerSet)
	scratch := m.newScratchSelectorsLocked()
	for i, routeGroup := range scope.routeGroupsToTry() {
		sim.Groups = append(sim.Groups, m.describeRouteGroupLocked(scope, routeGroup, i > 0, include, scratch.deps, now))
	}
	tried := make(map[string]struct{})
	picked := make([]*Auth, 0)
	for len(picked) < maxSimulatedAttempts {
		selected, routeGroup, err := service.pickLocked(ctx, scope, "mixed", opts, tried, include, scratch.selectorFor)
		if err != nil {
			return picked, err
		}
		if _, again := tried[selected.ID]; again {
			break
		}
		tried[selected.ID] = struct{}{}
		picked = append(picked, selected.Clone())
		sim.Attempts = append(sim.Attempts, SimulatedAttempt{
			AuthID: selected.ID,
			Group:  routeGroup,
			Queued: m.concurrencyLimiter.saturated(selected),
		})
	}
	return picked, nil
}

// markAttempts records each candidate's place in the failover order in the
// group that picked it.
func (s *SelectionSimulation) markAttempts() {
	for place, attempt := range s.Attempts {
		for g := range s.Groups {
			if s.Groups[g].Name != attempt.Group {
				continue
			}
			for c := range s.Groups[g].Credentials {
				if s.Groups[g].Credentials[c].AuthID == attempt.AuthID {
					s.Groups[g].Credentials[c].Attempt = place + 1
				}
			}
			break
		}
	}
}

// describeRouteGroupLocked lists every credential of the request's providers
// the tenant may use, as routeGroup sees it. Callers hold m.mu.
func (m *Manager) describeRouteGroupLocked(scope selectionScope, routeGroup string, fallback bool, include func(*Auth) bool, deps *schedulerDeps, now time.Time) RouteGroupSimulation {
	scheduling := scopedScheduling(scope.cfg, routeGroup, scope.allowedGroups)
	group := RouteGroupSimulation{
		Name:         routeGroup,
		Fallback:     fallback,
		Distribution: sdkconfig.NormalizeDistribution(scheduling.Distribution),
		Sticky:       scheduling.StickyEnabled,
	}
	if scheduling.StickyEnabled {
		group.StickyMaxRequests = scheduling.StickyMax
		group.StickyReleaseAtLoad = scheduling.StickyRelease
	}
	registryRef := m.modelRegistry
	ids := make([]string, 0, len(m.auths))
	for id := range m.auths {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	candidates := make([]*Auth, 0)
	for _, id := range ids {
		auth := m.auths[id]
		// Credentials the tenant cannot use are not the tenant's business, and
		// other providers' credentials could never serve the model.
		if auth == nil || !authUsableByTenant(auth, scope.tenantID, scope.grants) || !include(auth) {
			continue
		}
		entry := CredentialSimulation{
			AuthID:   auth.ID,
			Label:    auth.ChannelName(),
			Provider: auth.Provider,
			Shared:   normalizedTenantID(auth.TenantID) != scope.tenantID,
		}
		entry.Excluded = scope.candidateExclusion(auth, routeGroup, nil, registryRef)
		if entry.Excluded == ExcludedModel {
			groups := authGroups(scope.cfg, auth)
			if !modelAllowedByRoutingGroupScopes(scope.cfg, auth.TenantID, scope.modelKey, groups, routeGroup, scope.allowedGroups) {
				entry.ModelExcludedBy, entry.ModelNotAllowedBy = blockingRouteGroups(scope.cfg, auth.TenantID, scope.modelKey, groups, routeGroup, scope.allowedGroups)
				sort.Strings(entry.ModelExcludedBy)
				sort.Strings(entry.ModelNotAllowedBy)
			}
		}
		if entry.Excluded == "" {
			candidate := prepareCandidateForSelection(scope.cfg, auth, routeGroup, scope.allowedGroups)
			describeCandidate(&entry, candidate, scope.model, deps, m.concurrencyLimiter, now)
			candidates = append(candidates, candidate)
		}
		group.Credentials = append(group.Credentials, entry)
	}

	switch group.Distribution {
	case sdkconfig.DistributionLeastLoad:
		selector := &LeastLoadSelector{deps: deps}
		for i := range group.Credentials {
			if candidate := candidateByID(candidates, group.Credentials[i].AuthID); candidate != nil {
				group.Credentials[i].Score = selector.score(candidate, scope.model, now)
			}
		}
	case sdkconfig.DistributionResetAware:
		for _, standing := range rankResetAware(candidates, scope.model, deps.quotaWindows()) {
			for i := range group.Credentials {
				if group.Credentials[i].AuthID == standing.auth.ID {
					group.Credentials[i].Reset = resetStandingOf(standing, deps.quotaWindows(), scope.model)
				}
			}
		}
	}
	if len(candidates) == 0 {
		group.Error = &SimulatedError{Code: "auth_not_found", Message: "no auth available"}
		if diagnosis := newSelectorService(m).diagnoseEmptyCandidates(scope, routeGroup, registryRef, include); diagnosis != nil {
			group.Error = simulatedErrorOf(diagnosis)
		}
	}
	return group
}

// describeCandidate fills in why a candidate can or cannot take a request now.
func describeCandidate(entry *CredentialSimulation, candidate *Auth, model string, deps *schedulerDeps, limiter *AccountConcurrencyLimiter, now time.Time) {
	entry.Weight = authSelectionWeight(candidate)
	entry.Blocked, entry.BlockedUntil, entry.BlockCause, entry.BlockHTTPStatus = candidateBlockOf(candidate, model, now)
	if entry.Blocked == "" && entry.Weight <= 0 {
		entry.Blocked = SimulatedBlockWeight
	}
	entry.InFlight, entry.ConcurrencyLimit = limiter.usage(candidate)
	entry.Saturated = entry.ConcurrencyLimit > 0 && entry.InFlight >= entry.ConcurrencyLimit
	entry.LoadRatio = deps.loadRatio(candidate, model)
}

// candidateBlockOf says why auth cannot take a request for model now, as a
// SimulatedBlock* value (empty when it can), when the block ends (zero when it
// waits for a probe or names no time), the CooldownReason* code of the failure
// behind it and that failure's HTTP status. The routing simulator and the
// route trace both describe accounts with it. A weight of 0 is the caller's
// to add: it depends on the group the account is seen in.
func candidateBlockOf(auth *Auth, model string, now time.Time) (blocked string, until time.Time, cause string, status int) {
	block := authBlockForModel(auth, model, now)
	if !block.blocked {
		return "", time.Time{}, "", 0
	}
	switch block.reason {
	case blockReasonDisabled:
		blocked = SimulatedBlockDisabled
	case blockReasonCooldown:
		blocked = SimulatedBlockCooldown
	default:
		blocked = SimulatedBlockUnavailable
	}
	var quota QuotaState
	if block.quota != nil {
		quota = *block.quota
	}
	cause = CooldownReasonOf(block.lastErr, quota)
	if block.lastErr != nil {
		status = block.lastErr.HTTPStatus
	}
	return blocked, block.next, cause, status
}

func resetStandingOf(standing resetAwareStanding, source QuotaWindowSource, model string) *ResetStanding {
	out := &ResetStanding{Used: standing.used}
	switch standing.tier {
	case resetAwareRoom:
		out.Tier = ResetStandingRoom
	case resetAwareLow:
		out.Tier = ResetStandingLow
	case resetAwareSpent:
		out.Tier = ResetStandingSpent
	default:
		out.Tier = ResetStandingUnknown
	}
	if source != nil {
		if windows, known := source.QuotaWindows(standing.auth, canonicalModelKey(model)); known {
			out.Windows = append(out.Windows, windows...)
			sort.SliceStable(out.Windows, func(i, j int) bool {
				if out.Windows[i].Span != out.Windows[j].Span {
					return out.Windows[i].Span > out.Windows[j].Span
				}
				return out.Windows[i].Key < out.Windows[j].Key
			})
		}
	}
	return out
}

func candidateByID(candidates []*Auth, id string) *Auth {
	for _, candidate := range candidates {
		if candidate.ID == id {
			return candidate
		}
	}
	return nil
}

// simulatedRetryWait reports whether a request that failed with err would wait
// for a cooldown and run selection again, and which wait, the way
// nextRetryPlan decides on the first failure of a request that may busy-wait
// (chat and messages requests may; token counts may not). It takes no waiter
// slot and no round of the request's budget.
func (m *Manager) simulatedRetryWait(ctx context.Context, err error, providers []string, model string, meta map[string]any) (time.Duration, string) {
	_, maxWait := m.retrySettings()
	if wait, ok := m.shouldRetryAfterError(err, 0, providers, model, maxWait, meta); ok {
		return wait, SimulatedRetryWaitRequestRetry
	}
	opts := cliproxyexecutor.Options{Metadata: meta}
	if wait, ok := m.busyWaitUntilRecovery(ctx, err, providers, model, opts, &retryWaitBudget{}); ok {
		return wait, SimulatedRetryWaitBusyWait
	}
	return 0, ""
}

// simulatedErrorOf renders what a selection error tells the client.
func simulatedErrorOf(err error) *SimulatedError {
	if err == nil {
		return nil
	}
	out := &SimulatedError{Message: err.Error(), HTTPStatus: statusCodeFromError(err)}
	var authErr *Error
	if errors.As(err, &authErr) {
		out.Code = authErr.Code
		out.Message = authErr.Message
	}
	var cooldown *modelCooldownError
	if errors.As(err, &cooldown) {
		out.Code, out.Message, out.RetryAfter = "model_cooldown", cooldown.message(), cooldown.resetIn
	}
	var unavailable *modelUnavailableError
	if errors.As(err, &unavailable) {
		out.Code, out.Message, out.RetryAfter = "model_unavailable", unavailable.message(), unavailable.resetIn
	}
	return out
}

// scratchSelectors hands the simulator private copies of the distribution
// selectors. A copy keeps the live selector's state as of the simulation and
// advances it pick by pick, as the live one would for a request that fails
// over, without moving anything live requests are placed by.
type scratchSelectors struct {
	manager *Manager
	deps    *schedulerDeps
	byName  map[string]Selector
}

func (m *Manager) newScratchSelectorsLocked() *scratchSelectors {
	return &scratchSelectors{manager: m, deps: m.scheduler.scratchCopy(), byName: make(map[string]Selector)}
}

// selectorFor returns the copy serving a routing scope. A sticky group gets its
// distribution: stickiness only pins conversations already running, and a
// simulated request belongs to none.
func (s *scratchSelectors) selectorFor(cfg *runtimeConfigSnapshot, routeGroup string, allowedGroups map[string]struct{}) Selector {
	name := sdkconfig.NormalizeDistribution(scopedScheduling(cfg, routeGroup, allowedGroups).Distribution)
	if selector, ok := s.byName[name]; ok {
		return selector
	}
	var selector Selector
	switch name {
	case sdkconfig.DistributionFillFirst:
		selector = &FillFirstSelector{deps: s.deps}
	case sdkconfig.DistributionLeastLoad:
		selector = &LeastLoadSelector{deps: s.deps}
	case sdkconfig.DistributionResetAware:
		selector = &ResetAwareSelector{deps: s.deps}
	default:
		selector = s.manager.roundRobinSelector.scratchCopy(s.deps)
	}
	s.byName[name] = selector
	return selector
}

// scratchCopy returns deps whose selection pressure is a copy, so observing a
// simulated pick does not count against the live account. The quota source and
// the concurrency limiter are only read.
func (d *schedulerDeps) scratchCopy() *schedulerDeps {
	if d == nil {
		return nil
	}
	return &schedulerDeps{tracker: d.tracker.snapshot(), limiter: d.limiter, quota: d.quota}
}

func (t *selectionPressureTracker) snapshot() *selectionPressureTracker {
	if t == nil {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	copied := &selectionPressureTracker{counters: make(map[string]decayCounter, len(t.counters)), maxKeys: t.maxKeys}
	for id, counter := range t.counters {
		copied.counters[id] = counter
	}
	return copied
}

// scratchCopy returns a round-robin selector that starts from this one's
// cursors and weighted scores. One case is not reproducible: a gemini-cli
// virtual-parent cursor the live selector has not created yet starts at a
// random credential (Pick seeds it with rand.IntN), so for the first request of
// such a model the copy and the live selector may start on different ones.
func (s *RoundRobinSelector) scratchCopy(deps *schedulerDeps) *RoundRobinSelector {
	copied := &RoundRobinSelector{deps: deps}
	if s == nil {
		return copied
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	copied.maxKeys = s.maxKeys
	copied.cursors = make(map[string]int, len(s.cursors))
	for key, cursor := range s.cursors {
		copied.cursors[key] = cursor
	}
	copied.weighted = make(map[string]*weightedCursorState, len(s.weighted))
	for key, state := range s.weighted {
		if state == nil {
			continue
		}
		current := make(map[string]int, len(state.current))
		for id, score := range state.current {
			current[id] = score
		}
		copied.weighted[key] = &weightedCursorState{current: current, tieCursor: state.tieCursor}
	}
	return copied
}

// usage reports an account's running requests and the limit this process
// enforces for it, 0 for none.
func (l *AccountConcurrencyLimiter) usage(auth *Auth) (inFlight, limit int) {
	if l == nil || auth == nil {
		return 0, 0
	}
	l.mu.RLock()
	defer l.mu.RUnlock()
	limit = l.effectiveLimit(auth.ConcurrencyLimit())
	if limit < 0 {
		limit = 0
	}
	return l.active[auth.ID], limit
}

// saturated reports whether the account is at its concurrency limit.
func (l *AccountConcurrencyLimiter) saturated(auth *Auth) bool {
	inFlight, limit := l.usage(auth)
	return limit > 0 && inFlight >= limit
}
