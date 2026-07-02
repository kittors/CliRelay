package middleware

import (
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/gin-gonic/gin"
)

// setupQuotaTestCost overrides the injected usage query funcs so the daily-spending
// limit check sees a fixed today-cost. It restores no-ops on cleanup.
func setupQuotaTestCost(t *testing.T, todayCost float64) {
	t.Helper()
	set := func(cost float64) {
		InitQuotaUsageFuncs(
			func(string) (int64, error) { return 0, nil },
			func(string) (int64, error) { return 0, nil },
			func(string) (float64, error) { return 0, nil },
			func(string) (float64, error) { return cost, nil },
		)
	}
	set(todayCost)
	t.Cleanup(func() { set(0) })
}

// TestQuotaMiddlewareDailySpendingLimit verifies the per-day cost cap rejects
// requests once the same-day cost reaches the configured limit, and otherwise
// lets them through.
func TestQuotaMiddlewareDailySpendingLimit(t *testing.T) {
	gin.SetMode(gin.TestMode)
	resetQuotaMiddlewareState(t)
	// Injected same-day cost is $50 for all cases.
	setupQuotaTestCost(t, 50)

	tests := []struct {
		name       string
		metadata   map[string]string
		wantStatus int
	}{
		{
			name:       "exceeded rejects with 429",
			metadata:   map[string]string{"daily-spending-limit": "40"},
			wantStatus: http.StatusTooManyRequests,
		},
		{
			name:       "exactly at limit rejects with 429",
			metadata:   map[string]string{"daily-spending-limit": "50"},
			wantStatus: http.StatusTooManyRequests,
		},
		{
			name:       "under limit allowed",
			metadata:   map[string]string{"daily-spending-limit": "100"},
			wantStatus: http.StatusOK,
		},
		{
			name:       "no limit configured allowed",
			metadata:   map[string]string{},
			wantStatus: http.StatusOK,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := gin.New()
			g.Use(func(c *gin.Context) {
				c.Set("apiKey", "sk-test-daily-spending")
				c.Set("accessMetadata", tt.metadata)
				c.Next()
			})
			g.Use(QuotaMiddleware())
			g.POST("/v1/test", func(c *gin.Context) { c.Status(http.StatusOK) })

			req := httptest.NewRequest(http.MethodPost, "/v1/test", nil)
			w := httptest.NewRecorder()
			g.ServeHTTP(w, req)

			if w.Code != tt.wantStatus {
				t.Errorf("status = %d, want %d", w.Code, tt.wantStatus)
			}
		})
	}
}

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
	resetQuotaMiddlewareGlobals()
	t.Cleanup(resetQuotaMiddlewareGlobals)
}

func resetQuotaMiddlewareGlobals() {
	rpmTrackers = sync.Map{}
	tpmTrackers = sync.Map{}
	snapshotLimits = sync.Map{}
	InitQuotaUsageFuncs(
		func(string) (int64, error) { return 0, nil },
		func(string) (int64, error) { return 0, nil },
		func(string) (float64, error) { return 0, nil },
		func(string) (float64, error) { return 0, nil },
	)
	inFlightMu.Lock()
	inFlightByKey = map[string]int{}
	inFlightMu.Unlock()
}
