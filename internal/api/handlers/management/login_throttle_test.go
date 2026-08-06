package management

import (
	"fmt"
	"sync"
	"testing"
	"time"
)

// newTestThrottle builds a loginThrottle without starting the cleanup goroutine,
// so tests control purging explicitly with a fake clock instead of racing a real
// 10-minute ticker.
func newTestThrottle(policies map[throttleScope]throttlePolicy) *loginThrottle {
	th := &loginThrottle{stop: make(chan struct{})}
	for i := range th.shards {
		th.shards[i].entries = make(map[string]*throttleEntry)
	}
	th.setPolicies(policies)
	return th
}

// expireArmedBlocks rewinds every armed block into the past so a test can observe
// post-ban behaviour deterministically. Tests that drive the throttle through the
// HTTP middleware cannot inject a clock — the middleware reads time.Now() itself —
// and sleeping out a real block is unreliable: the bcrypt comparisons that arm the
// ban are slow enough under -race that a block short enough to sleep through is
// also short enough to expire before the assertion that depends on it runs.
func expireArmedBlocks(t *loginThrottle) {
	past := time.Now().Add(-time.Second)
	for i := range t.shards {
		shard := &t.shards[i]
		shard.mu.Lock()
		for _, entry := range shard.entries {
			if !entry.blockedUntil.IsZero() {
				entry.blockedUntil = past
			}
		}
		shard.mu.Unlock()
	}
}

// TestSlidingWindowDropsFailuresOutsideWindow is the B1 regression anchor: the old
// throttle counted failures with no time dimension at all, so four-month-old
// mistypes and this second's mistype counted the same.
func TestSlidingWindowDropsFailuresOutsideWindow(t *testing.T) {
	t.Parallel()

	policy := throttlePolicy{
		Short:      throttleWindow{Limit: 5, Window: 15 * time.Minute},
		Backoff:    []time.Duration{time.Minute},
		ResetAfter: 12 * time.Hour,
	}
	th := newTestThrottle(map[throttleScope]throttlePolicy{scopeUserAccount: policy})
	key := accountThrottleKey(scopeUserAccount, "acct-window")
	t0 := time.Now()

	for i := 0; i < 4; i++ {
		d := th.recordFailure(key, t0)
		if d.Outcome != outcomeAllow {
			t.Fatalf("failure %d: outcome = %v, want allow", i+1, d.Outcome)
		}
	}

	// The four failures above are now outside the 15m short window, so this 5th
	// failure must be counted as if it were the first.
	d := th.recordFailure(key, t0.Add(16*time.Minute))
	if d.Outcome != outcomeAllow {
		t.Fatalf("outcome after a %v quiet gap = %v, want allow (stale failures must not count)", 16*time.Minute, d.Outcome)
	}
}

// TestSlidingWindowBlocksAtThresholdInsideWindow confirms the counterpart: failures
// that land inside one window do trip the block, and the first rung of the
// escalation ladder is used.
func TestSlidingWindowBlocksAtThresholdInsideWindow(t *testing.T) {
	t.Parallel()

	policy := throttlePolicy{
		Short:      throttleWindow{Limit: 5, Window: 15 * time.Minute},
		Backoff:    []time.Duration{2 * time.Minute, 10 * time.Minute},
		ResetAfter: 12 * time.Hour,
		HardBlock:  true,
	}
	th := newTestThrottle(map[throttleScope]throttlePolicy{scopeManagementKey: policy})
	key := throttleKey{Scope: scopeManagementKey, Value: "203.0.113.9/32"}
	t0 := time.Now()

	var last throttleDecision
	for i := 0; i < 5; i++ {
		last = th.recordFailure(key, t0.Add(time.Duration(i)*time.Second))
	}
	if last.Outcome != outcomeBlocked {
		t.Fatalf("outcome at threshold = %v, want blocked", last.Outcome)
	}
	if last.RetryAfter != policy.Backoff[0] {
		t.Fatalf("RetryAfter = %v, want first backoff rung %v", last.RetryAfter, policy.Backoff[0])
	}
}

