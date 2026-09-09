package auth

import (
	"context"
	"fmt"
	"net/http"
	"sync/atomic"
	"time"
)

// DefaultAccountConcurrencyWait is the queue timeout applied when the host does
// not configure one. Queuing is what callers expect from a per-account limit:
// failing the request outright while the account is merely busy turns a short
// wait into a hard error for the client.
const DefaultAccountConcurrencyWait = 30 * time.Second

// AccountConcurrencyError reports that an auth credential is at its concurrency limit.
//
// It carries a 429 status on purpose: the account is healthy and the upstream was
// never contacted, so surfacing a 500 would tell clients the proxy is broken and
// suppress their own rate-limit backoff.
type AccountConcurrencyError struct {
	// AuthID identifies the saturated credential.
	AuthID string
	// Active is the in-flight count observed when the request gave up.
	Active int
	// Limit is the account's configured concurrency limit.
	Limit int
	// Waited is how long the request sat in the queue. Zero means queuing was
	// disabled, so the request never waited at all.
	Waited time.Duration
}

// Error implements the error interface.
func (e *AccountConcurrencyError) Error() string {
	if e == nil {
		return ""
	}
	msg := fmt.Sprintf("%s: account %s active=%d max=%d", ErrAccountConcurrencyExceeded.Error(), e.AuthID, e.Active, e.Limit)
	if e.Waited > 0 {
		msg += fmt.Sprintf(" waited=%s", e.Waited.Round(time.Millisecond))
	}
	return msg
}

// Unwrap keeps errors.Is(err, ErrAccountConcurrencyExceeded) working for callers
// that classify failures by sentinel.
func (e *AccountConcurrencyError) Unwrap() error { return ErrAccountConcurrencyExceeded }

// StatusCode reports the failure as a rate limit rather than a server fault.
func (e *AccountConcurrencyError) StatusCode() int { return http.StatusTooManyRequests }

// slotWaiter is a request queued for a slot on one or more accounts.
//
// A waiter may sit in several account queues at once so a multi-candidate request
// resumes as soon as any of them frees a slot; claimed makes sure exactly one
// account hands over a slot and that a timed-out waiter cannot also receive one.
type slotWaiter struct {
	granted chan string
	claimed atomic.Bool
	authIDs []string
	// auths keeps the credential behind each queue so a release can read the
	// account's current limit instead of the one cached when it was acquired.
	auths map[string]*Auth
}

func newSlotWaiter(auths []*Auth) *slotWaiter {
	waiter := &slotWaiter{
		granted: make(chan string, 1),
		authIDs: make([]string, 0, len(auths)),
		auths:   make(map[string]*Auth, len(auths)),
	}
	for _, auth := range auths {
		waiter.authIDs = append(waiter.authIDs, auth.ID)
		waiter.auths[auth.ID] = auth
	}
	return waiter
}

// limitFor reports the account's currently configured concurrency limit,
// 0 meaning unlimited.
func (w *slotWaiter) limitFor(authID string) int {
	if auth := w.auths[authID]; auth != nil {
		return auth.ConcurrencyLimit()
	}
	return 0
}

// tryGrant transfers ownership of an in-flight slot to this waiter. It returns
// false when the waiter already took a slot elsewhere or gave up, so the
// releasing account moves on to the next queued caller. Callers must hold l.mu.
func (w *slotWaiter) tryGrant(authID string) bool {
	if !w.claimed.CompareAndSwap(false, true) {
		return false
	}
	// Buffered by one and guarded by claimed, so this never blocks the releaser.
	w.granted <- authID
	return true
}

// abandon marks the waiter as gone. It returns false when a slot was handed over
// concurrently, in which case the caller MUST take that slot: dropping it would
// leak account capacity until the process restarts.
func (w *slotWaiter) abandon() bool {
	return w.claimed.CompareAndSwap(false, true)
}

// AcquireSlotWait acquires a slot for one of auths, queuing when all of them are
// saturated. It returns the release function and the auth ID that granted the
// slot; the caller MUST invoke the release function.
//
// Ordering is FIFO per account: ReleaseSlot hands the freed slot to the
// longest-waiting caller. When timeout is <= 0 queuing is disabled and the call
// degrades to a non-blocking attempt. maxQueueDepth caps queued callers per
// account (0 means unlimited) so an unreachable upstream cannot pile up requests
// without bound.
func (l *AccountConcurrencyLimiter) AcquireSlotWait(ctx context.Context, auths []*Auth, timeout time.Duration, maxQueueDepth int) (func(), string, error) {
	if l == nil {
		return func() {}, "", nil
	}
	candidates := make([]*Auth, 0, len(auths))
	for _, candidate := range auths {
		if candidate != nil && candidate.ID != "" {
			candidates = append(candidates, candidate)
		}
	}
	if len(candidates) == 0 {
		return nil, "", &AccountConcurrencyError{}
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, "", err
	}

	waiter, release, authID, err := l.acquireOrEnqueue(candidates, timeout, maxQueueDepth)
	if err != nil {
		return nil, "", err
	}
	if waiter == nil {
		return release, authID, nil
	}

	timer := time.NewTimer(timeout)
	defer timer.Stop()
	start := time.Now()

	select {
	case granted := <-waiter.granted:
		// Leave the queues of the accounts that did not serve us; a stale entry
		// would still count against their queue depth.
		l.dequeue(waiter)
		return l.releaseFunc(granted), granted, nil
	case <-ctx.Done():
		return l.giveUp(waiter, candidates, ctx.Err(), time.Since(start))
	case <-timer.C:
		return l.giveUp(waiter, candidates, nil, time.Since(start))
	}
}

