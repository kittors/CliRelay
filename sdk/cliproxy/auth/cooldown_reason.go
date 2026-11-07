package auth

import (
	"net/http"
	"sort"
	"strings"
	"time"
)

// Why a model or an account is held back, as the management API reports it. The
// panel keys its wording on these codes, so they are part of the API: add new
// ones, never rename.
const (
	// CooldownReasonRateLimited is short-term throttling (429 about request rate).
	CooldownReasonRateLimited = "rate_limited"
	// CooldownReasonQuotaExhausted is an exhausted allowance that lasts until a
	// reset time or a top-up.
	CooldownReasonQuotaExhausted = "quota_exhausted"
	// CooldownReasonUnauthorized is a 401 the account may recover from by itself.
	CooldownReasonUnauthorized = "unauthorized"
	// CooldownReasonTokenRefreshing is an expired token being renewed.
	CooldownReasonTokenRefreshing = "token_refreshing"
	// CooldownReasonReauthRequired is a revoked or deactivated credential: the
	// operator has to sign the account in again.
	CooldownReasonReauthRequired = "reauth_required"
	// CooldownReasonPaymentRequired is a 402/403: billing or permission.
	CooldownReasonPaymentRequired = "payment_required"
	// CooldownReasonModelUnavailable is a 404 whose message says the model does
	// not exist or the plan does not include it.
	CooldownReasonModelUnavailable = "model_unavailable"
	// CooldownReasonNotFound is any other 404.
	CooldownReasonNotFound = "not_found"
	// CooldownReasonUpstreamError is a timeout or 5xx from the upstream.
	CooldownReasonUpstreamError = "upstream_error"
)

// CooldownReasonOf names why a model or account is held back, from the state the
// failure left behind. It reads that state instead of remembering a label, so
// every way a cooldown can start (a local failure, a peer node's notice, a
// provider-specific path) is described the same way. It returns "" when the
// state records no cause.
func CooldownReasonOf(lastErr *Error, quota QuotaState) string {
	if quota.Exceeded {
		return CooldownReasonQuotaExhausted
	}
	if lastErr == nil {
		return ""
	}
	switch status := lastErr.HTTPStatus; {
	case status == http.StatusUnauthorized:
		return unauthorizedCooldownReason(lastErr)
	case status == http.StatusPaymentRequired, status == http.StatusForbidden:
		return CooldownReasonPaymentRequired
	case status == http.StatusNotFound:
		if isModelUnavailableMessage(lastErr.Message) {
			return CooldownReasonModelUnavailable
		}
		return CooldownReasonNotFound
	case status == http.StatusTooManyRequests:
		return CooldownReasonRateLimited
	case status == http.StatusRequestTimeout, status >= 500 && status <= 599:
		return CooldownReasonUpstreamError
	}
	return ""
}

func unauthorizedCooldownReason(err *Error) string {
	code := strings.ToLower(strings.TrimSpace(err.Code))
	switch codexUnauthorizedKindOf(code) {
	case codexUnauthorizedExpired:
		return CooldownReasonTokenRefreshing
	case codexUnauthorizedReauth:
		return CooldownReasonReauthRequired
	}
	return CooldownReasonUnauthorized
}

// isModelUnavailableMessage reports whether a 404 says the model itself is the
// problem: it does not exist, or this account's plan does not include it. Only
// explicit wording counts; a bare 404 may be a sporadic routing miss and is left
// as not_found.
func isModelUnavailableMessage(message string) bool {
	lower := strings.ToLower(message)
	if strings.TrimSpace(lower) == "" {
		return false
	}
	for _, phrase := range []string{"model_not_found", "model not found", "unknown model", "unsupported model", "model is not supported"} {
		if strings.Contains(lower, phrase) {
			return true
		}
	}
	if strings.Contains(lower, "model") {
		for _, phrase := range []string{
			"does not exist", "doesn't exist", "not available", "no access", "do not have access", "does not have access",
			"not included in your plan", "not part of your plan", "not available on your plan", "not available for your",
		} {
			if strings.Contains(lower, phrase) {
				return true
			}
		}
	}
	// Chinese relays: 模型不存在, 无权使用该模型, 该模型不可用, 套餐不包含该模型.
	if strings.Contains(message, "模型") {
		for _, phrase := range []string{"不存在", "不可用", "无权", "不支持", "未开通", "不包含", "没有权限"} {
			if strings.Contains(message, phrase) {
				return true
			}
		}
	}
	return false
}

// ModelCooldown is one model that cannot be used on an account right now.
type ModelCooldown struct {
	Model string
	// Reason is one of the CooldownReason constants, "" when unknown.
	Reason string
	// Until is when the model may be tried again; zero when the state names no
	// time (a recovery gate that waits for a probe).
	Until time.Time
	// HTTPStatus is the status of the failure behind the cooldown, 0 if unknown.
	HTTPStatus int
	// QuotaExceeded marks an exhausted allowance rather than a backoff.
	QuotaExceeded bool
}

// ActiveModelCooldowns lists the models of auth that are held back at now, in
// model-name order. A model whose state records an error but no time to wait is
// not held back (the scheduler still picks it) and is not listed.
func ActiveModelCooldowns(auth *Auth, now time.Time) []ModelCooldown {
	if auth == nil || len(auth.ModelStates) == 0 {
		return nil
	}
	out := make([]ModelCooldown, 0, len(auth.ModelStates))
	for model, state := range auth.ModelStates {
		until, held := modelCooldownUntil(state, now)
		if !held {
			continue
		}
		entry := ModelCooldown{
			Model:         model,
			Reason:        CooldownReasonOf(state.LastError, state.Quota),
			Until:         until,
			QuotaExceeded: state.Quota.Exceeded,
		}
		if state.LastError != nil {
			entry.HTTPStatus = state.LastError.HTTPStatus
		}
		out = append(out, entry)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Model < out[j].Model })
	return out
}

// modelCooldownUntil mirrors how the selector reads a model state, so what the
// panel lists is exactly what routing skips.
func modelCooldownUntil(state *ModelState, now time.Time) (time.Time, bool) {
	if state == nil || state.Status == StatusDisabled {
		return time.Time{}, false
	}
	if quotaNeedsConfirmedRecovery(state.Quota, now) {
		next := state.Quota.NextRecoverAt
		if state.NextRetryAfter.After(now) && state.NextRetryAfter.Before(next) {
			next = state.NextRetryAfter
		}
		return next, true
	}
	if !state.Unavailable || !state.NextRetryAfter.After(now) {
		return time.Time{}, false
	}
	next := state.NextRetryAfter
	if state.Quota.NextRecoverAt.After(now) {
		next = state.Quota.NextRecoverAt
	}
	return next, true
}
