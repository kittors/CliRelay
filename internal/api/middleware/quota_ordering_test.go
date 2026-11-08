package middleware

import (
	"fmt"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/accesspolicy"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/quota"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/quota/inflight"
)

// These regressions reproduced on dev 068757f3 before the ordering fix.
// They use the existing local fixtures, with no real upstream or database.
// A failure means the safety invariant did not hold for the forced schedule.

func TestModelGateCannotOvertakeUnknownEarlierModel(t *testing.T) {
	access := &accesspolicy.Policy{
		TenantID: "tenant-1", APIKeyID: "key-1", EndUserID: "user-1",
		KeyRules: []quota.ModelRule{{
			ID: "one-opus", Models: []string{"claude-opus-*"},
			Limits: matrixWith(quota.MetricRequests, quota.PeriodWeek, 1),
		}},
	}
	installPolicy(t, access, nil, nil, quota.Matrix{}, quota.Matrix{})
	// A takes its reservation, then is delayed before its model decision.
	a := quotaLedger.Reserve("review-a", policyKeyOwner)
	defer a.Cancel()
	b := quotaLedger.Reserve("review-b", policyKeyOwner)
	defer b.Cancel()
	// B overtakes A: A's model is unknown, so B excludes it.
	_, bVerdict := resolveModelPolicy(access, b, "claude-opus-4-7")
	if bVerdict != nil {
		t.Fatalf("the first model decision should be admitted: %+v", bVerdict)
	}
	// A resumes while B is still in flight. B has a later reservation number.
	_, aVerdict := resolveModelPolicy(access, a, "claude-opus-4-7")
	if aVerdict == nil {
		t.Fatal("both model decisions admitted against a one-request model cap")
	}
}

func TestPolicyCountCannotMissConcurrentSettlement(t *testing.T) {
	for _, tc := range []struct {
		scope  string
		metric quota.Metric
		window quota.Period
		limit  float64
		code   string
	}{
		{"account", quota.MetricRequests, quota.PeriodWeek, 1, "request_limit_exceeded"},
		{"account", quota.MetricTokens, quota.PeriodWeek, 100, "token_limit_exceeded"},
		{"key", quota.MetricRequests, quota.PeriodWeek, 1, "request_limit_exceeded"},
		{"key", quota.MetricTokens, quota.PeriodWeek, 100, "token_limit_exceeded"},
		{"key", quota.MetricCost, quota.PeriodLifetime, 1, "spending_limit_exceeded"},
	} {
		t.Run(tc.scope+"/"+string(tc.metric), func(t *testing.T) {
			policy := &accesspolicy.Policy{TenantID: "tenant-1", APIKeyID: "key-1", EndUserID: "user-1"}
			if tc.scope == "account" {
				policy.AccountLimits = matrixWith(tc.metric, tc.window, tc.limit)
			} else {
				policy.KeyLimits = matrixWith(tc.metric, tc.window, tc.limit)
			}
			installPolicy(t, policy, nil, nil, quota.Matrix{}, quota.Matrix{})
			teach(policyKeyOwner, 1, 100)
			b := newBurst(t, map[string]map[string]string{policyTestKey: policyKeyMetadata})
			first := b.hold(policyTestKey, "gpt-5", true)
			first.finish(t)
			if got := quotaLedger.Held(policyKeyOwner.Account).Requests; got != 1 {
				t.Fatalf("setup: held requests = %v, want 1", got)
			}
			read := func(string, string) (quota.UsageSnapshot, error) {
				// The database selected this pre-commit snapshot. Settlement
				// releases the old hold before the result reaches admission.
				oldSnapshot := quota.UsageSnapshot{}
				quotaLedger.Settle(first.requestID, true, 1, 100)
				return oldSnapshot, nil
			}
			if tc.scope == "account" {
				queryAccountSnapshotFunc = read
			} else {
				queryKeySnapshotFunc = read
			}
			rec := b.send(policyTestKey, "gpt-5")
			assertRefused(t, rec, fmt.Sprint(tc.limit), tc.code)
			if got := rec.Header().Get("X-CliRelay-Quota-Scope"); got != tc.scope {
				t.Fatalf("scope = %q, want %q", got, tc.scope)
			}
		})
	}
}

func TestPeriodCostCannotMissConcurrentSettlement(t *testing.T) {
	for _, scope := range []string{"key", "account"} {
		for _, period := range quota.OrderedPeriods {
			t.Run(scope+"/"+string(period), func(t *testing.T) {
				resetQuotaMiddlewareState(t)
				metadata := map[string]string{"tenant-id": "tenant-1", "api-key-id": "key-1", "end-user-id": "user-1", scope + "-period-spending-limit-" + string(period): "1"}
				b := newBurst(t, map[string]map[string]string{contractKey: metadata})
				teach(inflight.Owner{Key: inflight.KeyName("key-1"), Account: inflight.AccountName("user-1")}, 1, 100)
				first := b.hold(contractKey, "gpt-5", true)
				first.finish(t)
				if got := quotaLedger.Held(inflight.KeyName("key-1")).Cost; got != 1 {
					t.Fatalf("setup: held cost = %v, want 1", got)
				}
				read := func(string, string) (quota.PeriodSpendingUsage, error) {
					oldSnapshot := quota.PeriodSpendingUsage{}
					quotaLedger.Settle(first.requestID, true, 1, 100)
					return oldSnapshot, nil
				}
				if scope == "key" {
					queryPeriodByKeyFunc = read
				} else {
					queryPeriodByEndUserFunc = read
				}
				rec := b.send(contractKey, "gpt-5")
				assertRefused(t, rec, "1", "spending limit")
				if got := rec.Header().Get("X-CliRelay-Quota-Scope"); got != scope {
					t.Fatalf("scope = %q, want %q", got, scope)
				}
			})
		}
	}
}
