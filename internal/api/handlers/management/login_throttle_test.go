package management

import (
	"testing"
	"time"
)

func newTestThrottle() *loginThrottle {
	// Constructed directly so the test does not start the cleanup goroutine;
	// purgeStale is driven explicitly with a fake clock instead.
	return &loginThrottle{
		attempts: make(map[string]*loginAttempt),
		stop:     make(chan struct{}),
	}
}

func TestLoginThrottleBlocksAfterMaxFailures(t *testing.T) {
	t.Parallel()

	throttle := newTestThrottle()
	now := time.Now()

	for i := range loginMaxFailures - 1 {
		throttle.recordFailure("198.51.100.10", now)
		if blocked := throttle.blockedFor("198.51.100.10", now); blocked > 0 {
			t.Fatalf("blocked after %d failures, want block only at %d", i+1, loginMaxFailures)
		}
	}

	throttle.recordFailure("198.51.100.10", now)
	blocked := throttle.blockedFor("198.51.100.10", now)
	if blocked <= 0 {
		t.Fatalf("blockedFor = %v after %d failures, want a block", blocked, loginMaxFailures)
	}
	if blocked > loginBlockDuration {
		t.Fatalf("blockedFor = %v, want at most %v", blocked, loginBlockDuration)
	}
}

func TestLoginThrottleReleasesAfterBlockExpires(t *testing.T) {
	t.Parallel()

	throttle := newTestThrottle()
	now := time.Now()
	for range loginMaxFailures {
		throttle.recordFailure("198.51.100.11", now)
	}

	if throttle.blockedFor("198.51.100.11", now.Add(loginBlockDuration-time.Second)) <= 0 {
		t.Fatal("client released before the block expired")
	}
	if blocked := throttle.blockedFor("198.51.100.11", now.Add(loginBlockDuration)); blocked != 0 {
		t.Fatalf("blockedFor = %v at expiry, want 0", blocked)
	}
}

func TestLoginThrottleSuccessClearsFailures(t *testing.T) {
	t.Parallel()

	throttle := newTestThrottle()
	now := time.Now()
	for range loginMaxFailures - 1 {
		throttle.recordFailure("198.51.100.12", now)
	}
	throttle.recordSuccess("198.51.100.12")

	// The counter must restart, so one more failure cannot trip the block.
	throttle.recordFailure("198.51.100.12", now)
	if blocked := throttle.blockedFor("198.51.100.12", now); blocked > 0 {
		t.Fatalf("blockedFor = %v, want 0 after a successful login reset the count", blocked)
	}
}

// Guards the invariant that a block cannot be shed by going quiet. The state is
// built directly because the current constants (30m block, 2h idle) make this
// unreachable through recordFailure; the guard exists for a future config where
// a block outlasts the idle window.
func TestLoginThrottlePurgeKeepsBlockedEntries(t *testing.T) {
	t.Parallel()

	throttle := newTestThrottle()
	now := time.Now()
	throttle.attempts["198.51.100.13"] = &loginAttempt{
		blockedUntil: now.Add(24 * time.Hour),
		lastActivity: now.Add(-loginMaxIdleTime - time.Hour),
	}

	throttle.purgeStale(now)

	if blocked := throttle.blockedFor("198.51.100.13", now); blocked <= 0 {
		t.Fatal("purge dropped a still-blocked client, letting it shed the block by idling")
	}
}

func TestLoginThrottlePurgeDropsIdleEntries(t *testing.T) {
	t.Parallel()

	throttle := newTestThrottle()
	now := time.Now()
	throttle.recordFailure("198.51.100.14", now)

	throttle.purgeStale(now.Add(loginMaxIdleTime + time.Minute))

	throttle.mu.Lock()
	remaining := len(throttle.attempts)
	throttle.mu.Unlock()
	if remaining != 0 {
		t.Fatalf("attempts retained %d idle entry/entries, want 0", remaining)
	}
}

func TestLoginThrottleNilIsInert(t *testing.T) {
	t.Parallel()

	var throttle *loginThrottle
	// Handlers may run with an unset throttle in tests; none of these may panic.
	throttle.recordFailure("198.51.100.15", time.Now())
	throttle.recordSuccess("198.51.100.15")
	throttle.purgeStale(time.Now())
	throttle.close()
	if blocked := throttle.blockedFor("198.51.100.15", time.Now()); blocked != 0 {
		t.Fatalf("blockedFor = %v on nil throttle, want 0", blocked)
	}
}
