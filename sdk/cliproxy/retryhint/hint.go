// Package retryhint reads "when may this be tried again" hints out of a failed
// upstream response, and tells throttling apart from an exhausted allowance.
//
// Providers say it in many ways: the standard Retry-After header, vendor
// rate-limit headers, a field in the JSON error, or a sentence in the message
// (often not in English). The scheduler used to read a handful of them in a
// handful of executors, so most upstreams were retried on a fixed schedule that
// had nothing to do with what the upstream asked for: told to wait a minute, we
// came back after one to four seconds and drew more 429s.
//
// Everything here is advisory and conservative. A value that cannot be read, is
// in the past, or is implausibly far away yields no hint, and the caller then
// keeps doing what it did before. A parser failure must never change behaviour.
package retryhint

import (
	"net/http"
	"time"
)

// MaxDelay bounds the wait a hint may ask for. The longest real windows are
// monthly allowances; anything beyond that is far more likely to be a unit
// mix-up (milliseconds read as seconds, an epoch read as a duration) than an
// upstream asking to be left alone for months.
const MaxDelay = 35 * 24 * time.Hour

// statusSiteOverloaded is Anthropic's non-standard "overloaded" status.
const statusSiteOverloaded = 529

// Source names where a hint was read from. It is for logs and tests; callers
// must not branch on it.
type Source string

const (
	SourceRetryAfter       Source = "retry-after"
	SourceRetryAfterMillis Source = "retry-after-ms"
	SourceRateLimitReset   Source = "ratelimit-reset"
	SourceBodyField        Source = "body-field"
	SourceMessageText      Source = "message-text"
)

// Hint is the wait an upstream asked for.
type Hint struct {
	// Delay is how long to wait from now. Zero means there is no hint.
	Delay time.Duration
	// ResetAt is the moment the provider said the limit lifts, when it gave an
	// absolute time. It is zero for hints given as a relative wait.
	ResetAt time.Time
	// Source says which part of the response the hint came from.
	Source Source
}

// Valid reports whether the hint carries a usable wait.
func (h Hint) Valid() bool { return h.Delay > 0 }

// Parse reads the retry hint of a failed upstream response.
//
//   - 429: every source, in order of how much each is trusted: Retry-After,
//     retry-after-ms, vendor rate-limit reset headers, fields of the JSON error,
//     then a sentence in the message.
//   - 503 and 529: only Retry-After and retry-after-ms. A busy provider that
//     names a time is worth believing, but its prose and its rate-limit headers
//     (which describe request quotas, not the outage) are not.
//   - any other status: no hint.
//
// now anchors absolute times (HTTP dates, reset timestamps); a zero now means
// time.Now().
func Parse(status int, headers http.Header, body []byte, now time.Time) Hint {
	if now.IsZero() {
		now = time.Now()
	}
	switch status {
	case http.StatusTooManyRequests:
		if hint, ok := hintFromRetryAfterHeaders(headers, now); ok {
			return hint
		}
		if hint, ok := hintFromRateLimitHeaders(headers, now); ok {
			return hint
		}
		if hint, ok := hintFromBody(body, now); ok {
			return hint
		}
	case http.StatusServiceUnavailable, statusSiteOverloaded:
		if hint, ok := hintFromRetryAfterHeaders(headers, now); ok {
			return hint
		}
	}
	return Hint{}
}

// withinBounds reports whether d is a wait worth acting on.
func withinBounds(d time.Duration) bool {
	return d > 0 && d <= MaxDelay
}
