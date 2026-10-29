package auth

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

// Live quota observation.
//
// An executor that reads quota usage off ordinary upstream answers reports it
// through cliproxyexecutor.ObserveQuota. Two things follow from it here:
//
//   - A window at 100% parks the model until the window resets, the same state a
//     refusal carrying the reset time would have produced, minus the refused
//     request. Anything below 100% changes nothing about scheduling: usage is
//     information, not a routing weight.
//   - The windows are handed to the host's QuotaObservationHook so the console
//     can show them. Answers arrive far faster than the console needs them, so
//     writes are coalesced per account.

const (
	// quotaObservationMinInterval is the fastest one account's observed windows
	// are handed to the hook. Changes that arrive sooner are held and only the
	// newest is written.
	quotaObservationMinInterval = 20 * time.Second
	// quotaObservationRefreshInterval is how long an unchanged observation is
	// ignored before it is written again, which keeps the "last confirmed" time
	// the console shows moving without a write per request.
	quotaObservationRefreshInterval = 5 * time.Minute
	// observedQuotaMaxHorizon is how far ahead a reset may be and still be
	// believed. No allowance resets further out than this, so a later time is a
	// malformed answer, not a reason to park an account for months.
	observedQuotaMaxHorizon = 35 * 24 * time.Hour

	// observedQuotaExhaustedCode marks the error recorded on a model parked by an
	// observation, to tell it from a refusal the upstream actually sent.
	observedQuotaExhaustedCode = "quota_window_exhausted"
)

// QuotaObservationHook is an optional Hook extension. The manager calls it, off
// the request path and at most once per quotaObservationMinInterval per account,
// with the newest quota windows an executor observed. The auth is a copy.
type QuotaObservationHook interface {
	OnQuotaObserved(ctx context.Context, auth *Auth, observation cliproxyexecutor.QuotaObservation)
}

// quotaObserverFor binds an observer to one attempt: the account it runs on and
// the model it was routed as, which is the key every result of that attempt is
// recorded under.
func (m *Manager) quotaObserverFor(authID, routeModel string) cliproxyexecutor.QuotaObserver {
	authID = strings.TrimSpace(authID)
	if m == nil || authID == "" {
		return nil
	}
	return func(observation cliproxyexecutor.QuotaObservation) {
		m.observeQuota(authID, routeModel, observation)
	}
}

func (m *Manager) observeQuota(authID, routeModel string, observation cliproxyexecutor.QuotaObservation) {
	if m == nil || len(observation.Windows) == 0 {
		return
	}
	now := time.Now()
	if observation.ObservedAt.IsZero() {
		observation.ObservedAt = now
	}
	if gate, ok := observedQuotaGateFor(observation, now); ok && routeModel != "" {
		m.gateOnObservedQuota(authID, routeModel, gate, now)
	}
	m.queueQuotaObservation(authID, observation, now)
}

// observedQuotaGate is the cooldown an observation justifies.
type observedQuotaGate struct {
	until         time.Time
	window        string
	windowMinutes int
}

// observedQuotaGateFor decides whether an observation means the account cannot
// serve until some window resets.
//
// The account is out until the last exhausted window resets, so that window sets
// the deadline. Anything the answer leaves unclear declines to gate rather than
// guess: a refusal from the upstream will still arrive and carry the facts.
func observedQuotaGateFor(observation cliproxyexecutor.QuotaObservation, now time.Time) (observedQuotaGate, bool) {
	if observation.OverageAvailable {
		return observedQuotaGate{}, false
	}
	var gate observedQuotaGate
	for _, window := range observation.Windows {
		if window.UsedPercent < 100 {
			continue
		}
		if window.ResetAt.IsZero() {
			// Exhausted with no way to say for how long.
			return observedQuotaGate{}, false
		}
		if !window.ResetAt.After(now) {
			// Already past its own reset: the percentage is stale, not a block.
			continue
		}
		if window.ResetAt.After(now.Add(observedQuotaMaxHorizon)) {
			return observedQuotaGate{}, false
		}
		if window.ResetAt.After(gate.until) {
			gate = observedQuotaGate{until: window.ResetAt, window: window.Label, windowMinutes: window.WindowMinutes}
		}
	}
	return gate, !gate.until.IsZero()
}

