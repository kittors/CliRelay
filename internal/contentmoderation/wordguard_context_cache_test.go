package contentmoderation

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"
)

// stamped gives a policy the version and save time a stored one has.
func stamped(p *WordPolicy, tenantID string, version int64, savedAt time.Time) *WordPolicy {
	p.TenantID, p.Version, p.UpdatedAt = tenantID, version, savedAt
	return p
}

func matchesWord(c wordContext, text string) bool {
	return len(c.matcher.find(normalizeForMatch(text, c.fold), findOptions{}).hits) > 0
}

func anyContextMatches(contexts []wordContext, text string) bool {
	for _, c := range contexts {
		if matchesWord(c, text) {
			return true
		}
	}
	return false
}

func TestWordContextCacheKeepsTheContextsWhileThePoliciesAreUnchanged(t *testing.T) {
	var cache wordContextCache
	saved := time.Date(2026, 11, 1, 10, 0, 0, 0, time.UTC)
	platform := stamped(policyOf(t, WordScopePlatform, "", nil, kw("pfw")), "system", 2, saved)
	tenant := stamped(tenantOf(t, WordActionBlock, kw("alpha")), "t1", 3, saved)

	first := cache.contextsFor(platform, tenant, nil)
	if len(first) != 2 {
		t.Fatalf("contexts = %d, want the platform's and the tenant's", len(first))
	}
	second := cache.contextsFor(platform, tenant, nil)
	if &first[0] != &second[0] {
		t.Fatal("unchanged policies were built into contexts again")
	}
	// The store hands out a new object when it reloads a policy, but the same
	// saved state has the same stamp.
	reloaded := *tenant
	if third := cache.contextsFor(platform, &reloaded, nil); &third[0] != &first[0] {
		t.Fatal("a reloaded copy of the same saved state was built again")
	}
}

// A saved policy is told from the state before it by its version and by the time
// of the save. Both change on every save; each must count on its own, because a
// policy that is deleted and created again starts over at version 1, and two
// saves can fall in the same instant on a coarse clock.
func TestWordContextCacheRebuildsAfterASave(t *testing.T) {
	saved := time.Date(2026, 11, 1, 10, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name    string
		version int64
		at      time.Time
	}{
		{"the version and the time change", 4, saved.Add(time.Minute)},
		{"only the version changes", 4, saved},
		{"only the time changes, as for a policy created again", 3, saved.Add(time.Hour)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var cache wordContextCache
			before := cache.contextsFor(nil, stamped(tenantOf(t, WordActionBlock, kw("alpha")), "t1", 3, saved), nil)
			if !matchesWord(before[0], "an alpha here") || matchesWord(before[0], "a beta here") {
				t.Fatal("the first contexts do not match what the policy says")
			}
			after := cache.contextsFor(nil, stamped(tenantOf(t, WordActionBlock, kw("beta")), "t1", tc.version, tc.at), nil)
			if matchesWord(after[0], "an alpha here") || !matchesWord(after[0], "a beta here") {
				t.Fatal("a saved policy is still judged by the contexts built from the one before")
			}
			if !matchesWord(before[0], "an alpha here") {
				t.Fatal("building the new contexts changed the old ones")
			}
		})
	}
}

// Two policies in the same saved state (the first save of each, in the same
// instant) are still different policies. Sharing contexts between them would
// show one tenant's words to another.
func TestWordContextCacheNeverSharesContextsBetweenPolicies(t *testing.T) {
	saved := time.Date(2026, 11, 1, 10, 0, 0, 0, time.UTC)
	var cache wordContextCache
	tenantA := cache.contextsFor(nil, stamped(tenantOf(t, WordActionBlock, kw("alpha")), "tenant-a", 1, saved), nil)
	tenantB := cache.contextsFor(nil, stamped(tenantOf(t, WordActionBlock, kw("beta")), "tenant-b", 1, saved), nil)
	if matchesWord(tenantB[0], "an alpha here") || !matchesWord(tenantB[0], "a beta here") {
		t.Fatal("a tenant was judged by the words of another tenant")
	}
	if !matchesWord(tenantA[0], "an alpha here") {
		t.Fatal("building the second tenant's contexts changed the first's")
	}

	tenant := stamped(tenantOf(t, WordActionBlock, kw("base")), "tenant-a", 1, saved)
	keyOne := stamped(policyOf(t, WordScopeKey, "key-1", nil, kw("one")), "tenant-a", 1, saved)
	keyTwo := stamped(policyOf(t, WordScopeKey, "key-2", nil, kw("two")), "tenant-a", 1, saved)
	withOne := cache.contextsFor(nil, tenant, keyOne)
	withTwo := cache.contextsFor(nil, tenant, keyTwo)
	if !matchesWord(withOne[0], "say one") || matchesWord(withOne[0], "say two") {
		t.Fatal("the first key's override does not apply to it")
	}
	if matchesWord(withTwo[0], "say one") || !matchesWord(withTwo[0], "say two") {
		t.Fatal("a key was judged by the override of another key")
	}
}

