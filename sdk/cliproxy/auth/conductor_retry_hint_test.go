package auth

import (
	"net/http"
	"testing"
	"time"
)

func retryHeaders(kv ...string) http.Header {
	h := http.Header{}
	for i := 0; i+1 < len(kv); i += 2 {
		h.Add(kv[i], kv[i+1])
	}
	return h
}

func TestWithInferredRetryAfter(t *testing.T) {
	now := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	published := 10 * time.Second
	cases := []struct {
		name         string
		result       Result
		want         time.Duration
		wantAdvisory bool
	}{
		{
			name:   "429 reads the Retry-After header",
			result: Result{Error: &Error{HTTPStatus: 429, Message: "rate limit exceeded"}, Headers: retryHeaders("Retry-After", "120")},
			want:   120 * time.Second, wantAdvisory: true,
		},
		{
			name:   "429 reads a wait out of the message",
			result: Result{Error: &Error{HTTPStatus: 429, Message: `{"error":{"message":"Rate limit reached. Please try again in 20s."}}`}},
			want:   20 * time.Second, wantAdvisory: true,
		},
		{
			name:   "429 reads a Chinese relay's wording",
			result: Result{Error: &Error{HTTPStatus: 429, Message: "请求过于频繁，请 30 秒后再试"}},
			want:   30 * time.Second, wantAdvisory: true,
		},
		{
			name:   "a sub-second wait is raised to the floor",
			result: Result{Error: &Error{HTTPStatus: 429, Message: "rate limit"}, Headers: retryHeaders("Retry-After-Ms", "200")},
			want:   time.Second, wantAdvisory: true,
		},
		{
			name:   "throttling is believed for at most an hour",
			result: Result{Error: &Error{HTTPStatus: 429, Message: "rate limit"}, Headers: retryHeaders("Retry-After", "21600")},
			want:   time.Hour, wantAdvisory: true,
		},
		{
			name:   "an exhausted allowance is believed for at most a week",
			result: Result{Error: &Error{HTTPStatus: 429, Message: "daily quota exceeded"}, Headers: retryHeaders("Retry-After", "2592000")},
			want:   7 * 24 * time.Hour, wantAdvisory: true,
		},
		{
			name:   "an exhausted allowance keeps a long reset",
			result: Result{Error: &Error{HTTPStatus: 429, Message: "daily quota exceeded"}, Headers: retryHeaders("Retry-After", "172800")},
			want:   48 * time.Hour, wantAdvisory: true,
		},
		{
			name:   "an unrecognised 429 is bounded like an allowance",
			result: Result{Error: &Error{HTTPStatus: 429, Message: "nope"}, Headers: retryHeaders("Retry-After", "172800")},
			want:   48 * time.Hour, wantAdvisory: true,
		},
		{
			name:   "503 follows Retry-After",
			result: Result{Error: &Error{HTTPStatus: 503, Message: "unavailable"}, Headers: retryHeaders("Retry-After", "30")},
			want:   30 * time.Second, wantAdvisory: true,
		},
		{
			name:   "503 is believed for at most ten minutes",
			result: Result{Error: &Error{HTTPStatus: 503, Message: "unavailable"}, Headers: retryHeaders("Retry-After", "3600")},
			want:   10 * time.Minute, wantAdvisory: true,
		},
		{
			name:   "529 follows Retry-After",
			result: Result{Error: &Error{HTTPStatus: 529, Message: "overloaded"}, Headers: retryHeaders("Retry-After", "45")},
			want:   45 * time.Second, wantAdvisory: true,
		},
		{
			name:   "503 takes a wait from the header only",
			result: Result{Error: &Error{HTTPStatus: 503, Message: "please try again in 20s"}},
		},
		{
			name:   "500 ignores Retry-After",
			result: Result{Error: &Error{HTTPStatus: 500, Message: "boom"}, Headers: retryHeaders("Retry-After", "30")},
		},
		{
			name:   "502 ignores Retry-After",
			result: Result{Error: &Error{HTTPStatus: 502, Message: "bad gateway"}, Headers: retryHeaders("Retry-After", "30")},
		},
		{
			name:   "401 ignores Retry-After",
			result: Result{Error: &Error{HTTPStatus: 401, Message: "unauthorized"}, Headers: retryHeaders("Retry-After", "30")},
		},
		{
			name:   "a hint the executor published is left alone",
			result: Result{Error: &Error{HTTPStatus: 429, Message: "rate limit"}, Headers: retryHeaders("Retry-After", "999"), RetryAfter: &published},
			want:   published,
		},
		{
			name:   "a success is left alone",
			result: Result{Success: true, Headers: retryHeaders("Retry-After", "30")},
		},
		{
			name:   "no error is left alone",
			result: Result{Headers: retryHeaders("Retry-After", "30")},
		},
		{
			name:   "no hint, no wait",
			result: Result{Error: &Error{HTTPStatus: 429, Message: "rate limit"}},
		},
		{
			name:   "an unreadable header gives no wait",
			result: Result{Error: &Error{HTTPStatus: 429, Message: "rate limit"}, Headers: retryHeaders("Retry-After", "soon")},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := withInferredRetryAfter(tc.result, now)
			if tc.want == 0 {
				if got.RetryAfter != nil || got.RetryAfterAdvisory {
					t.Fatalf("got wait %v advisory=%v, want none", got.RetryAfter, got.RetryAfterAdvisory)
				}
				return
			}
			if got.RetryAfter == nil || *got.RetryAfter != tc.want {
				t.Fatalf("wait = %v, want %v", got.RetryAfter, tc.want)
			}
			if got.RetryAfterAdvisory != tc.wantAdvisory {
				t.Fatalf("advisory = %v, want %v", got.RetryAfterAdvisory, tc.wantAdvisory)
			}
		})
	}
}