func (m *Manager) gateOnObservedQuota(authID, routeModel string, gate observedQuotaGate, now time.Time) {
	var (
		notice      *CooldownNotice
		applied     bool
		registryRef ModelRegistry
	)
	m.mu.Lock()
	if auth := m.auths[authID]; auth != nil && !auth.Disabled && auth.Status != StatusDisabled {
		applied = applyObservedQuotaGateLocked(auth, routeModel, gate, now)
		if applied && m.cooldownPublishing() {
			notice = &CooldownNotice{
				AuthID:     authID,
				Model:      routeModel,
				Until:      gate.until,
				StatusCode: http.StatusTooManyRequests,
				Reason:     "quota",
				Quota:      true,
			}
		}
	}
	registryRef = m.modelRegistry
	m.mu.Unlock()

	if !applied {
		return
	}
	// The same bookkeeping a refused request would have caused, so everything that
	// reads model availability from the registry agrees with the scheduler.
	if registryRef != nil {
		registryRef.SetModelQuotaExceeded(authID, routeModel)
		registryRef.SuspendClientModel(authID, routeModel, "quota")
	}
	m.publishCooldown(notice)
}

// applyObservedQuotaGateLocked parks routeModel until gate.until and reports
// whether it changed anything. Callers must hold m.mu.
//
// The state has the shape applyModelQuotaFailureLocked gives a refusal that
// carries a reset time, and like it ignores disable-cooling: that switch only
// governs backoffs this package computes itself, never a reset the upstream
// announced (see quotaFailureCooldown). It is not a probe-confirmed gate, so a
// success does not clear it (applySuccessLocked keeps any live quota cooldown):
// the request that carried the observation was itself such a success.
func applyObservedQuotaGateLocked(auth *Auth, routeModel string, gate observedQuotaGate, now time.Time) bool {
	if existing := auth.ModelStates[routeModel]; existing != nil {
		if existing.Status == StatusDisabled {
			return false
		}
		// A probe-confirmed gate lifts only on a probe's word and tracks the
		// probe's own deadline; a window read off a request must not move it.
		if quotaRecoveryGateArmed(existing.Quota) {
			return false
		}
	}
	// Only ever extend: a longer cooldown already in force stays.
	if current, _ := cooldownDeadline(auth, routeModel, now); !gate.until.After(current) {
		return false
	}

	label := gate.window
	if label == "" {
		label = "quota"
	}
	message := fmt.Sprintf("%s window is fully used; resets at %s", label, gate.until.UTC().Format(time.RFC3339))
	failure := &Error{
		Code:               observedQuotaExhaustedCode,
		Message:            message,
		Retryable:          true,
		HTTPStatus:         http.StatusTooManyRequests,
		QuotaWindow:        gate.window,
		QuotaWindowMinutes: gate.windowMinutes,
	}
	state := ensureModelState(auth, routeModel)
	state.Unavailable = true
	state.Status = StatusError
	state.StatusMessage = message
	state.NextRetryAfter = gate.until
	state.LastError = cloneError(failure)
	state.Quota = QuotaState{
		Exceeded:      true,
		Reason:        "quota",
		Window:        gate.window,
		WindowMinutes: gate.windowMinutes,
		NextRecoverAt: gate.until,
		BackoffLevel:  state.Quota.BackoffLevel,
	}
	state.UpdatedAt = now
	auth.LastError = cloneError(failure)
	auth.StatusMessage = message
	auth.Status = StatusError
	auth.UpdatedAt = now
	updateAggregatedAvailability(auth, now)
	return true
}

// queueQuotaObservation hands the windows to the hook now, later, or never,
// according to quotaObservationBook.
func (m *Manager) queueQuotaObservation(authID string, observation cliproxyexecutor.QuotaObservation, now time.Time) {
	action, delay := m.quotaObservations.offer(authID, observation, now)
	switch action {
	case quotaObservationWriteNow:
		// Off the request path: the write is a database round trip.
		go m.writeQuotaObservation(authID, observation)
	case quotaObservationHoldAndArm:
		time.AfterFunc(delay, func() { m.flushHeldQuotaObservation(authID) })
	}
}

func (m *Manager) flushHeldQuotaObservation(authID string) {
	if observation, ok := m.quotaObservations.take(authID, time.Now()); ok {
		m.writeQuotaObservation(authID, observation)
	}
}