// TestLongWindowBlocksWhenShortWindowWouldNot is the D2 anchor: a lone short window
// still permits a low, steady drip that adds up to a lot of guesses per day. Here
// every burst stays comfortably under the short-window limit, yet the daily ceiling
// still arms.
func TestLongWindowBlocksWhenShortWindowWouldNot(t *testing.T) {
	t.Parallel()

	policy := throttlePolicy{
		Short:      throttleWindow{Limit: 50, Window: 15 * time.Minute},
		Long:       throttleWindow{Limit: 20, Window: 24 * time.Hour},
		Backoff:    []time.Duration{time.Minute},
		ResetAfter: 48 * time.Hour,
	}
	th := newTestThrottle(map[throttleScope]throttlePolicy{scopeUserAccount: policy})
	key := accountThrottleKey(scopeUserAccount, "acct-longwindow")
	t0 := time.Now()

	var last throttleDecision
	for burst := 0; burst < 5; burst++ {
		// 16 minutes is wider than the 15m short window, so each burst's count
		// rolls off before the next burst starts.
		burstStart := t0.Add(time.Duration(burst) * 16 * time.Minute)
		for i := 0; i < 4; i++ {
			last = th.recordFailure(key, burstStart)
		}
	}
	if last.Outcome != outcomeRateLimited {
		t.Fatalf("outcome after 20 failures spread across 80 minutes = %v, want rate limited by the long window", last.Outcome)
	}
	if last.ShortCount > policy.Short.Limit {
		t.Fatalf("short window count = %d, want it to stay under its own limit %d (the long window must be what armed the block)", last.ShortCount, policy.Short.Limit)
	}
}

// TestStagedBackoffEscalatesAndCapsAtMax walks the escalation ladder across
// repeated offenses, each separated far enough to roll the short window but not
// far enough to trigger the quiet-period reset, and confirms it plateaus at the
// last rung instead of overflowing past it.
func TestStagedBackoffEscalatesAndCapsAtMax(t *testing.T) {
	t.Parallel()

	policy := throttlePolicy{
		Short:      throttleWindow{Limit: 3, Window: 2 * time.Minute},
		Backoff:    []time.Duration{time.Minute, 5 * time.Minute, 15 * time.Minute, 30 * time.Minute},
		ResetAfter: time.Hour,
	}
	th := newTestThrottle(map[throttleScope]throttlePolicy{scopeUserAccount: policy})
	key := accountThrottleKey(scopeUserAccount, "acct-escalate")

	round := func(start time.Time) throttleDecision {
		var last throttleDecision
		for i := 0; i < 3; i++ {
			last = th.recordFailure(key, start.Add(time.Duration(i)*time.Second))
		}
		return last
	}

	t0 := time.Now()
	want := []time.Duration{time.Minute, 5 * time.Minute, 15 * time.Minute, 30 * time.Minute, 30 * time.Minute}
	// Gap between rounds: comfortably longer than the previous block plus the
	// short window (so counts roll over), comfortably shorter than ResetAfter (so
	// the escalation ladder is not cleared).
	gaps := []time.Duration{0, 3 * time.Minute, 8 * time.Minute, 18 * time.Minute, 33 * time.Minute}

	cursor := t0
	for i, gap := range gaps {
		cursor = cursor.Add(gap)
		got := round(cursor)
		if got.Outcome != outcomeRateLimited {
			t.Fatalf("round %d: outcome = %v, want rate limited", i+1, got.Outcome)
		}
		if got.RetryAfter != want[i] {
			t.Fatalf("round %d: RetryAfter = %v, want %v", i+1, got.RetryAfter, want[i])
		}
		cursor = cursor.Add(3 * time.Second)
	}
}

