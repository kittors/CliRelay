package contentmoderation

import (
	"fmt"
	"math/rand"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func (c *wordContextCache) entryCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.entries)
}

func (c *wordContextCache) byteCount() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.bytes
}

func (c *wordContextCache) holds(key wordCacheKey) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.entries[key] != nil
}

// tenantKeyOf is the cache key of the context made from a tenant policy alone.
func tenantKeyOf(t *testing.T, tenant *WordPolicy) wordCacheKey {
	t.Helper()
	stamp, ok := stampWordPolicy(tenant)
	if !ok {
		t.Fatal("the policy has no stamp")
	}
	return wordCacheKey{stamps: [2]wordPolicyStamp{stamp}}
}

// wordRulesN makes n different keywords, as many as a real list has; the seed
// changes which ones.
func wordRulesN(n int, seed int64) []WordRule {
	rules := make([]WordRule, 0, n)
	r := rand.New(rand.NewSource(seed))
	for i := 0; i < n; i++ {
		var word strings.Builder
		for j, length := 0, 4+r.Intn(10); j < length; j++ {
			word.WriteRune(rune('a' + r.Intn(26)))
		}
		rules = append(rules, kw(fmt.Sprintf("%s%d", word.String(), i)))
	}
	return rules
}

func heapInUse() int64 {
	runtime.GC()
	runtime.GC()
	var stats runtime.MemStats
	runtime.ReadMemStats(&stats)
	return int64(stats.HeapAlloc)
}

// checkWordCacheBooks checks that the cache's records agree with each other and
// with the rule the cache is for: nothing in it was built from a policy state
// other than the one the cache has for that policy.
func checkWordCacheBooks(t *testing.T, c *wordContextCache) {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.lru.Len() != len(c.entries) {
		t.Fatalf("%d entries but %d in the recency list", len(c.entries), c.lru.Len())
	}
	var bytes int64
	listed := map[*wordCacheEntry]bool{}
	for element := c.lru.Front(); element != nil; element = element.Next() {
		entry := element.Value.(*wordCacheEntry)
		if c.entries[entry.key] != entry || entry.elem != element {
			t.Fatalf("the recency list and the entries disagree about %+v", entry.key)
		}
		bytes += entry.bytes
		listed[entry] = true
	}
	if bytes != c.bytes {
		t.Fatalf("entries weigh %d bytes but the cache counts %d", bytes, c.bytes)
	}
	states := map[wordPolicyID]wordPolicyStamp{}
	for entry := range listed {
		for _, stamp := range entry.key.stamps {
			if stamp == (wordPolicyStamp{}) {
				continue
			}
			if other, ok := states[stamp.id]; ok && other != stamp {
				t.Fatalf("entries are built from two saved states of %+v: %+v and %+v", stamp.id, other, stamp)
			}
			states[stamp.id] = stamp
			refs := c.refs[stamp.id]
			if refs == nil || refs.stamp != stamp {
				t.Fatalf("the index has no record of the entries built from %+v", stamp)
			}
			if _, ok := refs.entries[entry]; !ok {
				t.Fatalf("the index misses an entry built from %+v", stamp)
			}
		}
	}
	for id, refs := range c.refs {
		if len(refs.entries) == 0 {
			t.Fatalf("the index keeps an empty record for %+v", id)
		}
		if states[id] != refs.stamp {
			t.Fatalf("the index says %+v is at %+v, the entries say %+v", id, refs.stamp, states[id])
		}
		for entry := range refs.entries {
			if !listed[entry] {
				t.Fatalf("the index points at an entry that was dropped (%+v)", entry.key)
			}
		}
	}
	if len(c.flights) != 0 {
		t.Fatalf("%d builds are recorded as running", len(c.flights))
	}
}

var cacheTestSaved = time.Date(2026, 11, 1, 10, 0, 0, 0, time.UTC)

// Editing a list again and again must not pile up the lists it used to be.
func TestWordContextCacheKeepsOnlyTheNewestSavedStateOfAPolicy(t *testing.T) {
	var cache wordContextCache
	var first int64
	for version := int64(1); version <= 50; version++ {
		tenant := stamped(tenantOf(t, WordActionBlock, wordRulesN(300, version)...), "tenant-a", version, cacheTestSaved.Add(time.Duration(version)*time.Minute))
		if contexts := cache.contextsFor(nil, tenant, nil); len(contexts) != 1 {
			t.Fatalf("save %d: %d contexts", version, len(contexts))
		}
		if got := cache.entryCount(); got != 1 {
			t.Fatalf("after %d saves the cache holds %d entries, want the newest alone", version, got)
		}
		checkWordCacheBooks(t, &cache)
		if version == 1 {
			first = cache.byteCount()
		}
	}
	if got := cache.byteCount(); got > first*2 {
		t.Fatalf("50 saves of the same policy weigh %d bytes against %d for one", got, first)
	}
}

