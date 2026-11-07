package executor

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	"github.com/tidwall/gjson"
)

func newCodexStatusErr(statusCode int, body []byte, headers ...http.Header) statusErr {
	err := statusErr{code: statusCode, msg: string(body)}
	if retryAfter := parseCodexRetryAfter(statusCode, body, time.Now()); retryAfter != nil {
		err.retryAfter = retryAfter
	}
	var header http.Header
	if len(headers) > 0 {
		header = headers[0]
	}
	if window, minutes := parseCodexQuotaWindow(statusCode, body, header); window != "" {
		err.quotaWindow = window
		err.quotaWindowMinutes = minutes
	}
	// Codex's own reset fields (parsed above) win. A failure without them still
	// may carry a plain Retry-After, which the scheduler reads from the headers.
	err.headers = failureHeaders(header)
	return err
}

func parseCodexQuotaWindow(statusCode int, errorBody []byte, header http.Header) (string, int) {
	if statusCode != http.StatusTooManyRequests || len(errorBody) == 0 || header == nil {
		return "", 0
	}
	if strings.TrimSpace(gjson.GetBytes(errorBody, "error.type").String()) != "usage_limit_reached" {
		return "", 0
	}

	bodyResetAt := gjson.GetBytes(errorBody, "error.resets_at").Int()
	if window, minutes := codexQuotaWindowFromHeaderReset(header, bodyResetAt); window != "" {
		return window, minutes
	}
	if window, minutes := codexQuotaExhaustedWindow(header, "Secondary"); window != "" {
		return window, minutes
	}
	if window, minutes := codexQuotaExhaustedWindow(header, "Primary"); window != "" {
		return window, minutes
	}
	return "", 0
}

func codexQuotaWindowFromHeaderReset(header http.Header, bodyResetAt int64) (string, int) {
	if bodyResetAt <= 0 {
		return "", 0
	}
	for _, prefix := range []string{"Primary", "Secondary"} {
		resetAt, ok := codexHeaderInt64(header, "X-Codex-"+prefix+"-Reset-At")
		if !ok || resetAt != bodyResetAt {
			continue
		}
		minutes, _ := codexHeaderInt(header, "X-Codex-"+prefix+"-Window-Minutes")
		return codexQuotaWindowLabel(minutes), minutes
	}
	return "", 0
}

func codexQuotaExhaustedWindow(header http.Header, prefix string) (string, int) {
	usedPercent, ok := codexHeaderFloat(header, "X-Codex-"+prefix+"-Used-Percent")
	if !ok || usedPercent < 100 {
		return "", 0
	}
	minutes, _ := codexHeaderInt(header, "X-Codex-"+prefix+"-Window-Minutes")
	return codexQuotaWindowLabel(minutes), minutes
}

func codexHeaderInt(header http.Header, key string) (int, bool) {
	value := strings.TrimSpace(header.Get(key))
	if value == "" {
		return 0, false
	}
	parsed, err := strconv.Atoi(value)
	if err != nil {
		return 0, false
	}
	return parsed, true
}

func codexHeaderInt64(header http.Header, key string) (int64, bool) {
	value := strings.TrimSpace(header.Get(key))
	if value == "" {
		return 0, false
	}
	parsed, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		return 0, false
	}
	return parsed, true
}

func codexHeaderFloat(header http.Header, key string) (float64, bool) {
	value := strings.TrimSpace(header.Get(key))
	if value == "" {
		return 0, false
	}
	parsed, err := strconv.ParseFloat(value, 64)
	if err != nil {
		return 0, false
	}
	return parsed, true
}

func codexQuotaWindowLabel(minutes int) string {
	switch minutes {
	case 300:
		return "5h"
	case 10080:
		return "week"
	default:
		if minutes > 0 {
			return fmt.Sprintf("%dm", minutes)
		}
		return ""
	}
}

func parseCodexRetryAfter(statusCode int, errorBody []byte, now time.Time) *time.Duration {
	if statusCode != http.StatusTooManyRequests || len(errorBody) == 0 {
		return nil
	}
	if strings.TrimSpace(gjson.GetBytes(errorBody, "error.type").String()) != "usage_limit_reached" {
		return nil
	}
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
	return nil
}

func parseCodexQuotaProbe(body []byte) *cliproxyauth.QuotaProbeResult {
	if len(body) == 0 {
		return nil
	}

	rateLimit := gjson.GetBytes(body, "rate_limit")
	if !rateLimit.Exists() {
		return nil
	}

	allowed := rateLimit.Get("allowed")
	limitReached := rateLimit.Get("limit_reached")
	if limitReached.Exists() && limitReached.Bool() {
		nextRecoverAt := codexQuotaProbeNextRecoverAt(rateLimit, true)
		if nextRecoverAt.IsZero() {
			nextRecoverAt = codexQuotaProbeNextRecoverAt(rateLimit, false)
		}
		return &cliproxyauth.QuotaProbeResult{
			Recovered:     false,
			NextRecoverAt: nextRecoverAt,
		}
	}

	hasWindowUsage := false
	hasExhaustedWindow := false
	nextRecoverAt := time.Time{}
	for _, path := range []string{"primary_window", "secondary_window"} {
		window := rateLimit.Get(path)
		if !window.Exists() {
			continue
		}
		usedPercent := window.Get("used_percent")
		windowExhausted := false
		if usedPercent.Exists() {
			hasWindowUsage = true
			windowExhausted = usedPercent.Float() >= 100
			if windowExhausted {
				hasExhaustedWindow = true
			}
		}
		if !windowExhausted {
			continue
		}
		if resetAt := codexQuotaWindowResetAt(window, time.Now()); !resetAt.IsZero() {
			if nextRecoverAt.IsZero() || resetAt.Before(nextRecoverAt) {
				nextRecoverAt = resetAt
			}
		}
	}

	if !hasExhaustedWindow {
		if allowed.Exists() {
			return &cliproxyauth.QuotaProbeResult{
				Recovered:     allowed.Bool(),
				NextRecoverAt: codexQuotaProbeNextRecoverAt(rateLimit, false),
			}
		}
		if hasWindowUsage {
			return &cliproxyauth.QuotaProbeResult{Recovered: true}
		}
	}

	return &cliproxyauth.QuotaProbeResult{
		Recovered:     false,
		NextRecoverAt: nextRecoverAt,
	}
}

func codexQuotaProbeNextRecoverAt(rateLimit gjson.Result, exhaustedOnly bool) time.Time {
	nextRecoverAt := time.Time{}
	for _, path := range []string{"primary_window", "secondary_window"} {
		window := rateLimit.Get(path)
		if !window.Exists() {
			continue
		}
		if exhaustedOnly {
			usedPercent := window.Get("used_percent")
			if usedPercent.Exists() && usedPercent.Float() < 100 {
				continue
			}
		}
		if resetAt := codexQuotaWindowResetAt(window, time.Now()); !resetAt.IsZero() {
			if nextRecoverAt.IsZero() || resetAt.Before(nextRecoverAt) {
				nextRecoverAt = resetAt
			}
		}
	}
	return nextRecoverAt
}

func codexQuotaWindowResetAt(window gjson.Result, now time.Time) time.Time {
	if !window.Exists() {
		return time.Time{}
	}
	if resetAt := window.Get("reset_at").Int(); resetAt > 0 {
		resetAtTime := time.Unix(resetAt, 0)
		if resetAtTime.After(now) {
			return resetAtTime
		}
	}
	if afterSeconds := window.Get("reset_after_seconds").Int(); afterSeconds > 0 {
		return now.Add(time.Duration(afterSeconds) * time.Second)
	}
	return time.Time{}
}
