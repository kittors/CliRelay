package sharedstate

import (
	"testing"
	"time"
)

func testThrottleSpec() ThrottleSpec {
	return ThrottleSpec{
		Short:      ThrottleWindow{Limit: 3, Window: 15 * time.Minute, Slots: 12},
		Long:       ThrottleWindow{Limit: 5, Window: 24 * time.Hour, Slots: 24},
		Backoff:    []time.Duration{time.Minute, 5 * time.Minute},
		ResetAfter: 12 * time.Hour,
		Retention:  12 * time.Hour,
	}
}

func TestThrottleArmsBlockAcrossNodes(t *testing.T) {
	env := newTestEnv(t)
	a, b := env.node(t, "a"), env.node(t, "b")
	bucket := unique(t, "user_account|alice")
	spec := testThrottleSpec()

	st, err := a.ThrottleCharge(bg, bucket, spec, env.clock.Now())
	if err != nil || st.Blocked || st.ShortCount != 1 || st.LongCount != 1 {
		t.Fatalf("first failure = %+v %v", st, err)
	}
	if st, _ = b.ThrottleCharge(bg, bucket, spec, env.clock.Now()); st.Blocked || st.ShortCount != 2 {
		t.Fatalf("second failure on another node = %+v", st)
	}
	st, _ = a.ThrottleCharge(bg, bucket, spec, env.clock.Now())
	if !st.Blocked || !st.NewlyArmed || st.RetryAfter != time.Minute || st.Generation != 1 || st.ShortCount != 3 {
		t.Fatalf("third failure must arm the first rung: %+v", st)
	}

	// While armed, failures are refused without being counted.
	env.clock.Advance(10 * time.Second)
	st, _ = b.ThrottleCharge(bg, bucket, spec, env.clock.Now())
	if !st.Blocked || st.NewlyArmed || st.RetryAfter != 50*time.Second || st.ShortCount != 3 {
		t.Fatalf("charge while blocked = %+v", st)
	}
	peek, _ := b.ThrottlePeek(bg, bucket, spec, env.clock.Now())
	if !peek.Blocked || peek.RetryAfter != 50*time.Second {
		t.Fatalf("peek while blocked = %+v", peek)
	}

	// After the block, the short window still holds three failures, so the
	// next one trips again and climbs the ladder.
	env.clock.Advance(time.Minute)
	if peek, _ = a.ThrottlePeek(bg, bucket, spec, env.clock.Now()); peek.Blocked || peek.Generation != 0 {
		t.Fatalf("peek after the block must allow and report nothing else: %+v", peek)
	}
	st, _ = a.ThrottleCharge(bg, bucket, spec, env.clock.Now())
	if !st.Blocked || st.RetryAfter != 5*time.Minute || st.Generation != 2 {
		t.Fatalf("second block must use the second rung: %+v", st)
	}
	// The ladder plateaus on its last rung.
	env.clock.Advance(6 * time.Minute)
	st, _ = a.ThrottleCharge(bg, bucket, spec, env.clock.Now())
	if !st.Blocked || st.RetryAfter != 5*time.Minute || st.Generation != 3 {
		t.Fatalf("ladder must plateau: %+v", st)
	}
}

func TestThrottleResetAfterQuietPeriodAndClear(t *testing.T) {
	env := newTestEnv(t)
	node := env.node(t, "a")
	bucket := unique(t, "user_account|bob")
	spec := testThrottleSpec()

	for i := 0; i < 2; i++ {
		_, _ = node.ThrottleCharge(bg, bucket, spec, env.clock.Now())
	}
	env.clock.Advance(13 * time.Hour)
	st, _ := node.ThrottleCharge(bg, bucket, spec, env.clock.Now())
	if st.ShortCount != 1 || st.LongCount != 1 || st.Generation != 0 {
		t.Fatalf("a quiet period longer than reset-after must clear the bucket: %+v", st)
	}

	_, _ = node.ThrottleCharge(bg, bucket, spec, env.clock.Now())
	if err := node.ThrottleClear(bg, bucket); err != nil {
		t.Fatal(err)
	}
	st, _ = node.ThrottleCharge(bg, bucket, spec, env.clock.Now())
	if st.ShortCount != 1 {
		t.Fatalf("a successful login clears the bucket: %+v", st)
	}
}

func TestThrottleShortWindowSlides(t *testing.T) {
	env := newTestEnv(t)
	node := env.node(t, "a")
	bucket := unique(t, "management_key|10.0.0.1/32")
	spec := testThrottleSpec()
	spec.Long = ThrottleWindow{}

	_, _ = node.ThrottleCharge(bg, bucket, spec, env.clock.Now())
	_, _ = node.ThrottleCharge(bg, bucket, spec, env.clock.Now())
	env.clock.Advance(16 * time.Minute)
	st, _ := node.ThrottleCharge(bg, bucket, spec, env.clock.Now())
	if st.Blocked || st.ShortCount != 1 || st.LongCount != 0 {
		t.Fatalf("failures older than the window must drop out: %+v", st)
	}
}

func testAutoBanSpec() AutoBanSpec {
	return AutoBanSpec{Window: 10 * time.Minute, Slots: 12, Threshold: 3, ClaimTTL: 10 * time.Second, Retention: 7 * 24 * time.Hour}
}

