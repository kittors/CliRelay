package auth

import (
	"net/http"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/retryhint"
)

// Hints the scheduler reads for itself.
//
// An executor that understands a provider's own format (Codex's resets_at, xAI's
// weekly window, Gemini's quota windows) publishes the wait it parsed in
// Result.RetryAfter, and that value is used as before. Everything else used to
// be retried on a fixed schedule that had nothing to do with what the upstream
// asked for. withInferredRetryAfter closes that gap for every executor at once,
// at the one place all failures pass through.

const (
	// minInferredRetryAfter keeps a sub-second hint from becoming a busy loop.
	minInferredRetryAfter = time.Second
	// maxInferredRateLimitRetryAfter bounds a wait taken from a throttling
	// answer. Throttling clears within minutes; a longer figure in the response
	// is more likely an allowance reset, and one request an hour later settles
	// which it was.
	maxInferredRateLimitRetryAfter = time.Hour
	// maxInferredQuotaRetryAfter bounds a wait taken from an exhausted-allowance
	// answer. It matches the longest window the scheduler already trusts
	// (xAI's weekly one); a longer claim is re-checked after this long.
	maxInferredQuotaRetryAfter = quotaWindowMaxCooldown
	// maxInferredOverloadRetryAfter bounds a wait taken from a 408/5xx. A busy
	// provider recovers in seconds to minutes, and parking a credential for longer
	// on one answer would empty a small pool.
	maxInferredOverloadRetryAfter = 10 * time.Minute

	// statusSiteOverloaded is Anthropic's non-standard "overloaded" status.
	statusSiteOverloaded = 529
)

// withInferredRetryAfter fills in the wait of a failed result when the executor
// did not publish one, reading the upstream's own words in the response.
//
// The inferred value is marked advisory: it never overrides disable-cooling (see
// Result.RetryAfterAdvisory). Anything unreadable leaves the result untouched,
// so a hint the parser does not understand costs nothing.
func withInferredRetryAfter(result Result, now time.Time) Result {
	if result.Success || result.RetryAfter != nil || result.Error == nil {
		return result
	}
	status := result.Error.StatusCode()
	hint := retryhint.Parse(status, result.Headers, []byte(result.Error.Message), now)
	if !hint.Valid() {
		return result
	}
	delay := boundInferredRetryAfter(status, result.Error.Message, hint.Delay)
	result.RetryAfter = &delay
	result.RetryAfterAdvisory = true
	return result
}

func boundInferredRetryAfter(status int, message string, delay time.Duration) time.Duration {
	ceiling := maxInferredOverloadRetryAfter
	if status == http.StatusTooManyRequests {
		ceiling = maxInferredQuotaRetryAfter
		if retryhint.ClassifyMessage(message) == retryhint.KindRateLimit {
			ceiling = maxInferredRateLimitRetryAfter
		}
	}
	return min(max(delay, minInferredRetryAfter), ceiling)
}

// upstreamFailureCooldown is the wait a 408/5xx/529 result asks for, bounded.
// These statuses never used the hint before, so any value present (inferred or
// published) is bounded here rather than at the source.
func upstreamFailureCooldown(result Result) (time.Duration, bool) {
	if result.RetryAfter == nil || *result.RetryAfter <= 0 {
		return 0, false
	}
	return min(max(*result.RetryAfter, minInferredRetryAfter), maxInferredOverloadRetryAfter), true
}

// dropAdvisoryRetryAfter removes a hint the scheduler inferred when cooling is
// off for this account. Disable-cooling is an operator override that asks for
// retries at once; only a wait an executor published itself (an allowance that
// really comes back at a stated time) survives it.
func dropAdvisoryRetryAfter(auth *Auth, result Result) Result {
	if !result.RetryAfterAdvisory || !quotaCooldownDisabledForAuth(auth) {
		return result
	}
	result.RetryAfter = nil
	result.RetryAfterAdvisory = false
	return result
}

// isProxyFailure reports whether a 5xx was produced by the outbound proxy rather
// than by the upstream (see retryhint.FromProxy).
func isProxyFailure(result Result) bool {
	return retryhint.FromProxy(result.Headers)
}