// A tenant's save makes out of date everything built with the tenant's old
// words, the key contexts included; waiting for each key to ask again before
// letting go of them would hold on to memory nobody can use.
func TestWordContextCacheDropsWhatWasBuiltFromAnOlderStateOfThePolicy(t *testing.T) {
	var cache wordContextCache
	tenantV1 := stamped(tenantOf(t, WordActionBlock, kw("base")), "tenant-a", 1, cacheTestSaved)
	keyOf := func(i int, version int64, word string) *WordPolicy {
		return stamped(policyOf(t, WordScopeKey, fmt.Sprintf("key-%d", i), nil, kw(word)), "tenant-a", version, cacheTestSaved.Add(time.Duration(version)*time.Minute))
	}
	cache.contextsFor(nil, tenantV1, nil)
	for i := 0; i < 5; i++ {
		cache.contextsFor(nil, tenantV1, keyOf(i, 1, fmt.Sprintf("only%d", i)))
	}
	if got := cache.entryCount(); got != 6 {
		t.Fatalf("entries = %d, want the tenant's and one per key", got)
	}

	tenantV2 := stamped(tenantOf(t, WordActionBlock, kw("changed")), "tenant-a", 2, cacheTestSaved.Add(time.Minute))
	contexts := cache.contextsFor(nil, tenantV2, keyOf(2, 1, "only2"))
	if got := cache.entryCount(); got != 1 {
		t.Fatalf("%d entries survive the tenant's save, want the one just built", got)
	}
	checkWordCacheBooks(t, &cache)
	if !matchesWord(contexts[0], "a changed day") || !matchesWord(contexts[0], "an only2 day") || matchesWord(contexts[0], "a base day") {
		t.Fatal("the context was not built from the new state of the tenant")
	}

	// A key saved again takes only its own old context with it.
	cache.contextsFor(nil, tenantV2, keyOf(3, 1, "only3"))
	cache.contextsFor(nil, tenantV2, keyOf(2, 2, "renamed2"))
	if got := cache.entryCount(); got != 2 {
		t.Fatalf("entries = %d, want key 3's and the new key 2's", got)
	}
	checkWordCacheBooks(t, &cache)
}

// Which of two saved states of a policy is the later cannot be read off their
// stamps: a policy created again starts over at version 1, and the times come
// from the clocks of whichever nodes saved. The state seen last is the current
// one, so a stamp that looks older is not refused a place.
func TestWordContextCacheTreatsTheLastStateSeenAsCurrent(t *testing.T) {
	var cache wordContextCache
	late := stamped(tenantOf(t, WordActionBlock, kw("alpha")), "tenant-a", 9, cacheTestSaved.Add(time.Hour))
	early := stamped(tenantOf(t, WordActionBlock, kw("beta")), "tenant-a", 1, cacheTestSaved)
	cache.contextsFor(nil, late, nil)
	cache.contextsFor(nil, early, nil)
	if got := cache.entryCount(); got != 1 || !cache.holds(tenantKeyOf(t, early)) {
		t.Fatalf("entries = %d, held = %v: the state seen last must be the one kept", got, cache.holds(tenantKeyOf(t, early)))
	}
	if again := cache.contextsFor(nil, early, nil); !matchesWord(again[0], "a beta day") || matchesWord(again[0], "an alpha day") {
		t.Fatal("the cached context is not the one for the state it was asked for")
	}
	checkWordCacheBooks(t, &cache)
	if back := cache.contextsFor(nil, late, nil); !matchesWord(back[0], "an alpha day") {
		t.Fatal("the other state was served the context of the first")
	}
	checkWordCacheBooks(t, &cache)
}

