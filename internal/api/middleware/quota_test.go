package middleware

import (
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestQuotaMiddlewareEnforcesConcurrencyLimitPerKey(t *testing.T) {
	gin.SetMode(gin.TestMode)
	resetQuotaMiddlewareState(t)

	entered := make(chan struct{})
	release := make(chan struct{})
	var enteredOnce sync.Once

	router := gin.New()
	router.Use(func(c *gin.Context) {
		key := c.GetHeader("X-Test-Key")
		if key == "" {
			key = "key-a"
		}
		c.Set("apiKey", key)
		c.Set("accessMetadata", map[string]string{"concurrency-limit": "1"})
		c.Next()
	})
	router.Use(QuotaMiddleware())
	router.POST("/v1/chat/completions", func(c *gin.Context) {
		if key, _ := c.Get("apiKey"); key == "key-a" {
			enteredOnce.Do(func() { close(entered) })
			<-release
		}
		c.Status(http.StatusNoContent)
	})

	firstDone := make(chan struct{})
	first := httptest.NewRecorder()
	go func() {
		defer close(firstDone)
		router.ServeHTTP(first, newQuotaPostRequest("key-a"))
	}()

	<-entered

	secondSameKey := httptest.NewRecorder()
	router.ServeHTTP(secondSameKey, newQuotaPostRequest("key-a"))
	if secondSameKey.Code != http.StatusTooManyRequests {
		t.Fatalf("same-key concurrent status = %d, want %d", secondSameKey.Code, http.StatusTooManyRequests)
	}

	secondOtherKey := httptest.NewRecorder()
	router.ServeHTTP(secondOtherKey, newQuotaPostRequest("key-b"))
	if secondOtherKey.Code != http.StatusNoContent {
		t.Fatalf("other-key concurrent status = %d, want %d", secondOtherKey.Code, http.StatusNoContent)
	}

	close(release)
	<-firstDone
	if first.Code != http.StatusNoContent {
		t.Fatalf("first status = %d, want %d", first.Code, http.StatusNoContent)
	}

	afterRelease := httptest.NewRecorder()
	router.ServeHTTP(afterRelease, newQuotaPostRequest("key-a"))
	if afterRelease.Code != http.StatusNoContent {
		t.Fatalf("after-release status = %d, want %d", afterRelease.Code, http.StatusNoContent)
	}
}

func newQuotaPostRequest(key string) *http.Request {
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	req.Header.Set("X-Test-Key", key)
	return req
}

func resetQuotaMiddlewareState(t *testing.T) {
	t.Helper()

	rpmTrackers = sync.Map{}
	tpmTrackers = sync.Map{}
	snapshotLimits = sync.Map{}
	inFlightMu.Lock()
	inFlightByKey = map[string]int{}
	inFlightMu.Unlock()
}
