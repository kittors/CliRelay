package auth

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const (
	ClaudeOAuthHealthMetadataKey = "claude_oauth_health"

	claudeOAuthHealthStatusActive         = "active"
	claudeOAuthHealthStatusRefreshPending = "refresh_pending"
	claudeOAuthHealthStatusRateLimited    = "rate_limited"
	claudeOAuthHealthStatusExhausted      = "exhausted"

	claudeOAuthReasonOAuth401        = "oauth_401"
	claudeOAuthReasonAnthropic5H     = "anthropic_5h_window_exhausted"
	claudeOAuthReasonAnthropic7D     = "anthropic_7d_window_exhausted"
	claudeOAuthRefreshPendingMessage = "claude_oauth_refresh_pending"

	claudeOAuth401Cooldown = 10 * time.Minute
)

type claudeOAuthRateLimitSpec struct {
	headerKey  string
	payloadKey string
	window     string
	minutes    int
	maxFuture  time.Duration
	reason     string
}

type claudeOAuthWindowDecision struct {
	window  string
	minutes int
	reason  string
	resetAt time.Time
}

var claudeOAuthRateLimitSpecs = []claudeOAuthRateLimitSpec{
	{
		headerKey:  "5h",
		payloadKey: "five_hour",
		window:     "5h",
		minutes:    300,
		maxFuture:  6 * time.Hour,
		reason:     claudeOAuthReasonAnthropic5H,
	},
	{
		headerKey:  "7d",
		payloadKey: "seven_day",
		window:     "7d",
		minutes:    7 * 24 * 60,
		maxFuture:  8 * 24 * time.Hour,
		reason:     claudeOAuthReasonAnthropic7D,
	},
}

func isClaudeOAuthAuth(auth *Auth) bool {
	if auth == nil || !strings.EqualFold(strings.TrimSpace(auth.Provider), "claude") {
		return false
	}
	if authRefreshToken(auth) != "" || authTokenValue(auth, "access_token", "accessToken") != "" {
		return true
	}
	if auth.Metadata != nil {
		if typ, _ := auth.Metadata["type"].(string); strings.EqualFold(strings.TrimSpace(typ), "claude") {
			return true
		}
	}
	accountType, _ := auth.AccountInfo()
	return strings.EqualFold(accountType, "oauth")
}

func claudeOAuthRefreshPending(auth *Auth) bool {
	if !isClaudeOAuthAuth(auth) || authRefreshToken(auth) == "" {
		return false
	}
	health := cloneClaudeOAuthHealth(auth)
	if len(health) == 0 {
		return false
	}
	status, _ := health["status"].(string)
	reason, _ := health["temporary_unschedulable_reason"].(string)
	return strings.EqualFold(strings.TrimSpace(status), claudeOAuthHealthStatusRefreshPending) ||
		strings.EqualFold(strings.TrimSpace(reason), claudeOAuthReasonOAuth401)
}

func markClaudeOAuthHealthSuccessLocked(auth *Auth, result Result, now time.Time) {
	if !isClaudeOAuthAuth(auth) {
		return
	}
	health := ensureClaudeOAuthHealth(auth, now)
	health["status"] = claudeOAuthHealthStatusActive
	health["last_runtime_at"] = formatClaudeOAuthHealthTime(now)
	delete(health, "temporary_unschedulable_until")
	delete(health, "temporary_unschedulable_reason")
	mergeClaudeOAuthWindowHealth(health, result.Headers, now)
	auth.Metadata[ClaudeOAuthHealthMetadataKey] = health
}

func markClaudeOAuthHealthRefreshSuccessLocked(auth *Auth, now time.Time) {
	if !isClaudeOAuthAuth(auth) {
		return
	}
	health := ensureClaudeOAuthHealth(auth, now)
	health["status"] = claudeOAuthHealthStatusActive
	health["last_refresh_at"] = formatClaudeOAuthHealthTime(now)
	delete(health, "temporary_unschedulable_until")
	delete(health, "temporary_unschedulable_reason")
	auth.Metadata[ClaudeOAuthHealthMetadataKey] = health
}

