package middleware

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/quota"
)

// serveDailyLimitedPOST sends one POST for a key with a daily request limit of
// 10 and returns the response status.
func serveDailyLimitedPOST(t *testing.T) int {
	t.Helper()
	router := gin.New()
	router.Use(func(c *gin.Context) {
		c.Set("apiKey", "sk-outage-XXXX")
		c.Set("accessMetadata", map[string]string{"daily-limit": "10", "api-key-id": "key-outage"})
		c.Next()
	})
	router.Use(QuotaMiddleware())
	router.POST("/v1/chat/completions", func(c *gin.Context) { c.Status(http.StatusNoContent) })
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, newQuotaPostRequest("sk-outage-XXXX"))
	return recorder.Code
}

func useQuotaStaleMaxAge(t *testing.T, maxAge time.Duration) {
	t.Helper()
	prev := time.Duration(quotaUsageStaleMaxAge.Load())
	SetQuotaUsageStaleMaxAge(maxAge)
	t.Cleanup(func() { SetQuotaUsageStaleMaxAge(prev) })
}

// ageQuotaUsageReadings moves every kept reading back by d.
func ageQuotaUsageReadings(d time.Duration, day string) {
	quotaUsageCacheMu.Lock()
	defer quotaUsageCacheMu.Unlock()
	for key, reading := range quotaUsageCache {
		reading.readAt = reading.readAt.Add(-d)
		if day != "" {
			reading.day = day
		}
		quotaUsageCache[key] = reading
	}
}

func TestQuotaAdmissionUsesARecentReadingWhileUsageIsUnavailable(t *testing.T) {
	gin.SetMode(gin.TestMode)
	resetQuotaMiddlewareState(t)
	useQuotaStaleMaxAge(t, 120*time.Second)
	used := int64(3)
	var unavailable bool
	countTodayByKeyFunc = func(string) (int64, error) {
		if unavailable {
			return 0, errors.New("database unavailable")
		}
		return used, nil
	}

	if code := serveDailyLimitedPOST(t); code != http.StatusNoContent {
		t.Fatalf("status with the database up = %d, want 204", code)
	}
	unavailable = true
	servedBefore := QuotaUsageStaleServed()
	if code := serveDailyLimitedPOST(t); code != http.StatusNoContent {
		t.Fatalf("status with a recent reading = %d, want 204", code)
	}
	if got := QuotaUsageStaleServed() - servedBefore; got != 1 {
		t.Fatalf("stale decisions = %d, want 1", got)
	}

	// The kept reading still enforces the limit: a key already at its limit
	// stays refused during the outage.
	unavailable, used = false, 10
	if code := serveDailyLimitedPOST(t); code != http.StatusTooManyRequests {
		t.Fatalf("status at the limit = %d, want 429", code)
	}
	unavailable = true
	if code := serveDailyLimitedPOST(t); code != http.StatusTooManyRequests {
		t.Fatalf("status at the limit during the outage = %d, want 429", code)
	}
}

func TestQuotaAdmissionRefusesOnceTheReadingIsTooOld(t *testing.T) {
	gin.SetMode(gin.TestMode)
	resetQuotaMiddlewareState(t)
	useQuotaStaleMaxAge(t, 120*time.Second)
	var unavailable bool
	countTodayByKeyFunc = func(string) (int64, error) {
		if unavailable {
			return 0, errors.New("database unavailable")
		}
		return 1, nil
	}
	if code := serveDailyLimitedPOST(t); code != http.StatusNoContent {
		t.Fatalf("status with the database up = %d, want 204", code)
	}
	unavailable = true
	ageQuotaUsageReadings(121*time.Second, "")
	if code := serveDailyLimitedPOST(t); code != http.StatusServiceUnavailable {
		t.Fatalf("status with an expired reading = %d, want 503", code)
	}
}

func TestQuotaAdmissionIgnoresYesterdaysDailyReading(t *testing.T) {
	gin.SetMode(gin.TestMode)
	resetQuotaMiddlewareState(t)
	useQuotaStaleMaxAge(t, 120*time.Second)
	var unavailable bool
	countTodayByKeyFunc = func(string) (int64, error) {
		if unavailable {
			return 0, errors.New("database unavailable")
		}
		return 1, nil
	}
	if code := serveDailyLimitedPOST(t); code != http.StatusNoContent {
		t.Fatalf("status with the database up = %d, want 204", code)
	}
	unavailable = true
	ageQuotaUsageReadings(0, time.Now().AddDate(0, 0, -1).Format(time.DateOnly))
	if code := serveDailyLimitedPOST(t); code != http.StatusServiceUnavailable {
		t.Fatalf("status with a reading from the previous day = %d, want 503", code)
	}
}

func TestQuotaAdmissionFallbackCanBeSwitchedOff(t *testing.T) {
	gin.SetMode(gin.TestMode)
	resetQuotaMiddlewareState(t)
	useQuotaStaleMaxAge(t, 0)
	var unavailable bool
	countTodayByKeyFunc = func(string) (int64, error) {
		if unavailable {
			return 0, errors.New("database unavailable")
		}
		return 1, nil
	}
	if code := serveDailyLimitedPOST(t); code != http.StatusNoContent {
		t.Fatalf("status with the database up = %d, want 204", code)
	}
	unavailable = true
	if code := serveDailyLimitedPOST(t); code != http.StatusServiceUnavailable {
		t.Fatalf("status with the fallback off = %d, want 503", code)
	}
}

func TestQuotaAdmissionKeepsReadingsPerSubjectAndWindow(t *testing.T) {
	resetQuotaMiddlewareState(t)
	useQuotaStaleMaxAge(t, 120*time.Second)
	read := func(window, subject string, value float64, err error) (float64, error) {
		return readQuotaUsage(window, subject, subject, false, func() (float64, error) { return value, err })
	}
	if _, err := read(quotaWindowTotalCost, "key:a", 1.5, nil); err != nil {
		t.Fatal(err)
	}
	down := errors.New("database unavailable")
	if got, err := read(quotaWindowTotalCost, "key:a", 0, down); err != nil || got != 1.5 {
		t.Fatalf("same subject and window = %v, %v; want the kept 1.5", got, err)
	}
	if _, err := read(quotaWindowTotalCost, "key:b", 0, down); err == nil {
		t.Fatal("another subject must not borrow key:a's reading")
	}
	if _, err := read(quotaWindowDayCost, "key:a", 0, down); err == nil {
		t.Fatal("another window must not borrow the total-cost reading")
	}
	period, err := readQuotaUsage(quotaWindowPeriodKey, "key:t/a", "key a", true, func() (quota.PeriodSpendingUsage, error) {
		return quota.PeriodSpendingUsage{Week: 7}, nil
	})
	if err != nil || period.Week != 7 {
		t.Fatalf("period read = %+v, %v", period, err)
	}
	period, err = readQuotaUsage(quotaWindowPeriodKey, "key:t/a", "key a", true, func() (quota.PeriodSpendingUsage, error) {
		return quota.PeriodSpendingUsage{}, down
	})
	if err != nil || period.Week != 7 {
		t.Fatalf("period fallback = %+v, %v; want the kept week 7", period, err)
	}
}

func TestQuotaStaleWarningNamesTheKeyWithoutItsSecret(t *testing.T) {
	policy := parseQuotaPolicy("sk-secret-XXXXXXXXXXXX", map[string]string{"daily-limit": "1"})
	_, logSubject := policy.usageSubjects()
	if strings.Contains(logSubject, "sk-secret-XXXXXXXXXXXX") {
		t.Fatalf("log subject %q exposes the API key", logSubject)
	}
}
