package executor_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/runtime/executor"
	_ "github.com/router-for-me/CLIProxyAPI/v6/internal/translator"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v6/sdk/translator"
)

// These tests send a failed upstream response through a real executor and the
// real Manager, and read the cooldown the Manager recorded for the model. They
// are the end-to-end proof that what the upstream asked for is what the account
// waits (the generic reader's own cases live in sdk/cliproxy/retryhint).

// stubReply is what the stub upstream answers every request with.
type stubReply struct {
	status int
	header map[string]string
	body   string
}

func replyWith(status int, body string, headers ...string) stubReply {
	reply := stubReply{status: status, body: body, header: map[string]string{}}
	for i := 0; i+1 < len(headers); i += 2 {
		reply.header[headers[i]] = headers[i+1]
	}
	return reply
}

type hintTarget struct {
	name     string
	provider string
	model    string
	source   sdktranslator.Format
	payload  string

	newExecutor func() coreauth.ProviderExecutor
	newAuth     func(baseURL string) *coreauth.Auth
	// fixedURL marks an executor whose upstream address is a constant in code:
	// the stub is reached through a transport that redirects every request to it.
	fixedURL bool
	// forwardsAll marks an executor that hands the caller the complete response
	// headers. The others keep only the ones the scheduler reads.
	forwardsAll bool
}

func openAIChatPayload(model string) string {
	return fmt.Sprintf(`{"model":%q,"messages":[{"role":"user","content":"hello"}]}`, model)
}

func hintTargets() []hintTarget {
	cfg := &config.Config{}
	apiKeyAuth := func(provider string, pathSuffix string) func(string) *coreauth.Auth {
		return func(base string) *coreauth.Auth {
			return &coreauth.Auth{Provider: provider, Attributes: map[string]string{"base_url": base + pathSuffix, "api_key": "test-key"}}
		}
	}
	return []hintTarget{
		{
			name: "openai-compat", provider: "hint-compat", model: "hint-compat-model",
			source: sdktranslator.FormatOpenAI, payload: openAIChatPayload("hint-compat-model"),
			newExecutor: func() coreauth.ProviderExecutor { return executor.NewOpenAICompatExecutor("hint-compat", cfg) },
			newAuth:     apiKeyAuth("hint-compat", "/v1"),
			forwardsAll: true,
		},
		{
			name: "claude", provider: "claude", model: "hint-claude-model",
			source:      sdktranslator.FromString("claude"),
			payload:     `{"model":"hint-claude-model","max_tokens":16,"messages":[{"role":"user","content":[{"type":"text","text":"hello"}]}]}`,
			newExecutor: func() coreauth.ProviderExecutor { return executor.NewClaudeExecutor(cfg) },
			newAuth:     apiKeyAuth("claude", ""),
			forwardsAll: true,
		},
		{
			name: "gemini", provider: "gemini", model: "hint-gemini-model",
			source:      sdktranslator.FromString("gemini"),
			payload:     `{"contents":[{"role":"user","parts":[{"text":"hello"}]}]}`,
			newExecutor: func() coreauth.ProviderExecutor { return executor.NewGeminiExecutor(cfg) },
			newAuth:     apiKeyAuth("gemini", ""),
		},
		{
			name: "iflow", provider: "iflow", model: "hint-iflow-model",
			source: sdktranslator.FormatOpenAI, payload: openAIChatPayload("hint-iflow-model"),
			newExecutor: func() coreauth.ProviderExecutor { return executor.NewIFlowExecutor(cfg) },
			newAuth:     apiKeyAuth("iflow", ""),
		},
		{
			name: "kimi", provider: "kimi", model: "kimi-hint-model",
			source: sdktranslator.FormatOpenAI, payload: openAIChatPayload("kimi-hint-model"),
			newExecutor: func() coreauth.ProviderExecutor { return executor.NewKimiExecutor(cfg) },
			newAuth: func(string) *coreauth.Auth {
				return &coreauth.Auth{Provider: "kimi", Attributes: map[string]string{"api_key": "test-key"}}
			},
			fixedURL: true,
		},
		{
			name: "codex", provider: "codex", model: "hint-codex-model",
			source:      sdktranslator.FromString("openai-response"),
			payload:     `{"model":"hint-codex-model","input":"hello"}`,
			newExecutor: func() coreauth.ProviderExecutor { return executor.NewCodexExecutor(cfg) },
			newAuth: func(base string) *coreauth.Auth {
				return &coreauth.Auth{
					Provider:   "codex",
					Attributes: map[string]string{"base_url": base + "/backend-api/codex"},
					Metadata:   map[string]any{"access_token": "token", "account_id": "account-1", "refresh_token": "refresh-1"},
				}
			},
		},
	}
}