func applyClaudeOAuthFailureLocked(auth *Auth, result Result, now time.Time, effects *resultStateEffects) bool {
	if !isClaudeOAuthAuth(auth) {
		return false
	}
	statusCode := statusCodeFromResult(result.Error)
	if statusCode != http.StatusUnauthorized && statusCode != http.StatusTooManyRequests {
		return false
	}

	health := ensureClaudeOAuthHealth(auth, now)
	health["last_runtime_status"] = statusCode
	health["last_runtime_at"] = formatClaudeOAuthHealthTime(now)
	decision := mergeClaudeOAuthWindowHealth(health, result.Headers, now)

	switch statusCode {
	case http.StatusUnauthorized:
		health["last_401_at"] = formatClaudeOAuthHealthTime(now)
		if result.Error != nil && strings.TrimSpace(result.Error.Message) != "" {
			health["last_401_message"] = trimClaudeOAuthHealthMessage(result.Error.Message)
		}
		if authRefreshToken(auth) == "" {
			health["status"] = "unauthorized"
			health["refresh_available"] = false
			auth.Metadata[ClaudeOAuthHealthMetadataKey] = health
			return false
		}
		next := now.Add(claudeOAuth401Cooldown)
		health["status"] = claudeOAuthHealthStatusRefreshPending
		health["temporary_unschedulable_until"] = formatClaudeOAuthHealthTime(next)
		health["temporary_unschedulable_reason"] = claudeOAuthReasonOAuth401
		auth.Metadata[ClaudeOAuthHealthMetadataKey] = health
		applyClaudeOAuthTemporaryFailureLocked(auth, result, now, next, claudeOAuthRefreshPendingMessage, claudeOAuthReasonOAuth401, effects)
		auth.NextRefreshAfter = time.Time{}
		return true
	case http.StatusTooManyRequests:
		if decision == nil {
			health["status"] = claudeOAuthHealthStatusRateLimited
			auth.Metadata[ClaudeOAuthHealthMetadataKey] = health
			return false
		}
		health["status"] = claudeOAuthHealthStatusExhausted
		health["temporary_unschedulable_until"] = formatClaudeOAuthHealthTime(decision.resetAt)
		health["temporary_unschedulable_reason"] = decision.reason
		auth.Metadata[ClaudeOAuthHealthMetadataKey] = health
		applyClaudeOAuthQuotaFailureLocked(auth, result, now, *decision, effects)
		return true
	default:
		return false
	}
}

func applyClaudeOAuthTemporaryFailureLocked(auth *Auth, result Result, now time.Time, next time.Time, message string, reason string, effects *resultStateEffects) {
	if result.Model != "" {
		state := ensureModelState(auth, result.Model)
		state.Unavailable = true
		state.Status = StatusActive
		state.StatusMessage = message
		state.NextRetryAfter = next
		state.LastError = cloneError(result.Error)
		state.Quota = QuotaState{}
		state.UpdatedAt = now
		if effects != nil {
			effects.suspendReason = reason
			effects.shouldSuspendModel = true
		}
		auth.LastError = cloneError(result.Error)
		auth.StatusMessage = message
		auth.Status = StatusActive
		auth.UpdatedAt = now
		updateAggregatedAvailability(auth, now)
		return
	}
	auth.Unavailable = true
	auth.Status = StatusActive
	auth.StatusMessage = message
	auth.NextRetryAfter = next
	auth.LastError = cloneError(result.Error)
	auth.UpdatedAt = now
}

