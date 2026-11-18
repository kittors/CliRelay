package auth

import (
	"fmt"
	"sort"
	"strings"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

// collectAvailableAuths partitions candidates into "can serve now" and the
// diagnostic counters used to explain an empty result. Weight filtering is not
// applied here: an excluded (weight 0) candidate must not be reported as being
// in cooldown, so the caller drops those separately.
func collectAvailableAuths(auths []*Auth, model string, now time.Time) (available []*Auth, cooldownCount int, cooldownEarliest time.Time, temporaryCount int, temporaryEarliest time.Time) {
	available = make([]*Auth, 0, len(auths))
	for i := 0; i < len(auths); i++ {
		candidate := auths[i]
		blocked, reason, next := isAuthBlockedForModel(candidate, model, now)
		if !blocked {
			available = append(available, candidate)
			continue
		}
		if reason == blockReasonCooldown {
			cooldownCount++
			if !next.IsZero() && (cooldownEarliest.IsZero() || next.Before(cooldownEarliest)) {
				cooldownEarliest = next
			}
		}
		if reason == blockReasonCooldown || reason == blockReasonOther {
			if !next.IsZero() {
				temporaryCount++
				if temporaryEarliest.IsZero() || next.Before(temporaryEarliest) {
					temporaryEarliest = next
				}
			}
		}
	}
	return available, cooldownCount, cooldownEarliest, temporaryCount, temporaryEarliest
}

func routeGroupSelectionScope(meta map[string]any) string {
	if len(meta) == 0 {
		return ""
	}
	switch raw := meta[cliproxyexecutor.RouteGroupMetadataKey].(type) {
	case string:
		return strings.TrimSpace(raw)
	case []byte:
		return strings.TrimSpace(string(raw))
	default:
		return ""
	}
}

func allowedChannelGroupsSelectionScope(meta map[string]any) string {
	if len(meta) == 0 {
		return ""
	}
	raw, ok := meta["allowed-channel-groups"]
	if !ok || raw == nil {
		return ""
	}
	var values []string
	switch v := raw.(type) {
	case string:
		values = strings.Split(v, ",")
	case []string:
		values = v
	case []any:
		values = make([]string, 0, len(v))
		for _, item := range v {
			values = append(values, fmt.Sprint(item))
		}
	case []byte:
		values = strings.Split(string(v), ",")
	default:
		return ""
	}
	normalized := make([]string, 0, len(values))
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		value = strings.ToLower(strings.TrimSpace(value))
		value = strings.Trim(value, "/")
		if value == "" {
			continue
		}
		if _, exists := seen[value]; exists {
			continue
		}
		seen[value] = struct{}{}
		normalized = append(normalized, value)
	}
	if len(normalized) == 0 {
		return ""
	}
	sort.Strings(normalized)
	return strings.Join(normalized, ",")
}

func weightedSelectionScope(meta map[string]any) string {
	if routeGroup := routeGroupSelectionScope(meta); routeGroup != "" {
		return "route:" + routeGroup
	}
	if allowedGroups := allowedChannelGroupsSelectionScope(meta); allowedGroups != "" {
		return "allowed:" + allowedGroups
	}
	return ""
}

// getAvailableAuths returns every candidate that can serve the request right
// now, in a stable order.
//
// It deliberately has one behaviour for all distribution modes. The previous
// implementation took an `includeAllPriorities` flag: round-robin passed true
// and treated the priority as a weight, while session-sticky and fill-first
// passed false and kept only the single highest priority tier. The same number
// therefore meant "share" in one group and "hard cutoff" in another, and an
// account with no configured priority (tier 0) was silently excluded from any
// group where somebody else had set 1.

// optimisticRetryWindow bounds how near a recovery has to be before a request is
// allowed through an empty pool. It covers a load-shed back-off without reaching
// the minute-scale waits that follow a real fault, or the half-hour ones that
// follow an auth or payment failure.
const optimisticRetryWindow = 10 * time.Second

// earliestOptimisticCandidate returns the credential worth trying while the pool
// is nominally empty, or nil when every wait is long enough to be worth honouring.
//
// Only non-quota blocks qualify: a credential parked because the provider shed
// load may well answer on the next attempt, whereas one parked on an exhausted
// quota certainly will not, and sending it upstream would waste a call and the
// caller's latency budget.
func earliestOptimisticCandidate(auths []*Auth, model string, now time.Time) *Auth {
	var best *Auth
	var bestNext time.Time
	deadline := now.Add(optimisticRetryWindow)
	for _, candidate := range auths {
		blocked, reason, next := isAuthBlockedForModel(candidate, model, now)
		if !blocked || reason != blockReasonOther {
			continue
		}
		if next.IsZero() || next.After(deadline) {
			continue
		}
		if !authParticipatesInSelection(candidate) {
			continue
		}
		if best == nil || next.Before(bestNext) || (next.Equal(bestNext) && candidate.ID < best.ID) {
			best, bestNext = candidate, next
		}
	}
	return best
}

