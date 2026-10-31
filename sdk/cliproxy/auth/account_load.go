package auth

import (
	"math"
	"sync"
	"time"
)

// QuotaLoadSource reports how close an account is to exhausting its quota
// window, as a ratio in [0,1]. It is injected from the usage layer, which owns
// the quota tables; the selector package must not reach into storage itself.
//
// Implementations must be safe for concurrent use and must not block: this runs
// on the selection hot path, so it is expected to serve from a cached snapshot
// and refresh out of band. Returning false means "no data", which leaves
// scheduling to the in-process signals alone.
type QuotaLoadSource interface {
	QuotaLoadRatio(auth *Auth) (float64, bool)
}

// QuotaResetUrgencySource is an optional extension of QuotaLoadSource that
// reports when an account has expiring quota due for reset soon.
//
// QuotaResetState returns:
//   - remaining: share of usable quota remaining in [0,1] (bounded by both
//     long and short windows so a short-window-exhausted account is not
//     treated as having burnable headroom)
//   - urgency: how close the long-window reset is in [0,1], where 0 means
//     beyond the discount horizon and 1 means within the full-burn window
//   - ok: true when quota state is known for auth.
type QuotaResetUrgencySource interface {
	QuotaResetState(auth *Auth) (remaining float64, urgency float64, ok bool)
}

const (
	// selectionDecayHalfLife controls how fast recent-selection pressure fades.
	// Five minutes is long enough to spread a burst of turns from one agent
	// session across accounts, short enough that an idle account recovers its
	// standing before the next task starts.
	selectionDecayHalfLife = 5 * time.Minute

	// selectionTrackerMaxKeys bounds the tracker's memory. Reaching it evicts
	// the coldest entries rather than clearing the whole map, so live accounts
	// keep their pressure history.
	selectionTrackerMaxKeys = 4096

	// minResetDrainRemaining is the minimum usable remaining quota share in
	// [0,1] required for an expiring account to actively pull traffic toward
	// itself before reset.
	minResetDrainRemaining = 0.25

	// resetDrainGapThreshold is the minimum gap (25 percentage points) in
	// expiring quota reserve or remaining quota required for an account due
	// for reset to preempt lower-priority candidates, including existing
	// sticky conversations.
	resetDrainGapThreshold = 0.25

	// resetUrgencyBonus rewards accounts with expiring quota in least-load
	// scoring so they attract new work even before the hard preemption gap
	// is reached and rank ahead of non-expiring idle accounts.
	resetUrgencyBonus = 6.0
)

type decayCounter struct {
	value     float64
	updatedAt time.Time
}

// decayedValue returns the counter's value faded to `now`.
func (c decayCounter) decayedValue(now time.Time) float64 {
	if c.value == 0 || c.updatedAt.IsZero() {
		return 0
	}
	elapsed := now.Sub(c.updatedAt)
	if elapsed <= 0 {
		return c.value
	}
	// Half-life decay: value * 0.5^(elapsed/halfLife).
	return c.value * math.Exp2(-elapsed.Seconds()/selectionDecayHalfLife.Seconds())
}

// selectionPressureTracker records how much traffic each account has recently
// been handed. It is the self-contained load signal behind least-load
// distribution: it needs no external data and still reflects the only thing
// that matters for balancing, which account this proxy has been pushing work to.
type selectionPressureTracker struct {
	mu       sync.Mutex
	counters map[string]decayCounter
	maxKeys  int
}

func newSelectionPressureTracker() *selectionPressureTracker {
	return &selectionPressureTracker{counters: make(map[string]decayCounter)}
}

