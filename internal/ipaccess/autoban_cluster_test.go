package ipaccess

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/cluster/sharedredis"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/cluster/sharedstate"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
)

func installClusterAutoBan(t *testing.T) *miniredis.Miniredis {
	t.Helper()
	mr := miniredis.RunT(t)
	client, err := sharedredis.New(config.ClusterRedisConfig{Addr: mr.Addr()}, sharedredis.Options{
		NodeID: "self", HealthInterval: 20 * time.Millisecond,
		MinBackoff: 10 * time.Millisecond, MaxBackoff: 50 * time.Millisecond, StableAfter: time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	client.Start()
	store := sharedstate.New(client, sharedstate.Options{NodeID: "self"})
	SetClusterAutoBanStore(store)
	t.Cleanup(func() {
		SetClusterAutoBanStore(nil)
		store.Close()
		client.Close()
	})
	return mr
}

func TestClusterAutoBanCountsFailuresFromEveryNode(t *testing.T) {
	installClusterAutoBan(t)
	policy := DefaultPolicy()
	policy.AutoBan.FailureThreshold = 3
	nodeA, nodeB := testRegistry(t, policy), testRegistry(t, policy)
	source := trustedAddress("203.0.113.40")

	for i := 0; i < 2; i++ {
		if out := nodeA.AutoBan().RecordFailure(context.Background(), source, "test"); out.Triggered {
			t.Fatalf("failure %d triggered early", i+1)
		}
	}
	crossing := nodeB.AutoBan().RecordFailure(context.Background(), source, "test")
	if !crossing.Triggered || crossing.Failures != 3 || crossing.CIDR != "203.0.113.40/32" {
		t.Fatalf("the third failure cluster-wide must trigger on whichever node sees it: %+v", crossing)
	}
	// The ban is known everywhere: node a must not trigger a second time.
	for i := 0; i < 3; i++ {
		if out := nodeA.AutoBan().RecordFailure(context.Background(), source, "test"); out.Triggered {
			t.Fatalf("node a re-triggered a ban node b already decided: %+v", out)
		}
	}
}

func TestClusterAutoBanFallsBackToLocalCounting(t *testing.T) {
	mr := installClusterAutoBan(t)
	policy := DefaultPolicy()
	policy.AutoBan.FailureThreshold = 3
	node := testRegistry(t, policy)
	source := trustedAddress("203.0.113.41")

	mr.Close()
	var out AutoBanOutcome
	for i := 0; i < 3; i++ {
		out = node.AutoBan().RecordFailure(context.Background(), source, "test")
	}
	if !out.Triggered || out.Failures != 3 {
		t.Fatalf("without the shared store the node counts on its own with the full threshold: %+v", out)
	}
}
