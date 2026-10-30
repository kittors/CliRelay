package authfiles

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

// BuildRestrictionPayload returns the public restriction payload for the
// management auth-file response. It summarizes auth-level and model-level
// unavailability, retry, error, and quota states.
func BuildRestrictionPayload(auth *coreauth.Auth, now time.Time) []map[string]any {
	if auth == nil {
		return nil
	}
	restrictions := make([]map[string]any, 0)
	if len(auth.ModelStates) == 0 || auth.Unavailable || auth.Quota.Exceeded || auth.NextRetryAfter.After(now) {
		if restriction := buildRestrictionEntry(
			"auth",
			"",
			auth.Status,
			auth.StatusMessage,
			auth.Unavailable,
			auth.NextRetryAfter,
			auth.LastError,
			auth.Quota,
			now,
		); restriction != nil {
			restrictions = append(restrictions, restriction)
		}
	}
	if len(auth.ModelStates) == 0 {
		return DeduplicateRestrictionEntries(restrictions)
	}

	models := make([]string, 0, len(auth.ModelStates))
	for model := range auth.ModelStates {
		models = append(models, model)
	}
	sort.Strings(models)
	for _, model := range models {
		state := auth.ModelStates[model]
		if state == nil {
			continue
		}
		if restriction := buildRestrictionEntry(
			"model",
			model,
			state.Status,
			state.StatusMessage,
			state.Unavailable,
			state.NextRetryAfter,
			state.LastError,
			state.Quota,
			now,
		); restriction != nil {
			restrictions = append(restrictions, restriction)
		}
	}
	return DeduplicateRestrictionEntries(restrictions)
}

// DeduplicateRestrictionEntries collapses duplicate auth/model restriction
// entries that share the same actionable surface. Auth scope wins over model
// scope so the management UI can show one readable summary for repeated errors.
func DeduplicateRestrictionEntries(entries []map[string]any) []map[string]any {
	if len(entries) <= 1 {
		return entries
	}

	type picked struct {
		entry map[string]any
		score int
	}

	bestByKey := make(map[string]picked, len(entries))
	order := make([]string, 0, len(entries))

	for _, entry := range entries {
		key := restrictionDedupeKey(entry)
		if key == "" {
			// Shouldn't happen, but keep it stable-ish.
			key = fmt.Sprintf("unknown:%d", len(order))
		}
		score := restrictionEntryScore(entry)
		if existing, ok := bestByKey[key]; ok {
			if score > existing.score {
				bestByKey[key] = picked{entry: entry, score: score}
			}
			continue
		}
		bestByKey[key] = picked{entry: entry, score: score}
		order = append(order, key)
	}

	out := make([]map[string]any, 0, len(order))
	for _, key := range order {
		out = append(out, bestByKey[key].entry)
	}
	return out
}

func restrictionEntryScore(entry map[string]any) int {
	scope, _ := entry["scope"].(string)
	if scope == "auth" {
		return 2
	}
	if scope == "model" {
		return 1
	}
	return 0
}

func restrictionDedupeKey(entry map[string]any) string {
	if entry == nil {
		return ""
	}

	status := fmt.Sprint(entry["status"])

	httpStatus := coerceInt(entry["http_status"])
	code, _ := entry["code"].(string)
	reason, _ := entry["reason"].(string)
	quotaExceeded := coerceBool(entry["quota_exceeded"])

	// Intentionally ignore "scope", "model", and "unavailable" to keep the list UI readable.
	// If a user hits a 429, seeing it once is enough even if it applies to multiple models.
	if httpStatus > 0 {
		return fmt.Sprintf("http=%d|quota=%t|reason=%s", httpStatus, quotaExceeded, strings.TrimSpace(reason))
	}
	return fmt.Sprintf("status=%s|code=%s|quota=%t|reason=%s",
		status,
		strings.TrimSpace(code),
		quotaExceeded,
		strings.TrimSpace(reason),
	)
}

func coerceInt(v any) int {
	switch val := v.(type) {
	case int:
		return val
	case int64:
		return int(val)
	case float64:
		return int(val)
	case json.Number:
		if i, err := val.Int64(); err == nil {
			return int(i)
		}
	}
	return 0
}