// observe records that authID was just selected.
func (t *selectionPressureTracker) observe(authID string, now time.Time) {
	if t == nil || authID == "" {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.counters == nil {
		t.counters = make(map[string]decayCounter)
	}
	t.evictIfNeededLocked(authID, now)
	current := t.counters[authID]
	t.counters[authID] = decayCounter{value: current.decayedValue(now) + 1, updatedAt: now}
}

// pressure returns the decayed selection count for authID.
func (t *selectionPressureTracker) pressure(authID string, now time.Time) float64 {
	if t == nil || authID == "" {
		return 0
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.counters[authID].decayedValue(now)
}

// evictIfNeededLocked drops the coldest entries once the map is full. Must be
// called with t.mu held.
func (t *selectionPressureTracker) evictIfNeededLocked(incoming string, now time.Time) {
	limit := t.maxKeys
	if limit <= 0 {
		limit = selectionTrackerMaxKeys
	}
	if _, exists := t.counters[incoming]; exists || len(t.counters) < limit {
		return
	}
	oldestID := ""
	oldestAt := time.Time{}
	for id, counter := range t.counters {
		if counter.decayedValue(now) < 0.01 {
			delete(t.counters, id)
			continue
		}
		if oldestID == "" || counter.updatedAt.Before(oldestAt) {
			oldestID = id
			oldestAt = counter.updatedAt
		}
	}
	if len(t.counters) >= limit && oldestID != "" {
		delete(t.counters, oldestID)
	}
}

// accountLoadRatio reports a candidate's overall load in [0,1], combining the
// in-flight concurrency ratio with the quota ratio when a source is available.
// The larger of the two wins: an account that is either saturated by concurrent
// requests or near its quota ceiling should attract less new traffic.
func accountLoadRatio(auth *Auth, limiter *AccountConcurrencyLimiter, quota QuotaLoadSource) float64 {
	if auth == nil {
		return 0
	}
	ratio := 0.0
	if limiter != nil {
		ratio = limiter.GetLoadRate(auth.ID, auth.ConcurrencyLimit())
	}
	if quota != nil {
		if quotaRatio, ok := quota.QuotaLoadRatio(auth); ok && quotaRatio > ratio {
			ratio = quotaRatio
		}
	}
	if ratio < 0 {
		return 0
	}
	if ratio > 1 {
		return 1
	}
	return ratio
}

func clampUnitRatio(v float64) float64 {
	if math.IsNaN(v) || v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}

func (d *schedulerDeps) resetState(auth *Auth) (remaining float64, urgency float64, ok bool) {
	if d == nil || auth == nil || d.quota == nil {
		return 0, 0, false
	}
	if source, hasUrgency := d.quota.(QuotaResetUrgencySource); hasUrgency {
		if rem, urg, found := source.QuotaResetState(auth); found {
			return clampUnitRatio(rem), clampUnitRatio(urg), true
		}
	}
	if load, found := d.quota.QuotaLoadRatio(auth); found {
		return clampUnitRatio(1 - load), 0, true
	}
	return 0, 0, false
}

// preferResetDrainAuths narrows available candidates when one or more healthy
// accounts are approaching their quota reset with plenty of remaining quota
// and open a large gap (>= resetDrainGapThreshold) over other candidates.
//
// Filtering before session-sticky honour and fallback selection ensures that:
//  1. New requests across weighted, fill-first, and least-load distributions
//     prioritize the expiring account(s).
//  2. Existing sticky conversations pinned to an account that trails the
//     expiring account by >= resetDrainGapThreshold (in expiring quota reserve
//     or remaining quota) release their old binding and re-pin to the expiring
//     account, while conversations already on an expiring account within the
//     threshold stay sticky without thrashing.
func preferResetDrainAuths(available []*Auth, deps *schedulerDeps) []*Auth {
	if len(available) <= 1 || deps == nil || deps.quota == nil {
		return available
	}
	if _, hasUrgency := deps.quota.(QuotaResetUrgencySource); !hasUrgency {
		return available
	}

	var (
		leader         *Auth
		leaderRem      float64
		leaderUrg      float64
		leaderExpiring float64
	)
	for _, candidate := range available {
		if candidate == nil {
			continue
		}
		if deps.loadRatio(candidate) >= fillFirstSwitchLoad {
			continue
		}
		rem, urg, ok := deps.resetState(candidate)
		if !ok || urg <= 0 || rem < minResetDrainRemaining {
			continue
		}
		expiring := urg * rem
		if leader == nil || expiring > leaderExpiring || (expiring == leaderExpiring && rem > leaderRem) {
			leader = candidate
			leaderRem = rem
			leaderUrg = urg
			leaderExpiring = expiring
		}
	}
	if leader == nil {
		return available
	}

	preferred := make([]*Auth, 0, len(available))
	for _, candidate := range available {
		if candidate == nil {
			continue
		}
		if !hasResetDrainGap(deps, leader, leaderRem, leaderUrg, leaderExpiring, candidate) {
			preferred = append(preferred, candidate)
		}
	}
	if len(preferred) > 0 && len(preferred) < len(available) {
		return preferred
	}
	return available
}

func hasResetDrainGap(deps *schedulerDeps, leader *Auth, leaderRem, leaderUrg, leaderExpiring float64, candidate *Auth) bool {
	if candidate == nil || leader == nil || candidate.ID == leader.ID {
		return false
	}
	if deps.loadRatio(candidate) >= fillFirstSwitchLoad {
		return true
	}
	cRem, cUrg, ok := deps.resetState(candidate)
	if !ok {
		return leaderExpiring >= resetDrainGapThreshold
	}
	cExpiring := cUrg * cRem
	if leaderExpiring-cExpiring >= resetDrainGapThreshold {
		return true
	}
	if leaderUrg >= cUrg && leaderRem-cRem >= resetDrainGapThreshold {
		return true
	}
	return false
}
