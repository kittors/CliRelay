package auth

import (
	"sync/atomic"
	"time"
)

type schedulerDeps struct {
	tracker  *selectionPressureTracker
	limiter  *AccountConcurrencyLimiter
	quota    QuotaLoadSource
	affinity atomic.Pointer[sessionAffinityRef]
}

func (d *schedulerDeps) observeSelection(auth *Auth, now time.Time) {
	if d == nil || auth == nil {
		return
	}
	if d.tracker != nil {
		d.tracker.observe(auth.ID, now)
	}
}
func (d *schedulerDeps) pressure(auth *Auth, now time.Time) float64 {
	if d == nil || d.tracker == nil || auth == nil {
		return 0
	}
	return d.tracker.pressure(auth.ID, now)
}
func (d *schedulerDeps) loadRatio(auth *Auth, model string) float64 {
	if d == nil {
		return 0
	}
	return accountLoadRatio(auth, model, d.limiter, d.quota)
}

func (d *schedulerDeps) quotaRatio(auth *Auth) (float64, bool) {
	if d == nil || d.quota == nil || auth == nil {
		return 0, false
	}
	source, ok := d.quota.(QuotaLoadSource)
	if !ok {
		return 0, false
	}
	return source.QuotaLoadRatio(auth)
}
func (d *schedulerDeps) quotaWindows() QuotaWindowSource {
	if d == nil {
		return nil
	}
	q, _ := d.quota.(QuotaWindowSource)
	return q
}
