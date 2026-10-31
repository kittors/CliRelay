// Package schedulingload feeds observed upstream quota usage into credential
// scheduling.
//
// The selector package deliberately knows nothing about storage, so this
// package adapts the persisted AI account status (which already carries the
// per-window quota percentages collected by the account status probes) into the
// coreauth.QuotaLoadSource and coreauth.QuotaResetUrgencySource contracts.
package schedulingload

import (
	"math"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	log "github.com/sirupsen/logrus"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/usage"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

// snapshotTTL bounds how stale a quota reading may be before a refresh is
// triggered. Quota percentages move on the order of minutes, and the account
// status probes themselves run far less often than request selection, so a
// short TTL here costs nothing and keeps the data fresh enough to steer traffic.
const snapshotTTL = 60 * time.Second

type accountQuotaState struct {
	shortUsed    float64
	longUsed     float64
	hasShort     bool
	hasLong      bool
	longResetAt  time.Time
	hasLongReset bool
}

type snapshot struct {
	accounts    map[string]accountQuotaState
	ratios      map[string]float64
	resets      map[string]time.Time
	refreshedAt time.Time
}

// Provider serves cached quota load ratios and reset urgency state, refreshing
// them out of band.
//
// Reads never block on the database: a stale snapshot is returned while a
// refresh runs in the background, and an empty snapshot simply reports "no
// data" so scheduling falls back to its in-process signals.
type Provider struct {
	current    atomic.Pointer[snapshot]
	refreshing atomic.Bool
	tenantsMu  sync.RWMutex
	tenants    map[string]struct{}
}

func NewProvider() *Provider {
	p := &Provider{tenants: make(map[string]struct{})}
	p.current.Store(&snapshot{
		accounts: map[string]accountQuotaState{},
		ratios:   map[string]float64{},
		resets:   map[string]time.Time{},
	})
	return p
}

// TrackTenant registers a tenant whose accounts should be included in refreshes.
func (p *Provider) TrackTenant(tenantID string) {
	tenantID = strings.TrimSpace(tenantID)
	if p == nil || tenantID == "" {
		return
	}
	p.tenantsMu.Lock()
	p.tenants[tenantID] = struct{}{}
	p.tenantsMu.Unlock()
}

// QuotaLoadRatio implements coreauth.QuotaLoadSource.
func (p *Provider) QuotaLoadRatio(auth *coreauth.Auth) (float64, bool) {
	if p == nil || auth == nil {
		return 0, false
	}
	key := loadKey(auth.TenantID, auth.Index)
	if key == "" {
		return 0, false
	}

	current := p.current.Load()
	if current == nil {
		p.triggerRefresh()
		return 0, false
	}
	if time.Since(current.refreshedAt) > snapshotTTL {
		p.triggerRefresh()
	}
	if state, ok := current.accounts[key]; ok {
		return state.effectiveLoad(time.Now()), true
	}
	ratio, ok := current.ratios[key]
	if !ok {
		return 0, false
	}
	return applyResetDiscount(key, ratio, current.resets), true
}

// QuotaResetState implements coreauth.QuotaResetUrgencySource.
func (p *Provider) QuotaResetState(auth *coreauth.Auth) (remaining float64, urgency float64, ok bool) {
	if p == nil || auth == nil {
		return 0, 0, false
	}
	key := loadKey(auth.TenantID, auth.Index)
	if key == "" {
		return 0, 0, false
	}

	current := p.current.Load()
	if current == nil {
		p.triggerRefresh()
		return 0, 0, false
	}
	if time.Since(current.refreshedAt) > snapshotTTL {
		p.triggerRefresh()
	}
	if state, found := current.accounts[key]; found {
		rem, urg := state.resetState(time.Now())
		return rem, urg, true
	}
	ratio, found := current.ratios[key]
	if !found {
		return 0, 0, false
	}
	rem := clampUnit(1 - ratio)
	urg := clampUnit(1 - resetDiscountFactor(key, current.resets))
	return rem, urg, true
}

func (p *Provider) triggerRefresh() {
	if !p.refreshing.CompareAndSwap(false, true) {
		return
	}
	go func() {
		defer p.refreshing.Store(false)
		p.Refresh()
	}()
}

// Refresh rebuilds the snapshot synchronously. Exported for startup priming and
// for tests that need deterministic timing.
func (p *Provider) Refresh() {
	if p == nil {
		return
	}
	p.tenantsMu.RLock()
	tenants := make([]string, 0, len(p.tenants))
	for tenantID := range p.tenants {
		tenants = append(tenants, tenantID)
	}
	p.tenantsMu.RUnlock()

	accounts := make(map[string]accountQuotaState)
	ratios := make(map[string]float64)
	resets := make(map[string]time.Time)
	now := time.Now()
	for _, tenantID := range tenants {
		records, err := usage.ListAIAccountStatusForTenant(tenantID, nil)
		if err != nil {
			// A missing or unreachable store must not break scheduling; the
			// selectors fall back to their in-process load signals.
			log.Debugf("schedulingload: list account status for tenant %s: %v", tenantID, err)
			continue
		}
		for _, record := range records {
			key := loadKey(tenantID, record.AuthIndex)
			if key == "" {
				continue
			}
			if state, ok := summarizeQuotaWindows(record.Quotas, now); ok {
				accounts[key] = state
				ratios[key] = state.rawWorstUsed()
				if state.hasLongReset {
					resets[key] = state.longResetAt
				}
			}
		}
	}
	p.current.Store(&snapshot{
		accounts:    accounts,
		ratios:      ratios,
		resets:      resets,
		refreshedAt: time.Now(),
	})
}

func isAuxiliaryQuotaWindow(quotaKey string) bool {
	k := strings.ToLower(strings.TrimSpace(quotaKey))
	return strings.HasPrefix(k, "review_") || strings.HasPrefix(k, "additional:") || k == "iguana_necktie"
}

func isShortQuotaWindow(windowSeconds int64) bool {
	return windowSeconds > 0 && windowSeconds < usage.WeeklyQuotaWindowSeconds
}

func clampUnit(v float64) float64 {
	if math.IsNaN(v) || v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}

// usedRatioFromRemainingPercent converts a QuotaWindowDTO.Percent value
// (remaining percentage in [0,100], where 100 means 100% remaining / 0% used)
// into a consumed load ratio in [0,1].
func usedRatioFromRemainingPercent(remainingPct float64) (float64, bool) {
	if math.IsNaN(remainingPct) || remainingPct < 0 {
		return 0, false
	}
	if remainingPct > 100 {
		remainingPct = 100
	}
	return clampUnit((100 - remainingPct) / 100), true
}

// summarizeQuotaWindows separates short burst windows (e.g. 5-hour limits) from
// long windows (weekly limits) and converts remaining percentages into used
// load ratios. Short-window load is never discounted by an upcoming weekly
// reset so a nearly-spent 5-hour window still sheds traffic.
func summarizeQuotaWindows(windows []usage.QuotaWindowDTO, now time.Time) (accountQuotaState, bool) {
	var state accountQuotaState
	hasPrimary := false
	for _, w := range windows {
		if w.Percent != nil && !isAuxiliaryQuotaWindow(w.QuotaKey) {
			hasPrimary = true
			break
		}
	}

	found := false
	for _, window := range windows {
		if hasPrimary && isAuxiliaryQuotaWindow(window.QuotaKey) {
			continue
		}
		shortWindow := isShortQuotaWindow(window.WindowSeconds)
		if !shortWindow && window.ResetAt != nil && window.ResetAt.After(now) {
			if !state.hasLongReset || window.ResetAt.Before(state.longResetAt) {
				state.longResetAt = *window.ResetAt
				state.hasLongReset = true
			}
		}
		if window.Percent == nil {
			continue
		}
		used, ok := usedRatioFromRemainingPercent(*window.Percent)
		if !ok {
			continue
		}
		found = true
		if shortWindow {
			if !state.hasShort || used > state.shortUsed {
				state.shortUsed = used
				state.hasShort = true
			}
		} else {
			if !state.hasLong || used > state.longUsed {
				state.longUsed = used
				state.hasLong = true
			}
		}
	}
	return state, found
}

func (s accountQuotaState) rawWorstUsed() float64 {
	worst := 0.0
	if s.hasShort && s.shortUsed > worst {
		worst = s.shortUsed
	}
	if s.hasLong && s.longUsed > worst {
		worst = s.longUsed
	}
	return worst
}

func (s accountQuotaState) effectiveLoad(now time.Time) float64 {
	load := 0.0
	if s.hasShort {
		load = s.shortUsed
	}
	if s.hasLong {
		discount := 1.0
		if s.hasLongReset {
			discount = resetDiscountFactorAt(s.longResetAt, now)
		}
		if discounted := s.longUsed * discount; discounted > load {
			load = discounted
		}
	}
	return clampUnit(load)
}

func (s accountQuotaState) resetState(now time.Time) (remaining float64, urgency float64) {
	remaining = 1.0
	if s.hasLong {
		remaining = clampUnit(1 - s.longUsed)
	}
	if s.hasShort {
		shortRem := clampUnit(1 - s.shortUsed)
		if !s.hasLong || shortRem < remaining {
			remaining = shortRem
		}
	}
	if s.hasLong && s.hasLongReset {
		urgency = clampUnit(1 - resetDiscountFactorAt(s.longResetAt, now))
	}
	return remaining, urgency
}

// worstQuotaRatio takes the most constrained window for the account. An account
// whose 5-hour window is nearly spent should shed traffic even when its weekly
// window still looks healthy.
func worstQuotaRatio(windows []usage.QuotaWindowDTO) (float64, bool) {
	state, ok := summarizeQuotaWindows(windows, time.Now())
	if !ok {
		return 0, false
	}
	return state.rawWorstUsed(), true
}

// soonestReset returns the earliest upcoming long-window reset across the
// account's windows. Short burst windows (such as 5-hour limits) are ignored
// so their rolling reset does not disguise a multi-day weekly window.
func soonestReset(windows []usage.QuotaWindowDTO, now time.Time) (time.Time, bool) {
	state, _ := summarizeQuotaWindows(windows, now)
	if state.hasLongReset {
		return state.longResetAt, true
	}
	return time.Time{}, false
}

// Reset-discount tuning for long (weekly) windows. Within fullBurnWindow of a
// reset the remaining quota is about to be wiped, so the seat should burn it
// freely rather than shed load. From fullBurnWindow out to discountHorizon the
// discount fades linearly back to zero, so a seat whose reset is still days
// away is protected as usual.
const (
	fullBurnWindow  = 6 * time.Hour
	discountHorizon = 48 * time.Hour
)

func resetDiscountFactorAt(reset, now time.Time) float64 {
	if reset.IsZero() {
		return 1
	}
	until := reset.Sub(now)
	if until <= fullBurnWindow {
		return 0
	}
	if until >= discountHorizon {
		return 1
	}
	return float64(until-fullBurnWindow) / float64(discountHorizon-fullBurnWindow)
}

// resetDiscountFactor scales a quota load ratio down as the account's soonest
// long-window reset approaches. It returns 1 when no reset is known.
func resetDiscountFactor(key string, resets map[string]time.Time) float64 {
	reset, ok := resets[key]
	if !ok {
		return 1
	}
	return resetDiscountFactorAt(reset, time.Now())
}

func applyResetDiscount(key string, ratio float64, resets map[string]time.Time) float64 {
	return ratio * resetDiscountFactor(key, resets)
}

func loadKey(tenantID, authIndex string) string {
	authIndex = strings.TrimSpace(authIndex)
	if authIndex == "" {
		return ""
	}
	return coreauth.NormalizedTenantID(tenantID) + "|" + authIndex
}
