package retryhint

import (
	"fmt"
	"net/http"
	"strconv"
	"testing"
	"time"
)

// testNow anchors every absolute time in these tests.
var testNow = time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)

func epochSeconds(offset time.Duration) string {
	return strconv.FormatInt(testNow.Add(offset).Unix(), 10)
}

func epochMillis(offset time.Duration) string {
	return strconv.FormatInt(testNow.Add(offset).UnixMilli(), 10)
}

func headers(pairs ...string) http.Header {
	h := http.Header{}
	for i := 0; i+1 < len(pairs); i += 2 {
		h.Add(pairs[i], pairs[i+1])
	}
	return h
}

func TestParseHeadersOn429(t *testing.T) {
	httpDate := func(offset time.Duration) string { return testNow.Add(offset).UTC().Format(http.TimeFormat) }
	cases := []struct {
		name   string
		header http.Header
		want   time.Duration
		source Source
	}{
		{"seconds", headers("Retry-After", "30"), 30 * time.Second, SourceRetryAfter},
		{"padded seconds", headers("Retry-After", "  45 "), 45 * time.Second, SourceRetryAfter},
		{"fractional seconds", headers("Retry-After", "1.5"), 1500 * time.Millisecond, SourceRetryAfter},
		{"http date", headers("Retry-After", httpDate(90*time.Second)), 90 * time.Second, SourceRetryAfter},
		{"go duration", headers("Retry-After", "1m30s"), 90 * time.Second, SourceRetryAfter},
		{"largest accepted wait", headers("Retry-After", strconv.Itoa(int(MaxDelay/time.Second))), MaxDelay, SourceRetryAfter},
		{"millisecond twin", headers("Retry-After-Ms", "2500"), 2500 * time.Millisecond, SourceRetryAfterMillis},
		{"millisecond twin wins over seconds", headers("Retry-After", "3", "Retry-After-Ms", "2500"), 2500 * time.Millisecond, SourceRetryAfterMillis},
		{"bad millisecond twin falls back to seconds", headers("Retry-After", "3", "Retry-After-Ms", "soon"), 3 * time.Second, SourceRetryAfter},
		{"non canonical header key", http.Header{"retry-after": {"12"}}, 12 * time.Second, SourceRetryAfter},

		{"ratelimit reset in epoch seconds", headers("X-RateLimit-Reset", epochSeconds(120*time.Second)), 120 * time.Second, SourceRateLimitReset},
		{"ratelimit reset in epoch millis", headers("X-RateLimit-Reset", epochMillis(75*time.Second)), 75 * time.Second, SourceRateLimitReset},
		{"ratelimit reset in relative seconds", headers("X-RateLimit-Reset", "45"), 45 * time.Second, SourceRateLimitReset},
		{"ietf ratelimit reset", headers("RateLimit-Reset", "9"), 9 * time.Second, SourceRateLimitReset},
		{"openai duration text", headers("X-Ratelimit-Reset-Requests", "1m30s", "X-Ratelimit-Remaining-Requests", "0"), 90 * time.Second, SourceRateLimitReset},
		{"openai milliseconds", headers("X-Ratelimit-Reset-Tokens", "250ms"), 250 * time.Millisecond, SourceRateLimitReset},
		{
			"only the exhausted window counts",
			headers(
				"X-Ratelimit-Reset-Requests", "20s", "X-Ratelimit-Remaining-Requests", "0",
				"X-Ratelimit-Reset-Tokens", "5m", "X-Ratelimit-Remaining-Tokens", "39000",
			),
			20 * time.Second, SourceRateLimitReset,
		},
		{
			"several exhausted windows use the longest wait",
			headers(
				"X-Ratelimit-Reset-Requests", "20s", "X-Ratelimit-Remaining-Requests", "0",
				"X-Ratelimit-Reset-Tokens", "2m", "X-Ratelimit-Remaining-Tokens", "0",
			),
			2 * time.Minute, SourceRateLimitReset,
		},
		{
			"unknown remaining counts as exhausted",
			headers("X-Ratelimit-Reset-Requests", "20s", "X-Ratelimit-Remaining-Requests", "n/a"),
			20 * time.Second, SourceRateLimitReset,
		},
		{
			"zero reset is skipped for the next window",
			headers("X-Ratelimit-Reset-Requests", "0s", "X-Ratelimit-Reset-Tokens", "1m"),
			time.Minute, SourceRateLimitReset,
		},
		{
			"anthropic rfc3339 reset",
			headers(
				"Anthropic-Ratelimit-Tokens-Reset", testNow.Add(40*time.Second).Format(time.RFC3339),
				"Anthropic-Ratelimit-Tokens-Remaining", "0",
			),
			40 * time.Second, SourceRateLimitReset,
		},
		{
			"anthropic window with room is not the cause",
			headers(
				"Anthropic-Ratelimit-Requests-Reset", testNow.Add(10*time.Second).Format(time.RFC3339),
				"Anthropic-Ratelimit-Requests-Remaining", "899",
				"Anthropic-Ratelimit-Tokens-Reset", testNow.Add(50*time.Second).Format(time.RFC3339),
				"Anthropic-Ratelimit-Tokens-Remaining", "0",
			),
			50 * time.Second, SourceRateLimitReset,
		},
		{
			"retry-after outranks ratelimit reset",
			headers("Retry-After", "7", "X-RateLimit-Reset", "300"),
			7 * time.Second, SourceRetryAfter,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Parse(http.StatusTooManyRequests, tc.header, nil, testNow)
			if got.Delay != tc.want || got.Source != tc.source {
				t.Fatalf("Parse() = {%v %s}, want {%v %s}", got.Delay, got.Source, tc.want, tc.source)
			}
			if !got.Valid() {
				t.Fatal("hint with a wait must be Valid()")
			}
		})
	}
}

