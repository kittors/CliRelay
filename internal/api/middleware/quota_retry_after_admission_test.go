package middleware

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"testing/synctest"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/accesspolicy"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/quota"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/usage"
)

// retryAfterSubject is the key quotaRouter authenticates as. It has no end user,
// so the key itself is the subject the windows are kept under.
const retryAfterSubject = "sk-XXXX-cluster-test"

func mustPass(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204; body=%s", rec.Code, rec.Body.String())
	}
}

// retryAfterSeconds reads the header; ok is false when the response has none.
func retryAfterSeconds(t *testing.T, rec *httptest.ResponseRecorder) (seconds int, ok bool) {
	t.Helper()
	raw := rec.Header().Get("Retry-After")
	if raw == "" {
		return 0, false
	}
	seconds, err := strconv.Atoi(raw)
	if err != nil || seconds < 1 {
		t.Fatalf("Retry-After = %q, want a positive whole number of seconds", raw)
	}
	return seconds, true
}

func assertRefusedWithRetryAfter(t *testing.T, rec *httptest.ResponseRecorder, wantCode string, want int) {
	t.Helper()
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429; body=%s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("X-CliRelay-Quota-Code"); got != wantCode {
		t.Fatalf("X-CliRelay-Quota-Code = %q, want %q", got, wantCode)
	}
	got, ok := retryAfterSeconds(t, rec)
	if !ok {
		t.Fatalf("%s refusal has no Retry-After; headers=%v", wantCode, rec.Header())
	}
	if got != want {
		t.Fatalf("%s Retry-After = %d, want %d", wantCode, got, want)
	}
}

func assertRefusedWithoutRetryAfter(t *testing.T, rec *httptest.ResponseRecorder, wantCode string) {
	t.Helper()
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429; body=%s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("X-CliRelay-Quota-Code"); got != wantCode {
		t.Fatalf("X-CliRelay-Quota-Code = %q, want %q", got, wantCode)
	}
	if got := rec.Header().Get("Retry-After"); got != "" {
		t.Fatalf("%s refusal carries Retry-After %q, but its limit has no boundary to report", wantCode, got)
	}
}

// The three requests of a limit-2 key land at 0s, 10s and 20s; the third is
// refused. Its Retry-After is the point where the request at 10s leaves the
// window, and following it exactly is enough to be admitted.
func TestRPMRefusalTellsWhenTheWindowOpensAgain(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		resetQuotaMiddlewareState(t)
		router := quotaRouter(map[string]string{"rpm-limit": "2"}, nil)

		mustPass(t, postQuota(router)) // 0s
		time.Sleep(10 * time.Second)
		mustPass(t, postQuota(router)) // 10s
		time.Sleep(10 * time.Second)
		// 20s: the entry that has to go is the one from 10s, which is old enough
		// at 70s, so the header is the first whole second past 50.
		assertRefusedWithRetryAfter(t, postQuota(router), "rpm_limit_exceeded", 51)

		// Retrying early is refused again, and the retry itself is counted: the
		// wait it gets is for the request at 20s to leave.
		time.Sleep(50 * time.Second) // 70s, exactly on the edge
		assertRefusedWithRetryAfter(t, postQuota(router), "rpm_limit_exceeded", 11)

		time.Sleep(11 * time.Second) // 81s
		mustPass(t, postQuota(router))
	})
}

func TestTPMRefusalTellsWhenTheWindowOpensAgain(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		resetQuotaMiddlewareState(t)
		router := quotaRouter(map[string]string{"tpm-limit": "100"}, nil)

		RecordTokenUsage(retryAfterSubject, 60) // 0s
		time.Sleep(20 * time.Second)
		RecordTokenUsage(retryAfterSubject, 60) // 20s: 120 in the window
		time.Sleep(10 * time.Second)
		// 30s: dropping the 60 from 0s brings the window to 60, under the limit,
		// and that entry is old enough at 60s.
		assertRefusedWithRetryAfter(t, postQuota(router), "tpm_limit_exceeded", 31)

		time.Sleep(30 * time.Second) // 60s: the entry is exactly a minute old
		assertRefusedWithRetryAfter(t, postQuota(router), "tpm_limit_exceeded", 1)

		time.Sleep(time.Second + time.Millisecond)
		mustPass(t, postQuota(router))
	})
}

