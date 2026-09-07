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
