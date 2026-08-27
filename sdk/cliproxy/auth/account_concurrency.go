package auth

import (
	"errors"
	"fmt"
	"sync"
)

var (
	// ErrAccountConcurrencyExceeded is returned when an auth credential has reached its max concurrency limit.
	ErrAccountConcurrencyExceeded = errors.New("account concurrency limit exceeded")
)

// AccountConcurrencyLimiter manages in-flight request slots per auth ID.
// It supports querying active load, checking availability, acquiring slots, and releasing slots.
type AccountConcurrencyLimiter struct {
	mu     sync.RWMutex
	active map[string]int
}

// NewAccountConcurrencyLimiter creates a new thread-safe concurrency limiter.
func NewAccountConcurrencyLimiter() *AccountConcurrencyLimiter {
	return &AccountConcurrencyLimiter{
		active: make(map[string]int),
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

// AcquireSlot attempts to acquire a concurrency slot for the given auth.
// Returns a release function and an error. If acquired, the caller MUST call the returned release function.
// If the limit is reached, it returns nil and ErrAccountConcurrencyExceeded.
func (l *AccountConcurrencyLimiter) AcquireSlot(auth *Auth) (func(), error) {
	if l == nil || auth == nil {
		return func() {}, nil
	}
	authID := auth.ID
	limit := auth.ConcurrencyLimit()

	l.mu.Lock()
	if limit > 0 {
		current := l.active[authID]
		if current >= limit {
			l.mu.Unlock()
			return nil, fmt.Errorf("%w: account %s active=%d max=%d", ErrAccountConcurrencyExceeded, authID, current, limit)
		}
	}
	l.active[authID]++
	l.mu.Unlock()

	var once sync.Once
	release := func() {
		once.Do(func() {
			l.ReleaseSlot(authID)
		})
	}
	return release, nil
}

// ReleaseSlot decrements the in-flight count for the given auth ID.
func (l *AccountConcurrencyLimiter) ReleaseSlot(authID string) {
	if l == nil || authID == "" {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	current := l.active[authID]
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