// Every limit that resets at a calendar boundary reports how far away it is.
func TestCalendarLimitRefusalsTellWhenTheirWindowEnds(t *testing.T) {
	cases := []struct {
		name     string
		metadata map[string]string
		setup    func()
		wantCode string
		// The boundary the stub reports for this window, and the header it yields.
		window   quota.Period
		wantWait int
	}{
		{
			name:     "daily requests",
			metadata: map[string]string{"daily-limit": "5"},
			setup:    func() { countTodayByKeyFunc = func(string) (int64, error) { return 5, nil } },
			wantCode: "daily_limit_exceeded", window: quota.PeriodDay, wantWait: 17*3600 + 30*60 + 1,
		},
		{
			name:     "daily requests of an account",
			metadata: map[string]string{"daily-limit": "5", "end-user-id": "user-a"},
			setup:    func() { countTodayByEndUserFunc = func(string) (int64, error) { return 6, nil } },
			wantCode: "daily_limit_exceeded", window: quota.PeriodDay, wantWait: 17*3600 + 30*60 + 1,
		},
		{
			name:     "daily spending",
			metadata: map[string]string{"daily-spending-limit": "2.5"},
			setup:    func() { queryTodayCostByKeyFunc = func(string) (float64, error) { return 3, nil } },
			wantCode: "daily_spending_limit_exceeded", window: quota.PeriodDay, wantWait: 17*3600 + 30*60 + 1,
		},
		{
			name:     "key day period",
			metadata: map[string]string{"tenant-id": "tenant-a", "api-key-id": "key-a", "key-period-spending-limit-day": "10"},
			setup: func() {
				queryPeriodByKeyFunc = func(string, string) (quota.PeriodSpendingUsage, error) {
					return quota.PeriodSpendingUsage{Day: 10}, nil
				}
			},
			wantCode: "daily_spending_limit_exceeded", window: quota.PeriodDay, wantWait: 17*3600 + 30*60 + 1,
		},
		{
			name: "account week period",
			metadata: map[string]string{
				"tenant-id": "tenant-a", "end-user-id": "user-a", "api-key-id": "key-a", "account-period-spending-limit-week": "10",
			},
			setup: func() {
				queryPeriodByEndUserFunc = func(string, string) (quota.PeriodSpendingUsage, error) {
					return quota.PeriodSpendingUsage{Week: 12}, nil
				}
			},
			wantCode: "period_spending_limit_exceeded", window: quota.PeriodWeek, wantWait: 3*86400 + 4*3600 + 1,
		},
		{
			name:     "key month period",
			metadata: map[string]string{"tenant-id": "tenant-a", "api-key-id": "key-a", "key-period-spending-limit-month": "10"},
			setup: func() {
				queryPeriodByKeyFunc = func(string, string) (quota.PeriodSpendingUsage, error) {
					return quota.PeriodSpendingUsage{Month: 10}, nil
				}
			},
			wantCode: "period_spending_limit_exceeded", window: quota.PeriodMonth, wantWait: 20*86400 + 3600 + 1,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				resetQuotaMiddlewareState(t)
				stubPeriodReset(t, func(period quota.Period, now time.Time) (time.Time, bool) {
					switch period {
					case quota.PeriodDay:
						return now.Add(17*time.Hour + 30*time.Minute), true
					case quota.PeriodWeek:
						return now.Add(3*24*time.Hour + 4*time.Hour), true
					case quota.PeriodMonth:
						return now.Add(20*24*time.Hour + time.Hour), true
					}
					return time.Time{}, false
				})
				tc.setup()
				rec := postQuota(quotaRouter(tc.metadata, nil))
				assertRefusedWithRetryAfter(t, rec, tc.wantCode, tc.wantWait)
				if got := rec.Header().Get("X-CliRelay-Quota-Period"); got != "" && got != string(tc.window) {
					t.Fatalf("X-CliRelay-Quota-Period = %q, want %q", got, tc.window)
				}
			})
		})
	}
}

// run.go hands the usage package's boundaries to the middleware; the header must
// be the distance to the boundary that function reports.
func TestCalendarLimitRefusalUsesTheUsagePackageBoundary(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		resetQuotaMiddlewareState(t)
		InitQuotaPeriodResetFunc(usage.PeriodResetAt)
		t.Cleanup(func() {
			periodResetAtFunc = func(quota.Period, time.Time) (time.Time, bool) { return time.Time{}, false }
		})
		countTodayByKeyFunc = func(string) (int64, error) { return 5, nil }

		end, ok := usage.PeriodResetAt(quota.PeriodDay, time.Now())
		if !ok {
			t.Fatal("the usage package reports no end for the day")
		}
		want := int(time.Until(end)/time.Second) + 1
		assertRefusedWithRetryAfter(t, postQuota(quotaRouter(map[string]string{"daily-limit": "5"}, nil)), "daily_limit_exceeded", want)
	})
}