func targetNamed(t *testing.T, name string) hintTarget {
	t.Helper()
	for _, target := range hintTargets() {
		if target.name == name {
			return target
		}
	}
	t.Fatalf("no target named %s", name)
	return hintTarget{}
}

// redirectTransport sends every request to the stub upstream, whatever host the
// executor addressed.
type redirectTransport struct{ to string }

func (r redirectTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	target, err := url.Parse(r.to)
	if err != nil {
		return nil, err
	}
	clone := req.Clone(req.Context())
	clone.URL.Scheme = target.Scheme
	clone.URL.Host = target.Host
	clone.Host = target.Host
	return http.DefaultTransport.RoundTrip(clone)
}

type failedCall struct {
	err   error
	auth  *coreauth.Auth
	state *coreauth.ModelState
	// wait is how long after the call the Manager keeps the model from being
	// picked; zero when it set no cooldown.
	wait  time.Duration
	calls int32
}

// runFailedCall performs one request that the stub upstream answers with reply
// and returns what the Manager recorded about it.
func runFailedCall(t *testing.T, target hintTarget, reply stubReply, stream bool, tweak ...func(*coreauth.Auth)) failedCall {
	t.Helper()
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		// Present on every reply so a test can tell whether an executor passes the
		// response headers on wholesale or keeps only the ones the scheduler reads.
		w.Header().Set("Set-Cookie", "session=upstream-secret")
		w.Header().Set("Content-Type", "application/json")
		for key, value := range reply.header {
			w.Header().Set(key, value)
		}
		w.WriteHeader(reply.status)
		_, _ = w.Write([]byte(reply.body))
	}))
	t.Cleanup(server.Close)

	id := strings.NewReplacer("/", "-", " ", "-", "=", "-").Replace(t.Name())
	manager := coreauth.NewManager(nil, nil, nil)
	manager.SetRetryConfig(0, 0)
	manager.RegisterExecutor(target.newExecutor())
	auth := target.newAuth(server.URL)
	auth.ID = id
	auth.Status = coreauth.StatusActive
	for _, apply := range tweak {
		apply(auth)
	}
	if _, err := manager.Register(context.Background(), auth); err != nil {
		t.Fatalf("register auth: %v", err)
	}
	registry.GetGlobalRegistry().RegisterClient(id, target.provider, []*registry.ModelInfo{{ID: target.model, Object: "model", OwnedBy: target.provider}})
	t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(id) })

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if target.fixedURL {
		ctx = coreexecutor.WithRoundTripper(ctx, redirectTransport{to: server.URL})
	}
	req := coreexecutor.Request{Model: target.model, Payload: []byte(target.payload), Format: target.source}
	opts := coreexecutor.Options{SourceFormat: target.source}

	start := time.Now()
	var err error
	if stream {
		_, err = manager.ExecuteStream(ctx, []string{target.provider}, req, opts)
	} else {
		_, err = manager.Execute(ctx, []string{target.provider}, req, opts)
	}
	if err == nil {
		t.Fatal("expected the upstream failure")
	}
	run := failedCall{err: err, calls: calls.Load()}
	if updated, ok := manager.GetByID(id); ok {
		run.auth = updated
		if state := updated.ModelStates[target.model]; state != nil {
			run.state = state
			if !state.NextRetryAfter.IsZero() {
				run.wait = state.NextRetryAfter.Sub(start)
			}
		}
	}
	return run
}

const waitSlack = 5 * time.Second

func (run failedCall) expectWait(t *testing.T, want time.Duration) {
	t.Helper()
	if run.state == nil {
		t.Fatalf("no model state after the failure: %v", run.err)
	}
	if run.wait < want-waitSlack || run.wait > want+waitSlack {
		t.Fatalf("cooldown = %s, want about %s (error: %v)", run.wait, want, run.err)
	}
}

func (run failedCall) expectQuota(t *testing.T, want bool) {
	t.Helper()
	if run.state == nil {
		t.Fatalf("no model state after the failure: %v", run.err)
	}
	if run.state.Quota.Exceeded != want {
		t.Fatalf("quota exceeded = %v, want %v", run.state.Quota.Exceeded, want)
	}
}

