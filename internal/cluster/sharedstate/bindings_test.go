package sharedstate

import (
	"sync"
	"testing"
	"time"
)

func TestAffinityBindLookupRelease(t *testing.T) {
	env := newTestEnv(t)
	a, b := env.node(t, "a"), env.node(t, "b")
	key := unique(t, "session")

	got, err := a.AffinityLookup(bg, key, time.Hour)
	if err != nil || got.Found {
		t.Fatalf("fresh key must be unbound: %+v %v", got, err)
	}
	bound, err := a.AffinityBind(bg, key, "auth-1", time.Hour)
	if err != nil || !bound.Created || bound.Account != "auth-1" || bound.Served != 0 {
		t.Fatalf("bind = %+v %v", bound, err)
	}
	// Another node binding the same session concurrently loses to the existing
	// binding, and that request counts against it.
	conflict, err := b.AffinityBind(bg, key, "auth-2", time.Hour)
	if err != nil || conflict.Created || conflict.Account != "auth-1" || conflict.Served != 1 {
		t.Fatalf("conflicting bind must return the existing binding: %+v %v", conflict, err)
	}
	seen, err := b.AffinityLookup(bg, key, time.Hour)
	if err != nil || !seen.Found || seen.Account != "auth-1" || seen.Served != 2 {
		t.Fatalf("lookup from the other node = %+v %v", seen, err)
	}

	// Releasing with a stale auth id must not delete someone else's binding.
	if err := b.AffinityRelease(bg, key, "auth-2"); err != nil {
		t.Fatal(err)
	}
	if still, _ := a.AffinityLookup(bg, key, time.Hour); !still.Found {
		t.Fatal("compare-and-delete removed a binding it did not own")
	}
	if err := b.AffinityRelease(bg, key, "auth-1"); err != nil {
		t.Fatal(err)
	}
	if gone, _ := a.AffinityLookup(bg, key, time.Hour); gone.Found {
		t.Fatalf("binding survived its release: %+v", gone)
	}
}

func TestAffinityLookupRefreshesTTL(t *testing.T) {
	env := newTestEnv(t)
	env.requireMiniredis(t)
	node := env.node(t, "a")
	key := unique(t, "session")
	if _, err := node.AffinityBind(bg, key, "auth-1", time.Hour); err != nil {
		t.Fatal(err)
	}
	env.mr.FastForward(50 * time.Minute)
	if got, _ := node.AffinityLookup(bg, key, time.Hour); !got.Found {
		t.Fatal("binding expired early")
	}
	// The lookup renewed it: another 50 minutes later it is still there.
	env.mr.FastForward(50 * time.Minute)
	if got, _ := node.AffinityLookup(bg, key, time.Hour); !got.Found {
		t.Fatal("reading a binding must extend its TTL")
	}
	env.mr.FastForward(61 * time.Minute)
	if got, _ := node.AffinityLookup(bg, key, time.Hour); got.Found {
		t.Fatal("an unused binding must expire after its TTL")
	}
}

func TestMintIDFirstCandidateWins(t *testing.T) {
	env := newTestEnv(t)
	a, b := env.node(t, "a"), env.node(t, "b")
	key := unique(t, "conversation")

	first, err := a.MintID(bg, "codex", key, "id-from-a", time.Hour, false)
	if err != nil || !first.Minted || first.ID != "id-from-a" || first.TTL != time.Hour {
		t.Fatalf("first mint = %+v %v", first, err)
	}
	second, err := b.MintID(bg, "codex", key, "id-from-b", time.Hour, false)
	if err != nil || second.Minted || second.ID != "id-from-a" {
		t.Fatalf("second node must adopt the first id: %+v %v", second, err)
	}
	other, err := b.MintID(bg, "claude-user", key, "user-id", time.Hour, false)
	if err != nil || !other.Minted || other.ID != "user-id" {
		t.Fatalf("namespaces must not collide: %+v %v", other, err)
	}
}