// A boundary nobody wired, or one the usage package cannot name, leaves the
// header out instead of guessing a day.
func TestCalendarLimitRefusalWithoutABoundaryHasNoRetryAfter(t *testing.T) {
	resetQuotaMiddlewareState(t)
	stubPeriodReset(t, func(quota.Period, time.Time) (time.Time, bool) { return time.Time{}, false })
	countTodayByKeyFunc = func(string) (int64, error) { return 5, nil }
	assertRefusedWithoutRetryAfter(t, postQuota(quotaRouter(map[string]string{"daily-limit": "5"}, nil)), "daily_limit_exceeded")
}

// The 5h window opens with the first spend and closes five hours later, so its
// end comes from the anchor the usage read reports.
func TestFiveHourPeriodRefusalTellsWhenTheWindowCloses(t *testing.T) {
	metadata := map[string]string{"tenant-id": "tenant-a", "api-key-id": "key-a", "key-period-spending-limit-5h": "10"}
	t.Run("window opened two hours ago", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			resetQuotaMiddlewareState(t)
			opened := time.Now().Add(-2 * time.Hour)
			queryPeriodByKeyFunc = func(string, string) (quota.PeriodSpendingUsage, error) {
				return quota.PeriodSpendingUsage{FiveHour: 12, FiveHourWindowStart: opened}, nil
			}
			rec := postQuota(quotaRouter(metadata, nil))
			assertRefusedWithRetryAfter(t, rec, "period_spending_limit_exceeded", 3*3600+1)
			if got := rec.Header().Get("X-CliRelay-Quota-Period"); got != "5h" {
				t.Fatalf("X-CliRelay-Quota-Period = %q, want 5h", got)
			}
		})
	})
	t.Run("account scope reads the account anchor", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			resetQuotaMiddlewareState(t)
			opened := time.Now().Add(-4*time.Hour - 30*time.Minute)
			queryPeriodByEndUserFunc = func(string, string) (quota.PeriodSpendingUsage, error) {
				return quota.PeriodSpendingUsage{FiveHour: 10, FiveHourWindowStart: opened}, nil
			}
			accountMetadata := map[string]string{
				"tenant-id": "tenant-a", "end-user-id": "user-a", "api-key-id": "key-a", "account-period-spending-limit-5h": "10",
			}
			assertRefusedWithRetryAfter(t, postQuota(quotaRouter(accountMetadata, nil)), "period_spending_limit_exceeded", 30*60+1)
		})
	})
	t.Run("a reading that outlived its window still gets a retry soon", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			resetQuotaMiddlewareState(t)
			opened := time.Now().Add(-6 * time.Hour)
			queryPeriodByKeyFunc = func(string, string) (quota.PeriodSpendingUsage, error) {
				return quota.PeriodSpendingUsage{FiveHour: 12, FiveHourWindowStart: opened}, nil
			}
			assertRefusedWithRetryAfter(t, postQuota(quotaRouter(metadata, nil)), "period_spending_limit_exceeded", 1)
		})
	})
	t.Run("no anchor means no boundary", func(t *testing.T) {
		resetQuotaMiddlewareState(t)
		queryPeriodByKeyFunc = func(string, string) (quota.PeriodSpendingUsage, error) {
			return quota.PeriodSpendingUsage{FiveHour: 12}, nil
		}
		assertRefusedWithoutRetryAfter(t, postQuota(quotaRouter(metadata, nil)), "period_spending_limit_exceeded")
	})
}

