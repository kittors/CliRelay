package auth

import (
	"context"
	"errors"
	"sort"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/routetrace"
)

// The scheduler's side of the route trace (package routetrace). Steps are
// named with the codes the routing simulator uses (SimulatedBlock*,
// CooldownReason*, SimulatedRetryWait*), so the request log and the simulator
// explain a placement in one vocabulary, and the panel words both alike.

// tracedAttemptInfo names a candidate's account for the trace.
func tracedAttemptInfo(candidate *mixedExecutionCandidate) routetrace.AttemptInfo {
	return routetrace.AttemptInfo{
		Group:     candidate.group,
		AuthID:    candidate.auth.ID,
		AuthLabel: candidate.auth.ChannelName(),
		Provider:  candidate.provider,
		Model:     candidate.execReq.Model,
	}
}

// beginTracedAttempt starts tracing an upstream attempt on candidate. From
// here the executor runs under candidate.execCtx, which carries the attempt so
// that the usage records it publishes wait for the attempt to settle.
//
// Every selector admits only accounts that are not blocked for the model
// (getAvailableAuths), save the one exception of earliestOptimisticCandidate.
// So an account still blocked in the copy selection handed over is that
// optimistic pick, and the trace marks it: unmarked, it would read as a request
// sent to an account the scheduler knew was out.
func beginTracedAttempt(ctx context.Context, candidate *mixedExecutionCandidate, routeModel string) *routetrace.Attempt {
	rec := routetrace.FromContext(ctx)
	if rec == nil {
		return nil
	}
	info := tracedAttemptInfo(candidate)
	if blocked, until, _, _ := candidateBlockOf(candidate.auth, routeModel, time.Now()); blocked != "" {
		info.Optimistic, info.CoolingUntil = true, until
	}
	attempt := rec.BeginAttempt(info)
	candidate.execCtx = attempt.Context(candidate.execCtx)
	return attempt
}

func canceledOutcome(err error) routetrace.Outcome {
	return routetrace.Outcome{Outcome: routetrace.OutcomeCanceled, Error: err.Error()}
}

// finishTracedAttempt records how an attempt ended. A failure is described
// after MarkResult has applied it, so the trace shows what the failure left
// the account in. beforeFirstByte applies to streams.
func (m *Manager) finishTracedAttempt(attempt *routetrace.Attempt, authID, model string, err error, beforeFirstByte bool) {
	if attempt == nil {
		return
	}
	if err == nil {
		attempt.Finish(routetrace.Outcome{Outcome: routetrace.OutcomeSuccess})
		return
	}
	outcome := tracedFailure(err)
	outcome.Blocked, outcome.BlockedUntil, outcome.Cause = m.accountBlockAfter(authID, model, outcome.Cause)
	outcome.BeforeFirstByte = beforeFirstByte
	attempt.Finish(outcome)
}

// tracedFailure describes a failed attempt from its error alone.
func tracedFailure(err error) routetrace.Outcome {
	out := routetrace.Outcome{Outcome: routetrace.OutcomeFailed, Status: statusCodeFromError(err), Error: err.Error()}
	if errors.Is(err, context.Canceled) {
		out.Outcome = routetrace.OutcomeCanceled
		return out
	}
	out.Cause = failureCause(err)
	return out
}

// failureCause classifies a failed attempt by what went wrong rather than by
// what it did to the account. A cooldown the failure starts may name the
// cause more precisely (an exhausted quota behind a 429); accountBlockAfter
// prefers that one.
func failureCause(err error) string {
	switch {
	case isRequestInvalidError(err):
		return routetrace.CauseRequestInvalid
	case isContentPolicyViolation(err):
		return routetrace.CauseContentPolicy
	}
	authErr := errorFromExecution(err)
	switch strings.TrimSpace(authErr.Code) {
	case FirstTokenTimeoutCode:
		return routetrace.CauseFirstTokenTimeout
	case "response_stream_incomplete":
		return routetrace.CauseStreamIncomplete
	}
	if authErr.HTTPStatus == 0 {
		authErr.HTTPStatus = statusCodeFromError(err)
	}
	if cause := CooldownReasonOf(authErr, QuotaState{}); cause != "" {
		return cause
	}
	if authErr.HTTPStatus == 0 {
		return routetrace.CauseNetwork
	}
	return ""
}

