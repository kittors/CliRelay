package auth

import (
	"context"
	"net/http"
	"testing"
	"time"
)

// failWith reports a failed upstream call the way the conductor does after an
// executor returned: the error, the response headers, and a wait only when the
// executor published one.
func failWith(m *Manager, authID, model string, err *Error, headers http.Header, published *time.Duration) {
	m.MarkResult(context.Background(), Result{
		AuthID: authID, Provider: "openai-compatibility", Model: model,
		Error: err, Headers: headers, RetryAfter: published,
	})
}

func modelStateOf(t *testing.T, m *Manager, authID, model string) *ModelState {
	t.Helper()
	auth, ok := m.GetByID(authID)
	if !ok || auth == nil {
		t.Fatalf("auth %s missing", authID)
	}
	state := auth.ModelStates[model]
	if state == nil {
		t.Fatalf("no state recorded for %s/%s", authID, model)
	}
	return state
}

// assertParkedFor checks the model is held back for about want from before.
// want == 0 means it must not be held back at all.
func assertParkedFor(t *testing.T, m *Manager, authID, model string, before time.Time, want time.Duration) {
	t.Helper()
	state := modelStateOf(t, m, authID, model)
	if want == 0 {
		if !state.NextRetryAfter.IsZero() || blockedFor(t, m, authID, model) {
			t.Fatalf("%s is held back until %v, want no cooldown", model, state.NextRetryAfter)
		}
		return
	}
	got := state.NextRetryAfter.Sub(before)
	if got < want-time.Second || got > want+3*time.Second {
		t.Fatalf("%s held back for %v, want about %v", model, got, want)
	}
	if !blockedFor(t, m, authID, model) {
		t.Fatalf("%s has a cooldown but the scheduler would still pick it", model)
	}
}

