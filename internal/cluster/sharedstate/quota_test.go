package sharedstate

import (
	"math"
	"testing"
	"time"
)

func TestAdmitCountsRequestsAcrossNodes(t *testing.T) {
	env := newTestEnv(t)
	a, b := env.node(t, "a"), env.node(t, "b")
	subject := unique(t, "key")

	var last Admission
	for i := 0; i < 3; i++ {
		adm, err := a.AdmitRequest(bg, subject, 0)
		if err != nil {
			t.Fatal(err)
		}
		last = adm
	}
	if last.RPM != 3 {
		t.Fatalf("node a RPM = %v, want 3", last.RPM)
	}
	adm, err := b.AdmitRequest(bg, subject, 0)
	if err != nil {
		t.Fatal(err)
	}
	if adm.RPM != 4 {
		t.Fatalf("node b must see node a's requests: RPM = %v, want 4", adm.RPM)
	}
	if adm.SlotTaken || adm.Held != 0 {
		t.Fatalf("no concurrency limit means no slot, got %+v", adm)
	}
	adm.Release() // must be a harmless no-op
}

// TestAdmitWindowWeightsPreviousMinute pins the sliding approximation: the
// previous minute counts in proportion to how much of it is still inside the
// trailing 60 seconds.
func TestAdmitWindowWeightsPreviousMinute(t *testing.T) {
	env := newTestEnv(t)
	node := env.node(t, "a")
	subject := unique(t, "key")

	env.clock.Set(time.Date(2026, 9, 24, 12, 0, 50, 0, time.UTC))
	for i := 0; i < 10; i++ {
		if _, err := node.AdmitRequest(bg, subject, 0); err != nil {
			t.Fatal(err)
		}
	}
	// 15s into the next minute, 45s of the previous one are still in the window.
	env.clock.Set(time.Date(2026, 9, 24, 12, 1, 15, 0, time.UTC))
	adm, err := node.AdmitRequest(bg, subject, 0)
	if err != nil {
		t.Fatal(err)
	}
	want := 10*0.75 + 1
	if math.Abs(adm.RPM-want) > 0.001 {
		t.Fatalf("RPM = %v, want %v", adm.RPM, want)
	}
	// Two minutes later nothing of that burst is left.
	env.clock.Set(time.Date(2026, 9, 24, 12, 3, 15, 0, time.UTC))
	adm, _ = node.AdmitRequest(bg, subject, 0)
	if adm.RPM != 1 {
		t.Fatalf("stale buckets must not count: RPM = %v", adm.RPM)
	}
}

func TestAdmitConcurrencyIsGlobal(t *testing.T) {
	env := newTestEnv(t)
	a, b := env.node(t, "a"), env.node(t, "b")
	subject := unique(t, "key")

	first, err := a.AdmitRequest(bg, subject, 2)
	if err != nil || !first.SlotTaken || first.Held != 1 {
		t.Fatalf("first = %+v, %v", first, err)
	}
	second, err := b.AdmitRequest(bg, subject, 2)
	if err != nil || !second.SlotTaken || second.Held != 2 {
		t.Fatalf("second = %+v, %v", second, err)
	}
	third, err := a.AdmitRequest(bg, subject, 2)
	if err != nil {
		t.Fatal(err)
	}
	if third.SlotTaken || third.Held != 2 {
		t.Fatalf("the limit is cluster-wide: third = %+v", third)
	}

	second.Release()
	waitUntil(t, "released slot to disappear", func() bool {
		adm, err := a.AdmitRequest(bg, subject, 2)
		if err != nil || !adm.SlotTaken {
			return false
		}
		adm.Release()
		return true
	})
}

// TestAdmitReclaimsExpiredLeases simulates a node that died holding slots: its
// leases are never renewed nor released, and after one lease TTL the slots are
// usable again.
func TestAdmitReclaimsExpiredLeases(t *testing.T) {
	env := newTestEnv(t)
	dead, alive := env.node(t, "dead"), env.node(t, "alive")
	subject := unique(t, "key")

	for i := 0; i < 2; i++ {
		if adm, err := dead.AdmitRequest(bg, subject, 2); err != nil || !adm.SlotTaken {
			t.Fatalf("setup acquire %d: %+v %v", i, adm, err)
		}
	}
	// The dead node never renews: stop its keeper without releasing.
	dead.keeper.mu.Lock()
	dead.keeper.held = map[leaseRef]struct{}{}
	dead.keeper.mu.Unlock()

	if adm, _ := alive.AdmitRequest(bg, subject, 2); adm.SlotTaken {
		t.Fatal("slots are still leased")
	}
	env.clock.Advance(DefaultLeaseTTL + time.Second)
	adm, err := alive.AdmitRequest(bg, subject, 2)
	if err != nil || !adm.SlotTaken || adm.Held != 1 {
		t.Fatalf("expired leases must be reclaimed: %+v %v", adm, err)
	}
}

