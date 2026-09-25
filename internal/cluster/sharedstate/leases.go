package sharedstate

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

// Concurrency slots are sorted sets: one member per held slot, scored by the
// time its lease expires. Taking a slot first drops expired members, so a slot
// held by a node that died comes back after one lease TTL without anyone
// having to notice the death.
//
// KEYS[1] slot set
// ARGV[1] now (ms)   ARGV[2] limit   ARGV[3] member   ARGV[4] lease expiry (ms)
// ARGV[5] set TTL (ms)
// Returns {taken (0/1), members held after the call}.
var acquireLeaseScript = redis.NewScript(`
redis.call('ZREMRANGEBYSCORE', KEYS[1], '-inf', ARGV[1])
local held = redis.call('ZCARD', KEYS[1])
if held >= tonumber(ARGV[2]) then
  return {0, held}
end
redis.call('ZADD', KEYS[1], ARGV[4], ARGV[3])
redis.call('PEXPIRE', KEYS[1], ARGV[5])
return {1, held + 1}
`)

// acquireLease takes one slot under limit for member.
func (s *Store) acquireLease(ctx context.Context, key, member string, limit int) (bool, int, error) {
	now := s.now()
	res, err := s.client.Eval(ctx, acquireLeaseScript, []string{key},
		now.UnixMilli(), limit, member, now.Add(s.leaseTTL).UnixMilli(), s.setTTL().Milliseconds())
	if err != nil {
		return false, 0, err
	}
	vals, err := int64s(res, 2)
	if err != nil {
		return false, 0, err
	}
	return vals[0] == 1, int(vals[1]), nil
}

// setTTL keeps a slot set alive while leases in it can still be renewed, and
// lets it vanish when every node holding one is gone.
func (s *Store) setTTL() time.Duration { return 2 * s.leaseTTL }

// newLeaseMember names one held slot. The node prefix is for operators reading
// the set; uniqueness comes from the random suffix.
func (s *Store) newLeaseMember() string {
	var buf [12]byte
	_, _ = rand.Read(buf[:])
	return s.nodeID + ":" + hex.EncodeToString(buf[:])
}

type leaseRef struct{ key, member string }

// leaseKeeper renews the leases this node holds and gives back released ones.
//
// Renewals and releases run on one goroutine, in order. That is what makes the
// unconditional renewal safe: a lease released while a renewal pipeline is in
// flight has its removal queued behind that pipeline, so it can never be
// resurrected by a renewal that raced it. The unconditional ZADD in turn lets a
// lease that expired during a Redis outage be counted again once Redis is back,
// for as long as the request holding it is still running.
type leaseKeeper struct {
	store *Store

	mu      sync.Mutex
	held    map[leaseRef]struct{}
	pending map[leaseRef]time.Time

	releases  chan leaseRef
	stop      chan struct{}
	done      chan struct{}
	startOnce sync.Once
	closeOnce sync.Once
	started   bool
}

func newLeaseKeeper(s *Store) *leaseKeeper {
	return &leaseKeeper{
		store:    s,
		held:     make(map[leaseRef]struct{}),
		pending:  make(map[leaseRef]time.Time),
		releases: make(chan leaseRef, 4096),
		stop:     make(chan struct{}),
		done:     make(chan struct{}),
	}
}

func (k *leaseKeeper) ensureStarted() {
	k.startOnce.Do(func() {
		k.mu.Lock()
		k.started = true
		k.mu.Unlock()
		go k.run()
	})
}

// track starts renewing a lease that was just taken.
func (k *leaseKeeper) track(key, member string) {
	k.ensureStarted()
	k.mu.Lock()
	k.held[leaseRef{key, member}] = struct{}{}
	k.mu.Unlock()
}

// release stops renewing a lease and removes it from Redis without blocking
// the caller. If Redis cannot be reached the removal is retried until the
// lease would have expired on its own.
func (k *leaseKeeper) release(key, member string) {
	ref := leaseRef{key, member}
	k.mu.Lock()
	delete(k.held, ref)
	k.mu.Unlock()
	k.ensureStarted()
	select {
	case k.releases <- ref:
	default:
		k.mu.Lock()
		k.pending[ref] = k.store.now().Add(k.store.leaseTTL)
		k.mu.Unlock()
	}
}

