package ipaccess

import (
	"context"
	"net"
	"sort"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"
)

const (
	// autoBanSlotCount divides the observation window into sub-buckets. Counting
	// per slot instead of storing a timestamp per failure keeps memory flat while
	// under attack, which is exactly when this structure is being filled.
	autoBanSlotCount = 12
	// maxTrackedSources bounds memory. An IPv6 /64 is free with most hosting, so
	// an attacker can mint sources at will; without a cap this map is the OOM.
	maxTrackedSources = 20000
	// autoBanEvictRatio is the fraction of the least recently seen sources
	// dropped when the table saturates.
	autoBanEvictRatio = 8
)

// AutoBanOutcome reports what the engine did with one failure.
type AutoBanOutcome struct {
	// Triggered is true when the failure count crossed the threshold.
	Triggered bool
	// Enforced is true when a deny rule was actually written. It is false in
	// observe mode, which is the shipped default.
	Enforced bool
	// CIDR is the network that was (or would be) banned.
	CIDR string
	// Until is the ban expiry when enforced.
	Until time.Time
	// Failures is the count inside the observation window.
	Failures int
}

type autoBanSlot struct {
	index int64
	count int32
}

type autoBanCounter struct {
	slots [autoBanSlotCount]autoBanSlot
	// banCount drives the escalation ladder and survives the window reset, so a
	// source that returns every hour gets progressively longer bans.
	banCount     int
	bannedUntil  time.Time
	lastActivity time.Time
}

// autoBanEngine promotes repeated authentication failures into temporary deny
// rules. It is deliberately separate from the login throttle: the throttle asks
// "has this credential been guessed too often", while this asks "is this source
// worth talking to at all", and the second question spans every surface.
type autoBanEngine struct {
	registry *Registry
	mu       sync.Mutex
	counters map[string]*autoBanCounter
}

func newAutoBanEngine(registry *Registry) *autoBanEngine {
	return &autoBanEngine{registry: registry, counters: make(map[string]*autoBanCounter)}
}

// BanCIDR renders the network an automatic ban covers: a single IPv4 host, or an
// IPv6 /64 because a single /64 is one customer allocation and banning /128 just
// invites the next address in the same block.
func BanCIDR(ip net.IP) string {
	if ip == nil {
		return ""
	}
	if v4 := ip.To4(); v4 != nil {
		return v4.String() + "/32"
	}
	return ip.Mask(net.CIDRMask(64, 128)).String() + "/64"
}

// RecordFailure charges one authentication failure against a source and applies
// the auto-ban policy.
func (e *autoBanEngine) RecordFailure(ctx context.Context, addr ClientAddress, reason string) AutoBanOutcome {
	if e == nil || e.registry == nil {
		return AutoBanOutcome{}
	}
	policy := e.registry.Policy().AutoBan
	if policy.Mode == AutoBanOff {
		return AutoBanOutcome{}
	}
	// An untrusted address is the proxy's, so banning it would take down every
	// client behind it.
	//
	// Loopback is skipped here in its broad sense — any loopback result, not just
	// a genuine local operator — because the two directions fail asymmetrically.
	// Admission uses the strict LocalOperator test: wrongly exempting a chain that
	// merely dead-ends on loopback lets everyone past. Banning uses the broad test:
	// a deny rule on a loopback address that a half-configured chain resolves to
	// would block every external request at once. Each side takes the safer error.
	if !addr.Trusted || addr.Loopback() || addr.IP == nil {
		return AutoBanOutcome{}
	}
	// An allow-listed source is exempt by definition; the engine must never be
	// able to override an operator's explicit decision.
	if e.registry.Matcher().AllowsAddress(addr.IP) {
		return AutoBanOutcome{}
	}
	// Nor may it ban the host itself, its reverse proxy, or an egress proxy.
	if _, ok := e.registry.Protected(addr.IP); ok {
		return AutoBanOutcome{}
	}

	cidr := BanCIDR(addr.IP)
	now := time.Now()
	failures, banCount, alreadyBanned := e.charge(cidr, policy, now)
	if alreadyBanned || failures < policy.FailureThreshold {
		return AutoBanOutcome{Failures: failures, CIDR: cidr}
	}

	outcome := AutoBanOutcome{Triggered: true, Failures: failures, CIDR: cidr}
	duration := policy.BanDuration(banCount)
	outcome.Until = now.Add(duration)

	if policy.Mode != AutoBanEnforce {
		// Observe mode still marks the source as banned internally so the log
		// records one decision per offender instead of one per request.
		e.markBanned(cidr, outcome.Until, now)
		log.Warnf("ip-access: auto-ban would trigger for %s (%d failures in %s, %s) — observe mode, no rule written",
			cidr, failures, policy.Window(), reason)
		e.registry.notifyBan(outcome, reason)
		return outcome
	}

	rule, created, err := e.registry.store.UpsertAutoBan(ctx, cidr, reason, outcome.Until)
	if err != nil {
		log.WithError(err).Warnf("ip-access: auto-ban write failed for %s", cidr)
		return outcome
	}
	// A pre-existing manual rule means an operator already decided; leave it be.
	if rule.Source == SourceManual {
		return outcome
	}
	e.markBanned(cidr, outcome.Until, now)
	outcome.Enforced = true
	if created {
		log.Warnf("ip-access: auto-banned %s until %s (%d failures in %s, %s)",
			cidr, outcome.Until.Format(time.RFC3339), failures, policy.Window(), reason)
	}
	if err = e.registry.Refresh(ctx); err != nil {
		log.WithError(err).Debug("ip-access: refresh after auto-ban failed")
	}
	e.registry.notifyBan(outcome, reason)
	return outcome
}

