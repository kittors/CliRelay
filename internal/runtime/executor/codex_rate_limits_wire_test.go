package executor

import (
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"

	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

func mustParseURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	parsed, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return parsed
}

// A realistic answer: the plan windows on the headers, the same numbers again as
// a codex.rate_limits event ahead of the response.
func codexRateLimitedResponse(req *http.Request, usedPercent string, event string) *http.Response {
	resp := codexSSEResponse(req)
	resp.Header.Set("x-codex-primary-used-percent", usedPercent)
	resp.Header.Set("x-codex-primary-window-minutes", "300")
	resp.Header.Set("x-codex-primary-reset-at", "1800003600")
	resp.Header.Set("x-codex-secondary-used-percent", "7")
	resp.Header.Set("x-codex-secondary-window-minutes", "10080")
	resp.Header.Set("x-codex-secondary-reset-at", "1800259200")
	resp.Body = io.NopCloser(strings.NewReader(event + codexCompletedSSE))
	return resp
}

func rateLimitUpstream(usedPercent string) func(int, codexWireCall) *http.Response {
	return func(_ int, _ codexWireCall) *http.Response {
		event := "event: codex.rate_limits\n" + sseEvent(rateLimitEventAt(42, 1800003600))
		return codexRateLimitedResponse(&http.Request{}, usedPercent, event)
	}
}

func TestCodexExecutorReportsQuotaFromHeadersAndStreamEvents(t *testing.T) {
	for _, stream := range []bool{false, true} {
		name := "execute"
		if stream {
			name = "execute-stream"
		}
		t.Run(name, func(t *testing.T) {
			env := newCodexExecEnv(t, nil)
			ctx, sink := withQuotaSink(env.Ctx)
			env.Ctx = ctx
			env.Upstream.respond = rateLimitUpstream("100") //nolint:bodyclose // test fake; the executor under test closes the body.

			out, err := env.Do(t, codexCall{Auth: codexOAuthAuth(), From: "openai-response", Payload: codexResponsesPayload, Stream: stream})
			if err != nil {
				t.Fatalf("request failed: %v", err)
			}

			got := sink.all()
			if len(got) != 2 {
				t.Fatalf("observations = %d, want the headers and the stream event", len(got))
			}
			// Headers arrive first, before the body is read: that is what lets the
			// scheduler park an account while its own response is still streaming.
			headers := got[0]
			if len(headers.Windows) != 2 || headers.Windows[0].UsedPercent != 100 || headers.Windows[0].Label != "5h" ||
				headers.Windows[1].UsedPercent != 7 || headers.Windows[1].Label != "week" {
				t.Fatalf("header observation = %+v", headers.Windows)
			}
			event := got[1]
			if len(event.Windows) != 1 || event.Windows[0].UsedPercent != 42 || event.Windows[0].WindowMinutes != 300 {
				t.Fatalf("event observation = %+v", event.Windows)
			}

			// Reading for quota must not change what the client receives.
			plainEnv := newCodexExecEnv(t, nil)
			plainEnv.Upstream.respond = rateLimitUpstream("100") //nolint:bodyclose // test fake; the executor under test closes the body.
			plain, err := plainEnv.Do(t, codexCall{Auth: codexOAuthAuth(), From: "openai-response", Payload: codexResponsesPayload, Stream: stream})
			if err != nil {
				t.Fatalf("baseline request failed: %v", err)
			}
			if string(out) != string(plain) {
				t.Fatalf("the downstream answer changed when a quota observer was attached:\n got  %s\n want %s", out, plain)
			}
		})
	}
}

