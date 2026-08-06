package management

import (
	"hash/fnv"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	log "github.com/sirupsen/logrus"
)

// throttleScope separates credential-guessing buckets from credential-less
// traffic. A request that carried no secret proves nothing about the secret,
// so counting it as a guess only produces false lockouts — and today it is a
// zero-cost way for anyone sharing an egress IP to ban the whole deployment.
type throttleScope uint8

const (
	scopeUnauthenticated throttleScope = iota
	scopeManagementKey
	scopeUserPassword
	scopeUserAccount
	scopeRefresh
	scopePortalPassword
	scopePortalAccount
)

func (s throttleScope) String() string {
	switch s {
	case scopeUnauthenticated:
		return "unauthenticated"
	case scopeManagementKey:
		return "management_key"
	case scopeUserPassword:
		return "user_password"
	case scopeUserAccount:
		return "user_account"
	case scopeRefresh:
		return "refresh"
	case scopePortalPassword:
		return "portal_password"
	case scopePortalAccount:
		return "portal_account"
	default:
		return "unknown"
	}
}

type throttleOutcome uint8

const (
	outcomeAllow throttleOutcome = iota
	// outcomeRateLimited is a soft 429 + Retry-After. Its message must never
	// contain "IP banned": the admin panel treats that string as proof the
	// session is dead and forces a logout.
	outcomeRateLimited
	// outcomeBlocked is a hard 403 ban, only reachable for wrong-credential
	// scopes whose key identifies a single client.
	outcomeBlocked
)

func (o throttleOutcome) String() string {
	switch o {
	case outcomeAllow:
		return "allow"
	case outcomeRateLimited:
		return "rate_limited"
	case outcomeBlocked:
		return "blocked"
	default:
		return "unknown"
	}
}

type throttleDecision struct {
	Outcome    throttleOutcome
	RetryAfter time.Duration
	Scope      throttleScope
	Generation int
	ShortCount int
	LongCount  int
	// NewlyArmed is true only on the transition into a block, so a brute-force
	// run logs one Warn instead of one per rejected request.
	NewlyArmed bool
}

const (
	throttleShortSlotCount  = 12
	throttleLongSlotCount   = 24
	throttleShardCount      = 16
	maxTrackedKeys          = 20000
	throttleCleanupInterval = 10 * time.Minute
	throttleEvictBatchRatio = 8
	// throttleMinIdleRetention floors how long an idle bucket is kept even when
	// every configured ResetAfter is shorter.
	throttleMinIdleRetention = 2 * time.Hour
)

// maxShardKeys is the per-shard capacity that enforces the global bound.
const maxShardKeys = maxTrackedKeys / throttleShardCount

// windowSlot is one sub-bucket of a sliding window. Storing a counter per slot
// instead of a timestamp per event keeps memory flat under attack: a burst of a
// million failures costs the same as a handful.
type windowSlot struct {
	index int64
	count int32
}

type throttleEntry struct {
	short        [throttleShortSlotCount]windowSlot
	long         [throttleLongSlotCount]windowSlot
	blockedUntil time.Time
	generation   int
	lastActivity time.Time
}

type throttleShard struct {
	mu      sync.Mutex
	entries map[string]*throttleEntry
}

// loginThrottle rate-limits management authentication attempts per bucket key.
//
// State is per process and deliberately in-memory: it protects against online
// guessing against a single instance, not against a distributed attack, and
// losing it on restart only costs an attacker's progress, never a legitimate
// user's access.
//
// Sharded because the credential-less bucket is charged on the middleware hot
// path by every unauthenticated request, where a single mutex would serialise
// the whole management surface.
type loginThrottle struct {
	shards   [throttleShardCount]throttleShard
	policies atomic.Pointer[map[throttleScope]throttlePolicy]
	stopOnce sync.Once
	stop     chan struct{}
}

func newLoginThrottle(policies map[throttleScope]throttlePolicy) *loginThrottle {
	t := &loginThrottle{stop: make(chan struct{})}
	for i := range t.shards {
		t.shards[i].entries = make(map[string]*throttleEntry)
	}
	t.setPolicies(policies)
	go t.cleanupLoop()
	return t
}

// setPolicies swaps the whole table atomically so a config hot-reload cannot be
// observed half-applied by an in-flight request.
func (t *loginThrottle) setPolicies(p map[throttleScope]throttlePolicy) {
	if t == nil {
		return
	}
	if p == nil {
		p = defaultThrottlePolicies()
	}
	snapshot := make(map[throttleScope]throttlePolicy, len(p))
	for scope, policy := range p {
		snapshot[scope] = policy
	}
	t.policies.Store(&snapshot)
}

func (t *loginThrottle) policySnapshot() map[throttleScope]throttlePolicy {
	if t == nil {
		return nil
	}
	if p := t.policies.Load(); p != nil {
		return *p
	}
	return nil
}

