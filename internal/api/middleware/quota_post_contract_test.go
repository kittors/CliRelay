package middleware

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/diagnostics"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/quota"
)

// The Responses WebSocket path reuses the POST quota checks and renders the same
// verdicts, so what a refused POST looks like on the wire is pinned here: status,
// the exact JSON body, every X-CliRelay-* header, and the request diagnostics.

const contractKey = "sk-contract"

type quotaContractCase struct {
	name       string
	metadata   map[string]string
	setup      func(t *testing.T)
	wantStatus int
	wantBody   string
	wantHeader map[string]string
	wantQuota  *diagnostics.QuotaSnapshot
	wantResp   *diagnostics.ResponseSnapshot
}

func rateLimitContract(limits diagnostics.QuotaSnapshot, rejectedBy string, limit, current float64, code, message string) (string, map[string]string, *diagnostics.QuotaSnapshot, *diagnostics.ResponseSnapshot) {
	body := fmt.Sprintf(`{"error":{"code":%q,"message":%q,"type":"rate_limit_exceeded"}}`, code, message)
	header := map[string]string{
		"X-CliRelay-Quota-Code":        code,
		"X-CliRelay-Quota-Limit":       formatQuotaNumber(limit),
		"X-CliRelay-Quota-Current":     formatQuotaNumber(current),
		"X-CliRelay-Quota-Rejected-By": rejectedBy,
	}
	q := limits
	q.Rejected, q.RejectedBy, q.Limit, q.Current = true, rejectedBy, limit, current
	q.ErrorCode, q.ErrorType, q.ErrorMessage = code, "rate_limit_exceeded", message
	resp := &diagnostics.ResponseSnapshot{Status: http.StatusTooManyRequests, ErrorCode: code, ErrorType: "rate_limit_exceeded", ErrorMessage: message, Source: "local_quota"}
	return body, header, &q, resp
}

func periodContract(limits diagnostics.QuotaSnapshot, scope string, period quota.Period, limit, current float64, code, message string) (string, map[string]string, *diagnostics.QuotaSnapshot, *diagnostics.ResponseSnapshot) {
	body := fmt.Sprintf(`{"error":{"code":%q,"message":%q,"period":%q,"scope":%q,"type":"rate_limit_exceeded"}}`, code, message, period, scope)
	header := map[string]string{
		"X-CliRelay-Quota-Code":        code,
		"X-CliRelay-Quota-Rejected-By": "period_spending",
		"X-CliRelay-Quota-Scope":       scope,
		"X-CliRelay-Quota-Period":      string(period),
		"X-CliRelay-Quota-Limit":       formatQuotaNumber(limit),
		"X-CliRelay-Quota-Current":     formatQuotaNumber(current),
	}
	q := limits
	q.Rejected, q.RejectedBy, q.Limit, q.Current = true, "period_spending", limit, current
	q.ErrorCode, q.ErrorType, q.ErrorMessage = code, "rate_limit_exceeded", message
	resp := &diagnostics.ResponseSnapshot{Status: http.StatusTooManyRequests, ErrorCode: code, ErrorType: "rate_limit_exceeded", ErrorMessage: message, Source: "local_quota"}
	return body, header, &q, resp
}

func unavailableContract(scope, period string) string {
	return fmt.Sprintf(`{"error":{"code":"quota_usage_unavailable","message":"Quota usage is temporarily unavailable","period":%q,"scope":%q}}`, period, scope)
}