func TestMarkResultCooldownFollowsTheUpstreamsHint(t *testing.T) {
	reset := time.Now().Add(2 * time.Hour).In(time.FixedZone("UTC+8", 8*3600))
	chineseReset := "当前额度已用尽，将在 " + reset.Format("2006-01-02 15:04:05") + " UTC+8 重置"

	cases := []struct {
		name      string
		err       *Error
		headers   http.Header
		want      time.Duration
		wantQuota bool
	}{
		// Throttling: a wait the upstream names replaces the default minute, and
		// the model is not declared out of quota.
		{name: "throttle with Retry-After", err: &Error{HTTPStatus: 429, Message: "Rate limit reached for requests"},
			headers: retryHeaders("Retry-After", "90"), want: 90 * time.Second},
		{name: "throttle with a wait in the message", err: &Error{HTTPStatus: 429, Message: `{"error":{"message":"Rate limit reached. Please try again in 20s."}}`},
			want: 20 * time.Second},
		{name: "throttle without a hint keeps the minute", err: &Error{HTTPStatus: 429, Message: "Rate limit reached for requests"},
			want: time.Minute},
		{name: "throttle hint beyond an hour is bounded", err: &Error{HTTPStatus: 429, Message: "rate limit"},
			headers: retryHeaders("Retry-After", "86400"), want: time.Hour},
		{name: "throttle with a sub-second hint waits the floor", err: &Error{HTTPStatus: 429, Message: "rate limit"},
			headers: retryHeaders("Retry-After-Ms", "100"), want: time.Second},

		// Chinese relays: the wording is throttling, not an exhausted allowance.
		{name: "chinese throttle", err: &Error{HTTPStatus: 429, Message: "请求过于频繁，请稍后再试"}, want: time.Minute},
		{name: "chinese throttle with seconds", err: &Error{HTTPStatus: 429, Message: "请求过于频繁，请 20 秒后重试"}, want: 20 * time.Second},
		{name: "chinese saturated group", err: &Error{HTTPStatus: 429, Message: "当前分组上游负载已饱和，请稍后再试"}, want: time.Minute},
		{name: "chinese allowance", err: &Error{HTTPStatus: 429, Message: "额度已用尽，请充值"}, want: time.Second, wantQuota: true},
		{name: "chinese allowance with a reset time", err: &Error{HTTPStatus: 429, Message: chineseReset}, want: 2 * time.Hour, wantQuota: true},
		{name: "chinese throttle that names an allowance stays an allowance", err: &Error{HTTPStatus: 429, Message: "请求过多：令牌额度不足"},
			want: time.Second, wantQuota: true},

		// Exhausted allowances.
		{name: "allowance with Retry-After", err: &Error{HTTPStatus: 429, Message: "daily quota exceeded"},
			headers: retryHeaders("Retry-After", "10800"), want: 3 * time.Hour, wantQuota: true},
		{name: "allowance hint beyond a week is bounded", err: &Error{HTTPStatus: 429, Message: "daily quota exceeded"},
			headers: retryHeaders("Retry-After", "2592000"), want: 7 * 24 * time.Hour, wantQuota: true},
		{name: "allowance without a hint backs off from a second", err: &Error{HTTPStatus: 429, Message: "daily quota exceeded"},
			want: time.Second, wantQuota: true},
		{name: "Gemini style retry delay field", err: &Error{HTTPStatus: 429, Message: `{"error":{"code":429,"status":"RESOURCE_EXHAUSTED","details":[{"retryDelay":"34s"}]}}`},
			want: 34 * time.Second},

		// Overload and unavailability: the upstream's own number replaces the
		// fixed minute, bounded at ten minutes.
		{name: "503 follows Retry-After", err: &Error{HTTPStatus: 503, Message: "service unavailable"},
			headers: retryHeaders("Retry-After", "30"), want: 30 * time.Second},
		{name: "503 hint beyond ten minutes is bounded", err: &Error{HTTPStatus: 503, Message: "service unavailable"},
			headers: retryHeaders("Retry-After", "7200"), want: 10 * time.Minute},
		{name: "503 without a hint keeps the minute", err: &Error{HTTPStatus: 503, Message: "service unavailable"}, want: time.Minute},
		{name: "503 overload without a hint keeps its short pause", err: &Error{HTTPStatus: 503, Message: `{"error":{"code":"server_is_overloaded"}}`},
			want: 5 * time.Second},
		{name: "503 overload with a hint follows the hint", err: &Error{HTTPStatus: 503, Message: `{"error":{"code":"server_is_overloaded"}}`},
			headers: retryHeaders("Retry-After", "20"), want: 20 * time.Second},
		{name: "503 message text is not a hint", err: &Error{HTTPStatus: 503, Message: "please try again in 20s"}, want: time.Minute},
		{name: "500 ignores Retry-After", err: &Error{HTTPStatus: 500, Message: "boom"},
			headers: retryHeaders("Retry-After", "300"), want: time.Minute},
		{name: "502 ignores Retry-After", err: &Error{HTTPStatus: 502, Message: "bad gateway"},
			headers: retryHeaders("Retry-After", "300"), want: time.Minute},
		{name: "504 without a hint keeps the minute", err: &Error{HTTPStatus: 504, Message: "gateway timeout"}, want: time.Minute},
		{name: "529 follows Retry-After", err: &Error{HTTPStatus: 529, Message: "overloaded_error"},
			headers: retryHeaders("Retry-After", "45"), want: 45 * time.Second},
		{name: "529 without a hint does not back off", err: &Error{HTTPStatus: 529, Message: "overloaded_error"}, want: 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			setGlobalCoolingDisabled(t, false)
			m := NewManager(nil, nil, nil)
			registerAuth(t, m, channelAuth("relay", ""))
			before := time.Now()
			failWith(m, "relay", "glm-5", tc.err, tc.headers, nil)
			assertParkedFor(t, m, "relay", "glm-5", before, tc.want)
			if got := modelStateOf(t, m, "relay", "glm-5").Quota.Exceeded; got != tc.wantQuota {
				t.Fatalf("Quota.Exceeded = %v, want %v", got, tc.wantQuota)
			}
		})
	}
}

// A wait the executor published after parsing the provider's own format beats
// whatever the response headers say.
func TestMarkResultPublishedWaitBeatsTheScheduler(t *testing.T) {
	setGlobalCoolingDisabled(t, false)
	m := NewManager(nil, nil, nil)
	registerAuth(t, m, channelAuth("relay", ""))
	published := 10 * time.Second
	before := time.Now()
	failWith(m, "relay", "glm-5", &Error{HTTPStatus: 429, Message: "rate limit"}, retryHeaders("Retry-After", "3600"), &published)
	assertParkedFor(t, m, "relay", "glm-5", before, 10*time.Second)
}