// policyFor resolves the effective policy for a key. The shared-key downgrade is
// applied here rather than stored on the entry so a key that becomes shared (or
// stops being shared) after trusted-proxies is fixed takes effect immediately.
func (t *loginThrottle) policyFor(key throttleKey) throttlePolicy {
	snapshot := t.policySnapshot()
	policy, ok := snapshot[key.Scope]
	if !ok {
		policy = defaultThrottlePolicies()[key.Scope]
	}
	if key.Shared {
		policy = applySharedKeyDowngrade(policy)
	}
	return policy
}

// limitsFor exposes the effective thresholds for logging, so an operator reading
// "short_count=9 limit=10" can tell how close a client is without reverse
// engineering the shared-key downgrade from the policy table.
func (t *loginThrottle) limitsFor(key throttleKey) (short, long int) {
	if t == nil {
		return 0, 0
	}
	policy := t.policyFor(key)
	return policy.Short.Limit, policy.Long.Limit
}

func (t *loginThrottle) shardFor(bucket string) *throttleShard {
	hasher := fnv.New32a()
	_, _ = hasher.Write([]byte(bucket))
	return &t.shards[hasher.Sum32()%throttleShardCount]
}

func bucketID(key throttleKey) string {
	return key.Scope.String() + "|" + key.Value
}

// evaluate reports the current standing of a bucket without charging it. It is
// the pre-flight check that must run before any password hashing, so a banned
// client cannot spend the server's CPU.
func (t *loginThrottle) evaluate(key throttleKey, now time.Time) throttleDecision {
	if t == nil {
		return throttleDecision{Scope: key.Scope}
	}
	policy := t.policyFor(key)
	bucket := bucketID(key)
	shard := t.shardFor(bucket)

	shard.mu.Lock()
	defer shard.mu.Unlock()
	entry := shard.entries[bucket]
	if entry == nil || !entry.blockedUntil.After(now) {
		return throttleDecision{Scope: key.Scope}
	}
	return throttleDecision{
		Outcome:    blockOutcome(policy),
		RetryAfter: entry.blockedUntil.Sub(now),
		Scope:      key.Scope,
		Generation: entry.generation,
		ShortCount: countWindow(entry.short[:], policy.Short, now),
		LongCount:  countWindow(entry.long[:], policy.Long, now),
	}
}

// recordFailure charges one failure against the bucket and reports the resulting
// standing. Requests arriving while a block is already armed are rejected
// without being counted, so an attacker cannot escalate their own penalty by
// hammering a closed door.
func (t *loginThrottle) recordFailure(key throttleKey, now time.Time) throttleDecision {
	if t == nil {
		return throttleDecision{Scope: key.Scope}
	}
	policy := t.policyFor(key)
	bucket := bucketID(key)
	shard := t.shardFor(bucket)

	shard.mu.Lock()
	defer shard.mu.Unlock()

	entry := shard.entries[bucket]
	if entry == nil {
		t.evictIfSaturatedLocked(shard, now)
		entry = &throttleEntry{}
		shard.entries[bucket] = entry
	} else if policy.ResetAfter > 0 && !entry.lastActivity.IsZero() &&
		now.Sub(entry.lastActivity) >= policy.ResetAfter {
		// A quiet period clears both the counters and the escalation ladder, so a
		// user who mistyped their password months ago does not start at a 30m ban.
		*entry = throttleEntry{}
	}
	entry.lastActivity = now

	if entry.blockedUntil.After(now) {
		return throttleDecision{
			Outcome:    blockOutcome(policy),
			RetryAfter: entry.blockedUntil.Sub(now),
			Scope:      key.Scope,
			Generation: entry.generation,
			ShortCount: countWindow(entry.short[:], policy.Short, now),
			LongCount:  countWindow(entry.long[:], policy.Long, now),
		}
	}

	addToWindow(entry.short[:], policy.Short, now)
	addToWindow(entry.long[:], policy.Long, now)
	shortCount := countWindow(entry.short[:], policy.Short, now)
	longCount := countWindow(entry.long[:], policy.Long, now)

	decision := throttleDecision{
		Scope:      key.Scope,
		Generation: entry.generation,
		ShortCount: shortCount,
		LongCount:  longCount,
	}
	shortTripped := policy.Short.Limit > 0 && shortCount >= policy.Short.Limit
	longTripped := policy.Long.Limit > 0 && longCount >= policy.Long.Limit
	if !shortTripped && !longTripped {
		return decision
	}

	wait := blockDurationFor(policy, entry.generation)
	if wait <= 0 {
		return decision
	}
	entry.blockedUntil = now.Add(wait)
	entry.generation++
	decision.Outcome = blockOutcome(policy)
	decision.RetryAfter = wait
	decision.Generation = entry.generation
	decision.NewlyArmed = true
	return decision
}

// recordSuccess clears exactly one bucket.
//
// NIST SP 800-63B §5.2.2: a successful login clears that account's failures. It
// must not clear a per-IP bucket, or an attacker holding one valid account of
// their own could reset their guessing budget between rounds.
func (t *loginThrottle) recordSuccess(key throttleKey) {
	if t == nil {
		return
	}
	bucket := bucketID(key)
	shard := t.shardFor(bucket)
	shard.mu.Lock()
	delete(shard.entries, bucket)
	shard.mu.Unlock()
}