// TestStagedBackoffResetsAfterQuietPeriod confirms the other half of the ladder:
// a quiet period longer than ResetAfter clears both the counters and the
// escalation stage, so a mistake made long ago does not start at a 30m ban.
func TestStagedBackoffResetsAfterQuietPeriod(t *testing.T) {
	t.Parallel()

	policy := throttlePolicy{
		Short:      throttleWindow{Limit: 3, Window: time.Minute},
		Backoff:    []time.Duration{time.Minute, 5 * time.Minute, 15 * time.Minute},
		ResetAfter: 10 * time.Minute,
	}
	th := newTestThrottle(map[throttleScope]throttlePolicy{scopeUserAccount: policy})
	key := accountThrottleKey(scopeUserAccount, "acct-reset")

	t0 := time.Now()
	var first throttleDecision
	var lastFailureAt time.Time
	for i := 0; i < 3; i++ {
		lastFailureAt = t0.Add(time.Duration(i) * time.Second)
		first = th.recordFailure(key, lastFailureAt)
	}
	if first.Outcome != outcomeRateLimited || first.RetryAfter != policy.Backoff[0] {
		t.Fatalf("first round = %+v, want rate limited at %v", first, policy.Backoff[0])
	}

	// The quiet gap must be measured from the last recorded failure, not from t0.
	quiet := lastFailureAt.Add(policy.ResetAfter + time.Second)
	var second throttleDecision
	for i := 0; i < 3; i++ {
		second = th.recordFailure(key, quiet.Add(time.Duration(i)*time.Second))
	}
	if second.Outcome != outcomeRateLimited {
		t.Fatalf("second round outcome = %v, want rate limited", second.Outcome)
	}
	if second.RetryAfter != policy.Backoff[0] {
		t.Fatalf("RetryAfter after a quiet period = %v, want the ladder to restart at %v", second.RetryAfter, policy.Backoff[0])
	}
}

// TestUnauthenticatedScopeNeverBlocks is the B2 regression anchor: a request that
// carried no secret proves nothing about the secret, so it may be rate-limited but
// must never be hard-banned, no matter how many times it repeats.
func TestUnauthenticatedScopeNeverBlocks(t *testing.T) {
	t.Parallel()

	th := newTestThrottle(defaultThrottlePolicies())
	key := throttleKey{Scope: scopeUnauthenticated, Value: "203.0.113.100/32"}
	now := time.Now()

	for i := 0; i < 1000; i++ {
		d := th.recordFailure(key, now)
		if d.Outcome == outcomeBlocked {
			t.Fatalf("attempt %d: outcome = blocked, want allow or rate_limited (never a hard ban for credential-less traffic)", i+1)
		}
	}
}

// TestSharedKeyDowngradesHardBlock confirms a key that cannot identify a single
// client (behind an untrusted proxy/NAT) never produces a 403, even for a scope
// whose policy normally hard-blocks.
func TestSharedKeyDowngradesHardBlock(t *testing.T) {
	t.Parallel()

	policy := throttlePolicy{
		Short:     throttleWindow{Limit: 3, Window: 15 * time.Minute},
		Backoff:   []time.Duration{time.Minute, 5 * time.Minute},
		HardBlock: true,
	}
	th := newTestThrottle(map[throttleScope]throttlePolicy{scopeManagementKey: policy})
	key := throttleKey{Scope: scopeManagementKey, Value: "198.51.100.1/32", Shared: true}
	now := time.Now()

	var last throttleDecision
	for i := 0; i < 3; i++ {
		last = th.recordFailure(key, now.Add(time.Duration(i)*time.Second))
	}
	if last.Outcome != outcomeRateLimited {
		t.Fatalf("shared-key outcome = %v, want rate_limited (429), never blocked (403)", last.Outcome)
	}
}

