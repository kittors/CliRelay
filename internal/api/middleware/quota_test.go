package middleware

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/quota"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/usage"
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
	if !strings.Contains(secondSameKey.Body.String(), "Concurrent request limit exceeded") {
		t.Fatalf("concurrency body = %s, want clear concurrency wording", secondSameKey.Body.String())
	}
	if got := secondSameKey.Header().Get("X-CliRelay-Quota-Code"); got != "concurrency_limit_exceeded" {
		t.Fatalf("concurrency header code = %q", got)
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

func TestQuotaMiddlewareDailySpendingLimit(t *testing.T) {
	gin.SetMode(gin.TestMode)
	resetQuotaMiddlewareState(t)

	queryTodayCostByKeyFunc = func(key string) (float64, error) {
		if key == "key-over" {
			return 50, nil
		}
		return 20, nil
	}

	router := gin.New()
	router.Use(func(c *gin.Context) {
		c.Set("apiKey", c.GetHeader("X-Test-Key"))
		c.Set("accessMetadata", map[string]string{"daily-spending-limit": "50"})
		c.Next()
	})
	router.Use(QuotaMiddleware())
	router.POST("/v1/chat/completions", func(c *gin.Context) {
		c.Status(http.StatusNoContent)
	})

	allowed := httptest.NewRecorder()
	router.ServeHTTP(allowed, newQuotaPostRequest("key-under"))
	if allowed.Code != http.StatusNoContent {
		t.Fatalf("under-limit status = %d, want %d", allowed.Code, http.StatusNoContent)
	}

	blocked := httptest.NewRecorder()
	router.ServeHTTP(blocked, newQuotaPostRequest("key-over"))
	if blocked.Code != http.StatusTooManyRequests {
		t.Fatalf("over-limit status = %d, want %d", blocked.Code, http.StatusTooManyRequests)
	}

	var body struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(blocked.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode error body: %v", err)
	}
	if body.Error.Code != "daily_spending_limit_exceeded" {
		t.Fatalf("error code = %q, want daily_spending_limit_exceeded", body.Error.Code)
	}
	if !strings.Contains(body.Error.Message, "Daily spending limit exceeded") {
		t.Fatalf("message = %q, want daily spending wording", body.Error.Message)
	}
	if got := blocked.Header().Get("X-CliRelay-Quota-Code"); got != "daily_spending_limit_exceeded" {
		t.Fatalf("X-CliRelay-Quota-Code = %q", got)
	}
	if got := blocked.Header().Get("X-CliRelay-Quota-Rejected-By"); got != "daily_spending" {
		t.Fatalf("X-CliRelay-Quota-Rejected-By = %q", got)
	}
}

func TestQuotaMiddlewareDistinctLimitMessages(t *testing.T) {
	gin.SetMode(gin.TestMode)
	resetQuotaMiddlewareState(t)

	countTodayByKeyFunc = func(string) (int64, error) { return 10, nil }
	countTotalByKeyFunc = func(string) (int64, error) { return 100, nil }
	queryTotalCostByKeyFunc = func(string) (float64, error) { return 25, nil }

	cases := []struct {
		name       string
		metadata   map[string]string
		wantCode   string
		wantPhrase string
	}{
		{
			name:       "daily requests",
			metadata:   map[string]string{"daily-limit": "10"},
			wantCode:   "daily_limit_exceeded",
			wantPhrase: "Daily request limit exceeded",
		},
		{
			name:       "total quota",
			metadata:   map[string]string{"total-quota": "100"},
			wantCode:   "total_quota_exceeded",
			wantPhrase: "Total request quota exhausted",
		},
		{
			name:       "rpm",
			metadata:   map[string]string{"rpm-limit": "1"},
			wantCode:   "rpm_limit_exceeded",
			wantPhrase: "Requests-per-minute (RPM) limit exceeded",
		},
		{
			name:       "spending",
			metadata:   map[string]string{"spending-limit": "25"},
			wantCode:   "spending_limit_exceeded",
			wantPhrase: "Lifetime spending limit exceeded",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resetQuotaMiddlewareState(t)
			countTodayByKeyFunc = func(string) (int64, error) { return 10, nil }
			countTotalByKeyFunc = func(string) (int64, error) { return 100, nil }
			queryTotalCostByKeyFunc = func(string) (float64, error) { return 25, nil }

			router := gin.New()
			router.Use(func(c *gin.Context) {
				c.Set("apiKey", "key-a")
				c.Set("accessMetadata", tc.metadata)
				c.Next()
			})
			router.Use(QuotaMiddleware())
			router.POST("/v1/chat/completions", func(c *gin.Context) {
				c.Status(http.StatusNoContent)
			})

			// RPM needs two posts so the second exceeds limit 1.
			if tc.wantCode == "rpm_limit_exceeded" {
				first := httptest.NewRecorder()
				router.ServeHTTP(first, newQuotaPostRequest("key-a"))
			}

			blocked := httptest.NewRecorder()
			router.ServeHTTP(blocked, newQuotaPostRequest("key-a"))
			if blocked.Code != http.StatusTooManyRequests {
				t.Fatalf("status = %d, want 429; body=%s", blocked.Code, blocked.Body.String())
			}
			var body struct {
				Error struct {
					Code    string `json:"code"`
					Message string `json:"message"`
				} `json:"error"`
			}
			if err := json.Unmarshal(blocked.Body.Bytes(), &body); err != nil {
				t.Fatalf("decode: %v body=%s", err, blocked.Body.String())
			}
			if body.Error.Code != tc.wantCode {
				t.Fatalf("code = %q, want %q", body.Error.Code, tc.wantCode)
			}
			if !strings.Contains(body.Error.Message, tc.wantPhrase) {
				t.Fatalf("message = %q, want phrase %q", body.Error.Message, tc.wantPhrase)
			}
			if got := blocked.Header().Get("X-CliRelay-Quota-Code"); got != tc.wantCode {
				t.Fatalf("header code = %q, want %q", got, tc.wantCode)
			}
		})
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
	admissionSubjects = sync.Map{}
	countTodayByKeyFunc = func(string) (int64, error) { return 0, nil }
	countTotalByKeyFunc = func(string) (int64, error) { return 0, nil }
	queryTotalCostByKeyFunc = func(string) (float64, error) { return 0, nil }
	queryTodayCostByKeyFunc = func(string) (float64, error) { return 0, nil }
	countTodayByEndUserFunc = func(string) (int64, error) { return 0, nil }
	countTotalByEndUserFunc = func(string) (int64, error) { return 0, nil }
	queryTotalCostByEndUserFunc = func(string) (float64, error) { return 0, nil }
	queryTodayCostByEndUserFunc = func(string) (float64, error) { return 0, nil }
	queryPeriodByKeyFunc = func(string, string) (quota.PeriodSpendingUsage, error) { return quota.PeriodSpendingUsage{}, nil }
	queryPeriodByEndUserFunc = func(string, string) (quota.PeriodSpendingUsage, error) { return quota.PeriodSpendingUsage{}, nil }
	t.Cleanup(func() {
		countTodayByKeyFunc = func(string) (int64, error) { return 0, nil }
		countTotalByKeyFunc = func(string) (int64, error) { return 0, nil }
		queryTotalCostByKeyFunc = func(string) (float64, error) { return 0, nil }
		queryTodayCostByKeyFunc = func(string) (float64, error) { return 0, nil }
		countTodayByEndUserFunc = func(string) (int64, error) { return 0, nil }
		countTotalByEndUserFunc = func(string) (int64, error) { return 0, nil }
		queryTotalCostByEndUserFunc = func(string) (float64, error) { return 0, nil }
		queryTodayCostByEndUserFunc = func(string) (float64, error) { return 0, nil }
		queryPeriodByKeyFunc = func(string, string) (quota.PeriodSpendingUsage, error) { return quota.PeriodSpendingUsage{}, nil }
		queryPeriodByEndUserFunc = func(string, string) (quota.PeriodSpendingUsage, error) { return quota.PeriodSpendingUsage{}, nil }
	})
}

func TestQuotaMiddlewareRejectsAccountAndKeyPeriodScopesSeparately(t *testing.T) {
	gin.SetMode(gin.TestMode)
	cases := []struct {
		name        string
		accountUsed float64
		keyUsed     float64
		wantScope   string
	}{
		{name: "account ceiling", accountUsed: 100, keyUsed: 10, wantScope: "account"},
		{name: "key sub quota", accountUsed: 10, keyUsed: 20, wantScope: "key"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resetQuotaMiddlewareState(t)
			queryPeriodByEndUserFunc = func(tenantID, endUserID string) (quota.PeriodSpendingUsage, error) {
				return quota.PeriodSpendingUsage{Day: tc.accountUsed}, nil
			}
			queryPeriodByKeyFunc = func(tenantID, apiKeyID string) (quota.PeriodSpendingUsage, error) {
				return quota.PeriodSpendingUsage{Day: tc.keyUsed}, nil
			}
			router := gin.New()
			router.Use(func(c *gin.Context) {
				c.Set("apiKey", "sk-period")
				c.Set("accessMetadata", map[string]string{
					"tenant-id": "tenant-a", "end-user-id": "user-a", "api-key-id": "key-a",
					"account-period-spending-limit-day": "100", "key-period-spending-limit-day": "20",
				})
				c.Next()
			})
			router.Use(QuotaMiddleware())
			router.POST("/v1/chat/completions", func(c *gin.Context) { c.Status(http.StatusNoContent) })
			recorder := httptest.NewRecorder()
			router.ServeHTTP(recorder, newQuotaPostRequest("sk-period"))
			if recorder.Code != http.StatusTooManyRequests {
				t.Fatalf("status = %d, want 429; body=%s", recorder.Code, recorder.Body.String())
			}
			if got := recorder.Header().Get("X-CliRelay-Quota-Scope"); got != tc.wantScope {
				t.Fatalf("scope = %q, want %q", got, tc.wantScope)
			}
			if got := recorder.Header().Get("X-CliRelay-Quota-Period"); got != "day" {
				t.Fatalf("period = %q, want day", got)
			}
		})
	}
}