func TestLeaseRenewalKeepsLongRequestsCounted(t *testing.T) {
	env := newTestEnv(t)
	a, b := env.node(t, "a"), env.node(t, "b")
	subject := unique(t, "key")

	stream, err := a.AdmitRequest(bg, subject, 1)
	if err != nil || !stream.SlotTaken {
		t.Fatalf("stream acquire: %+v %v", stream, err)
	}
	// A long stream: renewals every third of the TTL keep the lease alive well
	// past the TTL itself.
	for i := 0; i < 5; i++ {
		env.clock.Advance(DefaultLeaseTTL / 3)
		a.keeper.renew()
		if adm, _ := b.AdmitRequest(bg, subject, 1); adm.SlotTaken {
			t.Fatalf("renewed lease lost after %d renewals", i+1)
		}
	}
	stream.Release()
	stream.Release() // idempotent
	waitUntil(t, "release", func() bool {
		adm, err := b.AdmitRequest(bg, subject, 1)
		if err != nil || !adm.SlotTaken {
			return false
		}
		adm.Release()
		return true
	})
	if n := a.keeper.heldCount(); n != 0 {
		t.Fatalf("released lease still renewed: %d", n)
	}
}

func TestAdmitReadsSharedTokens(t *testing.T) {
	env := newTestEnv(t)
	a, b := env.node(t, "a"), env.node(t, "b")
	subject := unique(t, "key")

	a.AddTokens(subject, 1200)
	a.counts.flush()
	adm, err := b.AdmitRequest(bg, subject, 0)
	if err != nil {
		t.Fatal(err)
	}
	if adm.TPM != 1200 {
		t.Fatalf("TPM = %v, want 1200", adm.TPM)
	}
}

func TestCountRequestAndRates(t *testing.T) {
	env := newTestEnv(t)
	a, b := env.node(t, "a"), env.node(t, "b")
	limited, unlimited := unique(t, "limited"), unique(t, "unlimited")

	for i := 0; i < 2; i++ {
		if _, err := a.AdmitRequest(bg, limited, 0); err != nil {
			t.Fatal(err)
		}
	}
	b.CountRequest(unlimited)
	b.CountRequest(unlimited)
	b.CountRequest(unlimited)
	b.AddTokens(unlimited, 50)
	b.counts.flush()

	rates, err := a.Rates(bg, []string{limited, unlimited, unique(t, "idle")})
	if err != nil {
		t.Fatal(err)
	}
	if rates[limited].RPM != 2 || rates[unlimited].RPM != 3 || rates[unlimited].TPM != 50 {
		t.Fatalf("rates = %+v", rates)
	}
}

func TestQuotaBucketsExpire(t *testing.T) {
	env := newTestEnv(t)
	env.requireMiniredis(t)
	node := env.node(t, "a")
	subject := unique(t, "key")
	if _, err := node.AdmitRequest(bg, subject, 1); err != nil {
		t.Fatal(err)
	}
	for _, key := range env.mr.Keys() {
		if key == "clirelay:health:a" {
			continue
		}
		if ttl := env.mr.TTL(key); ttl <= 0 || ttl > 3*time.Minute {
			t.Fatalf("key %s has TTL %s; every quota key must expire on its own", key, ttl)
		}
	}
}

func TestUnavailableStoreReportsErrors(t *testing.T) {
	var nilStore *Store
	if nilStore.Available() {
		t.Fatal("nil store must be unavailable")
	}
	store := New(nil, Options{})
	defer store.Close()
	if _, err := store.AdmitRequest(bg, "x", 1); err == nil {
		t.Fatal("store without a client must fail")
	}
	store.CountRequest("x")
	store.AddTokens("x", 5)
	store.counts.flush() // drops silently while unavailable
	if _, err := store.Rates(bg, []string{"x"}); err == nil {
		t.Fatal("rates without a client must fail")
	}
}

func waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}