func coerceBool(v any) bool {
	switch val := v.(type) {
	case bool:
		return val
	case string:
		s := strings.TrimSpace(strings.ToLower(val))
		return s == "true" || s == "1" || s == "yes"
	case int:
		return val != 0
	case int64:
		return val != 0
	case float64:
		return val != 0
	}
	return false
}

func buildRestrictionEntry(scope, model string, status coreauth.Status, statusMessage string, unavailable bool, nextRetryAfter time.Time, lastError *coreauth.Error, quota coreauth.QuotaState, now time.Time) map[string]any {
	if !isActiveRestriction(status, unavailable, nextRetryAfter, lastError, quota, now) {
		return nil
	}
	entry := map[string]any{
		"scope":       scope,
		"status":      status,
		"unavailable": unavailable,
	}
	if model != "" {
		entry["model"] = model
	}
	statusMessage = strings.TrimSpace(statusMessage)
	if statusMessage == "" && lastError != nil {
		statusMessage = strings.TrimSpace(lastError.Message)
	}
	if statusMessage != "" {
		entry["status_message"] = statusMessage
	}
	if !nextRetryAfter.IsZero() && nextRetryAfter.After(now) {
		entry["next_retry_after"] = nextRetryAfter
	}
	if lastError != nil {
		if lastError.Code != "" {
			entry["code"] = lastError.Code
		}
		if lastError.HTTPStatus > 0 {
			entry["http_status"] = lastError.HTTPStatus
		}
		if lastError.Retryable {
			entry["retryable"] = true
		}
	}
	// Why the account or model is held back, as a stable code the panel words
	// itself. Only a restriction that is holding something back has one: an error
	// with no time to wait is shown, but nothing is cooling down.
	if reason := restrictionCooldownReason(nextRetryAfter, lastError, quota, now); reason != "" {
		entry["cooldown_reason"] = reason
	}
	if quota.Exceeded {
		entry["quota_exceeded"] = true
		if quota.Reason != "" {
			entry["reason"] = quota.Reason
		}
		if quota.Window != "" {
			entry["quota_window"] = quota.Window
		}
		if quota.WindowMinutes > 0 {
			entry["quota_window_minutes"] = quota.WindowMinutes
		}
		if !quota.NextRecoverAt.IsZero() && quota.NextRecoverAt.After(now) {
			entry["next_recover_at"] = quota.NextRecoverAt
		}
	}
	return entry
}

func isActiveRestriction(status coreauth.Status, unavailable bool, nextRetryAfter time.Time, lastError *coreauth.Error, quota coreauth.QuotaState, now time.Time) bool {
	hasErrorState := status == coreauth.StatusError || unavailable || lastError != nil
	hasActiveRetry := !nextRetryAfter.IsZero() && nextRetryAfter.After(now)
	hasActiveQuota := quota.Exceeded && (quota.NextRecoverAt.IsZero() || quota.NextRecoverAt.After(now))
	return hasErrorState || hasActiveRetry || hasActiveQuota
}

func restrictionCooldownReason(nextRetryAfter time.Time, lastError *coreauth.Error, quota coreauth.QuotaState, now time.Time) string {
	cooling := nextRetryAfter.After(now) ||
		(quota.Exceeded && (quota.NextRecoverAt.After(now) || quota.RecoveryRequired))
	if !cooling {
		return ""
	}
	return coreauth.CooldownReasonOf(lastError, quota)
}

// BuildModelCooldownPayload lists the models held back on an account, one entry
// each. The restriction list above collapses repeats of the same error into one
// line; a per-model action (retry this model now) needs every model.
func BuildModelCooldownPayload(auth *coreauth.Auth, now time.Time) []map[string]any {
	cooldowns := coreauth.ActiveModelCooldowns(auth, now)
	if len(cooldowns) == 0 {
		return nil
	}
	out := make([]map[string]any, 0, len(cooldowns))
	for _, cooldown := range cooldowns {
		entry := map[string]any{
			"model":          cooldown.Model,
			"quota_exceeded": cooldown.QuotaExceeded,
		}
		if cooldown.Reason != "" {
			entry["reason"] = cooldown.Reason
		}
		if !cooldown.Until.IsZero() {
			entry["until"] = cooldown.Until
		}
		if cooldown.HTTPStatus > 0 {
			entry["http_status"] = cooldown.HTTPStatus
		}
		out = append(out, entry)
	}
	return out
}
