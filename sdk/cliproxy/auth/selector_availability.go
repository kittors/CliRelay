package auth

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

func canonicalModelKey(model string) string {
	model = strings.TrimSpace(model)
	if model == "" {
		return ""
	}
	parsed := parseModelSuffix(model)
	modelName := strings.TrimSpace(parsed.ModelName)
	if modelName == "" {
		return model
	}
	return modelName
}

func authWebsocketsEnabled(auth *Auth) bool {
	if auth == nil {
		return false
	}
	if len(auth.Attributes) > 0 {
		if raw := strings.TrimSpace(auth.Attributes["websockets"]); raw != "" {
			parsed, errParse := strconv.ParseBool(raw)
			if errParse == nil {
				return parsed
			}
		}
	}
	if len(auth.Metadata) == 0 {
		return false
	}
	raw, ok := auth.Metadata["websockets"]
	if !ok || raw == nil {
		return false
	}
	switch v := raw.(type) {
	case bool:
		return v
	case string:
		parsed, errParse := strconv.ParseBool(strings.TrimSpace(v))
		if errParse == nil {
			return parsed
		}
	default:
	}
	return false
}

func preferCodexWebsocketAuths(ctx context.Context, provider string, available []*Auth) []*Auth {
	if len(available) == 0 {
		return available
	}
	if !cliproxyexecutor.DownstreamWebsocket(ctx) {
		return available
	}
	if !strings.EqualFold(strings.TrimSpace(provider), "codex") {
		return available
	}

	wsEnabled := make([]*Auth, 0, len(available))
	for i := 0; i < len(available); i++ {
		candidate := available[i]
		if authWebsocketsEnabled(candidate) {
			wsEnabled = append(wsEnabled, candidate)
		}
	}
	if len(wsEnabled) > 0 {
		return wsEnabled
	}
	return available
}

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
func getAvailableAuths(auths []*Auth, provider, model string, now time.Time) ([]*Auth, error) {
	if len(auths) == 0 {
		return nil, &Error{Code: "auth_not_found", Message: "no auth candidates"}
	}

	available, cooldownCount, earliest, temporaryCount, temporaryEarliest := collectAvailableAuths(auths, model, now)
	if len(available) == 0 {
		if cooldownCount == len(auths) && !earliest.IsZero() {
			providerForError := provider
			if providerForError == "mixed" {
				providerForError = ""
			}
			resetIn := earliest.Sub(now)
			if resetIn < 0 {
				resetIn = 0
			}
			return nil, newModelCooldownError(model, providerForError, resetIn)
		}
		if temporaryCount == len(auths) && !temporaryEarliest.IsZero() {
			providerForError := provider
			if providerForError == "mixed" {
				providerForError = ""
			}
			resetIn := temporaryEarliest.Sub(now)
			if resetIn < 0 {
				resetIn = 0
			}
			return nil, newModelUnavailableError(model, providerForError, resetIn)
		}
		return nil, &Error{Code: "auth_unavailable", Message: "no auth available"}
	}

	scheduled := make([]*Auth, 0, len(available))
	for _, candidate := range available {
		if authParticipatesInSelection(candidate) {
			scheduled = append(scheduled, candidate)
		}
	}
	if len(scheduled) == 0 {
		// Healthy accounts exist but every one of them is weighted 0. Saying
		// "no auth available" here would send the operator hunting for a quota
		// or auth problem that does not exist.
		return nil, &Error{Code: "auth_excluded_by_weight", Message: "every candidate in this routing scope is configured with weight 0"}
	}

	sort.Slice(scheduled, func(i, j int) bool { return scheduled[i].ID < scheduled[j].ID })
	return scheduled, nil
}

func isAuthBlockedForModel(auth *Auth, model string, now time.Time) (bool, blockReason, time.Time) {
	if auth == nil {
		return true, blockReasonOther, time.Time{}
	}
	if auth.Disabled || auth.Status == StatusDisabled {
		return true, blockReasonDisabled, time.Time{}
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
			return true, blockReasonCooldown, next
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
					return true, blockReasonDisabled, time.Time{}
				}
				if quotaNeedsConfirmedRecovery(state.Quota, now) {
					next := state.Quota.NextRecoverAt
					if state.NextRetryAfter.After(now) && (next.IsZero() || state.NextRetryAfter.Before(next)) {
						next = state.NextRetryAfter
					}
					if !next.After(now) {
						next = time.Time{}
					}
					return true, blockReasonCooldown, next
				}
				if state.Unavailable {
					if state.NextRetryAfter.IsZero() {
						return false, blockReasonNone, time.Time{}
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
							return true, blockReasonCooldown, next
						}
						return true, blockReasonOther, next
					}
				}
				return false, blockReasonNone, time.Time{}
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
						return true, blockReasonCooldown, next
					}
				}
			}
		}
		return false, blockReasonNone, time.Time{}
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
			return true, blockReasonCooldown, next
		}
		return true, blockReasonOther, next
	}
	return false, blockReasonNone, time.Time{}
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