// Limits that have no boundary to report must not invent one. The contract
// table lists every refusal the quota checks can produce; a new one has to be
// classified here before it can pass.
func TestEveryRefusalIsClassifiedForRetryAfter(t *testing.T) {
	stubPeriodReset(t, func(period quota.Period, now time.Time) (time.Time, bool) {
		return now.Add(time.Hour), period == quota.PeriodDay || period == quota.PeriodWeek || period == quota.PeriodMonth
	})
	wantHeader := map[string]bool{
		"concurrency":                         false, // clears when a running request ends
		"rpm":                                 true,
		"tpm":                                 true,
		"daily requests":                      true,
		"daily requests on the end-user pool": true,
		"total quota":                         false, // lifetime: an operator has to raise it
		"lifetime spending":                   false,
		"daily spending":                      true,
		"account day period":                  true,
		"key week period":                     true,
		"daily usage unavailable":             false, // 503, not a limit
		"account period usage unavailable":    false,
	}
	cases, _ := quotaRefusalContractCases()
	for _, tc := range cases {
		want, classified := wantHeader[tc.name]
		if !classified {
			t.Errorf("refusal %q has no Retry-After decision; add it to this table", tc.name)
			continue
		}
		t.Run(tc.name, func(t *testing.T) {
			resetQuotaMiddlewareState(t)
			tc.setup(t)
			recorder, _, _ := serveQuotaContract(t, tc.metadata, httptest.NewRequest(http.MethodPost, "/v1/responses", nil))
			got := recorder.Header().Get("Retry-After")
			if want && got == "" {
				t.Fatalf("no Retry-After on %s refusal", tc.name)
			}
			if !want && got != "" {
				t.Fatalf("Retry-After %q on %s refusal, which has no boundary", got, tc.name)
			}
			if want {
				if _, ok := retryAfterSeconds(t, recorder); !ok {
					t.Fatalf("unreadable Retry-After %q", got)
				}
			}
		})
	}
	for name := range wantHeader {
		found := false
		for _, tc := range cases {
			found = found || tc.name == name
		}
		if !found {
			t.Errorf("the table classifies %q, which is no longer a refusal case", name)
		}
	}
}

// A count that is only an estimate across the cluster cannot say when it clears.
func TestClusterEstimatedRefusalsHaveNoRetryAfter(t *testing.T) {
	t.Run("rpm", func(t *testing.T) {
		resetQuotaMiddlewareState(t)
		cluster := newQuotaCluster(t, 2)
		router := quotaRouter(map[string]string{"rpm-limit": "4"}, nil)
		for i := 0; i < 3; i++ {
			if _, err := cluster.peer.AdmitRequest(bg(), retryAfterSubject, 0); err != nil {
				t.Fatal(err)
			}
		}
		mustPass(t, postQuota(router))
		assertRefusedWithoutRetryAfter(t, postQuota(router), "rpm_limit_exceeded")
	})
	t.Run("tpm", func(t *testing.T) {
		resetQuotaMiddlewareState(t)
		cluster := newQuotaCluster(t, 2)
		router := quotaRouter(map[string]string{"tpm-limit": "1000"}, nil)
		mustPass(t, postQuota(router))
		RecordTokenUsage(retryAfterSubject, 600)
		cluster.self.Flush()
		cluster.peer.AddTokens(retryAfterSubject, 500)
		cluster.peer.Flush()
		assertRefusedWithoutRetryAfter(t, postQuota(router), "tpm_limit_exceeded")
	})
}

// With the shared counters gone a node enforces its own share on its own exact
// window, and that window can say when it opens again.
func TestClusterNodeShareRefusalKeepsRetryAfter(t *testing.T) {
	resetQuotaMiddlewareState(t)
	cluster := newQuotaCluster(t, 2)
	router := quotaRouter(map[string]string{"rpm-limit": "4"}, nil)
	cluster.mr.Close()
	mustPass(t, postQuota(router))
	mustPass(t, postQuota(router))
	rec := postQuota(router) // the node's share of 4 across 2 nodes is 2
	if rec.Code != http.StatusTooManyRequests || rec.Header().Get("X-CliRelay-Quota-Limit") != "2" {
		t.Fatalf("status = %d limit = %q, want a refusal at the node share of 2", rec.Code, rec.Header().Get("X-CliRelay-Quota-Limit"))
	}
	seconds, ok := retryAfterSeconds(t, rec)
	if !ok || seconds < 1 || seconds > 61 {
		t.Fatalf("Retry-After = %d, %v; want a wait of at most the 60s window", seconds, ok)
	}
}