// charge adds one failure and reports the window count, the number of previous
// bans, and whether a ban is already in force. In a cluster the cluster-wide
// count decides (autoban_cluster.go); the local one is kept for fallback.
func (e *autoBanEngine) charge(cidr string, policy AutoBanPolicy, now time.Time) (int, int, bool) {
	failures, bans, banned := e.chargeLocal(cidr, policy, now)
	if sharedFailures, sharedBans, sharedBanned, ok := chargeShared(cidr, policy, now); ok {
		return sharedFailures, sharedBans, sharedBanned
	}
	return failures, bans, banned
}

func (e *autoBanEngine) chargeLocal(cidr string, policy AutoBanPolicy, now time.Time) (int, int, bool) {
	window := policy.Window()
	if window <= 0 {
		return 0, 0, false
	}
	width := window / autoBanSlotCount

	e.mu.Lock()
	defer e.mu.Unlock()

	counter := e.counters[cidr]
	if counter == nil {
		e.evictIfSaturatedLocked()
		counter = &autoBanCounter{}
		e.counters[cidr] = counter
	}
	counter.lastActivity = now
	if counter.bannedUntil.After(now) {
		return 0, counter.banCount, true
	}

	index := now.UnixNano() / int64(width)
	slot := &counter.slots[int(index%autoBanSlotCount)]
	if slot.index != index {
		slot.index = index
		slot.count = 0
	}
	slot.count++

	oldest := index - autoBanSlotCount + 1
	total := 0
	for i := range counter.slots {
		if counter.slots[i].index >= oldest && counter.slots[i].index <= index {
			total += int(counter.slots[i].count)
		}
	}
	return total, counter.banCount, false
}

func (e *autoBanEngine) markBanned(cidr string, until time.Time, now time.Time) {
	markBannedShared(cidr, until, now)
	e.mu.Lock()
	defer e.mu.Unlock()
	counter := e.counters[cidr]
	if counter == nil {
		counter = &autoBanCounter{}
		e.counters[cidr] = counter
	}
	counter.bannedUntil = until
	counter.banCount++
	counter.lastActivity = now
	// Clear the window so the same failures cannot immediately re-trigger once
	// the ban lapses.
	for i := range counter.slots {
		counter.slots[i] = autoBanSlot{}
	}
}

func (e *autoBanEngine) evictIfSaturatedLocked() {
	if len(e.counters) < maxTrackedSources {
		return
	}
	type aged struct {
		key  string
		seen time.Time
	}
	candidates := make([]aged, 0, len(e.counters))
	for key, counter := range e.counters {
		// Never evict a source that is currently banned: dropping it would hand
		// the attacker an early release.
		if counter.bannedUntil.After(time.Now()) {
			continue
		}
		candidates = append(candidates, aged{key: key, seen: counter.lastActivity})
	}
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].seen.Before(candidates[j].seen) })
	evict := len(candidates) / autoBanEvictRatio
	if evict < 1 {
		evict = 1
	}
	for i := 0; i < evict && i < len(candidates); i++ {
		delete(e.counters, candidates[i].key)
	}
	log.Warnf("ip-access: auto-ban source table saturated, evicted %d entries", evict)
}

// TrackedSources reports how many sources are being counted. Test-facing.
func (e *autoBanEngine) TrackedSources() int {
	if e == nil {
		return 0
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	return len(e.counters)
}