// authBlock is why a credential cannot serve a model right now. lastErr and
// quota point at the failure state that decided it (the model's own state, one
// of the same Antigravity quota family, or the account's), so the routing
// simulator can name the cause with CooldownReasonOf instead of guessing which
// state the selector read. They are nil when the block has no failure state.
type authBlock struct {
	blocked bool
	reason  blockReason
	next    time.Time
	lastErr *Error
	quota   *QuotaState
}

func authBlockForModel(auth *Auth, model string, now time.Time) authBlock {
	if auth == nil {
		return authBlock{blocked: true, reason: blockReasonOther}
	}
	if auth.Disabled || auth.Status == StatusDisabled {
		return authBlock{blocked: true, reason: blockReasonDisabled}
	}

	// Quota exceeded is an auth-level cooldown signal. Once we know an auth is cooling down,
	// we should block *all* model requests for that auth until recovery, even if per-model
	// state hasn't been initialized yet. This prevents clients from burning extra upstream
	// requests by switching models during the same quota window.
	// Exception: Antigravity has separate quota pools for Gemini vs Claude/GPT.
	// A quota exhaustion on one pool should NOT block requests for the other pool.
	if auth.Quota.Exceeded && (!IsAntigravityAuth(auth) || isAntigravityExhaustedForModel(auth, model, now)) {
		next := auth.Quota.NextRecoverAt
		if quotaNeedsConfirmedRecovery(auth.Quota, now) || (!next.IsZero() && next.After(now)) {
			if auth.NextRetryAfter.After(now) && (next.IsZero() || auth.NextRetryAfter.Before(next)) {
				next = auth.NextRetryAfter
			}
			if !next.After(now) {
				next = time.Time{}
			}
			return authBlock{blocked: true, reason: blockReasonCooldown, next: next, lastErr: auth.LastError, quota: &auth.Quota}
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
					return authBlock{blocked: true, reason: blockReasonDisabled, lastErr: state.LastError, quota: &state.Quota}
				}
				if quotaNeedsConfirmedRecovery(state.Quota, now) {
					next := state.Quota.NextRecoverAt
					if state.NextRetryAfter.After(now) && (next.IsZero() || state.NextRetryAfter.Before(next)) {
						next = state.NextRetryAfter
					}
					if !next.After(now) {
						next = time.Time{}
					}
					return authBlock{blocked: true, reason: blockReasonCooldown, next: next, lastErr: state.LastError, quota: &state.Quota}
				}
				if state.Unavailable {
					if state.NextRetryAfter.IsZero() {
						return authBlock{}
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
							return authBlock{blocked: true, reason: blockReasonCooldown, next: next, lastErr: state.LastError, quota: &state.Quota}
						}
						return authBlock{blocked: true, reason: blockReasonOther, next: next, lastErr: state.LastError, quota: &state.Quota}
					}
				}
				return authBlock{}
			}
			// A 401 rejects the credential, so a model the account has not
			// served yet is held back with the ones it has.
			if credentialRejected(auth, now) {
				return authBlock{blocked: true, reason: blockReasonOther, next: auth.NextRetryAfter, lastErr: auth.LastError, quota: &auth.Quota}
			}
			// If this specific model has no state yet, check if other models in the same Antigravity family are in cooldown
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
						return authBlock{blocked: true, reason: blockReasonCooldown, next: next, lastErr: s.LastError, quota: &s.Quota}
					}
				}
			}
		}
		return authBlock{}
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
			return authBlock{blocked: true, reason: blockReasonCooldown, next: next, lastErr: auth.LastError, quota: &auth.Quota}
		}
		return authBlock{blocked: true, reason: blockReasonOther, next: next, lastErr: auth.LastError, quota: &auth.Quota}
	}
	return authBlock{}
}

// credentialRejected reports whether auth is held back as a whole because the
// upstream rejected its credential: every model it has state for is held back
// (see updateAggregatedAvailability) and the latest failure was a 401.
func credentialRejected(auth *Auth, now time.Time) bool {
	return auth.Unavailable && auth.NextRetryAfter.After(now) && statusCodeFromResult(auth.LastError) == 401
}

func isAntigravityExhaustedForModel(auth *Auth, model string, now time.Time) bool {
	if model == "" {
		// No specific model requested: if any model has quota left, don't block auth-wide
		hasRemaining := false
		for _, s := range auth.ModelStates {
			if s != nil && !activeModelQuotaCooldown(s, now) {
				hasRemaining = true
				break
			}
		}
		return !hasRemaining
	}
	targetFamily := ModelAntigravityQuotaFamily(model)
	// If any model in the target family has an active quota cooldown, block it
	for m, s := range auth.ModelStates {
		if s == nil || !s.Quota.Exceeded {
			continue
		}
		if ModelAntigravityQuotaFamily(m) == targetFamily && activeModelQuotaCooldown(s, now) {
			return true
		}
	}
	// Target family has no active cooldown -> not exhausted
	return false
}