// QueueDepth reports how many callers are queued for the given auth ID.
func (l *AccountConcurrencyLimiter) QueueDepth(authID string) int {
	if l == nil || authID == "" {
		return 0
	}
	l.mu.RLock()
	defer l.mu.RUnlock()
	return len(l.waiters[authID])
}

// acquireOrEnqueue takes a free slot when one exists, otherwise enqueues a waiter.
// Both happen under a single lock hold so a slot released in between cannot be
// missed, which would otherwise make the caller wait for the *next* release.
func (l *AccountConcurrencyLimiter) acquireOrEnqueue(candidates []*Auth, timeout time.Duration, maxQueueDepth int) (*slotWaiter, func(), string, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	for _, candidate := range candidates {
		if l.tryAcquireLocked(candidate) {
			return nil, l.releaseFunc(candidate.ID), candidate.ID, nil
		}
	}
	if timeout <= 0 {
		return nil, nil, "", l.saturatedErrorLocked(candidates[0], 0)
	}

	queueable := candidates
	if maxQueueDepth > 0 {
		queueable = make([]*Auth, 0, len(candidates))
		for _, candidate := range candidates {
			if len(l.waiters[candidate.ID]) < maxQueueDepth {
				queueable = append(queueable, candidate)
			}
		}
		if len(queueable) == 0 {
			return nil, nil, "", l.saturatedErrorLocked(candidates[0], 0)
		}
	}

	waiter := newSlotWaiter(queueable)
	for _, authID := range waiter.authIDs {
		l.waiters[authID] = append(l.waiters[authID], waiter)
	}
	return waiter, nil, "", nil
}

// giveUp abandons a queued waiter after a timeout or context cancellation.
func (l *AccountConcurrencyLimiter) giveUp(waiter *slotWaiter, candidates []*Auth, cause error, waited time.Duration) (func(), string, error) {
	if !waiter.abandon() {
		// A slot was handed over at the same moment we gave up. Take it rather
		// than let it leak; the caller still gets a usable slot.
		granted := <-waiter.granted
		return l.releaseFunc(granted), granted, nil
	}
	l.dequeue(waiter)
	if cause != nil {
		return nil, "", cause
	}
	l.mu.Lock()
	err := l.saturatedErrorLocked(candidates[0], waited)
	l.mu.Unlock()
	return nil, "", err
}

// dequeue drops an abandoned waiter from every queue it joined.
func (l *AccountConcurrencyLimiter) dequeue(waiter *slotWaiter) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, authID := range waiter.authIDs {
		queue := l.waiters[authID]
		for i, queued := range queue {
			if queued == waiter {
				queue = append(queue[:i], queue[i+1:]...)
				break
			}
		}
		if len(queue) == 0 {
			delete(l.waiters, authID)
		} else {
			l.waiters[authID] = queue
		}
	}
}

// peekWaiterLocked returns the longest-waiting caller for an auth ID without
// dequeuing it, so a release can check the account's limit before committing to
// a handoff. Callers must hold l.mu.
func (l *AccountConcurrencyLimiter) peekWaiterLocked(authID string) *slotWaiter {
	queue := l.waiters[authID]
	if len(queue) == 0 {
		return nil
	}
	return queue[0]
}

// popWaiterLocked removes and returns the longest-waiting caller for an auth ID.
// Callers must hold l.mu.
func (l *AccountConcurrencyLimiter) popWaiterLocked(authID string) *slotWaiter {
	queue := l.waiters[authID]
	if len(queue) == 0 {
		return nil
	}
	waiter := queue[0]
	if len(queue) == 1 {
		delete(l.waiters, authID)
	} else {
		queue[0] = nil // drop the reference so a long queue cannot pin dead waiters
		l.waiters[authID] = queue[1:]
	}
	return waiter
}

// saturatedErrorLocked builds the error reported when no slot could be obtained.
// Callers must hold l.mu.
func (l *AccountConcurrencyLimiter) saturatedErrorLocked(auth *Auth, waited time.Duration) *AccountConcurrencyError {
	if auth == nil {
		return &AccountConcurrencyError{Waited: waited}
	}
	return &AccountConcurrencyError{
		AuthID: auth.ID,
		Active: l.active[auth.ID],
		Limit:  auth.ConcurrencyLimit(),
		Waited: waited,
	}
}
