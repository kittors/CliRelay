package middleware

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/accesspolicy"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/quota"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/quota/inflight"
)

func TestConcurrentModelAdmissionAcrossTransportsAndScopes(t *testing.T) {
	for _, scope := range []string{"key", "account"} {
		for _, transport := range []string{"http", "websocket"} {
			t.Run(scope+"/"+transport, func(t *testing.T) {
				policy := &accesspolicy.Policy{TenantID: "tenant-1", APIKeyID: "key-1", EndUserID: "user-1"}
				rules := []quota.ModelRule{{ID: "one", Models: []string{"claude-opus-*"}, Limits: matrixWith(quota.MetricRequests, quota.PeriodWeek, 1)}}
				if scope == "key" {
					policy.KeyRules = rules
				} else {
					policy.AccountRules = rules
				}
				installPolicy(t, policy, nil, nil, quota.Matrix{}, quota.Matrix{})
				keys := map[string]map[string]string{policyTestKey: policyKeyMetadata}
				keyNames := []string{policyTestKey}
				if scope == "account" {
					sibling := *policy
					sibling.APIKeyID = "key-2"
					accesspolicy.Publish(map[string]*accesspolicy.Policy{policyTestKey: policy, "sk-sibling-XXXX": &sibling})
					keys["sk-sibling-XXXX"] = map[string]string{"tenant-id": "tenant-1", "api-key-id": "key-2", "end-user-id": "user-1"}
					keyNames = append(keyNames, "sk-sibling-XXXX")
				}
				const n = 12
				if transport == "http" {
					b := newBurst(t, keys)
					h := &heldRequest{entered: make(chan string, n), release: make(chan struct{})}
					b.held.Store("concurrent", h)
					var finish sync.Once
					t.Cleanup(func() { finish.Do(func() { close(h.release) }) })
					completed := make(chan *httptest.ResponseRecorder, n)
					start := make(chan struct{})
					for i := range n {
						go func(key string) { <-start; completed <- b.serve(key, "claude-opus-4-7", "concurrent") }(keyNames[i%len(keyNames)])
					}
					close(start)
					admitted := 0
					deadline := time.After(5 * time.Second)
					for range n {
						select {
						case <-h.entered:
							admitted++
						case rec := <-completed:
							assertRefused(t, rec, "1", "model_quota_exceeded")
						case <-deadline:
							t.Fatal("model admissions did not complete")
						}
					}
					finish.Do(func() { close(h.release) })
					for range admitted {
						assertAdmitted(t, <-completed)
					}
					if admitted != 1 {
						t.Fatalf("admitted %d requests against cap 1", admitted)
					}
				} else {
					// Each connection obtains its own reservation before all model
					// decisions race. Keep every turn alive until all decisions finish.
					results := make(chan bool, n)
					start := make(chan struct{})
					for i := range n {
						key := keyNames[i%len(keyNames)]
						gate, modelGate, _ := websocketGates(t, key, keys[key])
						release, rejection := gate()
						if rejection != nil {
							t.Fatalf("quota gate: %+v", rejection)
						}
						t.Cleanup(release)
						go func() { <-start; _, rejection := modelGate("claude-opus-4-7"); results <- rejection == nil }()
					}
					close(start)
					admitted := 0
					for range n {
						select {
						case ok := <-results:
							if ok {
								admitted++
							}
						case <-time.After(5 * time.Second):
							t.Fatal("model turn decisions did not complete")
						}
					}
					if admitted != 1 {
						t.Fatalf("admitted %d turns against cap 1", admitted)
					}
				}
			})
		}
	}
}

func TestModelFallbackKeepsCapturedHoldsAfterSettlement(t *testing.T) {
	for _, scope := range []string{"key", "account"} {
		t.Run(scope, func(t *testing.T) {
			policy := &accesspolicy.Policy{TenantID: "tenant-1", APIKeyID: "key-1", EndUserID: "user-1"}
			rules := []quota.ModelRule{
				{ID: "opus", Models: []string{"claude-opus-*"}, Limits: matrixWith(quota.MetricRequests, quota.PeriodWeek, 1), FallbackModel: "claude-sonnet-4-5"},
				{ID: "sonnet", Models: []string{"claude-sonnet-*"}, Limits: matrixWith(quota.MetricRequests, quota.PeriodWeek, 1)},
			}
			if scope == "key" {
				policy.KeyRules = rules
			} else {
				policy.AccountRules = rules
			}
			installPolicy(t, policy, nil, nil, quota.Matrix{}, quota.Matrix{})
			first := quotaLedger.Reserve("settling-sonnet", policyKeyOwner)
			first.SetModel("claude-sonnet-4-5")
			quotaLedger.Published("settling-sonnet")
			first.Finish()
			reads := 0
			read := func(string, string) (quota.UsageSnapshot, error) {
				reads++
				// Opus is exhausted. Sonnet commits after this read selected its
				// old snapshot, before the fallback rule reuses that snapshot.
				old := quota.UsageSnapshot{Models: map[string]quota.Matrix{"claude-opus-4-7": matrixWith(quota.MetricRequests, quota.PeriodWeek, 1)}}
				quotaLedger.Settle("settling-sonnet", true, 1, 1)
				return old, nil
			}
			if scope == "key" {
				queryKeySnapshotFunc = read
			} else {
				queryAccountSnapshotFunc = read
			}
			request := quotaLedger.Reserve("fallback-probe", policyKeyOwner)
			defer request.Cancel()
			decision, verdict := resolveModelPolicy(policy, request, "claude-opus-4-7")
			if verdict == nil || decision.Exceeded == nil || decision.Exceeded.Rule.ID != "sonnet" {
				t.Fatalf("decision = %+v, verdict = %+v; fallback must see Sonnet hold", decision, verdict)
			}
			if reads != 1 {
				t.Fatalf("snapshot reads = %d, want one for entire fallback chain", reads)
			}
			if held := quotaLedger.Held(policyKeyOwner.Key); held.Requests != 1 {
				t.Fatalf("settlement did not run independently: %+v", held)
			}
		})
	}
}

