package management

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

// TestPostgresAdminSignInWarnsThenReportsTheLock drives POST /v0/auth/login
// against a real database through the sequence a production administrator hit:
// three mistypes locked the account without a word, the correct password typed
// into the lock came back as "disabled or locked, contact your administrator",
// they read it as a wrong password, tried another one, and gave up for an hour
// and a half.
func TestPostgresAdminSignInWarnsThenReportsTheLock(t *testing.T) {
	dsn := strings.TrimSpace(os.Getenv("CLIRELAY_POSTGRES_TEST_DSN"))
	if dsn == "" {
		t.Skip("CLIRELAY_POSTGRES_TEST_DSN is not set")
	}
	ctx := context.Background()
	db, service := newDisposableIdentityService(t, ctx, dsn, "login_lockout")
	const correctPassword = "Bootstrap-Password-123!"

	newLoginHandler := func() *Handler {
		h := NewHandler(nil, "", nil)
		h.identityService = service
		t.Cleanup(h.Close)
		return h
	}
	signIn := func(h *Handler, password string) (int, errorEnvelope) {
		t.Helper()
		gin.SetMode(gin.TestMode)
		router := gin.New()
		router.POST("/v0/auth/login", h.PostLogin)
		payload, err := json.Marshal(map[string]string{"username": "admin", "password": password})
		if err != nil {
			t.Fatal(err)
		}
		req := httptest.NewRequest(http.MethodPost, "/v0/auth/login", bytes.NewReader(payload))
		req.Header.Set("Content-Type", "application/json")
		req.RemoteAddr = "203.0.113.7:4321"
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		if rec.Code == http.StatusOK {
			return rec.Code, errorEnvelope{}
		}
		return rec.Code, decodeErrorEnvelope(t, rec.Body.Bytes())
	}
	retryAfter := func(envelope errorEnvelope) int {
		seconds, _ := envelope.Error.Details["retry_after_seconds"].(float64)
		return int(seconds)
	}

	h := newLoginHandler()
	// Each wrong password short of the lock says how many attempts remain.
	for want := 4; want >= 1; want-- {
		status, envelope := signIn(h, "wrong-password")
		if status != http.StatusUnauthorized || envelope.Error.Code != "invalid_credentials" {
			t.Fatalf("wrong password = %d %q, want 401 invalid_credentials", status, envelope.Error.Code)
		}
		if got, _ := envelope.Error.Details["remaining_attempts"].(float64); int(got) != want {
			t.Fatalf("remaining_attempts = %v, want %d", envelope.Error.Details["remaining_attempts"], want)
		}
	}
	// The one that trips the lock says so, with the wait, instead of posing as
	// one more wrong password.
	status, envelope := signIn(h, "wrong-password")
	if status != http.StatusTooManyRequests {
		t.Fatalf("fifth wrong password = %d %q, want 429", status, envelope.Error.Code)
	}
	if seconds := retryAfter(envelope); seconds <= 0 || seconds > 60 {
		t.Fatalf("retry_after_seconds = %d, want within the one-minute rung", seconds)
	}
	// The correct password typed into the lock gets the lock and its wait,
	// never the administrative "account_locked".
	status, envelope = signIn(h, correctPassword)
	if status != http.StatusTooManyRequests || envelope.Error.Code == "account_locked" || retryAfter(envelope) <= 0 {
		t.Fatalf("correct password during the lock = %d %q (retry %d), want 429 with a wait", status, envelope.Error.Code, retryAfter(envelope))
	}

	// After a restart, or on another node, the in-memory buckets are empty and
	// only the database ladder holds the door. It must answer the same way.
	other := newLoginHandler()
	status, envelope = signIn(other, correctPassword)
	if status != http.StatusTooManyRequests || envelope.Error.Code != "login_cooldown" {
		t.Fatalf("correct password during the database cooldown = %d %q, want 429 login_cooldown", status, envelope.Error.Code)
	}
	if seconds := retryAfter(envelope); seconds <= 0 || seconds > 60 {
		t.Fatalf("retry_after_seconds = %d, want within the one-minute rung", seconds)
	}

	// The lock lapses on its own; nobody has to contact an administrator.
	if _, err := db.ExecContext(ctx,
		`UPDATE users SET locked_until = now() - interval '1 second' WHERE username_normalized = 'admin'`); err != nil {
		t.Fatal(err)
	}
	if status, envelope = signIn(other, correctPassword); status != http.StatusOK {
		t.Fatalf("correct password after the cooldown = %d %q, want 200", status, envelope.Error.Code)
	}
}
