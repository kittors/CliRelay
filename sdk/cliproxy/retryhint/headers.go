package retryhint

import (
	"net/http"
	"strings"
	"time"
)

// RetryAfterHeader reads the standard Retry-After header (seconds or HTTP date).
//
// It applies no policy bound, only the requirement that the wait is positive
// and fits a time.Duration. Executors with their own cooldown policy (xAI,
// OpenAI-compatible 429s) use it for the same parse the generic path does.
func RetryAfterHeader(headers http.Header, now time.Time) (time.Duration, bool) {
	if now.IsZero() {
		now = time.Now()
	}
	raw := headerValue(headers, "Retry-After")
	if raw == "" {
		return 0, false
	}
	return parseRetryAfterValue(raw, now)
}

// HintHeaders returns the response headers Parse reads: Retry-After and its
// millisecond twin, the vendor rate-limit reset headers and the remaining
// counters they are paired with. An executor that attaches headers to a failed
// upstream call only so the scheduler can read the wait can attach this subset
// instead of the whole response, and keep cookies and per-response framing
// headers out of an error that may be forwarded to the client.
//
// It returns nil when none of the headers is present.
func HintHeaders(src http.Header) http.Header {
	var dst http.Header
	for key, values := range src {
		lower := strings.ToLower(key)
		if len(values) == 0 || !isHintHeader(lower) {
			continue
		}
		if dst == nil {
			dst = make(http.Header)
		}
		dst[key] = append([]string(nil), values...)
	}
	return dst
}

func isHintHeader(lower string) bool {
	if lower == "retry-after" || lower == "retry-after-ms" {
		return true
	}
	return isRateLimitResetHeader(lower) ||
		(strings.Contains(lower, "remaining") && isRateLimitResetHeader(strings.Replace(lower, "remaining", "reset", 1)))
}

// hintFromRetryAfterHeaders reads Retry-After and its millisecond twin.
// retry-after-ms is what Azure and the OpenAI SDKs send next to Retry-After
// (whose resolution is whole seconds), so the more precise one wins.
func hintFromRetryAfterHeaders(headers http.Header, now time.Time) (Hint, bool) {
	if len(headers) == 0 {
		return Hint{}, false
	}
	if raw := headerValue(headers, "Retry-After-Ms"); raw != "" {
		if millis, ok := parseNumber(raw); ok {
			if d, ok := millisToDuration(millis); ok && withinBounds(d) {
				return Hint{Delay: d, Source: SourceRetryAfterMillis}, true
			}
		}
	}
	if raw := headerValue(headers, "Retry-After"); raw != "" {
		if d, ok := parseRetryAfterValue(raw, now); ok && withinBounds(d) {
			return Hint{Delay: d, Source: SourceRetryAfter}, true
		}
	}
	return Hint{}, false
}

// hintFromRateLimitHeaders reads the vendor rate-limit reset headers of a 429:
// X-RateLimit-Reset[-Requests|-Tokens], the IETF RateLimit-Reset and
// Anthropic's anthropic-ratelimit-*-reset.
//
// A response describes several windows (requests, tokens, ...) and only the
// one that ran out caused the 429. Each reset header is therefore paired with
// its remaining header: a window that still has room is not the reason and is
// skipped. When a response does not say how much is left, the window counts. If
// several windows did run out, the longest wait is used, because coming back
// when only one of them has reset would draw another 429.
func hintFromRateLimitHeaders(headers http.Header, now time.Time) (Hint, bool) {
	var (
		best    time.Duration
		bestAt  time.Time
		matched bool
	)
	for key, values := range headers {
		lower := strings.ToLower(key)
		if len(values) == 0 || !isRateLimitResetHeader(lower) {
			continue
		}
		if windowHasRoom(headers, lower) {
			continue
		}
		delay, resetAt, ok := parseResetValue(values[0], now)
		if !ok || !withinBounds(delay) {
			continue
		}
		if !matched || delay > best {
			best, bestAt, matched = delay, resetAt, true
		}
	}
	if !matched {
		return Hint{}, false
	}
	return Hint{Delay: best, ResetAt: bestAt, Source: SourceRateLimitReset}, true
}

func isRateLimitResetHeader(lower string) bool {
	switch {
	case strings.HasPrefix(lower, "x-ratelimit-reset"),
		strings.HasPrefix(lower, "x-rate-limit-reset"),
		strings.HasPrefix(lower, "ratelimit-reset"):
		return true
	}
	// Anthropic's four classic windows. The "unified" subscription headers are
	// deliberately not matched: the Claude OAuth path owns those and reads them
	// with their own thresholds.
	switch lower {
	case "anthropic-ratelimit-requests-reset",
		"anthropic-ratelimit-tokens-reset",
		"anthropic-ratelimit-input-tokens-reset",
		"anthropic-ratelimit-output-tokens-reset":
		return true
	}
	return false
}

// windowHasRoom reports whether the response says the window behind a reset
// header still has capacity. It answers false (so the window counts) when the
// remaining header is missing or unreadable.
func windowHasRoom(headers http.Header, resetKey string) bool {
	remainingKey := strings.Replace(resetKey, "reset", "remaining", 1)
	raw := headerValue(headers, remainingKey)
	if raw == "" {
		return false
	}
	remaining, ok := parseNumber(raw)
	return ok && remaining > 0
}

// headerValue returns the first value of a header. It falls back to a
// case-insensitive scan because executors and tests sometimes build a
// http.Header literal whose keys were never canonicalized, which Get misses.
func headerValue(headers http.Header, name string) string {
	if len(headers) == 0 {
		return ""
	}
	if value := strings.TrimSpace(headers.Get(name)); value != "" {
		return value
	}
	for key, values := range headers {
		if len(values) > 0 && strings.EqualFold(key, name) {
			if value := strings.TrimSpace(values[0]); value != "" {
				return value
			}
		}
	}
	return ""
}