// TestRecordSuccessClearsOnlyScopedKey is the D6 anchor: clearing an account's
// failures on a successful login must not also clear an unrelated per-IP bucket,
// or an attacker holding one valid account could reset their guessing budget.
func TestRecordSuccessClearsOnlyScopedKey(t *testing.T) {
	t.Parallel()

	policy := throttlePolicy{
		Short:      throttleWindow{Limit: 3, Window: 15 * time.Minute},
		Backoff:    []time.Duration{time.Minute},
		ResetAfter: 12 * time.Hour,
	}
	th := newTestThrottle(map[throttleScope]throttlePolicy{
		scopeUserAccount:  policy,
		scopeUserPassword: policy,
	})
	now := time.Now()

	acctKey := accountThrottleKey(scopeUserAccount, "u1")
	ipKey := throttleKey{Scope: scopeUserPassword, Value: "203.0.113.50/32"}

	th.recordFailure(acctKey, now)
	th.recordFailure(acctKey, now)
	th.recordFailure(ipKey, now)
	th.recordFailure(ipKey, now)

	// A successful login against the account bucket only.
	th.recordSuccess(acctKey)

	// The account bucket must be gone: one more failure should not trip it.
	if d := th.recordFailure(acctKey, now); d.Outcome != outcomeAllow {
		t.Fatalf("account bucket outcome after recordSuccess+1 failure = %v, want allow (bucket was cleared)", d.Outcome)
	}

	// The IP bucket already had 2 failures; a 3rd must still trip it, proving
	// recordSuccess(acctKey) never touched it.
	if d := th.recordFailure(ipKey, now); d.Outcome == outcomeAllow {
		t.Fatal("IP bucket was cleared by an unrelated account's successful login")
	}
}

// TestThrottleEvictsLeastRecentlyActiveAtCapacity confirms the memory bound holds
// under a flood of distinct keys (an IPv6 /64 holder minting buckets, or any other
// unbounded key space) and that eviction is LRU, not random: the most recently
// active key must survive.
func TestThrottleEvictsLeastRecentlyActiveAtCapacity(t *testing.T) {
	t.Parallel()

	policy := throttlePolicy{
		Short:      throttleWindow{Limit: 1_000_000, Window: 15 * time.Minute},
		Backoff:    []time.Duration{time.Minute},
		ResetAfter: 12 * time.Hour,
	}
	th := newTestThrottle(map[throttleScope]throttlePolicy{scopeUserAccount: policy})

	now := time.Now()
	total := maxTrackedKeys + 1000
	var lastKey throttleKey
	for i := 0; i < total; i++ {
		key := accountThrottleKey(scopeUserAccount, fmt.Sprintf("acct-%d", i))
		th.recordFailure(key, now.Add(time.Duration(i)*time.Millisecond))
		lastKey = key
	}

	if got := th.trackedKeys(); got > maxTrackedKeys {
		t.Fatalf("trackedKeys() = %d, want <= %d", got, maxTrackedKeys)
	}

	bucket := bucketID(lastKey)
	shard := th.shardFor(bucket)
	shard.mu.Lock()
	_, survived := shard.entries[bucket]
	shard.mu.Unlock()
	if !survived {
		t.Fatal("the most recently active key was evicted; eviction must be LRU")
	}
}

// TestPurgeStaleKeepsBlockedEntries guards the invariant that a block cannot be
// shed by going quiet: without this guard an attacker could wait out an idle
// window instead of the actual block duration.
func TestPurgeStaleKeepsBlockedEntries(t *testing.T) {
	t.Parallel()

	th := newTestThrottle(defaultThrottlePolicies())
	now := time.Now()
	key := accountThrottleKey(scopeUserAccount, "acct-blocked")
	bucket := bucketID(key)
	shard := th.shardFor(bucket)
	shard.mu.Lock()
	shard.entries[bucket] = &throttleEntry{
		blockedUntil: now.Add(24 * time.Hour),
		lastActivity: now.Add(-th.idleRetention() - time.Hour),
	}
	shard.mu.Unlock()

	th.purgeStale(now)

	shard.mu.Lock()
	_, ok := shard.entries[bucket]
	shard.mu.Unlock()
	if !ok {
		t.Fatal("purge dropped a still-blocked entry, letting it shed the block by idling")
	}
}