func (run failedCall) expectNoCooldown(t *testing.T) {
	t.Helper()
	if run.state == nil {
		return
	}
	if !run.state.NextRetryAfter.IsZero() || run.state.Quota.Exceeded {
		t.Fatalf("the model was parked: until %v, quota %v (error: %v)", run.state.NextRetryAfter, run.state.Quota.Exceeded, run.err)
	}
}

type upstreamError interface {
	error
	StatusCode() int
	Headers() http.Header
	RetryAfter() *time.Duration
}

func eachMode(t *testing.T, run func(t *testing.T, stream bool)) {
	t.Helper()
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream=%v", stream), func(t *testing.T) { run(t, stream) })
	}
}

// Whatever the executor, the wait the upstream names is the wait the model gets,
// and only the headers the scheduler reads travel with the error.
func TestEveryExecutorPassesTheUpstreamWaitToTheScheduler(t *testing.T) {
	inNinetySeconds := time.Now().Add(90 * time.Second).UTC().Format(http.TimeFormat)
	cases := []struct {
		name      string
		reply     stubReply
		wantWait  time.Duration
		wantQuota bool
	}{
		{"429 Retry-After seconds", replyWith(429, `{"error":{"message":"rate limit exceeded"}}`, "Retry-After", "45"), 45 * time.Second, false},
		{"429 Retry-After HTTP date", replyWith(429, `{"error":{"message":"rate limit exceeded"}}`, "Retry-After", inNinetySeconds), 90 * time.Second, false},
		{"429 without a hint keeps the minute", replyWith(429, `{"error":{"message":"rate limit exceeded"}}`), time.Minute, false},
		{"503 follows Retry-After", replyWith(503, `{"error":{"message":"service unavailable"}}`, "Retry-After", "45"), 45 * time.Second, false},
		{"529 follows Retry-After", replyWith(529, `{"error":{"message":"overloaded"}}`, "Retry-After", "45"), 45 * time.Second, false},
		{"500 ignores Retry-After", replyWith(500, `{"error":{"message":"boom"}}`, "Retry-After", "45"), time.Minute, false},
	}
	for _, target := range hintTargets() {
		for _, tc := range cases {
			t.Run(target.name+"/"+strings.ReplaceAll(tc.name, " ", "-"), func(t *testing.T) {
				eachMode(t, func(t *testing.T, stream bool) {
					run := runFailedCall(t, target, tc.reply, stream)
					run.expectWait(t, tc.wantWait)
					run.expectQuota(t, tc.wantQuota)
					if run.calls != 1 {
						t.Fatalf("upstream calls = %d, want 1", run.calls)
					}
					details, ok := run.err.(upstreamError)
					if !ok {
						t.Fatalf("upstream error metadata lost: %T %v", run.err, run.err)
					}
					if details.StatusCode() != tc.reply.status || details.Error() != tc.reply.body {
						t.Fatalf("status/body changed: %d %v", details.StatusCode(), run.err)
					}
					// The scheduler reads the response itself; nothing is published for it.
					if details.RetryAfter() != nil {
						t.Errorf("executor published a wait of its own: %v", *details.RetryAfter())
					}
					if want := tc.reply.header["Retry-After"]; want != "" && details.Headers().Get("Retry-After") != want {
						t.Errorf("Retry-After not passed on: %v", details.Headers())
					}
					if !target.forwardsAll && details.Headers().Get("Set-Cookie") != "" {
						t.Errorf("an executor that filters headers passed on the upstream's cookie: %v", details.Headers())
					}
				})
			})
		}
	}
}