// Total (lifetime) and concurrency limits clear when somebody acts, not when a
// clock does.
func TestLimitsThatDoNotClearByThemselvesHaveNoRetryAfter(t *testing.T) {
	stubPeriodReset(t, func(quota.Period, time.Time) (time.Time, bool) { return time.Now().Add(time.Hour), true })
	t.Run("concurrency", func(t *testing.T) {
		resetQuotaMiddlewareState(t)
		if _, ok := acquireKeyConcurrency(retryAfterSubject, 1); !ok {
			t.Fatal("could not occupy the only slot")
		}
		assertRefusedWithoutRetryAfter(t, postQuota(quotaRouter(map[string]string{"concurrency-limit": "1"}, nil)), "concurrency_limit_exceeded")
	})
	t.Run("total request quota", func(t *testing.T) {
		resetQuotaMiddlewareState(t)
		countTotalByKeyFunc = func(string) (int64, error) { return 9, nil }
		assertRefusedWithoutRetryAfter(t, postQuota(quotaRouter(map[string]string{"total-quota": "9"}, nil)), "total_quota_exceeded")
	})
	t.Run("lifetime spending", func(t *testing.T) {
		resetQuotaMiddlewareState(t)
		queryTotalCostByKeyFunc = func(string) (float64, error) { return 10, nil }
		assertRefusedWithoutRetryAfter(t, postQuota(quotaRouter(map[string]string{"spending-limit": "10"}, nil)), "spending_limit_exceeded")
	})
}

func matrixWith(metric quota.Metric, window quota.Period, value float64) quota.Matrix {
	m := quota.Matrix{}
	m.Set(metric, window, value)
	return m
}