// Disable-cooling asks for failures to be retried at once. A wait the scheduler
// only read out of the response must not override that; a wait the executor
// published (an allowance that comes back at a stated time) always stood.
func TestMarkResultDisableCoolingIgnoresInferredWaitsOnly(t *testing.T) {
	setGlobalCoolingDisabled(t, false)
	threeHours := 3 * time.Hour
	cases := []struct {
		name      string
		err       *Error
		headers   http.Header
		published *time.Duration
		want      time.Duration
	}{
		{name: "throttle hint is dropped", err: &Error{HTTPStatus: 429, Message: "rate limit"}, headers: retryHeaders("Retry-After", "90")},
		{name: "allowance hint is dropped", err: &Error{HTTPStatus: 429, Message: "daily quota exceeded"}, headers: retryHeaders("Retry-After", "10800")},
		{name: "chinese throttle hint is dropped", err: &Error{HTTPStatus: 429, Message: "请求过于频繁，请 20 秒后重试"}},
		{name: "503 hint is dropped", err: &Error{HTTPStatus: 503, Message: "unavailable"}, headers: retryHeaders("Retry-After", "30")},
		{name: "529 hint is dropped", err: &Error{HTTPStatus: 529, Message: "overloaded"}, headers: retryHeaders("Retry-After", "30")},
		{name: "published allowance wait still stands", err: &Error{HTTPStatus: 429, Message: "daily quota exceeded"}, published: &threeHours, want: 3 * time.Hour},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := NewManager(nil, nil, nil)
			registerAuth(t, m, channelAuth("relay", "true"))
			before := time.Now()
			failWith(m, "relay", "glm-5", tc.err, tc.headers, tc.published)
			assertParkedFor(t, m, "relay", "glm-5", before, tc.want)
		})
	}
}

func TestMarkResultWithoutAModelFollowsTheHintToo(t *testing.T) {
	setGlobalCoolingDisabled(t, false)
	cases := []struct {
		name    string
		err     *Error
		headers http.Header
		want    time.Duration
	}{
		{name: "503 follows Retry-After", err: &Error{HTTPStatus: 503, Message: "unavailable"}, headers: retryHeaders("Retry-After", "30"), want: 30 * time.Second},
		{name: "503 hint is bounded", err: &Error{HTTPStatus: 503, Message: "unavailable"}, headers: retryHeaders("Retry-After", "7200"), want: 10 * time.Minute},
		{name: "503 without a hint keeps the minute", err: &Error{HTTPStatus: 503, Message: "unavailable"}, want: time.Minute},
		{name: "529 follows Retry-After", err: &Error{HTTPStatus: 529, Message: "overloaded"}, headers: retryHeaders("Retry-After", "45"), want: 45 * time.Second},
		{name: "529 without a hint does not back off", err: &Error{HTTPStatus: 529, Message: "overloaded"}},
		{name: "429 follows Retry-After", err: &Error{HTTPStatus: 429, Message: "rate limit"}, headers: retryHeaders("Retry-After", "20"), want: 20 * time.Second},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := NewManager(nil, nil, nil)
			registerAuth(t, m, channelAuth("relay", ""))
			before := time.Now()
			m.MarkResult(context.Background(), Result{AuthID: "relay", Provider: "openai-compatibility", Error: tc.err, Headers: tc.headers})
			auth, _ := m.GetByID("relay")
			if tc.want == 0 {
				if !auth.NextRetryAfter.IsZero() {
					t.Fatalf("auth held back until %v, want no cooldown", auth.NextRetryAfter)
				}
				return
			}
			if got := auth.NextRetryAfter.Sub(before); got < tc.want-time.Second || got > tc.want+3*time.Second {
				t.Fatalf("auth held back for %v, want about %v", got, tc.want)
			}
		})
	}
	t.Run("disable cooling drops the hint", func(t *testing.T) {
		m := NewManager(nil, nil, nil)
		registerAuth(t, m, channelAuth("relay", "true"))
		m.MarkResult(context.Background(), Result{AuthID: "relay", Provider: "openai-compatibility",
			Error: &Error{HTTPStatus: 503, Message: "unavailable"}, Headers: retryHeaders("Retry-After", "30")})
		if auth, _ := m.GetByID("relay"); !auth.NextRetryAfter.IsZero() {
			t.Fatalf("auth held back until %v with cooling off", auth.NextRetryAfter)
		}
	})
}

