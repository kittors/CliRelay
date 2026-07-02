package management

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

func TestPostQuotaClearStatusClearsAuthByIndex(t *testing.T) {
	gin.SetMode(gin.TestMode)

	manager := coreauth.NewManager(nil, nil, nil)
	next := time.Now().Add(30 * time.Minute)
	registered, err := manager.Register(context.Background(), &coreauth.Auth{
		ID:             "codex-auth",
		Provider:       "codex",
		FileName:       "codex.json",
		Status:         coreauth.StatusError,
		StatusMessage:  "quota exhausted",
		Unavailable:    true,
		NextRetryAfter: next,
		LastError:      &coreauth.Error{Message: "quota exhausted", HTTPStatus: http.StatusTooManyRequests},
		Quota: coreauth.QuotaState{
			Exceeded:      true,
			Reason:        "quota",
			NextRecoverAt: next,
		},
	})
	if err != nil {
		t.Fatalf("Register() error = %v", err)
	}
	if registered.Index == "" {
		t.Fatalf("registered auth index is empty")
	}

	h := &Handler{authManager: manager}
	router := gin.New()
	router.POST("/quota/clear-status", h.PostQuotaClearStatus)

	body, err := json.Marshal(map[string]string{"authIndex": registered.Index})
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/quota/clear-status", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", rr.Code, http.StatusOK, rr.Body.String())
	}

	updated, ok := manager.GetByID(registered.ID)
	if !ok || updated == nil {
		t.Fatalf("GetByID() missing auth")
	}
	if updated.Status != coreauth.StatusActive {
		t.Fatalf("auth.Status = %q, want %q", updated.Status, coreauth.StatusActive)
	}
	if updated.StatusMessage != "" || updated.Unavailable || !updated.NextRetryAfter.IsZero() || updated.LastError != nil || updated.Quota != (coreauth.QuotaState{}) {
		t.Fatalf("quota runtime state was not cleared: %#v", updated)
	}
}