// Cells of the quota matrix and per-model rules refuse with the same boundaries.
func TestPolicyRefusalsCarryTheirWindowsEnd(t *testing.T) {
	withAnchor := func(account, key quota.Matrix, models map[string]quota.Matrix, opened time.Time) {
		queryAccountSnapshotFunc = func(string, string) (quota.UsageSnapshot, error) {
			return quota.UsageSnapshot{Total: account, Models: models, FiveHourWindowStart: opened}, nil
		}
		queryKeySnapshotFunc = func(string, string) (quota.UsageSnapshot, error) {
			return quota.UsageSnapshot{Total: key, FiveHourWindowStart: opened}, nil
		}
	}
	stub := func(t *testing.T) {
		stubPeriodReset(t, func(period quota.Period, now time.Time) (time.Time, bool) {
			switch period {
			case quota.PeriodDay:
				return now.Add(5 * time.Hour), true
			case quota.PeriodWeek:
				return now.Add(2 * 24 * time.Hour), true
			case quota.PeriodMonth:
				return now.Add(9 * 24 * time.Hour), true
			}
			return time.Time{}, false
		})
	}
	policy := func(p accesspolicy.Policy) *accesspolicy.Policy {
		p.TenantID, p.APIKeyID, p.EndUserID = "tenant-1", "key-1", "user-1"
		return &p
	}

	t.Run("account request cell on a calendar window", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			stub(t)
			installPolicy(t, policy(accesspolicy.Policy{AccountLimits: matrixWith(quota.MetricRequests, quota.PeriodWeek, 50)}),
				nil, nil, matrixWith(quota.MetricRequests, quota.PeriodWeek, 50), quota.Matrix{})
			rec := postModel(policyRouter(), "/v1/chat/completions", "gpt-5")
			assertRefusedWithRetryAfter(t, rec, "request_limit_exceeded", 2*86400+1)
		})
	})
	t.Run("key token cell on the 5h window", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			stub(t)
			used := matrixWith(quota.MetricTokens, quota.PeriodFiveHour, 1000)
			installPolicy(t, policy(accesspolicy.Policy{KeyLimits: matrixWith(quota.MetricTokens, quota.PeriodFiveHour, 1000)}),
				nil, nil, quota.Matrix{}, used)
			withAnchor(quota.Matrix{}, used, nil, time.Now().Add(-time.Hour))
			rec := postModel(policyRouter(), "/v1/chat/completions", "gpt-5")
			assertRefusedWithRetryAfter(t, rec, "token_limit_exceeded", 4*3600+1)
			if got := rec.Header().Get("X-CliRelay-Quota-Scope"); got != "key" {
				t.Fatalf("scope = %q, want key", got)
			}
		})
	})
	t.Run("lifetime cell never clears", func(t *testing.T) {
		stub(t)
		installPolicy(t, policy(accesspolicy.Policy{AccountLimits: matrixWith(quota.MetricTokens, quota.PeriodLifetime, 100)}),
			nil, nil, matrixWith(quota.MetricTokens, quota.PeriodLifetime, 100), quota.Matrix{})
		rec := postModel(policyRouter(), "/v1/chat/completions", "gpt-5")
		assertRefusedWithoutRetryAfter(t, rec, "token_limit_exceeded")
	})
	t.Run("5h cell whose window never opened has no boundary", func(t *testing.T) {
		stub(t)
		used := matrixWith(quota.MetricRequests, quota.PeriodFiveHour, 10)
		installPolicy(t, policy(accesspolicy.Policy{KeyLimits: matrixWith(quota.MetricRequests, quota.PeriodFiveHour, 10)}),
			nil, nil, quota.Matrix{}, used)
		rec := postModel(policyRouter(), "/v1/chat/completions", "gpt-5")
		assertRefusedWithoutRetryAfter(t, rec, "request_limit_exceeded")
	})
	t.Run("model rule on a calendar window", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			stub(t)
			rule := quota.ModelRule{ID: "opus", Models: []string{"claude-opus-*"}, Limits: costWeek(100)}
			installPolicy(t, policy(accesspolicy.Policy{AccountRules: []quota.ModelRule{rule}}),
				map[string]quota.Matrix{"claude-opus-4-5": costWeek(100)}, nil, quota.Matrix{}, quota.Matrix{})
			rec := postModel(policyRouter(), "/v1/chat/completions", "claude-opus-4-5")
			assertRefusedWithRetryAfter(t, rec, "model_quota_exceeded", 2*86400+1)
		})
	})
	t.Run("model rule on the 5h window reads the anchor of its own scope", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			stub(t)
			rule := quota.ModelRule{ID: "opus", Models: []string{"claude-opus-*"}, Limits: matrixWith(quota.MetricCost, quota.PeriodFiveHour, 5)}
			models := map[string]quota.Matrix{"claude-opus-4-5": matrixWith(quota.MetricCost, quota.PeriodFiveHour, 5)}
			installPolicy(t, policy(accesspolicy.Policy{KeyRules: []quota.ModelRule{rule}}), nil, nil, quota.Matrix{}, quota.Matrix{})
			// Only the key's snapshot carries an open window; the account's must not be read.
			queryKeySnapshotFunc = func(string, string) (quota.UsageSnapshot, error) {
				return quota.UsageSnapshot{Models: models, FiveHourWindowStart: time.Now().Add(-90 * time.Minute)}, nil
			}
			queryAccountSnapshotFunc = func(string, string) (quota.UsageSnapshot, error) {
				return quota.UsageSnapshot{FiveHourWindowStart: time.Now().Add(-10 * time.Minute)}, nil
			}
			rec := postModel(policyRouter(), "/v1/chat/completions", "claude-opus-4-5")
			assertRefusedWithRetryAfter(t, rec, "model_quota_exceeded", 3*3600+30*60+1)
		})
	})
	t.Run("model rule on the lifetime window never clears", func(t *testing.T) {
		stub(t)
		rule := quota.ModelRule{ID: "opus", Models: []string{"claude-opus-*"}, Limits: matrixWith(quota.MetricCost, quota.PeriodLifetime, 5)}
		installPolicy(t, policy(accesspolicy.Policy{AccountRules: []quota.ModelRule{rule}}),
			map[string]quota.Matrix{"claude-opus-4-5": matrixWith(quota.MetricCost, quota.PeriodLifetime, 5)}, nil, quota.Matrix{}, quota.Matrix{})
		rec := postModel(policyRouter(), "/v1/chat/completions", "claude-opus-4-5")
		assertRefusedWithoutRetryAfter(t, rec, "model_quota_exceeded")
	})
}

// A Responses WebSocket turn is refused with the same verdict, and the handler
// forwards its headers, so the wait reaches that client too.
func TestWebsocketTurnRefusalCarriesRetryAfter(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		resetQuotaMiddlewareState(t)
		policy := parseQuotaPolicy("sk-ws-turn", map[string]string{"rpm-limit": "1"})
		gate := policy.turnGate(nil)
		release, rejection := gate()
		if rejection != nil {
			t.Fatalf("first turn refused: %+v", rejection)
		}
		release()
		_, rejection = gate()
		if rejection == nil || rejection.StatusCode != http.StatusTooManyRequests {
			t.Fatalf("second turn = %+v, want a 429 refusal", rejection)
		}
		// Both turns are in the window and the limit is one, so both must leave.
		if got := rejection.Headers.Get("Retry-After"); got != "61" {
			t.Fatalf("Retry-After on the turn refusal = %q, want 61", got)
		}
	})
}