// Chinese relays and gateways put the throttle and the reset time in the message.
// A throttle is not an empty account, and a stated reset time is believed.
func TestRelayWordingDecidesBetweenThrottlingAndAnExhaustedAllowance(t *testing.T) {
	inTwoHours := time.Now().Add(2 * time.Hour).In(time.FixedZone("UTC+8", 8*3600)).Format("2006-01-02 15:04:05")
	cases := []struct {
		name      string
		message   string
		header    []string
		wantWait  time.Duration // 0: only the classification is asserted
		wantQuota bool
	}{
		{"throttle that names a wait", "请求过于频繁，请 30 秒后再试", nil, 30 * time.Second, false},
		{"throttle without a wait", "当前分组上游负载已饱和，请稍后再试", nil, time.Minute, false},
		{"per-minute cap", "已达到每分钟请求上限 (RPM)", nil, time.Minute, false},
		{"English throttle that names a wait", "Rate limit reached for requests. Please try again in 20s.", nil, 20 * time.Second, false},
		{"allowance that names a reset time", "账户额度已用尽，将在 " + inTwoHours + " UTC+8 重置", nil, 2 * time.Hour, true},
		{"allowance with a reset interval", "Your quota will reset after 3h19m30s.", nil, 3*time.Hour + 19*time.Minute + 30*time.Second, true},
		{"allowance without a wait", "该令牌额度已用尽", nil, 0, true},
		{"header beats wording", "请求过于频繁，请 30 秒后再试", []string{"Retry-After", "75"}, 75 * time.Second, false},
		{"reset header with a duration", "Rate limit reached for requests", []string{"X-RateLimit-Reset-Requests", "1m30s"}, 90 * time.Second, false},
	}
	target := targetNamed(t, "openai-compat")
	for _, tc := range cases {
		t.Run(strings.ReplaceAll(tc.name, " ", "-"), func(t *testing.T) {
			eachMode(t, func(t *testing.T, stream bool) {
				body := fmt.Sprintf(`{"error":{"message":%q}}`, tc.message)
				run := runFailedCall(t, target, replyWith(http.StatusTooManyRequests, body, tc.header...), stream)
				run.expectQuota(t, tc.wantQuota)
				if tc.wantWait > 0 {
					run.expectWait(t, tc.wantWait)
				}
				if tc.wantQuota && run.state.Quota.NextRecoverAt.IsZero() {
					t.Errorf("an exhausted allowance recorded no recovery time")
				}
			})
		})
	}
}

// Anthropic reports one reset per window (requests, tokens, ...). Only the window
// that ran out caused the 429; the one with room left must not set the wait.
func TestClaudeRateLimitHeadersPickTheExhaustedWindow(t *testing.T) {
	target := targetNamed(t, "claude")
	now := time.Now().UTC()
	reply := replyWith(429, `{"type":"error","error":{"type":"rate_limit_error","message":"This request would exceed your rate limit."}}`,
		"Anthropic-Ratelimit-Requests-Remaining", "40",
		"Anthropic-Ratelimit-Requests-Reset", now.Add(10*time.Second).Format(time.RFC3339),
		"Anthropic-Ratelimit-Tokens-Remaining", "0",
		"Anthropic-Ratelimit-Tokens-Reset", now.Add(90*time.Second).Format(time.RFC3339),
	)
	eachMode(t, func(t *testing.T, stream bool) {
		run := runFailedCall(t, target, reply, stream)
		run.expectWait(t, 90*time.Second)
		run.expectQuota(t, false)
	})
}

// A fault of the proxy in front of the upstream says nothing about the account
// behind it: the account is not parked for it.
func TestProxyFaultDoesNotCoolTheAccount(t *testing.T) {
	for _, target := range hintTargets() {
		t.Run(target.name, func(t *testing.T) {
			eachMode(t, func(t *testing.T, stream bool) {
				t.Run("proxy-error-page", func(t *testing.T) {
					runFailedCall(t, target, replyWith(502, "Bad Gateway", "X-Squid-Error", "ERR_CONNECT_FAIL 111"), stream).expectNoCooldown(t)
				})
				// The same 502 without the proxy's marker is the upstream's own: unchanged.
				t.Run("plain-502-control", func(t *testing.T) {
					runFailedCall(t, target, replyWith(502, "Bad Gateway"), stream).expectWait(t, time.Minute)
				})
			})
		})
	}
}

// A request the upstream refuses on content grounds is the request's fault, not
// the account's.
func TestContentPolicyRefusalDoesNotCoolTheAccount(t *testing.T) {
	body := `{"error":{"message":"Your request was rejected as a result of our safety system.","type":"invalid_request_error","code":"content_policy_violation"}}`
	for _, target := range hintTargets() {
		t.Run(target.name, func(t *testing.T) {
			eachMode(t, func(t *testing.T, stream bool) {
				runFailedCall(t, target, replyWith(400, body), stream).expectNoCooldown(t)
			})
		})
	}
}

