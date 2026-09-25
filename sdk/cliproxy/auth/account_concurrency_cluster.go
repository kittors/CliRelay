package auth

import (
	"context"
	"math/rand/v2"
	"time"
)

// AccountSlotCoordinator extends per-account concurrency limits across the
// processes that serve the same credentials. Without one, each process
// enforces an account's limit on its own, so N processes can run N times the
// limit against the upstream account.
//
// The limiter keeps its local bookkeeping (queues, handoffs, load figures)
// either way; the coordinator only decides whether a new slot may be taken.
// A slot handed from a finished request to a queued one on the same process
// stays taken, so handoffs never reach the coordinator.
type AccountSlotCoordinator interface {
	// Shared reports whether cluster-wide slot counting is usable right now.
	Shared() bool
	// NodeShare returns the limit one process enforces on its own while
	// cluster-wide counting is unavailable, typically ceil(limit/processes).
	NodeShare(limit int) int
	// Acquire takes one cluster-wide slot on authID when fewer than limit are
	// held. A non-nil error means the count could not be reached; the limiter
	// then falls back to NodeShare for that request.
	Acquire(authID string, limit int) (bool, error)
	// Release gives back one slot taken by Acquire. It is called with the
	// limiter's lock held and must not block.
	Release(authID string)
}

type accountSlotCoordinatorRef struct{ coordinator AccountSlotCoordinator }

// clusterSlotPollInterval is how often a request queued in cluster mode
// retries on its own. Local releases hand slots over directly, but a slot
// freed on another process produces no local event, so without polling a
// request queued behind other processes would only ever time out.
const clusterSlotPollInterval = 200 * time.Millisecond

// SetAccountSlotCoordinator makes per-account concurrency limits count across
// processes. nil restores per-process limits. Hosts set it before serving
// traffic.
func (m *Manager) SetAccountSlotCoordinator(c AccountSlotCoordinator) {
	if m == nil || m.concurrencyLimiter == nil {
		return
	}
	m.concurrencyLimiter.SetCoordinator(c)
}

// SetCoordinator installs c; see Manager.SetAccountSlotCoordinator.
func (l *AccountConcurrencyLimiter) SetCoordinator(c AccountSlotCoordinator) {
	if l == nil {
		return
	}
	if c == nil {
		l.coordinator.Store(nil)
		return
	}
	l.coordinator.Store(&accountSlotCoordinatorRef{coordinator: c})
}

func (l *AccountConcurrencyLimiter) coord() AccountSlotCoordinator {
	if l == nil {
		return nil
	}
	if ref := l.coordinator.Load(); ref != nil {
		return ref.coordinator
	}
	return nil
}

// effectiveLimit is the limit this process enforces locally: the configured
// one, or this process's share of it while cluster-wide counting is down.
func (l *AccountConcurrencyLimiter) effectiveLimit(limit int) int {
	if limit <= 0 {
		return limit
	}
	if c := l.coord(); c != nil && !c.Shared() {
		if share := c.NodeShare(limit); share > 0 {
			return share
		}
	}
	return limit
}

