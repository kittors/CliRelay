package management

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/cluster/sharedredis"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/cluster/sharedstate"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
)

// countingThrottleStore counts shared peeks, to show which checks reach Redis.
type countingThrottleStore struct {
	*sharedstate.Store
	peeks atomic.Int64
}

func (s *countingThrottleStore) ThrottlePeek(ctx context.Context, bucket string, spec sharedstate.ThrottleSpec, now time.Time) (sharedstate.ThrottleState, error) {
	s.peeks.Add(1)
	return s.Store.ThrottlePeek(ctx, bucket, spec, now)
}

func installClusterThrottle(t *testing.T) (*miniredis.Miniredis, *countingThrottleStore) {
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
	store := &countingThrottleStore{Store: sharedstate.New(client, sharedstate.Options{NodeID: "self"})}
	SetClusterThrottleStore(store)
	t.Cleanup(func() {
		SetClusterThrottleStore(nil)
		store.Close()
		client.Close()
	})
	return mr, store
}

func TestClusterLoginThrottleCountsFailuresFromEveryNode(t *testing.T) {
	installClusterThrottle(t)
	nodeA, nodeB := newTestThrottle(nil), newTestThrottle(nil)
	key := accountThrottleKey(scopeUserAccount, "alice")
	now := time.Now()

	// defaultAccountFailureLimit is 5: three failures on one node and two on the
	// other must arm the block, although neither node saw five.
	for i := 0; i < 3; i++ {
		if d := nodeA.recordFailure(key, now); d.Outcome != outcomeAllow {
			t.Fatalf("failure %d on node a = %+v", i+1, d)
		}
	}
	if d := nodeB.recordFailure(key, now); d.Outcome != outcomeAllow || d.ShortCount != 4 {
		t.Fatalf("fourth failure cluster-wide = %+v", d)
	}
	armed := nodeB.recordFailure(key, now)
	if armed.Outcome != outcomeRateLimited || !armed.NewlyArmed || armed.RetryAfter != time.Minute {
		t.Fatalf("fifth failure cluster-wide must arm the first rung: %+v", armed)
	}
	if d := nodeA.evaluate(key, now.Add(time.Second)); d.Outcome != outcomeRateLimited {
		t.Fatalf("node a must enforce a block armed by node b: %+v", d)
	}
}

func TestClusterLoginThrottleLocalBlockSkipsRedis(t *testing.T) {
	_, store := installClusterThrottle(t)
	node := newTestThrottle(nil)
	key := accountThrottleKey(scopeUserAccount, "bob")
	now := time.Now()
	for i := 0; i < defaultAccountFailureLimit; i++ {
		node.recordFailure(key, now)
	}
	before := store.peeks.Load()
	if d := node.evaluate(key, now.Add(time.Second)); d.Outcome == outcomeAllow {
		t.Fatalf("expected a block: %+v", d)
	}
	if store.peeks.Load() != before {
		t.Fatal("a block this node armed itself must be enforced without a Redis round trip")
	}
	if d := node.evaluate(accountThrottleKey(scopeUserAccount, "carol"), now); d.Outcome != outcomeAllow || store.peeks.Load() != before+1 {
		t.Fatalf("an unblocked bucket consults the cluster: %+v peeks=%d", d, store.peeks.Load())
	}
}

func TestClusterLoginThrottleSuccessClearsSharedBucket(t *testing.T) {
	installClusterThrottle(t)
	nodeA, nodeB := newTestThrottle(nil), newTestThrottle(nil)
	key := accountThrottleKey(scopeUserAccount, "dave")
	now := time.Now()
	for i := 0; i < 4; i++ {
		nodeB.recordFailure(key, now)
	}
	nodeA.recordSuccess(key)
	deadline := time.Now().Add(3 * time.Second)
	for {
		d := nodeA.recordFailure(key, now)
		if d.ShortCount == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("a success on node a must clear the failures node b recorded, got %+v", d)
		}
		nodeA.recordSuccess(key)
		time.Sleep(10 * time.Millisecond)
	}
}

// TestClusterLoginThrottleFallsBackToLocalThresholds pins the degraded mode:
// without the shared store each node counts on its own, with the full
// thresholds rather than a share of them.
func TestClusterLoginThrottleFallsBackToLocalThresholds(t *testing.T) {
	mr, store := installClusterThrottle(t)
	node := newTestThrottle(nil)
	key := accountThrottleKey(scopeUserAccount, "erin")
	now := time.Now()

	mr.Close()
	var d throttleDecision
	for i := 0; i < defaultAccountFailureLimit-1; i++ {
		if d = node.recordFailure(key, now); d.Outcome != outcomeAllow {
			t.Fatalf("failure %d below the full threshold = %+v", i+1, d)
		}
	}
	if store.Available() {
		t.Fatal("store must have noticed the outage")
	}
	if d = node.recordFailure(key, now); d.Outcome == outcomeAllow {
		t.Fatalf("the full local threshold still arms a block: %+v", d)
	}
}

func TestClusterManagementKeyClearIsDeduplicated(t *testing.T) {
	sharedClears.Range(func(k, _ any) bool {
		sharedClears.Delete(k)
		return true
	})
	now := time.Now()
	if !claimSharedClear("management_key|10.0.0.1/32", now) {
		t.Fatal("first clear must go through")
	}
	if claimSharedClear("management_key|10.0.0.1/32", now.Add(time.Second)) {
		t.Fatal("clears of one bucket are spaced out")
	}
	if !claimSharedClear("management_key|10.0.0.1/32", now.Add(sharedClearInterval)) {
		t.Fatal("after the interval the bucket is cleared again")
	}
}