// When the cache is full it lets go of what was asked for least recently, one
// entry at a time. Starting over would send every request to compile its list
// in the same instant.
func TestWordContextCacheLetsGoOfTheLeastRecentlyUsedFirst(t *testing.T) {
	tenant := func(i int) *WordPolicy {
		return stamped(tenantOf(t, WordActionBlock, wordRulesN(100, int64(i))...), fmt.Sprintf("tenant-%d", i), 1, cacheTestSaved)
	}
	var probe wordContextCache
	probe.contextsFor(nil, tenant(0), nil)
	one := probe.byteCount()

	cache := wordContextCache{limit: one*3 + one/2} // room for three
	for i := 1; i <= 3; i++ {
		cache.contextsFor(nil, tenant(i), nil)
	}
	cache.contextsFor(nil, tenant(1), nil) // 1 was asked for again; 2 is the one nobody has asked for
	cache.contextsFor(nil, tenant(4), nil)
	for i, want := range map[int]bool{1: true, 2: false, 3: true, 4: true} {
		if got := cache.holds(tenantKeyOf(t, tenant(i))); got != want {
			t.Errorf("tenant %d held = %v, want %v", i, got, want)
		}
	}
	checkWordCacheBooks(t, &cache)

	for i := 5; i <= 40; i++ {
		cache.contextsFor(nil, tenant(i), nil)
		if got := cache.entryCount(); got != 3 {
			t.Fatalf("after adding tenant %d the cache holds %d entries, want it full at 3 and not emptied", i, got)
		}
		if got := cache.byteCount(); got > cache.limit {
			t.Fatalf("the cache weighs %d bytes, over its bound of %d", got, cache.limit)
		}
	}
	checkWordCacheBooks(t, &cache)
}

// An entry larger than the whole bound is still kept, alone: the requests that
// follow the one that built it need it, and letting it go would have each of
// them build it again.
func TestWordContextCacheKeepsOneEntryThatIsOverTheBound(t *testing.T) {
	cache := wordContextCache{limit: 1}
	tenant := func(i int) *WordPolicy {
		return stamped(tenantOf(t, WordActionBlock, wordRulesN(50, int64(i))...), fmt.Sprintf("tenant-%d", i), 1, cacheTestSaved)
	}
	a, b := tenant(1), tenant(2)
	cache.contextsFor(nil, a, nil)
	if got := cache.entryCount(); got != 1 || !cache.holds(tenantKeyOf(t, a)) {
		t.Fatalf("the first entry was not kept: %d", got)
	}
	built := cache.contextsFor(nil, b, nil)
	if got := cache.entryCount(); got != 1 || !cache.holds(tenantKeyOf(t, b)) || cache.holds(tenantKeyOf(t, a)) {
		t.Fatalf("entries = %d: the newest alone should be kept", got)
	}
	if again := cache.contextsFor(nil, b, nil); again[0].matcher != built[0].matcher {
		t.Fatal("the entry over the bound was built again")
	}
	checkWordCacheBooks(t, &cache)
}

// Right after a save, and when the server starts, every request that arrives
// finds the policy not yet compiled. They must share one compile.
func TestWordContextCacheBuildsOnceForConcurrentRequests(t *testing.T) {
	var cache wordContextCache
	tenant := stamped(tenantOf(t, WordActionBlock, kw("alpha")), "tenant-a", 1, cacheTestSaved)
	key := tenantKeyOf(t, tenant)
	var builds atomic.Int32
	release := make(chan struct{})
	build := func() (wordContext, bool) {
		builds.Add(1)
		<-release
		return buildTenantContext(tenant, nil)
	}
	const requests = 24
	results := make([]wordContext, requests)
	var wg sync.WaitGroup
	for i := range results {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i], _ = cache.context(key, build)
		}()
	}
	// The first build is held up until every request has reached the cache, so
	// that the others have to wait for it, or (if builds were not shared) build
	// their own.
	time.Sleep(200 * time.Millisecond)
	close(release)
	wg.Wait()
	if got := builds.Load(); got != 1 {
		t.Fatalf("%d requests built the same context %d times, want once", requests, got)
	}
	for i, result := range results {
		if result.matcher == nil || result.matcher != results[0].matcher {
			t.Fatalf("request %d was handed another matcher than request 0", i)
		}
	}
	if _, _ = cache.context(key, build); builds.Load() != 1 {
		t.Fatal("a request after the build compiled again")
	}
	checkWordCacheBooks(t, &cache)
}

