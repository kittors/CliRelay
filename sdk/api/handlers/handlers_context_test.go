package handlers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/util"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v6/sdk/translator"
)

func TestRequestContextOrBackgroundUsesRequestContextWhenPresent(t *testing.T) {
	gin.SetMode(gin.TestMode)

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)

	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	reqCtx, cancel := context.WithCancel(req.Context())
	c.Request = req.WithContext(reqCtx)

	ctx := requestContextOrBackground(c)
	cancel()

	select {
	case <-ctx.Done():
	case <-time.After(250 * time.Millisecond):
		t.Fatal("expected requestContextOrBackground to return the request context when present")
	}
}

func TestCompletedToolBlockNegotiationIsExact(t *testing.T) {
	for _, values := range [][]string{{sdktranslator.CompletedToolBlocksVersion}, {"future-v2"}, {sdktranslator.CompletedToolBlocksVersion, sdktranslator.CompletedToolBlocksVersion}, nil} {
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
		for _, value := range values {
			c.Request.Header.Add(sdktranslator.CompletedToolBlocksHeader, value)
		}
		c.Request.Header.Set("Authorization", "fixture-secret")
		got := requestExecutionHeaders(context.WithValue(context.Background(), util.ContextKeyGin, c))
		want := len(values) == 1 && values[0] == sdktranslator.CompletedToolBlocksVersion
		if (got.Get(sdktranslator.CompletedToolBlocksHeader) == sdktranslator.CompletedToolBlocksVersion) != want || got.Get("Authorization") != "" {
			t.Fatal("incorrect protocol extraction")
		}
	}
}

func TestRequestContextOrBackgroundFallsBackWhenRequestMissing(t *testing.T) {
	gin.SetMode(gin.TestMode)

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)

	ctx := requestContextOrBackground(c)
	if ctx == nil {
		t.Fatal("expected non-nil fallback context")
	}

	select {
	case <-ctx.Done():
		t.Fatal("fallback context should not be canceled")
	default:
	}
}
