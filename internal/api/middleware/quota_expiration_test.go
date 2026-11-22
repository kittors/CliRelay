package middleware

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

func TestQuotaMiddlewareSubscriptionExpiration(t *testing.T) {
	gin.SetMode(gin.TestMode)

	runRequest := func(metadata map[string]string) *httptest.ResponseRecorder {
		r := gin.New()
		r.Use(func(c *gin.Context) {
			c.Set("apiKey", "sk-XXXXexpirytest00001")
			c.Set("accessMetadata", metadata)
			c.Next()
		})
		r.Use(QuotaMiddleware())
		r.POST("/v1/chat/completions", func(c *gin.Context) {
			c.JSON(http.StatusOK, gin.H{"ok": true})
		})
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		return rec
	}

	// Expired account subscription must return 402 even when no other quota limits are set.
	past := time.Now().UTC().Add(-time.Minute).Format(time.RFC3339)
	rec := runRequest(map[string]string{
		"end-user-id":        "eu-expired-1",
		"account-expires-at": past,
	})
	if rec.Code != http.StatusPaymentRequired {
		t.Fatalf("expired account status = %d, want 402; body=%s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("X-CliRelay-Quota-Code"); got != "subscription_expired" {
		t.Fatalf("X-CliRelay-Quota-Code = %q, want subscription_expired", got)
	}
	var payload struct {
		Error struct {
			Code  string `json:"code"`
			Scope string `json:"scope"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("unmarshal body: %v", err)
	}
	if payload.Error.Code != "subscription_expired" || payload.Error.Scope != "account" {
		t.Fatalf("unexpected error payload: %+v", payload)
	}

	// Expired key subscription must return 402 with scope=key.
	recKey := runRequest(map[string]string{
		"api-key-id":     "key-expired-1",
		"key-expires-at": past,
	})
	if recKey.Code != http.StatusPaymentRequired {
		t.Fatalf("expired key status = %d, want 402; body=%s", recKey.Code, recKey.Body.String())
	}

	// Active future subscription must pass through with 200.
	future := time.Now().UTC().Add(24 * time.Hour).Format(time.RFC3339)
	recOK := runRequest(map[string]string{
		"end-user-id":        "eu-active-1",
		"account-expires-at": future,
		"key-expires-at":     future,
	})
	if recOK.Code != http.StatusOK {
		t.Fatalf("active subscription status = %d, want 200; body=%s", recOK.Code, recOK.Body.String())
	}
}
