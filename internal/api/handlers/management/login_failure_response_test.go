package management

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/identity"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/loginlockout"
)

func newLoginFailureContext(t *testing.T) (*gin.Context, *httptest.ResponseRecorder) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v0/auth/login", nil)
	return c, recorder
}

// Users reported being locked out with no warning: every wrong password said
// only "incorrect username or password", so the lock was the first sign that
// there was a limit at all. Each failure short of the lock now says how many
// attempts remain.
func TestRespondCredentialFailureWarnsBeforeTheLock(t *testing.T) {
	h := NewHandler(nil, "", nil)
	t.Cleanup(h.Close)
	acctKey := accountThrottleKey(scopeUserAccount, "admin")
	now := time.Now()

	cases := []struct {
		name          string
		err           error
		acct          throttleDecision
		wantRemaining int
	}{
		{
			// An unknown username has no ladder, but the account bucket counts
			// the typed name all the same, so the warning reads exactly as it
			// would for a real account.
			name:          "unknown username uses the account bucket",
			err:           identity.ErrInvalidCredentials,
			acct:          throttleDecision{ShortCount: 1, LongCount: 1},
			wantRemaining: 4,
		},
		{
			// After a restart the in-memory bucket starts empty while the
			// database ladder remembers; the lock arrives at whichever is first.
			name:          "an existing account takes the smaller headroom",
			err:           loginlockout.NewFailure(identity.ErrInvalidCredentials, 3, time.Time{}, now),
			acct:          throttleDecision{ShortCount: 1, LongCount: 1},
			wantRemaining: 2,
		},
		{
			name:          "the daily ceiling counts too",
			err:           identity.ErrInvalidCredentials,
			acct:          throttleDecision{ShortCount: 1, LongCount: 48},
			wantRemaining: 2,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, recorder := newLoginFailureContext(t)
			h.respondCredentialFailure(c, tc.err, acctKey, throttleDecision{}, tc.acct)

			if recorder.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want %d; body=%s", recorder.Code, http.StatusUnauthorized, recorder.Body.String())
			}
			envelope := decodeErrorEnvelope(t, recorder.Body.Bytes())
			if envelope.Error.Code != "invalid_credentials" {
				t.Fatalf("code = %q, want invalid_credentials", envelope.Error.Code)
			}
			if got, ok := envelope.Error.Details["remaining_attempts"].(float64); !ok || int(got) != tc.wantRemaining {
				t.Fatalf("details.remaining_attempts = %v, want %d", envelope.Error.Details["remaining_attempts"], tc.wantRemaining)
			}
		})
	}
}

// The failure that trips a lock used to be answered as a plain wrong password,
// so users only found the lock by walking into it — and then were told "try
// again later" with no length. It now reports the lock it tripped, and the
// longest one when several trip at once: the user can sign in only after all
// of them have lapsed.
func TestRespondCredentialFailureReportsTheLockItTrips(t *testing.T) {
	h := NewHandler(nil, "", nil)
	t.Cleanup(h.Close)
	acctKey := accountThrottleKey(scopeUserAccount, "admin")
	now := time.Now()

	cases := []struct {
		name        string
		err         error
		ip, acct    throttleDecision
		wantCode    string
		wantSeconds int
	}{
		{
			name:        "the account bucket trips",
			err:         identity.ErrInvalidCredentials,
			acct:        throttleDecision{Outcome: outcomeRateLimited, RetryAfter: 59*time.Second + 200*time.Millisecond, ShortCount: 5},
			wantCode:    "login_rate_limited",
			wantSeconds: 60,
		},
		{
			name:        "the ladder's longer cooldown wins",
			err:         loginlockout.NewFailure(identity.ErrInvalidCredentials, 10, now.Add(5*time.Minute), now),
			acct:        throttleDecision{Outcome: outcomeRateLimited, RetryAfter: time.Minute, ShortCount: 5},
			wantCode:    "login_cooldown",
			wantSeconds: 300,
		},
		{
			name:        "the per-IP bucket trips",
			err:         identity.ErrInvalidCredentials,
			ip:          throttleDecision{Outcome: outcomeRateLimited, RetryAfter: 15 * time.Minute},
			acct:        throttleDecision{ShortCount: 2, LongCount: 2},
			wantCode:    "login_rate_limited",
			wantSeconds: 900,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, recorder := newLoginFailureContext(t)
			h.respondCredentialFailure(c, tc.err, acctKey, tc.ip, tc.acct)

			if recorder.Code != http.StatusTooManyRequests {
				t.Fatalf("status = %d, want %d; body=%s", recorder.Code, http.StatusTooManyRequests, recorder.Body.String())
			}
			envelope := decodeErrorEnvelope(t, recorder.Body.Bytes())
			if envelope.Error.Code != tc.wantCode {
				t.Fatalf("code = %q, want %q", envelope.Error.Code, tc.wantCode)
			}
			if got, ok := envelope.Error.Details["retry_after_seconds"].(float64); !ok || int(got) != tc.wantSeconds {
				t.Fatalf("details.retry_after_seconds = %v, want %d", envelope.Error.Details["retry_after_seconds"], tc.wantSeconds)
			}
			if got := recorder.Header().Get("Retry-After"); got == "" {
				t.Fatal("Retry-After header missing")
			}
		})
	}
}

// The admin sign-in sent the wait only in the Retry-After header. The panel
// reads error bodies, so every lock rendered as "try again later".
func TestAbortLoginThrottledReportsTheWaitInTheBody(t *testing.T) {
	c, recorder := newLoginFailureContext(t)

	abortLoginThrottled(c, throttleDecision{Outcome: outcomeRateLimited, RetryAfter: 4*time.Minute + 30*time.Second})

	if recorder.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusTooManyRequests)
	}
	if got := recorder.Header().Get("Retry-After"); got != "270" {
		t.Fatalf("Retry-After = %q, want 270", got)
	}
	envelope := decodeErrorEnvelope(t, recorder.Body.Bytes())
	if envelope.Error.Code != "login_rate_limited" {
		t.Fatalf("code = %q, want login_rate_limited", envelope.Error.Code)
	}
	if got, ok := envelope.Error.Details["retry_after_seconds"].(float64); !ok || int(got) != 270 {
		t.Fatalf("details.retry_after_seconds = %v, want 270", envelope.Error.Details["retry_after_seconds"])
	}
}