// The caller's Result is a copy: inferring a wait must not write through to a
// pointer the executor shares with its own error.
func TestWithInferredRetryAfterDoesNotMutateTheExecutorsValue(t *testing.T) {
	published := 10 * time.Second
	in := Result{Error: &Error{HTTPStatus: 429, Message: "rate limit"}, RetryAfter: &published, Headers: retryHeaders("Retry-After", "999")}
	out := withInferredRetryAfter(in, time.Now())
	if *out.RetryAfter != published || published != 10*time.Second {
		t.Fatalf("published wait changed: %v", published)
	}
	if in.RetryAfterAdvisory || out.RetryAfterAdvisory {
		t.Fatal("a published wait was marked advisory")
	}
}

func TestUpstreamFailureCooldownBoundsTheWait(t *testing.T) {
	d := func(v time.Duration) *time.Duration { return &v }
	cases := []struct {
		name string
		wait *time.Duration
		want time.Duration
		ok   bool
	}{
		{"none", nil, 0, false},
		{"zero", d(0), 0, false},
		{"negative", d(-time.Second), 0, false},
		{"sub-second is raised", d(10 * time.Millisecond), time.Second, true},
		{"as asked", d(45 * time.Second), 45 * time.Second, true},
		{"at the ceiling", d(10 * time.Minute), 10 * time.Minute, true},
		{"above the ceiling", d(time.Hour), 10 * time.Minute, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := upstreamFailureCooldown(Result{RetryAfter: tc.wait})
			if ok != tc.ok || got != tc.want {
				t.Fatalf("upstreamFailureCooldown = %v, %v; want %v, %v", got, ok, tc.want, tc.ok)
			}
		})
	}
}

func TestBoundInferredRetryAfter(t *testing.T) {
	cases := []struct {
		name    string
		status  int
		message string
		delay   time.Duration
		want    time.Duration
	}{
		{"throttling inside the hour", 429, "rate limit", 90 * time.Second, 90 * time.Second},
		{"throttling beyond the hour", 429, "rate limit", 5 * time.Hour, time.Hour},
		{"allowance beyond the hour", 429, "quota exceeded", 5 * time.Hour, 5 * time.Hour},
		{"allowance beyond the week", 429, "quota exceeded", 40 * 24 * time.Hour, 7 * 24 * time.Hour},
		{"floor", 429, "rate limit", time.Millisecond, time.Second},
		{"overload inside ten minutes", 503, "x", 5 * time.Minute, 5 * time.Minute},
		{"overload beyond ten minutes", 529, "x", time.Hour, 10 * time.Minute},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := boundInferredRetryAfter(tc.status, tc.message, tc.delay); got != tc.want {
				t.Fatalf("boundInferredRetryAfter = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestDropAdvisoryRetryAfterOnlyWhenCoolingIsOff(t *testing.T) {
	wait := time.Minute
	advisory := Result{RetryAfter: &wait, RetryAfterAdvisory: true}
	publishedByExecutor := Result{RetryAfter: &wait}
	coolingOff := channelAuth("off", "true")
	coolingOn := channelAuth("on", "false")

	setGlobalCoolingDisabled(t, false)
	if got := dropAdvisoryRetryAfter(coolingOff, advisory); got.RetryAfter != nil || got.RetryAfterAdvisory {
		t.Fatalf("advisory wait kept with cooling off: %+v", got)
	}
	if got := dropAdvisoryRetryAfter(coolingOn, advisory); got.RetryAfter == nil || !got.RetryAfterAdvisory {
		t.Fatalf("advisory wait dropped with cooling on: %+v", got)
	}
	if got := dropAdvisoryRetryAfter(coolingOff, publishedByExecutor); got.RetryAfter == nil {
		t.Fatal("a wait the executor published must survive disable-cooling, as it always did")
	}
	if got := dropAdvisoryRetryAfter(nil, advisory); got.RetryAfter == nil {
		t.Fatal("no auth, no override: the wait stays")
	}

	// The global switch counts when the channel says nothing.
	setGlobalCoolingDisabled(t, true)
	if got := dropAdvisoryRetryAfter(channelAuth("global", ""), advisory); got.RetryAfter != nil {
		t.Fatalf("advisory wait kept under the global switch: %+v", got)
	}
}

func TestIsProxyFailure(t *testing.T) {
	cases := []struct {
		name    string
		headers http.Header
		want    bool
	}{
		{"squid error page", retryHeaders("X-Squid-Error", "ERR_CONNECT_FAIL 111"), true},
		{"bright data error page", retryHeaders("X-Brd-Error", "target_40010"), true},
		{"luminati error page", retryHeaders("X-Luminati-Error", "Failed"), true},
		{"a plain 502 says nothing", retryHeaders("Server", "nginx"), false},
		{"no headers", nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isProxyFailure(Result{Headers: tc.headers}); got != tc.want {
				t.Fatalf("isProxyFailure = %v, want %v", got, tc.want)
			}
		})
	}
}