// quotaRefusalContractCases covers every refusal branch of the quota checks.
// budgetOnly marks the usage-based checks, which a WebSocket handshake also runs.
func quotaRefusalContractCases() (all []quotaContractCase, budgetOnly map[string]bool) {
	budgetOnly = map[string]bool{}
	add := func(c quotaContractCase, budget bool) {
		all = append(all, c)
		if budget {
			budgetOnly[c.name] = true
		}
	}
	unavailable := errors.New("database unavailable")

	{
		msg := "Concurrent request limit exceeded: 1 in-flight requests (limit 1). Wait for running requests to finish, or raise the concurrency limit in the permission profile."
		body, header, q, resp := rateLimitContract(diagnostics.QuotaSnapshot{ConcurrencyLimit: 1}, "concurrency", 1, 1, "concurrency_limit_exceeded", msg)
		add(quotaContractCase{
			name:     "concurrency",
			metadata: map[string]string{"concurrency-limit": "1"},
			setup: func(t *testing.T) {
				if _, ok := acquireKeyConcurrency(contractKey, 1); !ok {
					t.Fatal("could not occupy the only slot")
				}
			},
			wantStatus: http.StatusTooManyRequests, wantBody: body, wantHeader: header, wantQuota: q, wantResp: resp,
		}, false)
	}
	{
		msg := "Requests-per-minute (RPM) limit exceeded: 2/1 requests in the last minute. Slow down, or raise the RPM limit in the permission profile."
		body, header, q, resp := rateLimitContract(diagnostics.QuotaSnapshot{RPMLimit: 1}, "rpm", 1, 2, "rpm_limit_exceeded", msg)
		add(quotaContractCase{
			name:       "rpm",
			metadata:   map[string]string{"rpm-limit": "1"},
			setup:      func(*testing.T) { getRPMTracker(contractKey).add() },
			wantStatus: http.StatusTooManyRequests, wantBody: body, wantHeader: header, wantQuota: q, wantResp: resp,
		}, false)
	}
	{
		msg := "Tokens-per-minute (TPM) limit exceeded: 150/100 tokens in the last minute. Slow down, or raise the TPM limit in the permission profile."
		body, header, q, resp := rateLimitContract(diagnostics.QuotaSnapshot{TPMLimit: 100}, "tpm", 100, 150, "tpm_limit_exceeded", msg)
		add(quotaContractCase{
			name:       "tpm",
			metadata:   map[string]string{"tpm-limit": "100"},
			setup:      func(*testing.T) { RecordTokenUsage(contractKey, 150) },
			wantStatus: http.StatusTooManyRequests, wantBody: body, wantHeader: header, wantQuota: q, wantResp: resp,
		}, false)
	}
	{
		msg := "Daily request limit exceeded: 7/5 requests used today. Raise the daily request limit in the permission profile, or wait until the next project day."
		body, header, q, resp := rateLimitContract(diagnostics.QuotaSnapshot{DailyLimit: 5}, "daily", 5, 7, "daily_limit_exceeded", msg)
		add(quotaContractCase{
			name:       "daily requests",
			metadata:   map[string]string{"daily-limit": "5"},
			setup:      func(*testing.T) { countTodayByKeyFunc = func(string) (int64, error) { return 7, nil } },
			wantStatus: http.StatusTooManyRequests, wantBody: body, wantHeader: header, wantQuota: q, wantResp: resp,
		}, true)
	}
	{
		msg := "Daily request limit exceeded: 3/3 requests used today. Raise the daily request limit in the permission profile, or wait until the next project day."
		body, header, q, resp := rateLimitContract(diagnostics.QuotaSnapshot{DailyLimit: 3}, "daily", 3, 3, "daily_limit_exceeded", msg)
		add(quotaContractCase{
			name:     "daily requests on the end-user pool",
			metadata: map[string]string{"daily-limit": "3", "end-user-id": "user-a"},
			setup: func(*testing.T) {
				countTodayByEndUserFunc = func(id string) (int64, error) {
					if id != "user-a" {
						return 0, fmt.Errorf("unexpected end user %q", id)
					}
					return 3, nil
				}
			},
			wantStatus: http.StatusTooManyRequests, wantBody: body, wantHeader: header, wantQuota: q, wantResp: resp,
		}, true)
	}
	{
		msg := "Total request quota exhausted: 5/5 lifetime requests used. Raise the total request quota in the permission profile to continue."
		body, header, q, resp := rateLimitContract(diagnostics.QuotaSnapshot{TotalQuota: 5}, "total", 5, 5, "total_quota_exceeded", msg)
		add(quotaContractCase{
			name:       "total quota",
			metadata:   map[string]string{"total-quota": "5"},
			setup:      func(*testing.T) { countTotalByKeyFunc = func(string) (int64, error) { return 5, nil } },
			wantStatus: http.StatusTooManyRequests, wantBody: body, wantHeader: header, wantQuota: q, wantResp: resp,
		}, true)
	}
	{
		msg := "Lifetime spending limit exceeded: $12.50 of $10.00 used. Raise the spending limit to continue."
		body, header, q, resp := rateLimitContract(diagnostics.QuotaSnapshot{SpendingLimit: 10}, "spending", 10, 12.5, "spending_limit_exceeded", msg)
		add(quotaContractCase{
			name:       "lifetime spending",
			metadata:   map[string]string{"spending-limit": "10"},
			setup:      func(*testing.T) { queryTotalCostByKeyFunc = func(string) (float64, error) { return 12.5, nil } },
			wantStatus: http.StatusTooManyRequests, wantBody: body, wantHeader: header, wantQuota: q, wantResp: resp,
		}, true)
	}
	{
		msg := "Daily spending limit exceeded: $3.00 of $2.50 used today. Raise the daily spending limit in the permission profile, reset today's spending, or wait until the next project day."
		body, header, q, resp := rateLimitContract(diagnostics.QuotaSnapshot{DailySpendingLimit: 2.5}, "daily_spending", 2.5, 3, "daily_spending_limit_exceeded", msg)
		add(quotaContractCase{
			name:       "daily spending",
			metadata:   map[string]string{"daily-spending-limit": "2.5"},
			setup:      func(*testing.T) { queryTodayCostByKeyFunc = func(string) (float64, error) { return 3, nil } },
			wantStatus: http.StatusTooManyRequests, wantBody: body, wantHeader: header, wantQuota: q, wantResp: resp,
		}, true)
	}
	{
		// A day period limit supersedes the legacy daily-spending-limit, which is
		// skipped even though it is exceeded here.
		msg := "Account day spending limit exceeded: $10.00 of $10.00 used."
		body, header, q, resp := periodContract(diagnostics.QuotaSnapshot{DailySpendingLimit: 1}, "account", quota.PeriodDay, 10, 10, "daily_spending_limit_exceeded", msg)
		add(quotaContractCase{
			name: "account day period",
			metadata: map[string]string{
				"tenant-id": "tenant-a", "end-user-id": "user-a", "api-key-id": "key-a",
				"daily-spending-limit": "1", "account-period-spending-limit-day": "10",
			},
			setup: func(*testing.T) {
				queryTodayCostByEndUserFunc = func(string) (float64, error) { return 5, nil }
				queryPeriodByEndUserFunc = func(tenantID, endUserID string) (quota.PeriodSpendingUsage, error) {
					if tenantID != "tenant-a" || endUserID != "user-a" {
						return quota.PeriodSpendingUsage{}, fmt.Errorf("unexpected subject %q/%q", tenantID, endUserID)
					}
					return quota.PeriodSpendingUsage{Day: 10}, nil
				}
			},
			wantStatus: http.StatusTooManyRequests, wantBody: body, wantHeader: header, wantQuota: q, wantResp: resp,
		}, true)
	}
	{
		msg := "Key week spending limit exceeded: $12.50 of $10.00 used."
		body, header, q, resp := periodContract(diagnostics.QuotaSnapshot{}, "key", quota.PeriodWeek, 10, 12.5, "period_spending_limit_exceeded", msg)
		add(quotaContractCase{
			name:     "key week period",
			metadata: map[string]string{"tenant-id": "tenant-a", "api-key-id": "key-a", "key-period-spending-limit-week": "10"},
			setup: func(*testing.T) {
				queryPeriodByKeyFunc = func(tenantID, apiKeyID string) (quota.PeriodSpendingUsage, error) {
					if tenantID != "tenant-a" || apiKeyID != "key-a" {
						return quota.PeriodSpendingUsage{}, fmt.Errorf("unexpected subject %q/%q", tenantID, apiKeyID)
					}
					return quota.PeriodSpendingUsage{Week: 12.5}, nil
				}
			},
			wantStatus: http.StatusTooManyRequests, wantBody: body, wantHeader: header, wantQuota: q, wantResp: resp,
		}, true)
	}
	add(quotaContractCase{
		name:       "daily usage unavailable",
		metadata:   map[string]string{"daily-limit": "5"},
		setup:      func(*testing.T) { countTodayByKeyFunc = func(string) (int64, error) { return 0, unavailable } },
		wantStatus: http.StatusServiceUnavailable, wantBody: unavailableContract("key", "day"),
		wantQuota: &diagnostics.QuotaSnapshot{DailyLimit: 5},
	}, true)
	add(quotaContractCase{
		name:     "account period usage unavailable",
		metadata: map[string]string{"tenant-id": "tenant-a", "end-user-id": "user-a", "account-period-spending-limit-week": "100"},
		setup: func(*testing.T) {
			queryPeriodByEndUserFunc = func(string, string) (quota.PeriodSpendingUsage, error) {
				return quota.PeriodSpendingUsage{}, unavailable
			}
		},
		wantStatus: http.StatusServiceUnavailable, wantBody: unavailableContract("account", "period"),
		wantQuota: &diagnostics.QuotaSnapshot{},
	}, true)
	return all, budgetOnly
}

