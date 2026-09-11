package management

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/enduser"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/identity"
)

type errorEnvelope struct {
	Error struct {
		Code    string         `json:"code"`
		Message string         `json:"message"`
		Details map[string]any `json:"details"`
	} `json:"error"`
}

func decodeErrorEnvelope(t *testing.T, body []byte) errorEnvelope {
	t.Helper()
	var envelope errorEnvelope
	if err := json.Unmarshal(body, &envelope); err != nil {
		t.Fatalf("decode response %s: %v", body, err)
	}
	return envelope
}

// A rejected password must reach the browser as a per-rule code. Emitting only
// "validation_failed" is what forced the panel to print the raw English
// sentence in a toast: it had nothing to key a translation off.
func TestEndUserErrorEmitsPerRulePasswordCode(t *testing.T) {
	gin.SetMode(gin.TestMode)

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	_, err := enduser.HashPassword("alllowercase!")
	if err == nil {
		t.Fatal("fixture password was accepted; it must violate the uppercase rule")
	}

	endUserError(c, err)

	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusBadRequest)
	}
	envelope := decodeErrorEnvelope(t, recorder.Body.Bytes())
	if envelope.Error.Code != string(identity.PasswordMissingUpper) {
		t.Fatalf("code = %q, want %q", envelope.Error.Code, identity.PasswordMissingUpper)
	}
}

// identityError is the tenant-creation path: the reported symptom was an
// English toast when a tenant admin password missed a complexity rule.
func TestIdentityErrorEmitsPerRulePasswordCode(t *testing.T) {
	gin.SetMode(gin.TestMode)

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	_, err := identity.HashPassword("shortpw")
	if err == nil {
		t.Fatal("fixture password was accepted; it must violate the length rule")
	}

	identityError(c, err)

	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusBadRequest)
	}
	envelope := decodeErrorEnvelope(t, recorder.Body.Bytes())
	if envelope.Error.Code != string(identity.PasswordTooShort) {
		t.Fatalf("code = %q, want %q", envelope.Error.Code, identity.PasswordTooShort)
	}
	// The panel renders "at least N characters" from this instead of hardcoding
	// the bound a second time and drifting from the server.
	if limit, ok := envelope.Error.Details["limit"].(float64); !ok || int(limit) != identity.PasswordMinLength {
		t.Fatalf("details.limit = %v, want %d", envelope.Error.Details["limit"], identity.PasswordMinLength)
	}
}

// A cooldown response must say how long the wait is, in the body and in the
// standard header. Without it the panel can only say "try again later", which a
// user cannot distinguish from a one-minute or a one-hour lock.
func TestEndUserErrorReportsCooldownRemainingTime(t *testing.T) {
	gin.SetMode(gin.TestMode)

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)

	endUserError(c, &enduser.CooldownError{RetryAfter: 5 * time.Minute})

	if recorder.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusTooManyRequests)
	}
	if got := recorder.Header().Get("Retry-After"); got != "300" {
		t.Fatalf("Retry-After = %q, want %q", got, "300")
	}
	envelope := decodeErrorEnvelope(t, recorder.Body.Bytes())
	if envelope.Error.Code != "login_cooldown" {
		t.Fatalf("code = %q, want %q", envelope.Error.Code, "login_cooldown")
	}
	if seconds, ok := envelope.Error.Details["retry_after_seconds"].(float64); !ok || int(seconds) != 300 {
		t.Fatalf("details.retry_after_seconds = %v, want 300", envelope.Error.Details["retry_after_seconds"])
	}
}

// Only a compared-and-mismatched password may consume the guess budget.
//
// The removed throttle middleware inferred this from the HTTP status and
// charged every 401, which swept in the refresh endpoint rejecting a session
// the server itself had just revoked: resetting someone's password spent one of
// their login attempts before they ever reached the form.
func TestOnlyWrongPasswordConsumesThePortalGuessBudget(t *testing.T) {
	charging := []error{enduser.ErrInvalidCredentials}
	for _, err := range charging {
		if !isPortalGuessFailure(err) {
			t.Fatalf("isPortalGuessFailure(%v) = false, want true", err)
		}
	}

	free := []error{
		enduser.ErrSessionRevoked,
		enduser.ErrSessionExpired,
		enduser.ErrLoginCooldowned,
		&enduser.CooldownError{RetryAfter: time.Minute},
		enduser.ErrAccountDisabled,
		enduser.ErrAccountLocked,
		enduser.ErrTenantSuspended,
		enduser.ErrTenantExpired,
		errors.New("database unavailable"),
	}
	for _, err := range free {
		if isPortalGuessFailure(err) {
			t.Fatalf("isPortalGuessFailure(%v) = true, want false: it carries no information about the password", err)
		}
	}
}