// Anything unreadable, expired or implausible gives no hint at all, so the
// caller keeps its own schedule.
func TestParseHeadersWithoutAUsableHint(t *testing.T) {
	httpDate := func(offset time.Duration) string { return testNow.Add(offset).UTC().Format(http.TimeFormat) }
	cases := []struct {
		name   string
		header http.Header
	}{
		{"nil headers", nil},
		{"empty header", headers("Retry-After", "")},
		{"zero", headers("Retry-After", "0")},
		{"negative", headers("Retry-After", "-1")},
		{"negative fraction", headers("Retry-After", "-0.5")},
		{"not a number", headers("Retry-After", "not-a-delay")},
		{"http date in the past", headers("Retry-After", httpDate(-time.Minute))},
		{"http date now", headers("Retry-After", httpDate(0))},
		{"beyond the longest accepted wait", headers("Retry-After", strconv.Itoa(int(MaxDelay/time.Second)+1))},
		{"a unit mix-up: milliseconds sent as seconds", headers("Retry-After", "86400000")},
		{"seconds that would wrap a duration", headers("Retry-After", "9223372037")},
		{"seconds that wrap when widened", headers("Retry-After", "18446744074")},
		{"largest int64", headers("Retry-After", "9223372036854775807")},
		{"beyond int64", headers("Retry-After", "99999999999999999999")},
		{"not finite", headers("Retry-After", "Inf")},
		{"nan", headers("Retry-After", "NaN")},
		{"exponent overflow", headers("Retry-After", "1e400")},
		{"negative twin", headers("Retry-After-Ms", "-20")},
		{"epoch in the past", headers("X-RateLimit-Reset", epochSeconds(-time.Hour))},
		{"epoch absurdly far away", headers("X-RateLimit-Reset", "9999999999999")},
		{"relative seconds past the cap", headers("X-RateLimit-Reset", "900000000")},
		{"every window has room", headers("X-Ratelimit-Reset-Requests", "20s", "X-Ratelimit-Remaining-Requests", "5")},
		{"anthropic unified headers belong to the oauth path", headers("Anthropic-Ratelimit-Unified-5h-Reset", epochSeconds(time.Hour))},
		{"unrelated header", headers("X-Request-Id", "30")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Parse(http.StatusTooManyRequests, tc.header, nil, testNow); got.Valid() {
				t.Fatalf("Parse() = %+v, want no hint", got)
			}
		})
	}
}