// serveQuotaContract sends one request through QuotaMiddleware and reports
// whether the route handler ran, plus the diagnostics recorded for it.
func serveQuotaContract(t *testing.T, metadata map[string]string, req *http.Request) (*httptest.ResponseRecorder, bool, *diagnostics.RequestDiagnostic) {
	t.Helper()
	return serveQuotaContractWith(t, metadata, req, nil)
}

func serveQuotaContractWith(t *testing.T, metadata map[string]string, req *http.Request, inspect func(*gin.Context)) (*httptest.ResponseRecorder, bool, *diagnostics.RequestDiagnostic) {
	t.Helper()
	var diag *diagnostics.RequestDiagnostic
	handled := false
	router := gin.New()
	router.Use(func(c *gin.Context) {
		diag = diagnostics.EnsureGin(c, "req-contract")
		c.Set("apiKey", contractKey)
		if metadata != nil {
			c.Set("accessMetadata", metadata)
		}
		c.Next()
	})
	router.Use(QuotaMiddleware())
	serve := func(c *gin.Context) {
		handled = true
		if inspect != nil {
			inspect(c)
		}
		c.Status(http.StatusNoContent)
	}
	router.POST("/v1/responses", serve)
	router.GET("/v1/responses", serve)
	router.GET("/v1/models", serve)
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, req)
	return recorder, handled, diag
}