func TestQuotaMiddlewareFailsClosedWhenUsageUnavailable(t *testing.T) {
	gin.SetMode(gin.TestMode)
	resetQuotaMiddlewareState(t)
	queryPeriodByEndUserFunc = func(string, string) (quota.PeriodSpendingUsage, error) {
		return quota.PeriodSpendingUsage{}, errors.New("database unavailable")
	}
	router := gin.New()
	router.Use(func(c *gin.Context) {
		c.Set("apiKey", "sk-period")
		c.Set("accessMetadata", map[string]string{
			"tenant-id": "tenant-a", "end-user-id": "user-a", "api-key-id": "key-a",
			"account-period-spending-limit-week": "100",
		})
		c.Next()
	})
	router.Use(QuotaMiddleware())
	router.POST("/v1/chat/completions", func(c *gin.Context) { c.Status(http.StatusNoContent) })
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, newQuotaPostRequest("sk-period"))
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503; body=%s", recorder.Code, recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), `"code":"quota_usage_unavailable"`) {
		t.Fatalf("body = %s, want quota_usage_unavailable", recorder.Body.String())
	}
}

func TestQuotaMiddlewarePeriodUsageReflectsResetBaseline(t *testing.T) {
	gin.SetMode(gin.TestMode)
	resetQuotaMiddlewareState(t)
	if err := usage.InitDB(filepath.Join(t.TempDir(), "quota-period-reset.db"), config.RequestLogStorageConfig{}, time.UTC); err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	t.Cleanup(usage.CloseDB)
	tenantID := "11111111-1111-1111-1111-111111111111"
	keyID := "quota-reset-key"
	now := time.Now().UTC()
	insert := func(model string, cost float64) {
		t.Helper()
		if _, err := usage.RuntimeDB().Exec(`INSERT INTO usage_rollup_buckets
			(tenant_id,bucket_kind,bucket_start,api_key_id,model,cost_total,updated_at)
			VALUES (?,?,?,?,?,?,?)`, tenantID, "day", usage.LocalDayKeyAt(now), keyID, model, cost, now); err != nil {
			t.Fatalf("insert rollup: %v", err)
		}
	}
	insert("before-reset", 20)
	if _, err := usage.ResetPeriodSpendingByAPIKeyIDForTenant(tenantID, keyID, []quota.Period{quota.PeriodWeek}, usage.PeriodSpendingResetActor{Kind: "test"}); err != nil {
		t.Fatalf("reset week: %v", err)
	}
	queryPeriodByKeyFunc = usage.QueryPeriodSpendingByAPIKeyIDForTenant

	router := gin.New()
	router.Use(func(c *gin.Context) {
		c.Set("apiKey", "sk-quota-reset")
		c.Set("accessMetadata", map[string]string{
			"tenant-id": tenantID, "api-key-id": keyID, "key-period-spending-limit-week": "10",
		})
		c.Next()
	})
	router.Use(QuotaMiddleware())
	router.POST("/v1/chat/completions", func(c *gin.Context) { c.Status(http.StatusNoContent) })

	allowed := httptest.NewRecorder()
	router.ServeHTTP(allowed, newQuotaPostRequest("sk-quota-reset"))
	if allowed.Code != http.StatusNoContent {
		t.Fatalf("after reset status = %d body=%s, want 204", allowed.Code, allowed.Body.String())
	}
	insert("after-reset", 11)
	blocked := httptest.NewRecorder()
	router.ServeHTTP(blocked, newQuotaPostRequest("sk-quota-reset"))
	if blocked.Code != http.StatusTooManyRequests || blocked.Header().Get("X-CliRelay-Quota-Period") != "week" {
		t.Fatalf("after new spend status/header/body = %d %q %s", blocked.Code, blocked.Header().Get("X-CliRelay-Quota-Period"), blocked.Body.String())
	}
}