// TestPurgeStaleDropsIdleEntries confirms purge reclaims memory for entries that
// are not currently blocked and have been idle past retention.
func TestPurgeStaleDropsIdleEntries(t *testing.T) {
	t.Parallel()

	th := newTestThrottle(defaultThrottlePolicies())
	now := time.Now()
	key := accountThrottleKey(scopeUserAccount, "acct-idle")
	th.recordFailure(key, now)

	th.purgeStale(now.Add(th.idleRetention() + time.Minute))

	if got := th.trackedKeys(); got != 0 {
		t.Fatalf("trackedKeys() after purge = %d, want 0", got)
	}
}

// TestThrottleNilIsInert protects handlers constructed as a zero-value &Handler{}
// (used throughout this package's other tests), whose loginThrottle field is nil.
func TestThrottleNilIsInert(t *testing.T) {
	t.Parallel()

	var th *loginThrottle
	key := accountThrottleKey(scopeUserAccount, "acct-nil")
	if d := th.evaluate(key, time.Now()); d.Outcome != outcomeAllow {
		t.Fatalf("evaluate on nil throttle = %+v, want allow", d)
	}
	if d := th.recordFailure(key, time.Now()); d.Outcome != outcomeAllow {
		t.Fatalf("recordFailure on nil throttle = %+v, want allow", d)
	}
	th.recordSuccess(key)
	th.purgeStale(time.Now())
	th.setPolicies(defaultThrottlePolicies())
	th.close()
	th.close() // idempotent
	if got := th.trackedKeys(); got != 0 {
		t.Fatalf("trackedKeys() on nil throttle = %d, want 0", got)
	}
	if short, long := th.limitsFor(key); short != 0 || long != 0 {
		t.Fatalf("limitsFor on nil throttle = (%d, %d), want (0, 0)", short, long)
	}
}

// TestThrottleConcurrentRecordFailureIsRaceFree exercises the sharded-lock design
// under -race: many goroutines hammering recordFailure/recordSuccess/purgeStale
// concurrently across a shared key space must not race or panic.
func TestThrottleConcurrentRecordFailureIsRaceFree(t *testing.T) {
	th := newTestThrottle(defaultThrottlePolicies())
	now := time.Now()

	var wg sync.WaitGroup
	for w := 0; w < 64; w++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				key := accountThrottleKey(scopeUserAccount, fmt.Sprintf("acct-%d-%d", worker%8, i%16))
				th.recordFailure(key, now.Add(time.Duration(i)*time.Millisecond))
			}
		}(w)
	}
	for w := 0; w < 16; w++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			for i := 0; i < 100; i++ {
				key := accountThrottleKey(scopeUserAccount, fmt.Sprintf("acct-%d-%d", worker%8, i%16))
				th.recordSuccess(key)
				th.purgeStale(now)
			}
		}(w)
	}
	wg.Wait()
}

// TestThrottleSetPoliciesIsHotReloadSafe exercises the atomic.Pointer swap under
// -race: a config hot-reload racing with live traffic must never be observed
// half-applied or corrupt shared state.
func TestThrottleSetPoliciesIsHotReloadSafe(t *testing.T) {
	th := newTestThrottle(defaultThrottlePolicies())
	now := time.Now()

	stop := make(chan struct{})
	var reloader sync.WaitGroup
	reloader.Add(1)
	go func() {
		defer reloader.Done()
		alt := defaultThrottlePolicies()
		toggle := false
		for {
			select {
			case <-stop:
				return
			default:
			}
			policies := defaultThrottlePolicies()
			if toggle {
				policies = alt
			}
			toggle = !toggle
			th.setPolicies(policies)
		}
	}()

	// The reloader is not in this group: it is stopped explicitly below, after
	// the writers finish, so waiting on it here cannot deadlock against its own
	// exit condition.
	var writers sync.WaitGroup
	for w := 0; w < 32; w++ {
		writers.Add(1)
		go func(worker int) {
			defer writers.Done()
			for i := 0; i < 200; i++ {
				key := accountThrottleKey(scopeUserAccount, fmt.Sprintf("acct-hot-%d-%d", worker%4, i%8))
				th.recordFailure(key, now.Add(time.Duration(i)*time.Millisecond))
			}
		}(w)
	}

	writers.Wait()
	close(stop)
	reloader.Wait()
}