// A build that fails (the guard recovers from that one level up) must not leave
// the requests that waited for it waiting for ever.
func TestWordContextCacheFailedBuildDoesNotStrandItsWaiters(t *testing.T) {
	var cache wordContextCache
	tenant := stamped(tenantOf(t, WordActionBlock, kw("alpha")), "tenant-a", 1, cacheTestSaved)
	key := tenantKeyOf(t, tenant)
	var builds atomic.Int32
	started := make(chan struct{})
	release := make(chan struct{})
	build := func() (wordContext, bool) {
		if builds.Add(1) == 1 {
			close(started)
			<-release
			panic("the first build fails")
		}
		return buildTenantContext(tenant, nil)
	}
	failure := make(chan any, 1)
	go func() {
		defer func() { failure <- recover() }()
		_, _ = cache.context(key, build)
	}()
	<-started
	waiter := make(chan wordContext, 1)
	go func() {
		context, _ := cache.context(key, build)
		waiter <- context
	}()
	time.Sleep(100 * time.Millisecond) // the second request is waiting for the first
	close(release)
	select {
	case context := <-waiter:
		if context.matcher == nil || !matchesWord(context, "an alpha day") {
			t.Fatal("the waiter was not given a context of its own")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the waiter is stuck behind a build that failed")
	}
	if recovered := <-failure; recovered == nil {
		t.Fatal("the failure must still reach the request that built")
	}
	if got := builds.Load(); got != 2 {
		t.Fatalf("builds = %d, want the failed one and the waiter's", got)
	}
	checkWordCacheBooks(t, &cache)
}

func sameContexts(a, b []wordContext) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		x, y := a[i], b[i]
		if x.layer != y.layer || x.action != y.action || x.replacement != y.replacement || x.scanScope != y.scanScope ||
			x.blockMessage != y.blockMessage || x.fold != y.fold || x.reply != y.reply {
			return false
		}
		if x.matcher.fold != y.matcher.fold || !reflect.DeepEqual(x.matcher.rules, y.matcher.rules) {
			return false
		}
	}
	return true
}

// The cache may only remember the contexts, never change them: whatever the
// policies are, it hands out what buildWordContexts makes of them.
func TestWordContextCacheHandsOutWhatBuildWordContextsMakes(t *testing.T) {
	disabledRule := kw("never")
	disabledRule.Enabled = false
	off := func(p *WordPolicy) { p.Enabled = false }
	replacing := func(p *WordPolicy) { p.InheritTenant = false; p.Action = WordActionReplace; p.Replacement = "[x]" }
	foldKey := func(p *WordPolicy) { yes := true; p.FoldTraditionalChinese = &yes; p.ReplyFilter = &yes }
	policy := func(scope, id string, version int64, mutate func(*WordPolicy), rules ...WordRule) *WordPolicy {
		tenantID := "tenant-a"
		if scope == WordScopePlatform {
			tenantID = "system"
		}
		return stamped(policyOf(t, scope, id, mutate, rules...), tenantID, version, cacheTestSaved.Add(time.Duration(version)*time.Minute))
	}
	platforms := []*WordPolicy{nil, policy(WordScopePlatform, "", 1, off, kw("pf")), policy(WordScopePlatform, "", 2, nil, kw("pf"), rx(`p\d+`)), policy(WordScopePlatform, "", 3, nil, disabledRule)}
	tenants := []*WordPolicy{nil, policy(WordScopeTenant, "", 1, off, kw("tn")), policy(WordScopeTenant, "", 2, nil, kw("tn"), kw("tm")), policy(WordScopeTenant, "", 3, nil, disabledRule)}
	keys := []*WordPolicy{
		nil,
		policy(WordScopeKey, "key-1", 1, off, kw("ky")),
		policy(WordScopeKey, "key-1", 2, nil, kw("ky")),
		policy(WordScopeKey, "key-1", 3, replacing, kw("kz")),
		policy(WordScopeKey, "key-1", 4, foldKey, kw("繁體")),
		policy(WordScopeKey, "key-1", 5, nil, disabledRule),
		policy(WordScopeKey, "key-1", 6, replacing, disabledRule),
	}
	var cache wordContextCache
	for pi, platform := range platforms {
		for ti, tenant := range tenants {
			for ki, key := range keys {
				want := buildWordContexts(platform, tenant, key)
				for pass := 1; pass <= 2; pass++ { // the second pass is served from the cache
					if got := cache.contextsFor(platform, tenant, key); !sameContexts(got, want) {
						t.Fatalf("platform %d, tenant %d, key %d, pass %d: got %+v, want %+v", pi, ti, ki, pass, got, want)
					}
				}
			}
		}
	}
	checkWordCacheBooks(t, &cache)
}