func TestCodexExecutorOnlyReadsQuotaWhereItBelongsToTheAccount(t *testing.T) {
	relayAuth := codexOAuthAuth()
	relayAuth.Attributes = map[string]string{"base_url": "https://relay.example/backend-api/codex"}

	cases := []struct {
		name string
		auth *cliproxyauth.Auth
		want int
	}{
		{"ChatGPT OAuth account on chatgpt.com", codexOAuthAuth(), 2},
		// An API key's allowance is not the plan windows these headers describe.
		{"API key account", codexAPIKeyAuth(), 0},
		// A relay copies or invents these headers; they describe somebody else's account.
		{"OAuth account through another host", relayAuth, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := newCodexExecEnv(t, nil)
			ctx, sink := withQuotaSink(env.Ctx)
			env.Ctx = ctx
			env.Upstream.respond = rateLimitUpstream("100") //nolint:bodyclose // test fake; the executor under test closes the body.
			if _, err := env.Do(t, codexCall{Auth: tc.auth, From: "openai-response", Payload: codexResponsesPayload, Stream: true}); err != nil {
				t.Fatalf("request failed: %v", err)
			}
			if got := len(sink.all()); got != tc.want {
				t.Fatalf("observations = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestCodexExecutorReadsNothingWithoutAnObserver(t *testing.T) {
	// Probes and tests call the executor directly: no observer, no tee.
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     rateHeaders("x-codex-primary-used-percent", "100", "x-codex-primary-window-minutes", "300", "x-codex-primary-reset-at", "1800003600"),
		Body:       io.NopCloser(strings.NewReader(sseEvent(rateLimitEventAt(100, 1800003600)))),
	}
	original := resp.Body
	url := mustParseURL(t, codexResponsesURL)
	observeCodexRateLimits(t.Context(), codexOAuthAuth(), url, resp)
	if resp.Body != original {
		t.Fatal("the body was wrapped although nobody is listening")
	}
}

func TestCodexExecutorLeavesTheBodyAloneUnlessItIsASuccessfulStream(t *testing.T) {
	url := mustParseURL(t, codexResponsesURL)
	for _, status := range []int{http.StatusBadRequest, http.StatusUnauthorized, http.StatusTooManyRequests, http.StatusInternalServerError} {
		ctx, sink := withQuotaSink(t.Context())
		resp := &http.Response{
			StatusCode: status,
			Header:     rateHeaders("x-codex-primary-used-percent", "100", "x-codex-primary-window-minutes", "300", "x-codex-primary-reset-at", "1800003600"),
			Body:       io.NopCloser(strings.NewReader(`{"error":{"type":"usage_limit_reached"}}`)),
		}
		original := resp.Body
		observeCodexRateLimits(ctx, codexOAuthAuth(), url, resp)
		if resp.Body != original {
			t.Errorf("status %d: an error body was wrapped; error bodies are read whole by the caller and carry no events", status)
		}
		// The headers of a refusal are still the freshest word on the windows.
		if got := len(sink.all()); got != 1 {
			t.Errorf("status %d: observations = %d, want the headers' one", status, got)
		}
	}

	// No body at all (a handshake-style answer) must not trip anything.
	ctx, _ := withQuotaSink(t.Context())
	observeCodexRateLimits(ctx, codexOAuthAuth(), url, &http.Response{StatusCode: http.StatusOK, Header: http.Header{}})
	observeCodexRateLimits(ctx, codexOAuthAuth(), url, nil)
}

// The refusal is an error to the caller as before; the only difference is that
// the scheduler heard the windows first.
func TestCodexExecutorStillReportsTheRefusalItReadQuotaFrom(t *testing.T) {
	env := newCodexExecEnv(t, nil)
	ctx, sink := withQuotaSink(env.Ctx)
	env.Ctx = ctx
	env.Upstream.respond = func(int, codexWireCall) *http.Response {
		resp := codexErrorResponse(http.StatusTooManyRequests, `{"error":{"type":"usage_limit_reached","message":"The usage limit has been reached","resets_at":1800003600}}`)
		resp.Header.Set("x-codex-primary-used-percent", "100")
		resp.Header.Set("x-codex-primary-window-minutes", "300")
		resp.Header.Set("x-codex-primary-reset-at", "1800003600")
		return resp
	}

	_, err := env.Do(t, codexCall{Auth: codexOAuthAuth(), From: "openai-response", Payload: codexResponsesPayload})
	if err == nil {
		t.Fatal("a 429 must still be an error")
	}
	if se, ok := err.(interface{ StatusCode() int }); !ok || se.StatusCode() != http.StatusTooManyRequests {
		t.Fatalf("err = %v, want a 429 status error", err)
	}
	got := sink.all()
	if len(got) != 1 || got[0].Windows[0].UsedPercent != 100 {
		t.Fatalf("observations = %+v, want the headers of the refusal", got)
	}
}

// Only the answer the caller receives is reported: the refused first attempt of a
// retried request describes the same account moments earlier and is not.
func TestCodexExecutorReportsOnlyTheAnswerAfterAnInvalidCiphertextRetry(t *testing.T) {
	env := newCodexExecEnv(t, nil)
	ctx, sink := withQuotaSink(env.Ctx)
	env.Ctx = ctx
	env.Upstream.respond = func(n int, _ codexWireCall) *http.Response {
		if n == 0 {
			resp := codexErrorResponse(http.StatusBadRequest, `{"error":{"code":"invalid_encrypted_content","message":"could not decrypt"}}`)
			resp.Header.Set("x-codex-primary-used-percent", "31")
			resp.Header.Set("x-codex-primary-window-minutes", "300")
			return resp
		}
		return codexRateLimitedResponse(&http.Request{}, "32", "")
	}
	payload := `{"model":"gpt-5.4","input":[{"type":"reasoning","encrypted_content":"gAAAAB-foreign-account","summary":[]},` +
		`{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}]}`

	if _, err := env.Do(t, codexCall{Auth: codexOAuthAuth(), From: "openai-response", Payload: payload, Stream: true}); err != nil {
		t.Fatalf("request failed: %v", err)
	}
	if calls := env.Upstream.Calls(); len(calls) != 2 {
		t.Fatalf("upstream saw %d requests, want the refused one and the retry", len(calls))
	}
	got := sink.all()
	if len(got) != 1 || got[0].Windows[0].UsedPercent != 32 {
		t.Fatalf("observations = %+v, want only the retry's answer (32%%)", got)
	}
}

func TestCodexExecutorReportsQuotaOfEveryCallItMakes(t *testing.T) {
	// Each call is reported on its own; nothing is remembered between them here.
	// Deciding whether a repeat is worth writing is the scheduler's business.
	env := newCodexExecEnv(t, nil)
	ctx, sink := withQuotaSink(env.Ctx)
	env.Ctx = ctx
	env.Upstream.respond = func(int, codexWireCall) *http.Response {
		return codexRateLimitedResponse(&http.Request{}, "50", "")
	}
	for i := 0; i < 3; i++ {
		if _, err := env.Do(t, codexCall{Auth: codexOAuthAuth(), From: "openai-response", Payload: codexResponsesPayload, Stream: true}); err != nil {
			t.Fatal(err)
		}
	}
	if got := len(sink.all()); got != 3 {
		t.Fatalf("observations = %d, want one per call", got)
	}
}
