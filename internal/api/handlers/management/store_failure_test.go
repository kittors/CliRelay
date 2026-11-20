package management

import (
	"bytes"
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"syscall"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/identity"
)

// unreachableDBError is what pgx reports when the link to the database is
// down. It names the database host and role, which must not reach the client.
var unreachableDBError = fmt.Errorf("failed to connect to `user=cliproxy database=cliproxy`: 10.0.0.1:25432 (10.0.0.1): dial error: %w",
	&net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNREFUSED})

// unreachableConnector stands in for a database whose link is down: every
// connection attempt fails the way pgx's does.
type unreachableConnector struct{}

func (unreachableConnector) Connect(context.Context) (driver.Conn, error) {
	return nil, unreachableDBError
}
func (unreachableConnector) Driver() driver.Driver { return unreachableDriver{} }

type unreachableDriver struct{}

func (unreachableDriver) Open(string) (driver.Conn, error) { return nil, unreachableDBError }

func decodeErrorBody(t *testing.T, rec *httptest.ResponseRecorder) (code, message string) {
	t.Helper()
	var body struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode error body %q: %v", rec.Body.String(), err)
	}
	return body.Error.Code, body.Error.Message
}

func assertStoreUnavailable(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503; body=%s", rec.Code, rec.Body.String())
	}
	if code, _ := decodeErrorBody(t, rec); code != "identity_unavailable" {
		t.Fatalf("code = %q, want identity_unavailable", code)
	}
	if got := rec.Header().Get("Retry-After"); got != storeRetryAfterSeconds {
		t.Fatalf("Retry-After = %q, want %q", got, storeRetryAfterSeconds)
	}
	if body := rec.Body.String(); strings.Contains(body, "10.0.0.1") || strings.Contains(body, "cliproxy") {
		t.Fatalf("body leaks the connect error: %s", body)
	}
}

func TestIdentityErrorSeparatesStoreFailuresFromCredentialDecisions(t *testing.T) {
	gin.SetMode(gin.TestMode)
	answer := func(err error) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(rec)
		c.Request = httptest.NewRequest(http.MethodPost, "/v0/auth/refresh", nil)
		identityError(c, err)
		return rec
	}

	for _, tc := range []struct {
		err    error
		status int
		code   string
	}{
		{identity.ErrInvalidCredentials, http.StatusUnauthorized, "invalid_credentials"},
		{identity.ErrSessionRevoked, http.StatusUnauthorized, "session_revoked"},
		{identity.ErrSessionExpired, http.StatusUnauthorized, "session_expired"},
		{sql.ErrNoRows, http.StatusNotFound, "not_found"},
		{errors.New("boom"), http.StatusInternalServerError, "internal_error"},
	} {
		rec := answer(tc.err)
		if code, _ := decodeErrorBody(t, rec); rec.Code != tc.status || code != tc.code {
			t.Errorf("identityError(%v) = %d %q, want %d %q", tc.err, rec.Code, code, tc.status, tc.code)
		}
	}
	assertStoreUnavailable(t, answer(unreachableDBError))
}

func TestEndUserErrorAnswersStoreOutageWith503(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v0/portal/auth/refresh", nil)
	endUserError(c, unreachableDBError)
	assertStoreUnavailable(t, rec)

	// Other unexpected errors keep the existing 500 answer, message included.
	rec = httptest.NewRecorder()
	c, _ = gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodGet, "/v0/portal/keys", nil)
	endUserError(c, errors.New("boom"))
	if code, message := decodeErrorBody(t, rec); rec.Code != http.StatusInternalServerError || code != "internal_error" || message != "boom" {
		t.Fatalf("endUserError(boom) = %d %q %q, want 500 internal_error boom", rec.Code, code, message)
	}
}

// The 2026-11-20 regression: with the database unreachable for a minute, the
// panel's polling and its token refresh got 401 and the admin was signed out.
// Session checks, refresh and login must all report the outage instead.
func TestSessionEndpointsReportStoreOutageInsteadOf401(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := sql.OpenDB(unreachableConnector{})
	t.Cleanup(func() { _ = db.Close() })
	h := NewHandler(nil, "", nil)
	h.identityService = identity.NewService(db)
	t.Cleanup(h.Close)

	router := gin.New()
	reached := false
	router.POST("/v0/auth/refresh", h.PostRefresh)
	router.POST("/v0/auth/login", h.PostLogin)
	managed := router.Group("/v0/management", h.Middleware())
	managed.GET("/usage/monitor/in-flight", func(c *gin.Context) {
		reached = true
		c.Status(http.StatusOK)
	})

	serve := func(method, path, token string, body []byte) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(method, path, bytes.NewReader(body))
		req.RemoteAddr = "127.0.0.1:4321"
		req.Header.Set("Content-Type", "application/json")
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		return rec
	}

	t.Run("session check", func(t *testing.T) {
		assertStoreUnavailable(t, serve(http.MethodGet, "/v0/management/usage/monitor/in-flight", "cps_"+strings.Repeat("a", 43), nil))
		if reached {
			t.Fatal("request reached the handler without an authenticated session")
		}
	})
	t.Run("refresh", func(t *testing.T) {
		body := []byte(`{"refresh_token":"cpr_adm_` + strings.Repeat("a", 43) + `"}`)
		assertStoreUnavailable(t, serve(http.MethodPost, "/v0/auth/refresh", "", body))
	})
	t.Run("login", func(t *testing.T) {
		body := []byte(`{"username":"admin","password":"Some-Password-1!"}`)
		assertStoreUnavailable(t, serve(http.MethodPost, "/v0/auth/login", "", body))
	})
}
