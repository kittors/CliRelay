package management

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func TestDetachedAuthContextPreservesRequestInfoWithoutCancellation(t *testing.T) {
	gin.SetMode(gin.TestMode)

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)

	req := httptest.NewRequest(http.MethodGet, "/auth?provider=claude", nil)
	req.Header.Set("X-Test", "value")
	reqCtx, cancel := context.WithCancel(req.Context())
	c.Request = req.WithContext(reqCtx)

	ctx := detachedAuthContext(c)
	cancel()

	select {
	case <-ctx.Done():
		t.Fatal("detached auth context should not be canceled with request context")
	default:
	}

	info := coreauth.GetRequestInfo(ctx)
	if info == nil {
		t.Fatal("expected request info to be preserved")
	}
	if got := info.Query.Get("provider"); got != "claude" {
		t.Fatalf("expected provider query to be preserved, got %q", got)
	}
	if got := info.Headers.Get("X-Test"); got != "value" {
		t.Fatalf("expected header to be preserved, got %q", got)
	}
}

func TestRequestAuthContextFollowsRequestCancellationAndPreservesRequestInfo(t *testing.T) {
	gin.SetMode(gin.TestMode)

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)

	req := httptest.NewRequest(http.MethodGet, "/auth?provider=codex", nil)
	req.Header.Set("X-Test", "value")
	reqCtx, cancel := context.WithCancel(req.Context())
	c.Request = req.WithContext(reqCtx)

	ctx := requestAuthContext(c)

	info := coreauth.GetRequestInfo(ctx)
	if info == nil {
		t.Fatal("expected request info to be preserved")
	}
	if got := info.Query.Get("provider"); got != "codex" {
		t.Fatalf("expected provider query to be preserved, got %q", got)
	}
	if got := info.Headers.Get("X-Test"); got != "value" {
		t.Fatalf("expected header to be preserved, got %q", got)
	}

	cancel()

	select {
	case <-ctx.Done():
	case <-time.After(250 * time.Millisecond):
		t.Fatal("request auth context should be canceled with request context")
	}
}