func applyClaudeOAuthQuotaFailureLocked(auth *Auth, result Result, now time.Time, decision claudeOAuthWindowDecision, effects *resultStateEffects) {
	if result.Model != "" {
		state := ensureModelState(auth, result.Model)
		state.Unavailable = true
		state.Status = StatusError
		state.StatusMessage = decision.reason
		state.NextRetryAfter = decision.resetAt
		state.LastError = cloneError(result.Error)
		state.Quota = QuotaState{
			Exceeded:      true,
			Reason:        "quota",
			Window:        decision.window,
			WindowMinutes: decision.minutes,
			NextRecoverAt: decision.resetAt,
			BackoffLevel:  state.Quota.BackoffLevel,
		}
		state.UpdatedAt = now
		auth.LastError = cloneError(result.Error)
		auth.StatusMessage = decision.reason
		auth.Status = StatusError
		auth.UpdatedAt = now
		updateAggregatedAvailability(auth, now)
		if effects != nil {
			effects.suspendReason = decision.reason
			effects.shouldSuspendModel = true
			effects.setModelQuota = true
		}
		return
	}
	auth.Unavailable = true
	auth.Status = StatusError
	auth.StatusMessage = decision.reason
	auth.NextRetryAfter = decision.resetAt
	auth.LastError = cloneError(result.Error)
	auth.Quota = QuotaState{
		Exceeded:      true,
		Reason:        "quota",
		Window:        decision.window,
		WindowMinutes: decision.minutes,
		NextRecoverAt: decision.resetAt,
		BackoffLevel:  auth.Quota.BackoffLevel,
	}
	auth.UpdatedAt = now
}

func ensureClaudeOAuthHealth(auth *Auth, now time.Time) map[string]any {
	if auth.Metadata == nil {
		auth.Metadata = make(map[string]any)
	}
	health := cloneClaudeOAuthHealth(auth)
	health["enabled"] = true
	health["updated_at"] = formatClaudeOAuthHealthTime(now)
	health["refresh_available"] = authRefreshToken(auth) != ""
	health["runtime_profile"] = claudeOAuthRuntimeProfile(auth)
	return health
}

func cloneClaudeOAuthHealth(auth *Auth) map[string]any {
	if auth == nil || auth.Metadata == nil {
		return map[string]any{}
	}
	return cloneAnyMap(auth.Metadata[ClaudeOAuthHealthMetadataKey])
}

func mergeClaudeOAuthWindowHealth(health map[string]any, headers http.Header, now time.Time) *claudeOAuthWindowDecision {
	windows, decision := parseClaudeOAuthRateLimitHeaders(headers, now)
	if len(windows) == 0 {
		return decision
	}
	existing := cloneAnyMap(health["windows"])
	for key, value := range windows {
		existing[key] = value
	}
	health["windows"] = existing
	return decision
}

func parseClaudeOAuthRateLimitHeaders(headers http.Header, now time.Time) (map[string]any, *claudeOAuthWindowDecision) {
	if len(headers) == 0 {
		return nil, nil
	}
	windows := make(map[string]any)
	var decision *claudeOAuthWindowDecision
	for _, spec := range claudeOAuthRateLimitSpecs {
		window, ok, exceeded, resetAt := parseClaudeOAuthWindow(headers, spec, now)
		if !ok {
			continue
		}
		windows[spec.payloadKey] = window
		if !exceeded || resetAt.IsZero() {
			continue
		}
		current := claudeOAuthWindowDecision{
			window:  spec.window,
			minutes: spec.minutes,
			reason:  spec.reason,
			resetAt: resetAt,
		}
		if decision == nil || spec.window == "7d" {
			decision = &current
		}
	}
	if len(windows) == 0 {
		return nil, decision
	}
	return windows, decision
}

