package auth

import (
	"errors"
	"sync"
)

var (
	// ErrAccountConcurrencyExceeded is returned when an auth credential has reached its max concurrency limit.
	ErrAccountConcurrencyExceeded = errors.New("account concurrency limit exceeded")
)

// AccountConcurrencyLimiter manages in-flight request slots per auth ID.
// It supports querying active load, checking availability, acquiring slots, and releasing slots.
//
// Saturated accounts do not fail immediately: callers may queue through
// AcquireSlotWait, and ReleaseSlot hands a freed slot straight to the
// longest-waiting caller (see account_concurrency_queue.go).
type AccountConcurrencyLimiter struct {
	mu     sync.RWMutex
	active map[string]int
	// waiters holds the FIFO queue of callers blocked on each auth ID.
	waiters map[string][]*slotWaiter
}

// NewAccountConcurrencyLimiter creates a new thread-safe concurrency limiter.
func NewAccountConcurrencyLimiter() *AccountConcurrencyLimiter {
	return &AccountConcurrencyLimiter{
		active:  make(map[string]int),
		waiters: make(map[string][]*slotWaiter),
	}
}

// GetInFlight returns the number of active requests for the specified auth ID.
func (l *AccountConcurrencyLimiter) GetInFlight(authID string) int {
	if l == nil || authID == "" {
		return 0
	}
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.active[authID]
}

// GetLoadRate returns the load ratio (in-flight / maxLimit) between 0.0 and 1.0 (or >1.0 if overloaded).
// If limit <= 0 (unlimited), it returns 0.0.
func (l *AccountConcurrencyLimiter) GetLoadRate(authID string, maxLimit int) float64 {
	if maxLimit <= 0 {
		return 0.0
	}
	inFlight := l.GetInFlight(authID)
	return float64(inFlight) / float64(maxLimit)
}

// HasAvailableSlot checks whether an auth candidate can accept another request.
// If limit <= 0, concurrency is considered unlimited and returns true.
func (l *AccountConcurrencyLimiter) HasAvailableSlot(auth *Auth) bool {
	if l == nil || auth == nil {
		return true
	}
	limit := auth.ConcurrencyLimit()
	if limit <= 0 {
		return true
	}
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.active[auth.ID] < limit
}

// AcquireSlot attempts to acquire a concurrency slot for the given auth without waiting.
// Returns a release function and an error. If acquired, the caller MUST call the returned release function.
// If the limit is reached, it returns nil and an *AccountConcurrencyError wrapping ErrAccountConcurrencyExceeded.
func (l *AccountConcurrencyLimiter) AcquireSlot(auth *Auth) (func(), error) {
	if l == nil || auth == nil {
		return func() {}, nil
	}

	l.mu.Lock()
	if !l.tryAcquireLocked(auth) {
		err := l.saturatedErrorLocked(auth, 0)
		l.mu.Unlock()
		return nil, err
	}
	l.mu.Unlock()

	return l.releaseFunc(auth.ID), nil
}

// ReleaseSlot decrements the in-flight count for the given auth ID.
//
// When callers are queued on this account the slot is transferred to the
// longest-waiting one instead of being released and re-contended: without the
// handoff a burst of fresh requests could keep overtaking a queued caller.
func (l *AccountConcurrencyLimiter) ReleaseSlot(authID string) {
	if l == nil || authID == "" {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()

	current := l.active[authID]
	if current <= 0 {
		return
	}
	// Handing the slot over keeps the in-flight count unchanged, so it is only
	// safe while that count still fits the account's current limit. A limit
	// lowered mid-flight must drain instead, otherwise the account would stay
	// above its new ceiling for as long as requests keep queuing.
	for {
		waiter := l.peekWaiterLocked(authID)
		if waiter == nil {
			break
		}
		if limit := waiter.limitFor(authID); limit > 0 && current > limit {
			break
		}
		l.popWaiterLocked(authID)
		if waiter.tryGrant(authID) {
			return
		}
	}

	if current <= 1 {
		delete(l.active, authID)
	} else {
		l.active[authID] = current - 1
	}
}

// FilterAvailableCandidates partitions candidates into available vs saturated by concurrency limit.
// Candidates with reached limits (in-flight >= limit > 0) are filtered out if viable alternatives exist.
func (l *AccountConcurrencyLimiter) FilterAvailableCandidates(candidates []*Auth) []*Auth {
	if l == nil || len(candidates) <= 1 {
		return candidates
	}

	available := make([]*Auth, 0, len(candidates))

	l.mu.RLock()
	for _, c := range candidates {
		if c == nil {
			continue
		}
		limit := c.ConcurrencyLimit()
		inFlight := l.active[c.ID]
		if limit <= 0 || inFlight < limit {
			available = append(available, c)
		}
	}
	l.mu.RUnlock()

	if len(available) > 0 {
		return available
	}
	// If all candidates are saturated, return all candidates so selector can fail or attempt fallback
	return candidates
}

// tryAcquireLocked takes a slot when the account has room. Callers must hold l.mu.
func (l *AccountConcurrencyLimiter) tryAcquireLocked(auth *Auth) bool {
	if auth == nil || auth.ID == "" {
		return false
	}
	limit := auth.ConcurrencyLimit()
	if limit > 0 && l.active[auth.ID] >= limit {
		return false
	}
	l.active[auth.ID]++
	return true
}

// releaseFunc builds an idempotent release closure for an owned slot.
func (l *AccountConcurrencyLimiter) releaseFunc(authID string) func() {
	var once sync.Once
	return func() {
		once.Do(func() {
			l.ReleaseSlot(authID)
		})
	}
}