func TestModelStageRefreshesSnapshotAfterTotalAdmission(t *testing.T) {
	policy := &accesspolicy.Policy{
		TenantID: "tenant-1", APIKeyID: "key-1", EndUserID: "user-1",
		AccountLimits: matrixWith(quota.MetricRequests, quota.PeriodWeek, 10),
		AccountRules:  []quota.ModelRule{{ID: "opus", Models: []string{"claude-opus-*"}, Limits: matrixWith(quota.MetricRequests, quota.PeriodWeek, 1)}},
	}
	installPolicy(t, policy, nil, nil, quota.Matrix{}, quota.Matrix{})
	first := quotaLedger.Reserve("between-stages", policyKeyOwner)
	first.SetModel("claude-opus-4-7")
	quotaLedger.Published("between-stages")
	first.Finish()
	committed, reads := false, 0
	queryAccountSnapshotFunc = func(string, string) (quota.UsageSnapshot, error) {
		reads++
		if !committed {
			return quota.UsageSnapshot{}, nil
		}
		m := matrixWith(quota.MetricRequests, quota.PeriodWeek, 1)
		return quota.UsageSnapshot{Total: m, Models: map[string]quota.Matrix{"claude-opus-4-7": m}}, nil
	}
	router := gin.New()
	router.Use(func(c *gin.Context) {
		c.Set("apiKey", policyTestKey)
		c.Set("accessMetadata", policyKeyMetadata)
		c.Next()
	})
	router.Use(QuotaMiddleware())
	router.Use(func(c *gin.Context) { committed = true; quotaLedger.Settle("between-stages", true, 1, 1); c.Next() })
	router.Use(ModelPolicyMiddleware(nil))
	router.POST("/v1/chat/completions", func(c *gin.Context) { c.Status(http.StatusNoContent) })
	rec := postModel(router, "/v1/chat/completions", "claude-opus-4-7")
	assertRefused(t, rec, "1", "model_quota_exceeded")
	if reads != 2 {
		t.Fatalf("reads = %d, want fresh total and model snapshots", reads)
	}
}

func TestModelAdmissionReleasesGateOnErrorAndRefusal(t *testing.T) {
	policy := &accesspolicy.Policy{TenantID: "tenant-1", APIKeyID: "key-1", KeyRules: []quota.ModelRule{{ID: "one", Models: []string{"gpt-*"}, Limits: matrixWith(quota.MetricRequests, quota.PeriodWeek, 1)}}}
	installPolicy(t, policy, nil, nil, quota.Matrix{}, quota.Matrix{})
	SetQuotaUsageStaleMaxAge(0)
	t.Cleanup(func() { SetQuotaUsageStaleMaxAge(defaultQuotaUsageStaleMaxAge) })
	for i, outcome := range []string{"error", "refusal", "success"} {
		queryKeySnapshotFunc = func(string, string) (quota.UsageSnapshot, error) {
			if outcome == "error" {
				return quota.UsageSnapshot{}, errors.New("test read unavailable")
			}
			if outcome == "refusal" {
				return quota.UsageSnapshot{Models: map[string]quota.Matrix{"gpt-5": matrixWith(quota.MetricRequests, quota.PeriodWeek, 1)}}, nil
			}
			return quota.UsageSnapshot{}, nil
		}
		r := quotaLedger.Reserve(fmt.Sprintf("gate-release-%d", i), inflight.Owner{Key: inflight.KeyName("key-1")})
		done := make(chan *quotaVerdict, 1)
		go func() { _, verdict := resolveModelPolicy(policy, r, "gpt-5"); done <- verdict }()
		select {
		case verdict := <-done:
			if (verdict == nil) != (outcome == "success") {
				t.Fatalf("%s: verdict = %+v", outcome, verdict)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("a previous model decision kept its gate locked")
		}
		r.Cancel()
	}
}

func TestModelSettlementRefusalUsesFrozenInFlightNote(t *testing.T) {
	policy := &accesspolicy.Policy{TenantID: "tenant-1", APIKeyID: "key-1", KeyRules: []quota.ModelRule{{ID: "one", Models: []string{"gpt-*"}, Limits: matrixWith(quota.MetricRequests, quota.PeriodWeek, 1)}}}
	installPolicy(t, policy, nil, nil, quota.Matrix{}, quota.Matrix{})
	first := quotaLedger.Reserve("note-settle", policyKeyOwner)
	first.SetModel("gpt-5")
	quotaLedger.Published("note-settle")
	first.Finish()
	queryKeySnapshotFunc = func(string, string) (quota.UsageSnapshot, error) {
		quotaLedger.Settle("note-settle", true, 1, 1)
		return quota.UsageSnapshot{}, nil
	}
	rec := newBurst(t, map[string]map[string]string{policyTestKey: policyKeyMetadata}).send(policyTestKey, "gpt-5")
	assertRefused(t, rec, "1", "1 request still in flight")
	if !strings.Contains(rec.Body.String(), "model_quota_exceeded") {
		t.Fatalf("wrong refusal: %s", rec.Body)
	}
}
