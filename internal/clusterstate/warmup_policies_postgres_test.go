package clusterstate

import (
	"context"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/management/warmup"
)

func TestWarmupPoliciesStore(t *testing.T) {
	db := openTestDB(t)
	store := NewWarmupPolicies(db)
	ctx := context.Background()

	rev, err := store.SavePolicy(ctx, warmup.Policy{ID: "p1", TenantID: "t1", Name: "morning", Enabled: true, IntervalSeconds: 3600})
	if err != nil || rev != 1 {
		t.Fatalf("first save revision = %d err=%v", rev, err)
	}
	// Same id in another tenant is a different policy.
	if _, err := store.SavePolicy(ctx, warmup.Policy{ID: "p1", TenantID: "t2", Name: "other"}); err != nil {
		t.Fatal(err)
	}
	listed, err := store.ListPolicies(ctx, "t1")
	if err != nil || len(listed) != 1 || listed[0].Policy.Name != "morning" || listed[0].Revision != 1 {
		t.Fatalf("tenant list = %+v err=%v", listed, err)
	}
	if all, _ := store.ListPolicies(ctx, ""); len(all) != 2 {
		t.Fatalf("all tenants = %d, want 2", len(all))
	}

	ran := time.Now().UTC().Truncate(time.Second)
	withRun := listed[0].Policy
	withRun.LastRunAt = &ran
	withRun.TotalRuns = 1
	if wrote, err := store.SaveRunState(ctx, withRun, 1); err != nil || !wrote {
		t.Fatalf("run state write = %v %v", wrote, err)
	}
	if rev, err := store.SavePolicy(ctx, warmup.Policy{ID: "p1", TenantID: "t1", Name: "renamed", Enabled: true}); err != nil || rev != 2 {
		t.Fatalf("edit revision = %d err=%v", rev, err)
	}
	if wrote, _ := store.SaveRunState(ctx, withRun, 1); wrote {
		t.Fatal("run state read before an edit must not overwrite the edit")
	}
	listed, _ = store.ListPolicies(ctx, "t1")
	if listed[0].Policy.Name != "renamed" || listed[0].Revision != 2 {
		t.Fatalf("edit lost: %+v", listed[0])
	}
}

// Warmup policies survive a restart: a new scheduler over the same table
// picks up the policy and its run bookkeeping.
func TestWarmupPoliciesSurviveRestart(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	now := time.Date(2026, 9, 24, 10, 0, 0, 0, time.UTC)

	first := warmup.NewPolicyScheduler(nil, warmup.NewDriverRegistry(), nil)
	first.SetStore(NewWarmupPolicies(db))
	first.SetNowFunc(func() time.Time { return now })
	if err := first.SavePolicy(ctx, warmup.Policy{ID: "persisted", TenantID: "t1", Enabled: true, IntervalSeconds: 3600}); err != nil {
		t.Fatal(err)
	}
	first.EvaluateTick(ctx)

	restarted := warmup.NewPolicyScheduler(nil, warmup.NewDriverRegistry(), nil)
	restarted.SetStore(NewWarmupPolicies(db))
	policies, err := restarted.ListPolicies(ctx, "t1")
	if err != nil || len(policies) != 1 {
		t.Fatalf("policies after restart = %+v err=%v", policies, err)
	}
	if policies[0].TotalRuns != 1 || policies[0].LastRunAt == nil || !policies[0].LastRunAt.Equal(now) {
		t.Fatalf("run bookkeeping must persist: %+v", policies[0])
	}
}