// An account with cooling switched off asked for failures to be retried at
// once; a wait the scheduler merely read out of the response does not override
// that.
func TestAdvisoryWaitRespectsDisableCooling(t *testing.T) {
	target := targetNamed(t, "openai-compat")
	reply := replyWith(429, `{"error":{"message":"rate limit exceeded"}}`, "Retry-After", "45")
	off := func(auth *coreauth.Auth) { auth.Attributes[coreauth.DisableCoolingAttributeKey] = "true" }
	eachMode(t, func(t *testing.T, stream bool) {
		t.Run("cooling-off", func(t *testing.T) {
			runFailedCall(t, target, reply, stream, off).expectNoCooldown(t)
		})
		t.Run("cooling-on", func(t *testing.T) {
			runFailedCall(t, target, reply, stream).expectWait(t, 45*time.Second)
		})
	})
}

// Codex's own reset fields state when the allowance comes back; they win over a
// header that says something else.
func TestCodexPublishedResetBeatsTheRetryAfterHeader(t *testing.T) {
	target := targetNamed(t, "codex")
	resetsAt := time.Now().Add(time.Hour).Unix()
	body := fmt.Sprintf(`{"error":{"type":"usage_limit_reached","message":"The usage limit has been reached","resets_at":%d,"resets_in_seconds":3600}}`, resetsAt)
	eachMode(t, func(t *testing.T, stream bool) {
		run := runFailedCall(t, target, replyWith(429, body, "Retry-After", "5"), stream)
		run.expectWait(t, time.Hour)
		run.expectQuota(t, true)
		details, ok := run.err.(upstreamError)
		if !ok || details.RetryAfter() == nil {
			t.Fatalf("the Codex executor stopped publishing its reset: %v", run.err)
		}
	})
}

// A Codex 401 is two different failures. An expired access token is renewed
// within seconds; a revoked credential needs the account signed in again.
func TestCodexUnauthorizedSplitsExpiredFromRevoked(t *testing.T) {
	target := targetNamed(t, "codex")
	unauthorized := func(code string) stubReply {
		return replyWith(401, fmt.Sprintf(`{"error":{"message":"Provided authentication token is expired.","type":"invalid_request_error","code":%q}}`, code))
	}
	withoutRefreshToken := func(auth *coreauth.Auth) { delete(auth.Metadata, "refresh_token") }

	cases := []struct {
		name       string
		reply      stubReply
		tweak      []func(*coreauth.Auth)
		wantWait   time.Duration
		wantHealth string
		wantCode   string
		wantReason string
	}{
		{"expired with a refresh token", unauthorized("token_expired"), nil, 30 * time.Second, "refresh_pending", "token_expired", coreauth.CooldownReasonTokenRefreshing},
		{"expired with nothing to refresh with", unauthorized("token_expired"), []func(*coreauth.Auth){withoutRefreshToken}, 30 * time.Minute, "reauth_required", "token_expired_no_refresh", coreauth.CooldownReasonReauthRequired},
		{"revoked", unauthorized("token_revoked"), nil, 30 * time.Minute, "reauth_required", "token_revoked", coreauth.CooldownReasonReauthRequired},
		{"refresh token reused", unauthorized("refresh_token_reused"), nil, 30 * time.Minute, "reauth_required", "refresh_token_reused", coreauth.CooldownReasonReauthRequired},
		{"account deactivated", unauthorized("account_deactivated"), nil, 30 * time.Minute, "reauth_required", "account_deactivated", coreauth.CooldownReasonReauthRequired},
		// An unrecognised 401 keeps the handling every 401 always had.
		{"unknown 401", replyWith(401, `{"error":{"message":"Unauthorized"}}`), nil, 30 * time.Minute, "", "", coreauth.CooldownReasonUnauthorized},
	}
	for _, tc := range cases {
		t.Run(strings.ReplaceAll(tc.name, " ", "-"), func(t *testing.T) {
			eachMode(t, func(t *testing.T, stream bool) {
				run := runFailedCall(t, target, tc.reply, stream, tc.tweak...)
				run.expectWait(t, tc.wantWait)
				if got := run.state.LastError.Code; got != tc.wantCode {
					t.Errorf("recorded error code = %q, want %q", got, tc.wantCode)
				}
				if got := coreauth.CooldownReasonOf(run.state.LastError, run.state.Quota); got != tc.wantReason {
					t.Errorf("cooldown reason = %q, want %q", got, tc.wantReason)
				}
				health, _ := run.auth.Metadata[coreauth.CodexOAuthHealthMetadataKey].(map[string]any)
				gotHealth, _ := health["status"].(string)
				if gotHealth != tc.wantHealth {
					t.Errorf("health status = %q, want %q (health: %v)", gotHealth, tc.wantHealth, health)
				}
			})
		})
	}
}