func quotaHeaders(h http.Header) map[string]string {
	out := map[string]string{}
	for key, values := range h {
		if strings.HasPrefix(key, "X-Clirelay-") && len(values) > 0 {
			out[key] = values[0]
		}
	}
	return out
}

func canonicalHeaders(in map[string]string) map[string]string {
	out := map[string]string{}
	for key, value := range in {
		out[http.CanonicalHeaderKey(key)] = value
	}
	return out
}

func assertQuotaContract(t *testing.T, tc quotaContractCase, recorder *httptest.ResponseRecorder, handled bool, diag *diagnostics.RequestDiagnostic) {
	t.Helper()
	if handled {
		t.Fatalf("route handler ran for a refused request")
	}
	if recorder.Code != tc.wantStatus {
		t.Fatalf("status = %d, want %d; body=%s", recorder.Code, tc.wantStatus, recorder.Body.String())
	}
	if got := recorder.Body.String(); got != tc.wantBody {
		t.Fatalf("body mismatch\n got: %s\nwant: %s", got, tc.wantBody)
	}
	if got, want := quotaHeaders(recorder.Header()), canonicalHeaders(tc.wantHeader); !reflect.DeepEqual(got, want) {
		t.Fatalf("X-CliRelay headers = %v, want %v", got, want)
	}
	snapshot := diag.Snapshot()
	if !reflect.DeepEqual(snapshot.Quota, tc.wantQuota) {
		t.Fatalf("diagnostics quota = %+v, want %+v", snapshot.Quota, tc.wantQuota)
	}
	if !reflect.DeepEqual(snapshot.Response, tc.wantResp) {
		t.Fatalf("diagnostics response = %+v, want %+v", snapshot.Response, tc.wantResp)
	}
}

