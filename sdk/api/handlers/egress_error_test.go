package handlers

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/egressbreaker"
	"github.com/tidwall/gjson"
)

const egressTestProxy = "socks5://tenant-a:expired@203.0.113.7:443"

// proxyFailures returns the two errors a request can end with when its proxy
// is out, as an executor's http.Client returns them: the failure of the
// request that found the proxy broken, and the breaker's refusal of the next.
func proxyFailures(t *testing.T, fault error) (found, refused error) {
	t.Helper()
	breaker := egressbreaker.New(egressbreaker.Config{RejectionThreshold: 1, UnreachableThreshold: 1})
	permit, err := breaker.Allow(egressTestProxy)
	if err != nil {
		t.Fatalf("Allow: %v", err)
	}
	found = permit.Done(fault)
	_, refused = breaker.Allow(egressTestProxy)
	if refused == nil {
		t.Fatal("the breaker did not open")
	}
	wrap := func(inner error) error {
		return &url.Error{Op: "Post", URL: "https://chatgpt.com/backend-api/codex/responses", Err: inner}
	}
	return wrap(found), wrap(refused)
}

func socksRejection() error {
	return egressbreaker.AuthRejected(&net.OpError{
		Op: "socks connect", Net: "tcp4",
		Err: errors.New("username/password authentication failed"),
	})
}

func wantNoAddresses(t *testing.T, text string) {
	t.Helper()
	for _, secret := range []string{"203.0.113.7", "chatgpt.com", "tenant-a", "expired", "socks"} {
		if strings.Contains(text, secret) {
			t.Fatalf("%q names %q", text, secret)
		}
	}
}

// A proxy failure used to reach the client as a bare 500 quoting the upstream
// URL and the proxy's address.
func TestProxyFailureIsAnsweredWith503AndACode(t *testing.T) {
	found, refused := proxyFailures(t, socksRejection())

	for name, convert := range map[string]func(error) *StreamErrorMessage{
		"executionError":            executionError,
		"streamErrorMessageFromErr": streamErrorMessageFromErr,
	} {
		for kind, err := range map[string]error{"found": found, "refused": refused} {
			msg := convert(err)
			if msg.StatusCode != http.StatusServiceUnavailable {
				t.Fatalf("%s(%s): status = %d, want 503", name, kind, msg.StatusCode)
			}
			body := msg.Error.Error()
			if !gjson.Valid(body) {
				t.Fatalf("%s(%s): body is not JSON: %s", name, kind, body)
			}
			if code := gjson.Get(body, "error.code").String(); code != "egress_unavailable" {
				t.Fatalf("%s(%s): code = %q: %s", name, kind, code, body)
			}
			if errType := gjson.Get(body, "error.type").String(); errType != "server_error" {
				t.Fatalf("%s(%s): type = %q: %s", name, kind, errType, body)
			}
			if seconds := gjson.Get(body, "error.reset_seconds").Int(); seconds != 15 {
				t.Fatalf("%s(%s): reset_seconds = %d, want the breaker's 15: %s", name, kind, seconds, body)
			}
			message := gjson.Get(body, "error.message").String()
			if !strings.Contains(message, "proxy authentication rejected") || !strings.Contains(message, "Retry in 15 seconds") {
				t.Fatalf("%s(%s): message = %q", name, kind, message)
			}
			if sent := strings.Contains(message, "was not sent"); sent != (kind == "refused") {
				t.Fatalf("%s(%s): message = %q, 'not sent' belongs to the refusal only", name, kind, message)
			}
			wantNoAddresses(t, body)
		}
	}
}