// A policy that is not enabled takes no part in the context, so saving it must
// not make the context build again, and every key that has none shares the
// tenant's.
func TestWordContextCacheIgnoresPoliciesThatTakeNoPart(t *testing.T) {
	var cache wordContextCache
	tenant := stamped(tenantOf(t, WordActionBlock, kw("alpha")), "tenant-a", 1, cacheTestSaved)
	disabledKey := func(version int64, word string) *WordPolicy {
		return stamped(policyOf(t, WordScopeKey, "key-1", func(p *WordPolicy) { p.Enabled = false }, kw(word)), "tenant-a", version, cacheTestSaved.Add(time.Duration(version)*time.Minute))
	}
	first := cache.contextsFor(nil, tenant, nil)
	for version := int64(1); version <= 5; version++ {
		got := cache.contextsFor(nil, tenant, disabledKey(version, fmt.Sprintf("word%d", version)))
		if len(got) != 1 || got[0].matcher != first[0].matcher {
			t.Fatalf("saving a disabled key policy (version %d) built the tenant's context again", version)
		}
	}
	if got := cache.entryCount(); got != 1 {
		t.Fatalf("entries = %d, want the tenant's alone", got)
	}

	// The same holds for a disabled tenant policy under a key that is enabled.
	key := stamped(policyOf(t, WordScopeKey, "key-2", nil, kw("beta")), "tenant-a", 1, cacheTestSaved)
	disabledTenant := func(version int64) *WordPolicy {
		return stamped(policyOf(t, WordScopeTenant, "", func(p *WordPolicy) { p.Enabled = false }, kw("ignored")), "tenant-a", version, cacheTestSaved.Add(time.Duration(version)*time.Minute))
	}
	base := cache.contextsFor(nil, disabledTenant(1), key)
	if again := cache.contextsFor(nil, disabledTenant(2), key); len(again) != 1 || again[0].matcher != base[0].matcher {
		t.Fatal("saving a disabled tenant policy built the key's context again")
	}
	checkWordCacheBooks(t, &cache)
}

// The cache bounds its memory by an estimate, so the estimate has to follow
// what the heap really holds.
func TestWordMatcherSizeTracksTheHeap(t *testing.T) {
	before := heapInUse()
	matcher := newWordMatcher(ruleRefs(tenantOf(t, WordActionBlock, wordRulesN(10000, 7)...), WordScopeTenant), false)
	after := heapInUse()
	actual, estimate := after-before, matcher.size()
	t.Logf("10,000 keywords: the heap grew by %.2f MB, the estimate is %.2f MB", float64(actual)/1e6, float64(estimate)/1e6)
	if estimate < actual*6/10 || estimate > actual*17/10 {
		t.Fatalf("the estimate (%d bytes) is not close to what the heap holds (%d bytes)", estimate, actual)
	}
	runtime.KeepAlive(matcher)

	if (*wordMatcher)(nil).size() != 0 {
		t.Fatal("no matcher takes no memory")
	}
	small := newWordMatcher(refs("tenant", kw("a")), false)
	withRegex := newWordMatcher(refs("tenant", kw("a"), rx(`\d{11}`)), false)
	if withRegex.size() <= small.size() {
		t.Fatal("a regular expression must add to the estimate")
	}
}

// The case the review measured: a list edited again and again, with the old
// compiled lists kept until the cache filled up.
func TestWordContextCacheMemoryStaysAtOneListWhileItIsEdited(t *testing.T) {
	var cache wordContextCache
	save := func(version int64) {
		tenant := stamped(tenantOf(t, WordActionBlock, wordRulesN(10000, version)...), "tenant-a", version, cacheTestSaved.Add(time.Duration(version)*time.Minute))
		if got := cache.contextsFor(nil, tenant, nil); len(got) != 1 {
			t.Fatalf("save %d: %d contexts", version, len(got))
		}
	}
	save(1)
	baseline := heapInUse()
	one := cache.byteCount()
	for version := int64(2); version <= 12; version++ {
		save(version)
	}
	grown := heapInUse() - baseline
	t.Logf("11 more saves of a 10,000-keyword list: the heap grew by %.2f MB; one list is estimated at %.2f MB", float64(grown)/1e6, float64(one)/1e6)
	if got := cache.entryCount(); got != 1 {
		t.Fatalf("the cache holds %d entries, want the newest alone", got)
	}
	if got := cache.byteCount(); got > one*2 {
		t.Fatalf("the cache weighs %d bytes after 12 saves, one list is %d", got, one)
	}
	if limit := one * 3; grown > limit {
		t.Fatalf("the heap grew by %d bytes over 11 saves, more than %d: old lists are being kept", grown, limit)
	}
}

