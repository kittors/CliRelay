package management

import (
	"sync"
	"time"
)

const (
	// loginCleanupInterval controls how often stale IP entries are purged.
	loginCleanupInterval = 1 * time.Hour
	// loginMaxIdleTime controls how long an IP can be idle before cleanup.
	loginMaxIdleTime = 2 * time.Hour
	// loginMaxFailures is how many consecutive failures trigger a block.
	loginMaxFailures = 5
	// loginBlockDuration is how long a blocked IP stays blocked.
	loginBlockDuration = 30 * time.Minute
)

type loginAttempt struct {
	count        int
	blockedUntil time.Time
	lastActivity time.Time
}

// loginThrottle rate-limits management login attempts per client IP.
//
// State is per process and deliberately in-memory: it protects against online
// password guessing against a single instance, not against a distributed attack,
// and losing it on restart only costs an attacker's progress, never a legitimate
// user's access. Entries are purged on a timer so a hostile IP range cannot grow
// the map without bound.
type loginThrottle struct {
	mu       sync.Mutex
	attempts map[string]*loginAttempt

	stopOnce sync.Once
	stop     chan struct{}
}

func newLoginThrottle() *loginThrottle {
	t := &loginThrottle{
		attempts: make(map[string]*loginAttempt),
		stop:     make(chan struct{}),
	}
	go t.cleanupLoop()
	return t
}

// blockedFor reports the remaining block duration for the client, or zero when
// the client may attempt a login.
func (t *loginThrottle) blockedFor(clientIP string, now time.Time) time.Duration {
	if t == nil {
		return 0
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	attempt := t.attempts[clientIP]
	if attempt == nil || attempt.blockedUntil.IsZero() || !now.Before(attempt.blockedUntil) {
		return 0
	}
	return attempt.blockedUntil.Sub(now)
}

// recordFailure counts a failed login and blocks the client once it reaches
// loginMaxFailures.
func (t *loginThrottle) recordFailure(clientIP string, now time.Time) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	attempt := t.attempts[clientIP]
	if attempt == nil {
		attempt = &loginAttempt{}
		t.attempts[clientIP] = attempt
	}
	attempt.count++
	attempt.lastActivity = now
	if attempt.count >= loginMaxFailures {
		attempt.blockedUntil = now.Add(loginBlockDuration)
		attempt.count = 0
	}
}

// recordSuccess clears any recorded failures for the client.
func (t *loginThrottle) recordSuccess(clientIP string) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.attempts, clientIP)
}

func (t *loginThrottle) cleanupLoop() {
	ticker := time.NewTicker(loginCleanupInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			t.purgeStale(time.Now())
		case <-t.stop:
			return
		}
	}
}

// purgeStale drops entries that are idle beyond loginMaxIdleTime.
//
// Entries still inside their block window are kept. With the current constants
// that case cannot arise (a block lasts 30m, well under the 2h idle threshold,
// so any idle-enough entry is already unblocked), but the guard keeps the
// invariant true if loginBlockDuration is ever raised above loginMaxIdleTime —
// without it, an attacker could shed a long block simply by going quiet.
func (t *loginThrottle) purgeStale(now time.Time) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	for ip, attempt := range t.attempts {
		if !attempt.blockedUntil.IsZero() && now.Before(attempt.blockedUntil) {
			continue
		}
		if now.Sub(attempt.lastActivity) > loginMaxIdleTime {
			delete(t.attempts, ip)
		}
	}
}

// close stops the cleanup goroutine. Safe to call more than once.
func (t *loginThrottle) close() {
	if t == nil {
		return
	}
	t.stopOnce.Do(func() { close(t.stop) })
}