// The proxy in front of the upstream failed, not the credential: a healthy
// account must not be taken out of rotation for a minute because of it.
func TestMarkResultProxyFailureDoesNotCoolTheCredential(t *testing.T) {
	setGlobalCoolingDisabled(t, false)
	cases := []struct {
		name    string
		status  int
		headers http.Header
		want    time.Duration
	}{
		{name: "squid 502", status: 502, headers: retryHeaders("X-Squid-Error", "ERR_CONNECT_FAIL 111")},
		{name: "bright data 502", status: 502, headers: retryHeaders("X-Brd-Error", "target_40010")},
		{name: "luminati 503", status: 503, headers: retryHeaders("X-Luminati-Error", "Failed")},
		{name: "proxy page beats a Retry-After", status: 503, headers: retryHeaders("X-Squid-Error", "ERR_CONNECT_FAIL", "Retry-After", "30")},
		{name: "504 from the proxy", status: 504, headers: retryHeaders("X-Squid-Error", "ERR_READ_TIMEOUT")},
		// Without the proxy's own marker a 502 may be the upstream's, and keeps cooling.
		{name: "a bare 502 still cools", status: 502, headers: retryHeaders("Server", "nginx"), want: time.Minute},
		{name: "a bare 502 without headers still cools", status: 502, want: time.Minute},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := NewManager(nil, nil, nil)
			registerAuth(t, m, channelAuth("relay", ""))
			before := time.Now()
			failWith(m, "relay", "glm-5", &Error{HTTPStatus: tc.status, Message: "bad gateway"}, tc.headers, nil)
			assertParkedFor(t, m, "relay", "glm-5", before, tc.want)
		})
	}
}

// What a client's own mistake or a dropped connection says about the
// credential is nothing: neither may start a cooldown.
func TestMarkResultClientErrorsAndTransportFailuresDoNotCool(t *testing.T) {
	setGlobalCoolingDisabled(t, false)
	cases := []struct {
		name string
		err  *Error
	}{
		{name: "content policy rejection", err: &Error{HTTPStatus: 400, Message: `{"error":{"message":"Your request was rejected as a result of our safety system.","type":"invalid_request_error","code":"content_policy_violation"}}`}},
		{name: "content filtered by a relay", err: &Error{HTTPStatus: 400, Message: `{"error":{"message":"The response was filtered due to the prompt triggering content management policy","code":"content_filter"}}`}},
		{name: "plain 400", err: &Error{HTTPStatus: 400, Message: "bad request"}},
		{name: "invalid request", err: &Error{HTTPStatus: 400, Message: `{"error":{"type":"invalid_request_error","message":"messages: text content blocks must be non-empty"}}`}},
		{name: "422", err: &Error{HTTPStatus: 422, Message: "unprocessable"}},
		{name: "connection refused", err: &Error{Message: "dial tcp 10.0.0.1:443: connect: connection refused"}},
		{name: "proxy handshake failure", err: &Error{Message: "proxyconnect tcp: socks connect tcp 127.0.0.1:1080->api.example.com:443: unknown error host unreachable"}},
		{name: "timeout without a status", err: &Error{Message: "context deadline exceeded (Client.Timeout exceeded while awaiting headers)"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := NewManager(nil, nil, nil)
			registerAuth(t, m, channelAuth("relay", ""))
			failWith(m, "relay", "glm-5", tc.err, nil, nil)
			auth, _ := m.GetByID("relay")
			if state := auth.ModelStates["glm-5"]; state != nil {
				if !state.NextRetryAfter.IsZero() || state.Quota.Exceeded {
					t.Fatalf("%s started a cooldown: %+v", tc.name, state)
				}
			}
			if blockedFor(t, m, "relay", "glm-5") || auth.Quota.Exceeded || !auth.NextRetryAfter.IsZero() {
				t.Fatalf("%s took the credential out of rotation", tc.name)
			}
			if got := ActiveModelCooldowns(auth, time.Now()); len(got) != 0 {
				t.Fatalf("%s is listed as a cooldown: %+v", tc.name, got)
			}
		})
	}
}

// The authorization-class statuses keep their fixed parks; a hint on them is not
// read (a 401 is not going to clear itself in the seconds a header names).
func TestMarkResultFixedParksAreUnchanged(t *testing.T) {
	setGlobalCoolingDisabled(t, false)
	cases := []struct {
		name    string
		err     *Error
		headers http.Header
		want    time.Duration
	}{
		{name: "401", err: &Error{HTTPStatus: 401, Message: "invalid api key"}, want: 30 * time.Minute},
		{name: "401 with Retry-After", err: &Error{HTTPStatus: 401, Message: "invalid api key"}, headers: retryHeaders("Retry-After", "5"), want: 30 * time.Minute},
		{name: "402", err: &Error{HTTPStatus: 402, Message: "payment required"}, want: 30 * time.Minute},
		{name: "403", err: &Error{HTTPStatus: 403, Message: "forbidden"}, want: 30 * time.Minute},
		{name: "bare 404", err: &Error{HTTPStatus: 404, Message: "not found"}, want: 12 * time.Hour},
		{name: "404 that says the model is missing", err: &Error{HTTPStatus: 404, Message: `{"error":{"code":"model_not_found","message":"The model glm-5 does not exist"}}`}, want: 12 * time.Hour},
		{name: "404 with Retry-After", err: &Error{HTTPStatus: 404, Message: "not found"}, headers: retryHeaders("Retry-After", "5"), want: 12 * time.Hour},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := NewManager(nil, nil, nil)
			registerAuth(t, m, channelAuth("relay", ""))
			before := time.Now()
			failWith(m, "relay", "glm-5", tc.err, tc.headers, nil)
			assertParkedFor(t, m, "relay", "glm-5", before, tc.want)
		})
	}
}