func TestAutoBanClaimIsExclusive(t *testing.T) {
	env := newTestEnv(t)
	a, b := env.node(t, "a"), env.node(t, "b")
	cidr := unique(t, "203.0.113.7/32")
	spec := testAutoBanSpec()

	for i, node := range []*Store{a, b} {
		ch, err := node.AutoBanCharge(bg, cidr, spec, env.clock.Now())
		if err != nil || ch.Claimed || ch.AlreadyBanned || ch.Failures != i+1 {
			t.Fatalf("failure %d = %+v %v", i+1, ch, err)
		}
	}
	crossing, _ := a.AutoBanCharge(bg, cidr, spec, env.clock.Now())
	if !crossing.Claimed || crossing.Failures != 3 || crossing.Bans != 0 {
		t.Fatalf("the crossing failure must claim: %+v", crossing)
	}
	// The other node must not act on the same crossing.
	racing, _ := b.AutoBanCharge(bg, cidr, spec, env.clock.Now())
	if racing.Claimed || !racing.AlreadyBanned {
		t.Fatalf("a claimed source must look banned to other nodes: %+v", racing)
	}

	until := env.clock.Now().Add(time.Hour)
	if err := a.AutoBanMark(bg, cidr, until, env.clock.Now(), spec.Retention); err != nil {
		t.Fatal(err)
	}
	env.clock.Advance(30 * time.Minute)
	if during, _ := b.AutoBanCharge(bg, cidr, spec, env.clock.Now()); !during.AlreadyBanned {
		t.Fatalf("banned source: %+v", during)
	}
	// After the ban the window starts empty and the ban count drives escalation.
	env.clock.Advance(31 * time.Minute)
	after, _ := b.AutoBanCharge(bg, cidr, spec, env.clock.Now())
	if after.AlreadyBanned || after.Failures != 1 || after.Bans != 1 {
		t.Fatalf("after the ban: %+v", after)
	}
}

func TestAutoBanClaimLapsesWhenNotMarked(t *testing.T) {
	env := newTestEnv(t)
	node := env.node(t, "a")
	cidr := unique(t, "198.51.100.0/24")
	spec := testAutoBanSpec()
	spec.Threshold = 1

	if ch, _ := node.AutoBanCharge(bg, cidr, spec, env.clock.Now()); !ch.Claimed {
		t.Fatalf("threshold 1 must claim at once: %+v", ch)
	}
	// The claiming node failed to write the rule and never marked the ban.
	env.clock.Advance(spec.ClaimTTL + time.Second)
	again, _ := node.AutoBanCharge(bg, cidr, spec, env.clock.Now())
	if !again.Claimed || again.Bans != 0 {
		t.Fatalf("a lapsed claim must let the next failure retry: %+v", again)
	}
}

func TestSecurityKeysExpire(t *testing.T) {
	env := newTestEnv(t)
	env.requireMiniredis(t)
	node := env.node(t, "a")
	_, _ = node.ThrottleCharge(bg, unique(t, "b"), testThrottleSpec(), env.clock.Now())
	_, _ = node.AutoBanCharge(bg, unique(t, "c"), testAutoBanSpec(), env.clock.Now())
	for _, key := range env.mr.Keys() {
		if ttl := env.mr.TTL(key); ttl <= 0 {
			t.Fatalf("key %s has no TTL", key)
		}
	}
}

// TestEveryKeyHasTTL exercises every write path and checks that nothing is
// left without an expiry. The production instance evicts with volatile-lru:
// a key without a TTL could never be evicted, and once memory filled up every
// write would fail.
func TestEveryKeyHasTTL(t *testing.T) {
	env := newTestEnv(t)
	env.requireMiniredis(t)
	node := env.node(t, "a")

	adm, err := node.AdmitRequest(bg, unique(t, "key"), 2)
	if err != nil || !adm.SlotTaken {
		t.Fatalf("admit: %+v %v", adm, err)
	}
	node.keeper.renew()
	node.CountRequest(unique(t, "unlimited"))
	node.AddTokens(unique(t, "tokens"), 10)
	node.Flush()
	if ok, _, err := node.AcquireAccountSlot(bg, unique(t, "auth"), 1); err != nil || !ok {
		t.Fatalf("account slot: %v %v", ok, err)
	}
	sticky := unique(t, "session")
	_, _ = node.AffinityBind(bg, sticky, "auth-1", time.Hour)
	_, _ = node.AffinityLookup(bg, sticky, time.Hour)
	_, _ = node.MintID(bg, "codex", unique(t, "fixed"), "id-1", time.Hour, false)
	_, _ = node.MintID(bg, "uid", unique(t, "sliding"), "id-2", time.Hour, true)
	_, _ = node.ThrottleCharge(bg, unique(t, "bucket"), testThrottleSpec(), env.clock.Now())
	src := unique(t, "src")
	_, _ = node.AutoBanCharge(bg, src, testAutoBanSpec(), env.clock.Now())
	_ = node.AutoBanMark(bg, src, env.clock.Now().Add(time.Hour), env.clock.Now(), time.Hour)

	keys := env.mr.Keys()
	if len(keys) < 10 {
		t.Fatalf("expected every operation to write, got keys %v", keys)
	}
	for _, key := range keys {
		if ttl := env.mr.TTL(key); ttl <= 0 {
			t.Fatalf("key %s has no TTL", key)
		}
	}
}
