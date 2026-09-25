package dnswatch

import (
	"testing"
	"time"
)

var (
	okProbe   = probeResult{ok: true, status: 204}
	failProbe = probeResult{status: 503, reason: "unexpected status 503"}
	stateT0   = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
)

func testThresholds() thresholds {
	return thresholds{fail: 3, recover: 3, minChange: time.Minute}
}

func TestNodeStateNeedsFailThresholdToGoDown(t *testing.T) {
	s := &nodeState{healthy: true}
	now := stateT0
	for i := 1; i <= 2; i++ {
		now = now.Add(10 * time.Second)
		if obs := s.observe(failProbe, now, testThresholds()); obs.changed || !s.healthy {
			t.Fatalf("failure %d flipped the node early", i)
		}
	}
	now = now.Add(10 * time.Second)
	if obs := s.observe(failProbe, now, testThresholds()); !obs.changed || s.healthy {
		t.Fatal("third consecutive failure should mark the node unhealthy")
	}
	if s.failures != 3 || s.lastFailure != "unexpected status 503" || !s.lastChange.Equal(now) {
		t.Fatalf("unexpected state after going down: %+v", s)
	}
}

func TestNodeStateNeedsRecoverThresholdToComeBack(t *testing.T) {
	s := &nodeState{healthy: false}
	now := stateT0
	for i := 1; i <= 2; i++ {
		now = now.Add(10 * time.Second)
		if obs := s.observe(okProbe, now, testThresholds()); obs.changed || s.healthy {
			t.Fatalf("success %d flipped the node early", i)
		}
	}
	now = now.Add(10 * time.Second)
	if obs := s.observe(okProbe, now, testThresholds()); !obs.changed || !s.healthy {
		t.Fatal("third consecutive success should mark the node healthy")
	}
}

func TestNodeStateStreaksMustBeConsecutive(t *testing.T) {
	s := &nodeState{healthy: true}
	now := stateT0
	for _, result := range []probeResult{failProbe, failProbe, okProbe, failProbe, failProbe} {
		now = now.Add(10 * time.Second)
		if s.observe(result, now, testThresholds()).changed {
			t.Fatal("an interrupted failure streak must not flip the node")
		}
	}
	if !s.healthy || s.failures != 2 {
		t.Fatalf("expected a healthy node with a 2-failure streak, got %+v", s)
	}
}

func TestNodeStateMinChangeIntervalDefersFlip(t *testing.T) {
	flippedAt := stateT0
	s := &nodeState{healthy: false, lastChange: flippedAt}
	now := flippedAt
	for i := 1; i <= 3; i++ {
		now = now.Add(10 * time.Second)
		obs := s.observe(okProbe, now, testThresholds())
		if obs.changed {
			t.Fatalf("success %d flipped the node inside min_change_interval", i)
		}
		if i == 3 && !obs.deferredUntil.Equal(flippedAt.Add(time.Minute)) {
			t.Fatalf("deferredUntil = %v, want %v", obs.deferredUntil, flippedAt.Add(time.Minute))
		}
	}
	now = flippedAt.Add(time.Minute)
	if obs := s.observe(okProbe, now, testThresholds()); !obs.changed || !s.healthy {
		t.Fatal("the flip should happen on the first probe after min_change_interval")
	}
}

func TestNodeStateInitialStateIsNotDebounced(t *testing.T) {
	// The state inferred from DNS at startup is not a flip, so a node that is
	// actually down is pulled after fail_threshold probes, not a minute later.
	s := &nodeState{healthy: true}
	now := stateT0
	var obs observation
	for range 3 {
		now = now.Add(time.Second)
		obs = s.observe(failProbe, now, testThresholds())
	}
	if !obs.changed || s.healthy {
		t.Fatal("the first flip after startup must not wait for min_change_interval")
	}
}

func TestNodeStateConfirmation(t *testing.T) {
	s := &nodeState{healthy: true}
	s.observe(failProbe, stateT0, testThresholds())
	if s.confirmed {
		t.Fatal("a failed probe must not confirm the node")
	}
	s.observe(okProbe, stateT0.Add(time.Second), testThresholds())
	if !s.confirmed || !s.healthy {
		t.Fatal("a successful probe should confirm the node")
	}
}