// acquireSlotCluster is AcquireSlot when a coordinator is installed.
func (l *AccountConcurrencyLimiter) acquireSlotCluster(auth *Auth) (func(), error) {
	if authID, ok := l.tryAcquireAny([]*Auth{auth}); ok {
		return l.releaseFunc(authID), nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return nil, l.saturatedErrorLocked(auth, 0)
}

// tryAcquireAny takes a slot on the first candidate that has room on this
// process and, when counted cluster-wide, across the cluster.
//
// The local reservation and the cluster check are separate steps because the
// cluster check is a network call and must never run under the limiter's
// lock, which every account on this process shares.
func (l *AccountConcurrencyLimiter) tryAcquireAny(candidates []*Auth) (string, bool) {
	for _, candidate := range candidates {
		if candidate == nil || candidate.ID == "" {
			continue
		}
		l.mu.Lock()
		reserved := l.tryAcquireLocked(candidate)
		l.mu.Unlock()
		if !reserved {
			continue
		}
		if l.claimClusterSlot(candidate) {
			return candidate.ID, true
		}
	}
	return "", false
}

// claimClusterSlot backs a local reservation with a cluster-wide slot. When
// the cluster is at the limit it undoes the reservation and returns false.
func (l *AccountConcurrencyLimiter) claimClusterSlot(auth *Auth) bool {
	c := l.coord()
	limit := auth.ConcurrencyLimit()
	if c == nil || limit <= 0 {
		return true
	}
	if c.Shared() {
		ok, err := c.Acquire(auth.ID, limit)
		if err == nil {
			l.mu.Lock()
			if ok {
				l.clustered[auth.ID]++
			} else {
				l.unreserveLocked(auth.ID)
			}
			l.mu.Unlock()
			return ok
		}
	}
	// Counting falls back to this process's share. The reservation may have
	// been checked against the full limit just before the cluster count
	// became unreachable, so check it against the share now.
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.active[auth.ID] > c.NodeShare(limit) {
		l.unreserveLocked(auth.ID)
		return false
	}
	l.unshared[auth.ID]++
	return true
}

// unreserveLocked drops a reservation that never became a slot. Unlike
// ReleaseSlot it never hands anything to a queued caller: there was no slot to
// hand over. Callers must hold l.mu.
func (l *AccountConcurrencyLimiter) unreserveLocked(authID string) {
	if current := l.active[authID]; current <= 1 {
		delete(l.active, authID)
	} else {
		l.active[authID] = current - 1
	}
}

// releaseClusterSlotLocked gives back the cluster-wide side of a slot this
// process is freeing. Slots are fungible, so which request held which kind is
// unknown; slots taken while the cluster count was unreachable are given back
// first. They never held a cluster slot, and releasing one on their behalf
// would under-count a request still running. A slot of an account without a
// limit held neither and releases nothing. Callers must hold l.mu.
func (l *AccountConcurrencyLimiter) releaseClusterSlotLocked(authID string) {
	c := l.coord()
	if c == nil {
		return
	}
	if decrementCount(l.unshared, authID) {
		return
	}
	if decrementCount(l.clustered, authID) {
		c.Release(authID)
	}
}

func decrementCount(counts map[string]int, key string) bool {
	n := counts[key]
	switch {
	case n <= 0:
		return false
	case n == 1:
		delete(counts, key)
	default:
		counts[key] = n - 1
	}
	return true
}

// acquireSlotWaitCluster is AcquireSlotWait when a coordinator is installed.
// Queued callers still receive slots released on this process through the
// FIFO handoff, and additionally retry on their own for slots released
// elsewhere in the cluster.
func (l *AccountConcurrencyLimiter) acquireSlotWaitCluster(ctx context.Context, candidates []*Auth, timeout time.Duration, maxQueueDepth int) (func(), string, error) {
	if authID, ok := l.tryAcquireAny(candidates); ok {
		return l.releaseFunc(authID), authID, nil
	}
	l.mu.Lock()
	if timeout <= 0 {
		err := l.saturatedErrorLocked(candidates[0], 0)
		l.mu.Unlock()
		return nil, "", err
	}
	waiter, err := l.enqueueLocked(candidates, maxQueueDepth)
	l.mu.Unlock()
	if err != nil {
		return nil, "", err
	}

	timer := time.NewTimer(timeout)
	defer timer.Stop()
	// Jitter keeps queued callers of one process from polling in lockstep.
	poll := time.NewTicker(clusterSlotPollInterval + rand.N(clusterSlotPollInterval/2))
	defer poll.Stop()
	start := time.Now()
	for {
		select {
		case granted := <-waiter.granted:
			l.dequeue(waiter)
			return l.releaseFunc(granted), granted, nil
		case <-ctx.Done():
			return l.giveUp(waiter, candidates, ctx.Err(), time.Since(start))
		case <-timer.C:
			return l.giveUp(waiter, candidates, nil, time.Since(start))
		case <-poll.C:
			authID, ok := l.tryAcquireAny(candidates)
			if !ok {
				continue
			}
			if waiter.abandon() {
				l.dequeue(waiter)
				return l.releaseFunc(authID), authID, nil
			}
			// A local release handed us a slot while we were polling. Keep the
			// handed-over one and give the polled one back.
			granted := <-waiter.granted
			l.dequeue(waiter)
			l.ReleaseSlot(authID)
			return l.releaseFunc(granted), granted, nil
		}
	}
}

// unsharedSlots reports slots taken while cluster-wide counting was
// unavailable. Test-facing.
func (l *AccountConcurrencyLimiter) unsharedSlots(authID string) int {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.unshared[authID]
}