// A model of the cache: whatever sequence of requests, saves and evictions
// happens, the books stay consistent and every request is judged by the policy
// states it was given.
func TestWordContextCacheBooksStayConsistentUnderRandomUse(t *testing.T) {
	const tenants, keys = 3, 3
	// The trailing x keeps the word of one version from being the beginning of
	// the word of another (v1 and v10).
	word := func(tenant, key int, version int64) string {
		return fmt.Sprintf("w%dk%dv%dx", tenant, key, version)
	}
	for seed := int64(1); seed <= 4; seed++ {
		r := rand.New(rand.NewSource(seed))
		versions := map[[2]int]int64{} // [tenant, key+1 (0 for the tenant policy)] -> current version
		var cache wordContextCache
		cache.limit = 6 * wordCacheEntryBytes // a handful of entries
		policyAt := func(tenant, key int) *WordPolicy {
			version := versions[[2]int{tenant, key}] + 1 // the first save is version 1
			if key == 0 {
				return stamped(tenantOf(t, WordActionBlock, kw(word(tenant, 0, version))), fmt.Sprintf("tenant-%d", tenant), version, cacheTestSaved.Add(time.Duration(version)*time.Minute))
			}
			return stamped(policyOf(t, WordScopeKey, fmt.Sprintf("key-%d", key), nil, kw(word(tenant, key, version))), fmt.Sprintf("tenant-%d", tenant), version, cacheTestSaved.Add(time.Duration(version)*time.Minute))
		}
		for op := 0; op < 3000; op++ {
			tenant, key := r.Intn(tenants), r.Intn(keys+1)
			switch r.Intn(5) {
			case 0: // a save
				versions[[2]int{tenant, key}]++
			default:
				tenantPolicy := policyAt(tenant, 0)
				var keyPolicy *WordPolicy
				if key > 0 && r.Intn(4) > 0 {
					keyPolicy = policyAt(tenant, key)
				}
				contexts := cache.contextsFor(nil, tenantPolicy, keyPolicy)
				if len(contexts) != 1 {
					t.Fatalf("seed %d op %d: %d contexts", seed, op, len(contexts))
				}
				wantTenant := word(tenant, 0, versions[[2]int{tenant, 0}]+1)
				if !matchesWord(contexts[0], "say "+wantTenant) {
					t.Fatalf("seed %d op %d: the tenant's current word %q is not matched", seed, op, wantTenant)
				}
				if keyPolicy != nil {
					wantKey := word(tenant, key, versions[[2]int{tenant, key}]+1)
					if !matchesWord(contexts[0], "say "+wantKey) {
						t.Fatalf("seed %d op %d: the key's current word %q is not matched", seed, op, wantKey)
					}
				}
				for older := int64(1); older <= versions[[2]int{tenant, 0}]; older++ {
					if matchesWord(contexts[0], "say "+word(tenant, 0, older)) {
						t.Fatalf("seed %d op %d: a word of the tenant's old version %d still matches", seed, op, older)
					}
				}
			}
			checkWordCacheBooks(t, &cache)
			if cache.byteCount() > cache.limit && cache.entryCount() > 1 {
				t.Fatalf("seed %d op %d: %d entries weigh %d bytes, over the bound of %d", seed, op, cache.entryCount(), cache.byteCount(), cache.limit)
			}
		}
	}
}

func TestWordContextCacheBooksStayConsistentUnderConcurrentUse(t *testing.T) {
	var cache wordContextCache
	cache.limit = 8 * wordCacheEntryBytes
	var wg sync.WaitGroup
	for worker := 0; worker < 8; worker++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r := rand.New(rand.NewSource(int64(worker)))
			for op := 0; op < 400; op++ {
				tenant, version := r.Intn(4), int64(1+r.Intn(3))
				policy := stamped(tenantOf(t, WordActionBlock, kw(fmt.Sprintf("t%dv%d", tenant, version))), fmt.Sprintf("tenant-%d", tenant), version, cacheTestSaved.Add(time.Duration(version)*time.Minute))
				if got := cache.contextsFor(nil, policy, nil); len(got) != 1 || !matchesWord(got[0], fmt.Sprintf("say t%dv%d", tenant, version)) {
					t.Errorf("worker %d: wrong context for tenant %d version %d", worker, tenant, version)
					return
				}
			}
		}()
	}
	wg.Wait()
	checkWordCacheBooks(t, &cache)
}