func TestWordContextCacheNeverKeepsAPolicyWithoutAStamp(t *testing.T) {
	var cache wordContextCache
	// No version and no save time: what a draft or a hand-built policy looks like.
	tenant := tenantOf(t, WordActionBlock, kw("alpha"))
	if first := cache.contextsFor(nil, tenant, nil); !matchesWord(first[0], "an alpha here") {
		t.Fatal("the first contexts do not match")
	}
	tenant.Rules[0].Pattern = "beta"
	second := cache.contextsFor(nil, tenant, nil)
	if matchesWord(second[0], "an alpha here") || !matchesWord(second[0], "a beta here") {
		t.Fatal("a policy that cannot be told apart from its earlier self was served from the cache")
	}
	if len(cache.entries) != 0 {
		t.Fatalf("%d entries were kept for policies without a stamp", len(cache.entries))
	}
}

func TestWordContextCacheHasNothingToRememberWithoutPolicies(t *testing.T) {
	var cache wordContextCache
	if got := cache.contextsFor(nil, nil, nil); got != nil || len(cache.entries) != 0 {
		t.Fatalf("contexts = %v, entries = %d", got, len(cache.entries))
	}
}

func TestWordContextCacheStaysWithinItsBound(t *testing.T) {
	var cache wordContextCache
	saved := time.Date(2026, 11, 1, 10, 0, 0, 0, time.UTC)
	for i := 0; i < maxCachedWordContexts+10; i++ {
		tenant := stamped(tenantOf(t, WordActionBlock, kw("alpha")), fmt.Sprintf("tenant-%d", i), 1, saved)
		if got := cache.contextsFor(nil, tenant, nil); len(got) != 1 {
			t.Fatalf("tenant %d: %d contexts", i, len(got))
		}
	}
	if len(cache.entries) > maxCachedWordContexts {
		t.Fatalf("%d entries kept, bound is %d", len(cache.entries), maxCachedWordContexts)
	}
}

func TestWordContextCacheIsSafeForConcurrentRequests(t *testing.T) {
	var cache wordContextCache
	saved := time.Date(2026, 11, 1, 10, 0, 0, 0, time.UTC)
	var wg sync.WaitGroup
	for worker := 0; worker < 8; worker++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 50; i++ {
				tenant := stamped(tenantOf(t, WordActionBlock, kw("alpha")), fmt.Sprintf("tenant-%d", i%5), 1, saved)
				if got := cache.contextsFor(nil, tenant, nil); len(got) != 1 || !matchesWord(got[0], "an alpha here") {
					t.Error("wrong contexts")
					return
				}
			}
		}()
	}
	wg.Wait()
}

// The guard has to follow an edit made after it has been serving the old policy:
// that is the one thing the cache must not get in the way of.
func TestGuardRequestFollowsASavedPolicyChange(t *testing.T) {
	saved := time.Date(2026, 11, 1, 10, 0, 0, 0, time.UTC)
	resolver := &wordResolverStub{policies: WordPolicies{Tenant: stamped(tenantOf(t, WordActionBlock, kw("alpha")), "tenant-a", 3, saved)}}
	moderator := guardModerator(resolver)
	check := func(text string) bool {
		req, opts := guardOptions("tenant-a", chatRequest(text))
		return moderator.GuardRequest(context.Background(), req, opts).Blocked
	}

	if !check("an alpha here") || check("a beta here") {
		t.Fatal("the first policy is not applied")
	}
	if !check("an alpha here") {
		t.Fatal("the same policy stopped applying")
	}

	resolver.mu.Lock()
	resolver.policies = WordPolicies{Tenant: stamped(tenantOf(t, WordActionBlock, kw("beta")), "tenant-a", 4, saved.Add(time.Minute))}
	resolver.mu.Unlock()
	if check("an alpha here") || !check("a beta here") {
		t.Fatal("the guard keeps judging by the policy it saw before the save")
	}
}