// accountBlockAfter reads what the account of a finished attempt is left in,
// as the routing simulator describes a candidate. The block's own cause
// replaces errorCause when the account cools down for a reason of the generic
// kind; the specific causes of failureCause (a malformed request, a
// first-token timeout) are kept, since they say more than the cooldown does.
func (m *Manager) accountBlockAfter(authID, model, errorCause string) (string, time.Time, string) {
	m.mu.RLock()
	auth := m.auths[authID]
	var (
		blocked    string
		until      time.Time
		blockCause string
	)
	if auth != nil {
		blocked, until, blockCause, _ = candidateBlockOf(auth, model, time.Now())
	}
	m.mu.RUnlock()
	switch errorCause {
	case routetrace.CauseRequestInvalid, routetrace.CauseContentPolicy, routetrace.CauseFirstTokenTimeout, routetrace.CauseStreamIncomplete:
		return blocked, until, errorCause
	}
	if blocked != "" && blockCause != "" {
		return blocked, until, blockCause
	}
	return blocked, until, errorCause
}

// traceSkip records a candidate passed over for being at its concurrency
// limit.
func traceSkip(ctx context.Context, candidate *mixedExecutionCandidate) {
	routetrace.FromContext(ctx).Skip(routetrace.SkipSaturated, tracedAttemptInfo(candidate))
}

// traceExhausted records a pass that ends with err because no candidate could
// take the request, with the state of every candidate. Describing them walks
// the pool, so it runs only for traced requests whose client is still there.
func (s executionService) traceExhausted(ctx context.Context, scope *mixedExecutionScope, err error) {
	rec := routetrace.FromContext(ctx)
	if rec == nil || err == nil || ctx.Err() != nil {
		return
	}
	sim := simulatedErrorOf(err)
	rec.Exhausted(routetrace.Exhaustion{
		Status:     sim.HTTPStatus,
		Code:       sim.Code,
		Message:    sim.Message,
		RetryAfter: sim.RetryAfter,
		Candidates: s.manager.tracedCandidates(scope, time.Now()),
	})
}

// tracedCandidates lists the accounts of the route group that governs the
// request (the first one selection finds candidates in, as selection itself
// falls back only past an empty group) and why each cannot serve, soonest
// recovery first.
func (m *Manager) tracedCandidates(scope *mixedExecutionScope, now time.Time) []routetrace.Candidate {
	meta := scope.opts.Metadata
	selection := newSelectionScope(m.currentRuntimeConfigForTenant(tenantIDFromMetadata(meta)), scope.routeModel, meta)
	selection.grants = m.sharedGrantIndex()
	providerSet := make(map[string]struct{}, len(scope.providers))
	for _, provider := range scope.providers {
		if key := strings.TrimSpace(strings.ToLower(provider)); key != "" {
			providerSet[key] = struct{}{}
		}
	}

	m.mu.RLock()
	defer m.mu.RUnlock()
	service := newSelectorService(m)
	include := withSelectionCapabilities(scope.opts, service.mixedCandidateFilterLocked(providerSet))
	for _, routeGroup := range selection.routeGroupsToTry() {
		candidates := service.buildCandidatesLocked(selection, routeGroup, nil, m.modelRegistry, include)
		if len(candidates) == 0 {
			continue
		}
		out := make([]routetrace.Candidate, 0, len(candidates))
		for _, candidate := range candidates {
			entry := routetrace.Candidate{AuthID: candidate.ID, Label: candidate.ChannelName(), Provider: candidate.Provider}
			var until time.Time
			entry.State, until, entry.Cause, entry.Status = candidateBlockOf(candidate, selection.model, now)
			if entry.State == "" {
				switch {
				case !authParticipatesInSelection(candidate):
					entry.State = SimulatedBlockWeight
				case m.concurrencyLimiter.saturated(candidate):
					entry.State = routetrace.SkipSaturated
				}
			}
			// An account the request already failed on is out for that reason
			// whatever else holds; its cooldown still says when it is back.
			if _, tried := scope.tried[candidate.ID]; tried {
				entry.State = routetrace.CandidateTried
			}
			if !until.IsZero() {
				until = until.Truncate(time.Millisecond)
				entry.Until = &until
			}
			out = append(out, entry)
		}
		sort.SliceStable(out, func(i, j int) bool {
			a, b := out[i].Until, out[j].Until
			if (a == nil) != (b == nil) {
				return a != nil
			}
			if a != nil && !a.Equal(*b) {
				return a.Before(*b)
			}
			return out[i].AuthID < out[j].AuthID
		})
		return out
	}
	return nil
}