func (m *Manager) writeQuotaObservation(authID string, observation cliproxyexecutor.QuotaObservation) {
	m.mu.RLock()
	current := m.auths[authID]
	var snapshot *Auth
	if current != nil {
		snapshot = current.Clone()
	}
	hook := m.hook
	m.mu.RUnlock()
	if snapshot == nil {
		return
	}
	if observer, ok := hook.(QuotaObservationHook); ok {
		observer.OnQuotaObserved(context.Background(), snapshot, observation)
	}
}

type quotaObservationAction int

const (
	// quotaObservationDrop: the stored snapshot already says this.
	quotaObservationDrop quotaObservationAction = iota
	// quotaObservationWriteNow: write it.
	quotaObservationWriteNow
	// quotaObservationHold: kept as the newest, an earlier call armed the timer.
	quotaObservationHold
	// quotaObservationHoldAndArm: kept as the newest; the caller must call take
	// after the returned delay.
	quotaObservationHoldAndArm
)

// quotaObservationBook remembers, per account, what was last handed to the hook
// and what is waiting for its turn. The zero value is ready to use.
type quotaObservationBook struct {
	mu      sync.Mutex
	entries map[string]*quotaObservationEntry
	// minInterval and refresh override the package defaults; tests shrink them.
	minInterval time.Duration
	refresh     time.Duration
}

type quotaObservationEntry struct {
	signature  string
	writtenAt  time.Time
	held       *cliproxyexecutor.QuotaObservation
	heldSig    string
	timerArmed bool
}

func (b *quotaObservationBook) intervals() (time.Duration, time.Duration) {
	minInterval, refresh := b.minInterval, b.refresh
	if minInterval <= 0 {
		minInterval = quotaObservationMinInterval
	}
	if refresh <= 0 {
		refresh = quotaObservationRefreshInterval
	}
	return minInterval, refresh
}

// offer decides what happens to an observation of one account's windows.
func (b *quotaObservationBook) offer(authID string, observation cliproxyexecutor.QuotaObservation, now time.Time) (quotaObservationAction, time.Duration) {
	signature := quotaObservationSignature(observation)
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.entries == nil {
		b.entries = make(map[string]*quotaObservationEntry)
	}
	entry := b.entries[authID]
	if entry == nil {
		entry = &quotaObservationEntry{}
		b.entries[authID] = entry
	}
	minInterval, refresh := b.intervals()
	sinceWrite := now.Sub(entry.writtenAt)

	if signature == entry.signature && sinceWrite < refresh {
		// What is stored already matches the newest answer, so a held change that
		// has since been undone has nothing left to write.
		entry.held = nil
		return quotaObservationDrop, 0
	}
	if sinceWrite >= minInterval {
		entry.signature, entry.writtenAt, entry.held = signature, now, nil
		return quotaObservationWriteNow, 0
	}
	held := observation
	entry.held, entry.heldSig = &held, signature
	if entry.timerArmed {
		return quotaObservationHold, 0
	}
	entry.timerArmed = true
	return quotaObservationHoldAndArm, minInterval - sinceWrite
}

// take returns the held observation, if any, and counts it as written.
func (b *quotaObservationBook) take(authID string, now time.Time) (cliproxyexecutor.QuotaObservation, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	entry := b.entries[authID]
	if entry == nil {
		return cliproxyexecutor.QuotaObservation{}, false
	}
	entry.timerArmed = false
	if entry.held == nil {
		return cliproxyexecutor.QuotaObservation{}, false
	}
	observation := *entry.held
	entry.signature, entry.writtenAt, entry.held = entry.heldSig, now, nil
	return observation, true
}

// quotaObservationSignature summarises what a write would change. Usage is
// compared as whole percent and the reset to the minute: finer differences are
// jitter (a reset derived from "seconds from now" moves every answer) that would
// turn every request into a write.
func quotaObservationSignature(observation cliproxyexecutor.QuotaObservation) string {
	parts := make([]string, 0, len(observation.Windows))
	for _, window := range observation.Windows {
		resetMinute := int64(0)
		if !window.ResetAt.IsZero() {
			resetMinute = window.ResetAt.Unix() / 60
		}
		parts = append(parts, fmt.Sprintf("%d/%d/%d", window.WindowMinutes, int(math.Round(window.UsedPercent)), resetMinute))
	}
	sort.Strings(parts)
	return strings.Join(parts, "|")
}