func parseClaudeOAuthWindow(headers http.Header, spec claudeOAuthRateLimitSpec, now time.Time) (map[string]any, bool, bool, time.Time) {
	prefix := "anthropic-ratelimit-unified-" + spec.headerKey + "-"
	status := strings.ToLower(strings.TrimSpace(headers.Get(prefix + "status")))
	resetRaw := strings.TrimSpace(headers.Get(prefix + "reset"))
	utilRaw := strings.TrimSpace(headers.Get(prefix + "utilization"))
	surpassedRaw := strings.TrimSpace(headers.Get(prefix + "surpassed-threshold"))
	if status == "" && resetRaw == "" && utilRaw == "" && surpassedRaw == "" {
		return nil, false, false, time.Time{}
	}

	resetAt, resetValid := parseClaudeOAuthReset(resetRaw, now, spec.maxFuture)
	utilization, hasUtilization := parseClaudeOAuthUtilization(utilRaw)
	surpassed, hasSurpassed := parseClaudeOAuthBool(surpassedRaw)
	exceeded := status == "rejected" || (hasUtilization && utilization >= 1.0) || (hasSurpassed && surpassed)

	if status == "" {
		status = "unknown"
	}
	out := map[string]any{
		"status":     status,
		"exceeded":   exceeded,
		"updated_at": formatClaudeOAuthHealthTime(now),
	}
	if resetValid {
		out["reset_at"] = formatClaudeOAuthHealthTime(resetAt)
	}
	if hasUtilization {
		out["utilization"] = utilization
	}
	if hasSurpassed {
		out["surpassed_threshold"] = surpassed
	}
	return out, true, exceeded, resetAt
}

func parseClaudeOAuthReset(raw string, now time.Time, maxFuture time.Duration) (time.Time, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return time.Time{}, false
	}
	parsed, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || parsed <= 0 {
		return time.Time{}, false
	}
	if parsed > 100_000_000_000 {
		parsed = parsed / 1000
	}
	resetAt := time.Unix(parsed, 0).UTC()
	if !resetAt.After(now) {
		return time.Time{}, false
	}
	if maxFuture > 0 && resetAt.Sub(now) > maxFuture {
		return time.Time{}, false
	}
	return resetAt, true
}

func parseClaudeOAuthUtilization(raw string) (float64, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, false
	}
	parsed, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		return 0, false
	}
	return parsed, true
}

func parseClaudeOAuthBool(raw string) (bool, bool) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "true", "1", "yes":
		return true, true
	case "false", "0", "no":
		return false, true
	default:
		return false, false
	}
}

func claudeOAuthRuntimeProfile(auth *Auth) map[string]any {
	egress := "direct_or_global"
	if auth != nil {
		if strings.TrimSpace(auth.ProxyID) != "" {
			egress = "proxy_pool"
		} else if strings.TrimSpace(auth.ProxyURL) != "" {
			egress = "proxy_url"
		}
	}
	return map[string]any{
		"name":                 "claude_oauth_runtime",
		"identity_fingerprint": "claude_headers",
		"transport":            "go_http_transport",
		"egress":               egress,
	}
}

func cloneAnyMap(value any) map[string]any {
	switch typed := value.(type) {
	case map[string]any:
		out := make(map[string]any, len(typed))
		for key, val := range typed {
			out[key] = cloneAnyValue(val)
		}
		return out
	case map[string]string:
		out := make(map[string]any, len(typed))
		for key, val := range typed {
			out[key] = val
		}
		return out
	default:
		return map[string]any{}
	}
}

func cloneAnyValue(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		return cloneAnyMap(typed)
	case map[string]string:
		return cloneAnyMap(typed)
	case []any:
		out := make([]any, len(typed))
		for i, val := range typed {
			out[i] = cloneAnyValue(val)
		}
		return out
	default:
		return typed
	}
}

func formatClaudeOAuthHealthTime(ts time.Time) string {
	if ts.IsZero() {
		return ""
	}
	return ts.UTC().Format(time.RFC3339Nano)
}

func trimClaudeOAuthHealthMessage(message string) string {
	message = strings.TrimSpace(message)
	const maxLen = 512
	if len(message) <= maxLen {
		return message
	}
	return fmt.Sprintf("%s...", message[:maxLen])
}
