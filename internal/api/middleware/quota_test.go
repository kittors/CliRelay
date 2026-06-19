package middleware

import (
	"net/http"
	"net/http/httptest"
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