// 529 is Anthropic's non-standard "overloaded". With cooling on it backs off
// only when the upstream names a time. With cooling off it never does, not even
// for a wait an executor published: a busy upstream is not an announced reset
// that the operator's switch has to respect, the way an exhausted allowance is.
func TestMarkResultOverload529FollowsDisableCooling(t *testing.T) {
	setGlobalCoolingDisabled(t, false)
	published := 45 * time.Second
	overloaded := &Error{HTTPStatus: 529, Message: "overloaded_error"}

	t.Run("model level, cooling on honours the published wait", func(t *testing.T) {
		m := NewManager(nil, nil, nil)
		registerAuth(t, m, channelAuth("relay", ""))
		before := time.Now()
		failWith(m, "relay", "glm-5", overloaded, nil, &published)
		assertParkedFor(t, m, "relay", "glm-5", before, published)
	})
	t.Run("model level, cooling off ignores it", func(t *testing.T) {
		m := NewManager(nil, nil, nil)
		registerAuth(t, m, channelAuth("relay", "true"))
		failWith(m, "relay", "glm-5", overloaded, nil, &published)
		assertParkedFor(t, m, "relay", "glm-5", time.Now(), 0)
	})
	t.Run("account level, cooling on honours the published wait", func(t *testing.T) {
		m := NewManager(nil, nil, nil)
		registerAuth(t, m, channelAuth("relay", ""))
		before := time.Now()
		m.MarkResult(context.Background(), Result{AuthID: "relay", Provider: "openai-compatibility", Error: overloaded, RetryAfter: &published})
		auth, _ := m.GetByID("relay")
		if got := auth.NextRetryAfter.Sub(before); got < published-time.Second || got > published+3*time.Second {
			t.Fatalf("account held back for %v, want about %v", got, published)
		}
	})
	t.Run("account level, cooling off ignores it", func(t *testing.T) {
		m := NewManager(nil, nil, nil)
		registerAuth(t, m, channelAuth("relay", "true"))
		m.MarkResult(context.Background(), Result{AuthID: "relay", Provider: "openai-compatibility", Error: overloaded, RetryAfter: &published})
		if auth, _ := m.GetByID("relay"); !auth.NextRetryAfter.IsZero() {
			t.Fatalf("account held back until %v with cooling off", auth.NextRetryAfter)
		}
	})
}

// Switching a channel's cooling off frees what a 529 parked, as it does for the
// other overload statuses; only the authorization-class statuses stay put.
func TestUpdateReleasesA529CooldownWhenChannelTurnsCoolingOff(t *testing.T) {
	setGlobalCoolingDisabled(t, false)
	overloaded := &Error{HTTPStatus: 529, Message: "overloaded_error"}

	t.Run("model level", func(t *testing.T) {
		m := NewManager(nil, nil, nil)
		registerAuth(t, m, channelAuth("relay", ""))
		failWith(m, "relay", "glm-5", overloaded, retryHeaders("Retry-After", "600"), nil)
		if !blockedFor(t, m, "relay", "glm-5") {
			t.Fatal("a 529 that names a wait did not park the model")
		}
		if _, err := m.Update(context.Background(), channelAuth("relay", "true")); err != nil {
			t.Fatalf("update: %v", err)
		}
		if blockedFor(t, m, "relay", "glm-5") {
			t.Fatal("a 529 cooldown survived switching cooling off")
		}
	})
	t.Run("account level", func(t *testing.T) {
		m := NewManager(nil, nil, nil)
		registerAuth(t, m, channelAuth("relay", ""))
		m.MarkResult(context.Background(), Result{AuthID: "relay", Provider: "openai-compatibility", Error: overloaded, Headers: retryHeaders("Retry-After", "600")})
		if !blockedFor(t, m, "relay", "") {
			t.Fatal("a 529 that names a wait did not park the account")
		}
		if _, err := m.Update(context.Background(), channelAuth("relay", "true")); err != nil {
			t.Fatalf("update: %v", err)
		}
		if blockedFor(t, m, "relay", "") {
			t.Fatal("an account-level 529 cooldown survived switching cooling off")
		}
	})
}
