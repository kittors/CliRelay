package management

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

func TestPostQuotaClearStatusClearsUnauthorizedAuthByIndex(t *testing.T) {
	gin.SetMode(gin.TestMode)

	manager := coreauth.NewManager(nil, nil, nil)
	registered, err := manager.Register(context.Background(), &coreauth.Auth{
		ID:        "xai-auth",
		Provider:  "xai",
		FileName:  "xai.json",
		Status:    coreauth.StatusError,
		LastError: &coreauth.Error{HTTPStatus: http.StatusUnauthorized},
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
	var response struct {
		Changed bool `json:"changed"`
	}
	if err = json.Unmarshal(rr.Body.Bytes(), &response); err != nil {
		t.Fatalf("Unmarshal(response) error = %v", err)
	}
	if !response.Changed {
		t.Fatalf("response.changed = false, want true; body=%s", rr.Body.String())
	}

	updated, ok := manager.GetByID(registered.ID)
	if !ok || updated == nil {
		t.Fatalf("GetByID() missing auth")
	}
	if updated.Status != coreauth.StatusActive {
		t.Fatalf("auth.Status = %q, want %q", updated.Status, coreauth.StatusActive)
	}
	if updated.StatusMessage != "" || updated.Unavailable || !updated.NextRetryAfter.IsZero() || updated.LastError != nil || updated.Quota != (coreauth.QuotaState{}) {
		t.Fatalf("401 runtime state was not cleared: %#v", updated)
	}
}