// Go's dial timeout matches context.DeadlineExceeded, which made a proxy that
// could not be reached look like the client giving up (499).
func TestUnreachableProxyIsNotReportedAsAClientCancellation(t *testing.T) {
	dialTimeout := &egressbreaker.HopError{
		Fault: egressbreaker.FaultUnreachable,
		Err:   &net.OpError{Op: "dial", Net: "tcp4", Err: context.DeadlineExceeded},
	}
	found, refused := proxyFailures(t, dialTimeout)
	if !errors.Is(found, context.DeadlineExceeded) {
		t.Fatal("the test error does not carry the deadline it is about")
	}
	for kind, err := range map[string]error{"found": found, "refused": refused} {
		msg := executionError(err)
		if msg.StatusCode != http.StatusServiceUnavailable {
			t.Fatalf("%s: status = %d, want 503", kind, msg.StatusCode)
		}
		if !strings.Contains(msg.Error.Error(), "cannot connect to the proxy") {
			t.Fatalf("%s: body = %s", kind, msg.Error)
		}
	}

	// The client really giving up, and any other failure, are answered as before.
	if msg := executionError(context.Canceled); msg.StatusCode != 499 {
		t.Fatalf("a cancelled request is answered with %d, want 499", msg.StatusCode)
	}
	if msg := executionError(errors.New("unexpected EOF")); msg.StatusCode != http.StatusInternalServerError {
		t.Fatalf("an unclassified failure is answered with %d, want 500", msg.StatusCode)
	}
	if msg := streamErrorMessageFromErr(errors.New("unexpected EOF")); msg.StatusCode != http.StatusInternalServerError {
		t.Fatalf("an unclassified stream failure is answered with %d, want 500", msg.StatusCode)
	}
}

func TestProxyFailureResponseInEachProtocol(t *testing.T) {
	gin.SetMode(gin.TestMode)
	_, refused := proxyFailures(t, socksRejection())

	write := func(format ErrorFormat) *httptest.ResponseRecorder {
		recorder := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(recorder)
		c.Request = httptest.NewRequest(http.MethodPost, "/", nil)
		// passthrough-headers is off: the Retry-After is the server's own and
		// goes out regardless.
		NewBaseAPIHandlers(nil, nil).WriteErrorResponseAs(c, executionError(refused), format)
		return recorder
	}

	for name, format := range map[string]ErrorFormat{"openai": ErrorFormatOpenAI, "claude": ErrorFormatClaude, "gemini": ErrorFormatGemini} {
		recorder := write(format)
		if recorder.Code != http.StatusServiceUnavailable {
			t.Fatalf("%s: status = %d, want 503", name, recorder.Code)
		}
		if got := recorder.Header().Get("Retry-After"); got != "15" {
			t.Fatalf("%s: Retry-After = %q, want 15", name, got)
		}
		wantNoAddresses(t, recorder.Body.String())
	}

	openai := write(ErrorFormatOpenAI).Body.Bytes()
	if gjson.GetBytes(openai, "error.code").String() != "egress_unavailable" || gjson.GetBytes(openai, "error.reset_seconds").Int() != 15 {
		t.Fatalf("openai body = %s", openai)
	}
	claude := write(ErrorFormatClaude).Body.Bytes()
	if gjson.GetBytes(claude, "type").String() != "error" || gjson.GetBytes(claude, "error.type").String() != "overloaded_error" {
		t.Fatalf("claude body = %s, want an overloaded_error clients back off on", claude)
	}
	if !strings.Contains(gjson.GetBytes(claude, "error.message").String(), "egress_unavailable") {
		t.Fatalf("claude body = %s, want the message to name the code", claude)
	}
	gemini := write(ErrorFormatGemini).Body.Bytes()
	if gjson.GetBytes(gemini, "error.status").String() != "UNAVAILABLE" || gjson.GetBytes(gemini, "error.code").Int() != 503 {
		t.Fatalf("gemini body = %s", gemini)
	}
}

func TestLocalRetryAfterIsOnlySentForOurOwnErrors(t *testing.T) {
	if got := localRetryAfter(nil); got != "" {
		t.Fatalf("localRetryAfter(nil) = %q", got)
	}
	upstream := &StreamErrorMessage{StatusCode: http.StatusTooManyRequests, Error: errors.New("rate limit"), Addon: http.Header{"Retry-After": {"30"}}}
	if got := localRetryAfter(upstream); got != "" {
		t.Fatalf("an upstream Retry-After was taken for our own: %q", got)
	}
	// A proxy failure below the breaker's threshold names no wait.
	breaker := egressbreaker.New(egressbreaker.Config{})
	permit, _ := breaker.Allow(egressTestProxy)
	stillClosed := permit.Done(&egressbreaker.HopError{Fault: egressbreaker.FaultUnreachable, Err: errors.New("connect: connection refused")})
	msg := executionError(stillClosed)
	if msg.StatusCode != http.StatusServiceUnavailable || localRetryAfter(msg) != "" {
		t.Fatalf("status = %d, Retry-After = %q; want 503 without a wait", msg.StatusCode, localRetryAfter(msg))
	}
	if gjson.Get(msg.Error.Error(), "error.reset_seconds").Exists() {
		t.Fatalf("body names a wait the breaker did not set: %s", msg.Error)
	}
}