func blockOutcome(policy throttlePolicy) throttleOutcome {
	if policy.HardBlock {
		return outcomeBlocked
	}
	return outcomeRateLimited
}

// slotDuration is the width of one sub-bucket. Precision is therefore
// ±Window/slotCount, and the error always over-counts stale failures, which is
// the conservative direction.
func slotDuration(window throttleWindow, slotCount int) time.Duration {
	if window.Window <= 0 || slotCount <= 0 {
		return 0
	}
	return window.Window / time.Duration(slotCount)
}

func addToWindow(slots []windowSlot, window throttleWindow, now time.Time) {
	if window.Limit <= 0 {
		return
	}
	width := slotDuration(window, len(slots))
	if width <= 0 {
		return
	}
	index := now.UnixNano() / int64(width)
	slot := &slots[int(index%int64(len(slots)))]
	if slot.index != index {
		slot.index = index
		slot.count = 0
	}
	slot.count++
}

func countWindow(slots []windowSlot, window throttleWindow, now time.Time) int {
	if window.Limit <= 0 {
		return 0
	}
	width := slotDuration(window, len(slots))
	if width <= 0 {
		return 0
	}
	current := now.UnixNano() / int64(width)
	oldest := current - int64(len(slots)) + 1
	total := 0
	for i := range slots {
		if slots[i].index >= oldest && slots[i].index <= current {
			total += int(slots[i].count)
		}
	}
	return total
}

// evictIfSaturatedLocked drops the least recently active fraction of a shard once
// it reaches capacity, bounding memory at ~maxTrackedKeys entries. Eviction is
// LRU rather than random so an active attacker's bucket is the last thing
// dropped; the alternative is letting an IPv6 /64 holder mint buckets until the
// process OOMs.
func (t *loginThrottle) evictIfSaturatedLocked(shard *throttleShard, now time.Time) {
	total := len(shard.entries)
	if total < maxShardKeys {
		return
	}
	type aged struct {
		key  string
		seen time.Time
	}
	candidates := make([]aged, 0, total)
	for key, entry := range shard.entries {
		candidates = append(candidates, aged{key: key, seen: entry.lastActivity})
	}
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].seen.Before(candidates[j].seen) })

	evict := len(candidates) / throttleEvictBatchRatio
	if evict < 1 {
		evict = 1
	}
	for i := 0; i < evict; i++ {
		delete(shard.entries, candidates[i].key)
	}
	warnThrottleSaturation(evict, total)
}

func (t *loginThrottle) cleanupLoop() {
	ticker := time.NewTicker(throttleCleanupInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			t.purgeStale(time.Now())
		case <-t.stop:
			return
		}
	}
}

// idleRetention is the longest ResetAfter in the active policy table: past that
// point a bucket would be reset on its next charge anyway, so dropping it is
// semantically identical and just reclaims memory.
func (t *loginThrottle) idleRetention() time.Duration {
	retention := throttleMinIdleRetention
	for _, policy := range t.policySnapshot() {
		if policy.ResetAfter > retention {
			retention = policy.ResetAfter
		}
	}
	return retention
}

// purgeStale drops entries idle beyond idleRetention.
//
// Entries still inside their block window are kept regardless of idleness —
// without that guard an attacker could shed a long block simply by going quiet.
func (t *loginThrottle) purgeStale(now time.Time) {
	if t == nil {
		return
	}
	retention := t.idleRetention()
	for i := range t.shards {
		shard := &t.shards[i]
		shard.mu.Lock()
		for key, entry := range shard.entries {
			if entry.blockedUntil.After(now) {
				continue
			}
			if now.Sub(entry.lastActivity) > retention {
				delete(shard.entries, key)
			}
		}
		shard.mu.Unlock()
	}
}

// trackedKeys reports the total number of live buckets. Test-facing helper that
// also keeps the shard iteration lock discipline in one place.
func (t *loginThrottle) trackedKeys() int {
	if t == nil {
		return 0
	}
	total := 0
	for i := range t.shards {
		shard := &t.shards[i]
		shard.mu.Lock()
		total += len(shard.entries)
		shard.mu.Unlock()
	}
	return total
}

// close stops the cleanup goroutine. Safe to call more than once.
func (t *loginThrottle) close() {
	if t == nil {
		return
	}
	t.stopOnce.Do(func() { close(t.stop) })
}

// throttleSaturationWarn deduplicates the saturation warning: under the attack
// that causes it, one log line per evicted batch would itself be a log flood.
var throttleSaturationWarn struct {
	mu   sync.Mutex
	last time.Time
}

const warnDedupInterval = 10 * time.Minute

func warnThrottleSaturation(evicted, total int) {
	now := time.Now()
	throttleSaturationWarn.mu.Lock()
	if !throttleSaturationWarn.last.IsZero() && now.Sub(throttleSaturationWarn.last) < warnDedupInterval {
		throttleSaturationWarn.mu.Unlock()
		return
	}
	throttleSaturationWarn.last = now
	throttleSaturationWarn.mu.Unlock()
	log.Warnf("auth-throttle: key table saturated, evicted %d of %d entries", evicted, total)
}
