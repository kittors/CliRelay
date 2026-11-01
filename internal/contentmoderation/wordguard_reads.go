package contentmoderation

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"
)

// A request asks the policy store which word policies apply to it. The guard
// stands in front of every request, so the store must never be able to hold
// requests up for long: a database under load, or a pool all of whose
// connections are busy with slow statements, would otherwise turn into latency
// on every request the server takes.
//
// The reads are therefore not made by the requests. A request asks for a read
// and waits for it for as long as it can afford (the caller's deadline); when
// that runs out it goes on with the policies remembered from earlier requests,
// which is the guard's fail-open. The read itself is not given up on: it
// carries on in the background, so that the next request finds the policy it
// was waiting for. That matters for a policy that is large, or new, and takes
// longer to read than a request can wait, and it keeps the database from seeing
// a query cancelled mid-flight for every request that lost patience.
//
// Requests of one tenant and API key that arrive while a read is running share
// it, so a burst of them costs the database one query.

const (
	// wordPolicyReadTimeout is how long a read may run in the background.
	wordPolicyReadTimeout = 5 * time.Second
	// wordPolicySlowFor is how long, after a request gave up waiting, requests
	// stop waiting at all unless a read finishes first: while the store is not
	// answering, a wait would only add its length to every request.
	wordPolicySlowFor = 2 * time.Second
)

// errWordPolicySlow is returned, with the remembered policies, when the store is
// known to be slow and the read has not finished.
var errWordPolicySlow = errors.New("the word policy store is slow to answer")

type wordReadKey struct{ tenantID, keyID string }

// wordRead is one read of the policies of a tenant and key.
type wordRead struct {
	done     chan struct{}
	policies WordPolicies
	err      error
}

type wordPolicyReads struct {
	mu      sync.Mutex
	running map[wordReadKey]*wordRead

	// slowUntil is the time, in Unix nanoseconds, until which requests do not
	// wait for a read that has not finished. Zero when the store is answering.
	slowUntil atomic.Int64

	// Zero values mean the constants above; tests shorten them.
	readTimeout time.Duration
	slowFor     time.Duration
}

// join returns the running read for key, or starts one with run. run gets a
// context that does not end with any request.
func (r *wordPolicyReads) join(key wordReadKey, run func(ctx context.Context) (WordPolicies, error)) *wordRead {
	r.mu.Lock()
	if read := r.running[key]; read != nil {
		r.mu.Unlock()
		return read
	}
	read := &wordRead{done: make(chan struct{})}
	if r.running == nil {
		r.running = make(map[wordReadKey]*wordRead)
	}
	r.running[key] = read
	r.mu.Unlock()

	timeout := r.readTimeout
	if timeout <= 0 {
		timeout = wordPolicyReadTimeout
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		defer func() {
			// A read that panics must not leave its waiters waiting for ever.
			if recovered := recover(); recovered != nil {
				read.err = errors.New("reading the word policies failed")
			}
			r.mu.Lock()
			delete(r.running, key)
			r.mu.Unlock()
			if read.err == nil {
				r.slowUntil.Store(0)
			}
			close(read.done)
		}()
		read.policies, read.err = run(ctx)
	}()
	return read
}

// slow reports whether requests should not wait for a read that is still
// running.
func (r *wordPolicyReads) slow(now time.Time) bool {
	until := r.slowUntil.Load()
	return until != 0 && now.UnixNano() < until
}

// markSlow notes that a request gave up waiting.
func (r *wordPolicyReads) markSlow(now time.Time) {
	span := r.slowFor
	if span <= 0 {
		span = wordPolicySlowFor
	}
	r.slowUntil.Store(now.Add(span).UnixNano())
}
