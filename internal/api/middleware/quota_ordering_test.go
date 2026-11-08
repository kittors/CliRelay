package middleware

import (
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
	bDecision, bVerdict := resolveModelPolicy(access, b, "claude-opus-4-7")
	if bVerdict != nil {
		return // Correctly refused one of the two requests.
	}
	b.SetModel(bDecision.FinalModel)
	// A resumes while B is still in flight. B has a later reservation number.
	_, aVerdict := resolveModelPolicy(access, a, "claude-opus-4-7")
	if aVerdict == nil {
		t.Fatal("both model decisions admitted against a one-request model cap")
	}
}

func TestPolicyCountCannotMissConcurrentSettlement(t *testing.T) {
	installPolicy(t, &accesspolicy.Policy{
		TenantID: "tenant-1", APIKeyID: "key-1", EndUserID: "user-1",
		AccountLimits: matrixWith(quota.MetricRequests, quota.PeriodWeek, 1),
	}, nil, nil, quota.Matrix{}, quota.Matrix{})
	b := newBurst(t, map[string]map[string]string{policyTestKey: policyKeyMetadata})
	first := b.hold(policyTestKey, "gpt-5", true)
	first.finish(t)
	if got := quotaLedger.Held(policyKeyOwner.Account).Requests; got != 1 {
		t.Fatalf("setup: held requests = %v, want 1", got)
	}
	queryAccountSnapshotFunc = func(string, string) (quota.UsageSnapshot, error) {
		// The read has already selected a pre-commit snapshot. The other
		// request now commits and releases its hold before this read returns.
		oldSnapshot := quota.UsageSnapshot{}
		quotaLedger.Settle(first.requestID, true, 1, 1)
		return oldSnapshot, nil
	}
	assertRefused(t, b.send(policyTestKey, "gpt-5"), "1", "request limit")
}

func TestPeriodCostCannotMissConcurrentSettlement(t *testing.T) {
	resetQuotaMiddlewareState(t)
	queryPeriodByKeyFunc = func(string, string) (quota.PeriodSpendingUsage, error) {
		return quota.PeriodSpendingUsage{}, nil
	}
	b := newBurst(t, map[string]map[string]string{
		contractKey: {"tenant-id": "tenant-1", "api-key-id": "key-1", "key-period-spending-limit-day": "1"},
	})
	teach(inflight.Owner{Key: inflight.KeyName("key-1")}, 1, 100)
	first := b.hold(contractKey, "gpt-5", true)
	first.finish(t)
	if got := quotaLedger.Held(inflight.KeyName("key-1")).Cost; got != 1 {
		t.Fatalf("setup: held cost = %v, want 1", got)
	}
	queryPeriodByKeyFunc = func(string, string) (quota.PeriodSpendingUsage, error) {
		oldSnapshot := quota.PeriodSpendingUsage{}
		quotaLedger.Settle(first.requestID, true, 1, 100)
		return oldSnapshot, nil
	}
	assertRefused(t, b.send(contractKey, "gpt-5"), "1", "spending limit")
}