func (k *leaseKeeper) run() {
	defer close(k.done)
	ticker := time.NewTicker(k.store.leaseTTL / 3)
	defer ticker.Stop()
	for {
		select {
		case <-k.stop:
			k.releaseAllOnClose()
			return
		case ref := <-k.releases:
			k.removeBatch(ref)
		case <-ticker.C:
			k.renew()
			k.retryPending()
		}
	}
}

// removeBatch removes ref and whatever else is already queued in one pipeline.
func (k *leaseKeeper) removeBatch(first leaseRef) {
	batch := []leaseRef{first}
	for len(batch) < 256 {
		select {
		case ref := <-k.releases:
			batch = append(batch, ref)
			continue
		default:
		}
		break
	}
	if err := k.remove(batch); err != nil {
		deadline := k.store.now().Add(k.store.leaseTTL)
		k.mu.Lock()
		for _, ref := range batch {
			k.pending[ref] = deadline
		}
		k.mu.Unlock()
	}
}

func (k *leaseKeeper) remove(batch []leaseRef) error {
	return k.store.client.PipelineBackground(context.Background(), func(pipe redis.Pipeliner) error {
		for _, ref := range batch {
			pipe.ZRem(context.Background(), ref.key, ref.member)
		}
		return nil
	})
}

func (k *leaseKeeper) renew() {
	k.mu.Lock()
	refs := make([]leaseRef, 0, len(k.held))
	for ref := range k.held {
		refs = append(refs, ref)
	}
	k.mu.Unlock()
	if len(refs) == 0 || !k.store.Available() {
		return
	}
	expiry := k.store.now().Add(k.store.leaseTTL).UnixMilli()
	setTTL := k.store.setTTL()
	_ = k.store.client.TxPipelineBackground(context.Background(), func(pipe redis.Pipeliner) error {
		for _, ref := range refs {
			pipe.ZAdd(context.Background(), ref.key, redis.Z{Score: float64(expiry), Member: ref.member})
			pipe.PExpire(context.Background(), ref.key, setTTL)
		}
		return nil
	})
}

func (k *leaseKeeper) retryPending() {
	now := k.store.now()
	k.mu.Lock()
	var batch []leaseRef
	for ref, deadline := range k.pending {
		if !deadline.After(now) {
			delete(k.pending, ref) // expired on its own by now
			continue
		}
		batch = append(batch, ref)
	}
	k.mu.Unlock()
	if len(batch) == 0 || !k.store.Available() {
		return
	}
	if err := k.remove(batch); err == nil {
		k.mu.Lock()
		for _, ref := range batch {
			delete(k.pending, ref)
		}
		k.mu.Unlock()
	}
}

func (k *leaseKeeper) releaseAllOnClose() {
	k.mu.Lock()
	batch := make([]leaseRef, 0, len(k.held)+len(k.pending))
	for ref := range k.held {
		batch = append(batch, ref)
	}
	for ref := range k.pending {
		batch = append(batch, ref)
	}
	k.held = make(map[leaseRef]struct{})
	k.pending = make(map[leaseRef]time.Time)
	k.mu.Unlock()
	for {
		select {
		case ref := <-k.releases:
			batch = append(batch, ref)
			continue
		default:
		}
		break
	}
	if len(batch) > 0 && k.store.Available() {
		_ = k.remove(batch)
	}
}

func (k *leaseKeeper) close() {
	k.closeOnce.Do(func() {
		close(k.stop)
		k.mu.Lock()
		started := k.started
		k.mu.Unlock()
		if started {
			<-k.done
		}
	})
}

// heldCount reports how many leases this node is renewing. Test-facing.
func (k *leaseKeeper) heldCount() int {
	k.mu.Lock()
	defer k.mu.Unlock()
	return len(k.held)
}

func int64s(res any, want int) ([]int64, error) {
	list, ok := res.([]any)
	if !ok || len(list) < want {
		return nil, fmt.Errorf("sharedstate: unexpected script reply %T %v", res, res)
	}
	out := make([]int64, len(list))
	for i, item := range list {
		switch v := item.(type) {
		case int64:
			out[i] = v
		case nil:
			out[i] = 0
		default:
			return nil, fmt.Errorf("sharedstate: unexpected script reply element %T %v", item, item)
		}
	}
	return out, nil
}