func TestQuotaMiddlewarePOSTRefusalContract(t *testing.T) {
	gin.SetMode(gin.TestMode)
	cases, _ := quotaRefusalContractCases()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resetQuotaMiddlewareState(t)
			tc.setup(t)
			recorder, handled, diag := serveQuotaContract(t, tc.metadata, httptest.NewRequest(http.MethodPost, "/v1/responses", nil))
			assertQuotaContract(t, tc, recorder, handled, diag)
		})
	}
}

func TestQuotaMiddlewarePOSTAdmissionSideEffects(t *testing.T) {
	gin.SetMode(gin.TestMode)

	t.Run("key without metadata is counted for the dashboard and passes", func(t *testing.T) {
		resetQuotaMiddlewareState(t)
		recorder, handled, diag := serveQuotaContract(t, nil, httptest.NewRequest(http.MethodPost, "/v1/responses", nil))
		if !handled || recorder.Code != http.StatusNoContent {
			t.Fatalf("status = %d handled = %v, want 204 and handled", recorder.Code, handled)
		}
		if got := getRPMTracker(contractKey).count(); got != 1 {
			t.Fatalf("rpm count = %d, want 1", got)
		}
		if snapshot := diag.Snapshot(); snapshot.Quota != nil {
			t.Fatalf("diagnostics quota = %+v, want none", snapshot.Quota)
		}
	})

	t.Run("key with metadata but no limits records its limits and passes", func(t *testing.T) {
		resetQuotaMiddlewareState(t)
		recorder, handled, diag := serveQuotaContract(t, map[string]string{"tenant-id": "tenant-a"}, httptest.NewRequest(http.MethodPost, "/v1/responses", nil))
		if !handled || recorder.Code != http.StatusNoContent {
			t.Fatalf("status = %d handled = %v, want 204 and handled", recorder.Code, handled)
		}
		if got := getRPMTracker(contractKey).count(); got != 1 {
			t.Fatalf("rpm count = %d, want 1", got)
		}
		if _, ok := snapshotLimits.Load(contractKey); !ok {
			t.Fatal("dashboard limits were not recorded")
		}
		if snapshot := diag.Snapshot(); !reflect.DeepEqual(snapshot.Quota, &diagnostics.QuotaSnapshot{}) {
			t.Fatalf("diagnostics quota = %+v, want an empty limit snapshot", snapshot.Quota)
		}
	})

	t.Run("admitted request holds its slot until the handler returns", func(t *testing.T) {
		resetQuotaMiddlewareState(t)
		metadata := map[string]string{"concurrency-limit": "2", "rpm-limit": "10", "daily-limit": "10"}
		inFlight := -1
		router := gin.New()
		router.Use(func(c *gin.Context) {
			c.Set("apiKey", contractKey)
			c.Set("accessMetadata", metadata)
			c.Next()
		})
		router.Use(QuotaMiddleware())
		router.POST("/v1/responses", func(c *gin.Context) {
			inFlight = keyConcurrencyCount(contractKey)
			c.Status(http.StatusNoContent)
		})
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/v1/responses", nil))
		if recorder.Code != http.StatusNoContent {
			t.Fatalf("status = %d, want 204; body=%s", recorder.Code, recorder.Body.String())
		}
		if inFlight != 1 {
			t.Fatalf("in-flight during handler = %d, want 1", inFlight)
		}
		if got := keyConcurrencyCount(contractKey); got != 0 {
			t.Fatalf("in-flight after handler = %d, want 0", got)
		}
		if got := getRPMTracker(contractKey).count(); got != 1 {
			t.Fatalf("rpm count = %d, want 1", got)
		}
	})

	t.Run("refused request gives its slot back", func(t *testing.T) {
		resetQuotaMiddlewareState(t)
		countTodayByKeyFunc = func(string) (int64, error) { return 10, nil }
		metadata := map[string]string{"concurrency-limit": "1", "daily-limit": "10"}
		recorder, handled, _ := serveQuotaContract(t, metadata, httptest.NewRequest(http.MethodPost, "/v1/responses", nil))
		if handled || recorder.Code != http.StatusTooManyRequests {
			t.Fatalf("status = %d handled = %v, want 429 and not handled", recorder.Code, handled)
		}
		if got := keyConcurrencyCount(contractKey); got != 0 {
			t.Fatalf("in-flight after refusal = %d, want 0", got)
		}
	})
}
