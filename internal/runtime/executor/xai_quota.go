package executor

import (
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/tidwall/gjson"
)

const (
	// xAI SuperGrok weekly included usage window label/metadata only.
	// Do not treat this length as remaining cooldown when reset is unknown.
	xaiWeeklyWindowMinutes = 10080
	xaiWeeklyWindowLabel   = "week"
)

func newXAIStatusErr(statusCode int, body []byte, headers ...http.Header) statusErr {
	err := statusErr{
		code:         statusCode,
		msg:          string(body),
		upstreamBody: append([]byte(nil), body...),
	}
	var header http.Header
	if len(headers) > 0 && headers[0] != nil {
		header = headers[0].Clone()
		err.headers = header
	}
	if window, minutes := parseXAIQuotaWindow(statusCode, body); window != "" {
		err.quotaWindow = window
		err.quotaWindowMinutes = minutes
	}
	if retryAfter := parseXAIRetryAfter(statusCode, body, header, time.Now()); retryAfter != nil {
		err.retryAfter = retryAfter
	}
	return err
}

// parseXAIQuotaWindow maps known xAI balance-exhausted 402s onto the weekly
// included-usage window. Grok Build returns HTTP 402 with
// {"error":"Grok Build usage balance exhausted"} when the weekly allowance is
// spent; treating that as a generic 30m payment_required cooldown causes
// thrashing until the real weekly reset.
func parseXAIQuotaWindow(statusCode int, errorBody []byte) (string, int) {
	if statusCode != http.StatusPaymentRequired {
		return "", 0
	}
	if !isXAIUsageBalanceExhausted(errorBody) {
		return "", 0
	}
	return xaiWeeklyWindowLabel, xaiWeeklyWindowMinutes
}

func parseXAIRetryAfter(statusCode int, errorBody []byte, header http.Header, now time.Time) *time.Duration {
	if statusCode != http.StatusPaymentRequired && statusCode != http.StatusTooManyRequests {
		return nil
	}
	if retryAfter := parseHTTPRetryAfterHeader(header, now); retryAfter != nil {
		return retryAfter
	}
	if len(errorBody) > 0 {
		if resetsAt := gjson.GetBytes(errorBody, "error.resets_at").Int(); resetsAt > 0 {
			resetAtTime := time.Unix(resetsAt, 0)
			if resetAtTime.After(now) {
				retryAfter := resetAtTime.Sub(now)
				return &retryAfter
			}
		}
		if resetsInSeconds := gjson.GetBytes(errorBody, "error.resets_in_seconds").Int(); resetsInSeconds > 0 {
			retryAfter := time.Duration(resetsInSeconds) * time.Second
			return &retryAfter
		}
	}
	// No explicit reset from upstream. Leave retryAfter unset so conductor uses
	// short probe backoff instead of inventing now+7d (window length ≠ remaining).
	return nil
}

func isXAIUsageBalanceExhausted(errorBody []byte) bool {
	if len(errorBody) == 0 {
		return false
	}
	if msg := strings.TrimSpace(gjson.GetBytes(errorBody, "error").String()); isXAIUsageBalanceExhaustedText(msg) {
		return true
	}
	if msg := strings.TrimSpace(gjson.GetBytes(errorBody, "error.message").String()); isXAIUsageBalanceExhaustedText(msg) {
		return true
	}
	return isXAIUsageBalanceExhaustedText(string(errorBody))
}

func isXAIUsageBalanceExhaustedText(text string) bool {
	lower := strings.ToLower(strings.TrimSpace(text))
	if lower == "" {
		return false
	}
	return strings.Contains(lower, "usage balance exhausted") ||
		strings.Contains(lower, "balance exhausted")
}

func parseHTTPRetryAfterHeader(header http.Header, now time.Time) *time.Duration {
	if header == nil {
		return nil
	}
	raw := strings.TrimSpace(header.Get("Retry-After"))
	if raw == "" {
		return nil
	}
	if seconds, err := strconv.ParseInt(raw, 10, 64); err == nil {
		if seconds <= 0 || seconds > math.MaxInt64/int64(time.Second) {
			return nil
		}
		retryAfter := time.Duration(seconds) * time.Second
		return &retryAfter
	}
	if when, err := http.ParseTime(raw); err == nil && when.After(now) {
		retryAfter := when.Sub(now)
		return &retryAfter
	}
	return nil
}