func TestMintIDFixedAndSlidingTTL(t *testing.T) {
	env := newTestEnv(t)
	env.requireMiniredis(t)
	node := env.node(t, "a")
	fixed, sliding := unique(t, "fixed"), unique(t, "sliding")

	if _, err := node.MintID(bg, "codex", fixed, "f1", time.Hour, false); err != nil {
		t.Fatal(err)
	}
	if _, err := node.MintID(bg, "uid", sliding, "s1", time.Hour, true); err != nil {
		t.Fatal(err)
	}
	env.mr.FastForward(40 * time.Minute)
	got, err := node.MintID(bg, "codex", fixed, "f2", time.Hour, false)
	if err != nil || got.ID != "f1" || got.TTL > 21*time.Minute || got.TTL < 19*time.Minute {
		t.Fatalf("a fixed id reports its remaining lifetime so local caches expire with it: %+v %v", got, err)
	}
	gotSliding, err := node.MintID(bg, "uid", sliding, "s2", time.Hour, true)
	if err != nil || gotSliding.ID != "s1" || gotSliding.TTL != time.Hour {
		t.Fatalf("a sliding id is renewed on read: %+v %v", gotSliding, err)
	}
	env.mr.FastForward(30 * time.Minute)
	if again, _ := node.MintID(bg, "codex", fixed, "f3", time.Hour, false); again.ID != "f3" || !again.Minted {
		t.Fatalf("an expired fixed id must be re-minted: %+v", again)
	}
	if again, _ := node.MintID(bg, "uid", sliding, "s3", time.Hour, true); again.ID != "s1" {
		t.Fatalf("a sliding id in use must survive: %+v", again)
	}
}

func TestMintIDConcurrentCallersAgree(t *testing.T) {
	env := newTestEnv(t)
	nodes := []*Store{env.node(t, "a"), env.node(t, "b"), env.node(t, "c")}
	key := unique(t, "conversation")
	var wg sync.WaitGroup
	ids := make([]string, 30)
	for i := range ids {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			res, err := nodes[i%len(nodes)].MintID(bg, "codex", key, unique(t, "candidate"), time.Hour, false)
			if err == nil {
				ids[i] = res.ID
			}
		}(i)
	}
	wg.Wait()
	for i, id := range ids {
		if id == "" || id != ids[0] {
			t.Fatalf("caller %d got %q, caller 0 got %q: exactly one candidate may win", i, id, ids[0])
		}
	}
}

func TestAccountSlotsAreGlobalAndFungible(t *testing.T) {
	env := newTestEnv(t)
	a, b := env.node(t, "a"), env.node(t, "b")
	authID := unique(t, "auth")

	for i := 0; i < 2; i++ {
		if ok, _, err := a.AcquireAccountSlot(bg, authID, 3); err != nil || !ok {
			t.Fatalf("a acquire %d: %v %v", i, ok, err)
		}
	}
	ok, held, err := b.AcquireAccountSlot(bg, authID, 3)
	if err != nil || !ok || held != 3 {
		t.Fatalf("b acquire: %v %d %v", ok, held, err)
	}
	if ok, held, _ := b.AcquireAccountSlot(bg, authID, 3); ok || held != 3 {
		t.Fatalf("fourth slot must be refused cluster-wide, got %v %d", ok, held)
	}
	if a.AccountSlotsHeld(authID) != 2 || b.AccountSlotsHeld(authID) != 1 {
		t.Fatalf("held a=%d b=%d", a.AccountSlotsHeld(authID), b.AccountSlotsHeld(authID))
	}

	a.ReleaseAccountSlot(authID)
	if a.AccountSlotsHeld(authID) != 1 {
		t.Fatalf("release must pop one lease, held %d", a.AccountSlotsHeld(authID))
	}
	waitUntil(t, "the released slot", func() bool {
		ok, _, err := b.AcquireAccountSlot(bg, authID, 3)
		return err == nil && ok
	})
	b.ReleaseAccountSlot("never-held") // no-op
}
