package management

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
)

func TestPostOAuthCallbackAcceptsAlreadyProcessedState(t *testing.T) {
	gin.SetMode(gin.TestMode)

	useIsolatedOAuthSessions(t, time.Minute)

	RegisterOAuthSession("session-1", "codex")
	CompleteOAuthSession("session-1")
	CompleteOAuthSessionsByProvider("codex")

	h := &Handler{
		cfg: &config.Config{
			AuthDir: t.TempDir(),
		},
	}

	body := []byte(`{"provider":"codex","redirect_url":"http://localhost:1455/auth/callback?code=test-code&state=session-1"}`)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	req := httptest.NewRequest(http.MethodPost, "/oauth-callback", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	c.Request = req

	h.PostOAuthCallback(c)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d, body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	if !bytes.Contains(rec.Body.Bytes(), []byte(`"already_processed":true`)) {
		t.Fatalf("expected already_processed response, got %s", rec.Body.String())
	}
}

func TestPostOAuthCallbackReturnsSessionStatusWhenFlowIsNoLongerPending(t *testing.T) {
	gin.SetMode(gin.TestMode)

	useIsolatedOAuthSessions(t, time.Minute)

	RegisterOAuthSession("session-timeout", "codex")
	SetOAuthSessionError("session-timeout", "Timeout waiting for OAuth callback")

	h := &Handler{
		cfg: &config.Config{
			AuthDir: t.TempDir(),
		},
	}

	body := []byte(`{"provider":"codex","redirect_url":"http://localhost:1455/auth/callback?code=test-code&state=session-timeout"}`)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	req := httptest.NewRequest(http.MethodPost, "/oauth-callback", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	c.Request = req

	h.PostOAuthCallback(c)

	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want %d, body=%s", rec.Code, http.StatusConflict, rec.Body.String())
	}
	if !bytes.Contains(rec.Body.Bytes(), []byte(`Timeout waiting for OAuth callback`)) {
		t.Fatalf("expected session status in response, got %s", rec.Body.String())
	}
}

func TestPostOAuthCallbackAcceptsDirectCodeAndState(t *testing.T) {
	gin.SetMode(gin.TestMode)

	useIsolatedOAuthSessions(t, time.Minute)

	RegisterOAuthSession("xai-state", "xai")

	h := &Handler{
		cfg: &config.Config{
			AuthDir: t.TempDir(),
		},
	}

	body := []byte(`{"provider":"xai","code":"manual-code","state":"xai-state"}`)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	req := httptest.NewRequest(http.MethodPost, "/oauth-callback", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	c.Request = req

	h.PostOAuthCallback(c)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d, body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	payload, err := WaitOAuthCallbackFile(h.cfg.AuthDir, "xai", "xai-state", time.Second)
	if err != nil {
		t.Fatalf("WaitOAuthCallbackFile() error = %v", err)
	}
	if payload["code"] != "manual-code" || payload["state"] != "xai-state" {
		t.Fatalf("payload = %#v, want manual code and xai-state", payload)
	}
}