func TestParseStatusScope(t *testing.T) {
	retryAfter := headers("Retry-After", "20")
	resetHeaders := headers("X-RateLimit-Reset", "30")
	body := []byte(`{"error":{"message":"Please try again in 5s"}}`)

	for _, status := range []int{http.StatusServiceUnavailable, 529} {
		t.Run(fmt.Sprintf("%d reads Retry-After", status), func(t *testing.T) {
			got := Parse(status, retryAfter, nil, testNow)
			if got.Delay != 20*time.Second {
				t.Fatalf("Parse() = %v, want 20s", got.Delay)
			}
		})
		t.Run(fmt.Sprintf("%d ignores rate limit headers and prose", status), func(t *testing.T) {
			if got := Parse(status, resetHeaders, body, testNow); got.Valid() {
				t.Fatalf("Parse() = %+v, want no hint", got)
			}
		})
	}
	for _, status := range []int{0, 200, 400, 401, 402, 403, 404, 408, 500, 502, 504} {
		t.Run(fmt.Sprintf("%d is out of scope", status), func(t *testing.T) {
			if got := Parse(status, retryAfter, body, testNow); got.Valid() {
				t.Fatalf("Parse(%d) = %+v, want no hint", status, got)
			}
		})
	}
}

func TestRetryAfterHeaderAppliesNoPolicyBound(t *testing.T) {
	cases := []struct {
		name  string
		value string
		want  time.Duration
		ok    bool
	}{
		{"seconds", "7", 7 * time.Second, true},
		// The generic path caps a wait at MaxDelay; this entry point is only a
		// parse, so a week-long Retry-After comes through untouched.
		{"longer than MaxDelay", "99999999", 99999999 * time.Second, true},
		{"largest representable", "9223372036", 9223372036 * time.Second, true},
		{"first unrepresentable", "9223372037", 0, false},
		{"would wrap when widened", "18446744074", 0, false},
		{"largest int64", "9223372036854775807", 0, false},
		{"zero", "0", 0, false},
		{"negative", "-1", 0, false},
		{"garbage", "not-a-delay", 0, false},
		{"empty", "", 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := RetryAfterHeader(headers("Retry-After", tc.value), testNow)
			if ok != tc.ok || got != tc.want {
				t.Fatalf("RetryAfterHeader(%q) = %v, %v; want %v, %v", tc.value, got, ok, tc.want, tc.ok)
			}
		})
	}
	if _, ok := RetryAfterHeader(nil, testNow); ok {
		t.Fatal("nil headers must give no wait")
	}
}

func TestHintHeadersKeepsOnlyWhatParseReads(t *testing.T) {
	src := headers(
		"Retry-After", "30",
		"Retry-After-Ms", "2500",
		"X-RateLimit-Reset-Requests", "20s",
		"X-RateLimit-Remaining-Requests", "0",
		"X-Ratelimit-Limit-Requests", "100",
		"RateLimit-Reset", "9",
		"Anthropic-Ratelimit-Tokens-Reset", "2026-10-30T12:01:00Z",
		"Anthropic-Ratelimit-Tokens-Remaining", "0",
		"Anthropic-Ratelimit-Unified-5h-Reset", "1",
		"Set-Cookie", "session=secret",
		"Content-Length", "123",
		"X-Request-Id", "req-1",
	)
	got := HintHeaders(src)
	want := []string{
		"Retry-After", "Retry-After-Ms", "X-Ratelimit-Reset-Requests", "X-Ratelimit-Remaining-Requests",
		"Ratelimit-Reset", "Anthropic-Ratelimit-Tokens-Reset", "Anthropic-Ratelimit-Tokens-Remaining",
	}
	if len(got) != len(want) {
		t.Fatalf("HintHeaders() kept %v, want exactly %v", got, want)
	}
	for _, name := range want {
		if got.Get(name) == "" {
			t.Fatalf("HintHeaders() dropped %s", name)
		}
	}
	// What it keeps is enough to give the same answer as the whole response.
	if a, b := Parse(http.StatusTooManyRequests, src, nil, testNow), Parse(http.StatusTooManyRequests, got, nil, testNow); a != b {
		t.Fatalf("Parse on the subset = %+v, on the whole = %+v", b, a)
	}
	if HintHeaders(headers("Set-Cookie", "x")) != nil || HintHeaders(nil) != nil {
		t.Fatal("no hint headers must give nil")
	}
	// The copy does not alias the response.
	got["Retry-After"][0] = "changed"
	if src.Get("Retry-After") != "30" {
		t.Fatal("HintHeaders must copy values")
	}
}
