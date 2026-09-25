package management

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/cluster/sharedstate"
)

// Login throttling across a cluster.
//
// Each node's throttle only sees the guesses that reach it, so spreading a
// guessing run over N nodes used to buy N times the budget. With a
// ClusterThrottleStore installed, failures are counted and blocks armed in
// the shared Redis, with the same windows, ladder and reset rules as the
// in-memory throttle, and every node enforces them.
//
// The in-memory throttle keeps running underneath: failures are recorded
// there too, a block this node armed is enforced without asking Redis (so a
// client hammering a closed door costs no round trip), and it is what the
// node falls back to when the shared store is unavailable. The thresholds are
// not divided by the node count in that case: the throttle guards against
// guessing, and per-node limits are the pre-cluster behaviour.

// ClusterThrottleStore shares login throttle buckets between nodes.
type ClusterThrottleStore interface {
	Available() bool
	ThrottleCharge(ctx context.Context, bucket string, spec sharedstate.ThrottleSpec, now time.Time) (sharedstate.ThrottleState, error)
	ThrottlePeek(ctx context.Context, bucket string, spec sharedstate.ThrottleSpec, now time.Time) (sharedstate.ThrottleState, error)
	ThrottleClear(ctx context.Context, bucket string) error
}

type clusterThrottleRef struct{ store ClusterThrottleStore }

var clusterThrottle atomic.Pointer[clusterThrottleRef]

// SetClusterThrottleStore installs the shared throttle store. nil restores
// per-node throttling.
func SetClusterThrottleStore(store ClusterThrottleStore) {
	if store == nil {
		clusterThrottle.Store(nil)
		return
	}
	clusterThrottle.Store(&clusterThrottleRef{store: store})
}

func currentClusterThrottle() ClusterThrottleStore {
	if ref := clusterThrottle.Load(); ref != nil && ref.store.Available() {
		return ref.store
	}
	return nil
}

// sharedClearInterval bounds how often a successful management-key request
// clears the same shared bucket. A valid key succeeds on every panel call, and
// one clear per client every half minute is enough to wipe the failures other
// nodes recorded for it. Interactive logins are rare and always clear.
const sharedClearInterval = 30 * time.Second

var (
	sharedClears      sync.Map // bucket ID -> time.Time of the last clear
	sharedClearWrites atomic.Int64
)

// sharedClearSweepEvery bounds the dedup map: every so many clears, entries
// past their interval are dropped. Every client that ever succeeds lands in
// it, so without the sweep it would grow for the life of the process.
const sharedClearSweepEvery = 1024

func (t *loginThrottle) sharedSpec(key throttleKey) sharedstate.ThrottleSpec {
	policy := t.policyFor(key)
	return sharedstate.ThrottleSpec{
		Short:      sharedstate.ThrottleWindow{Limit: policy.Short.Limit, Window: policy.Short.Window, Slots: throttleShortSlotCount},
		Long:       sharedstate.ThrottleWindow{Limit: policy.Long.Limit, Window: policy.Long.Window, Slots: throttleLongSlotCount},
		Backoff:    policy.Backoff,
		ResetAfter: policy.ResetAfter,
		Retention:  t.idleRetention(),
	}
}

func (t *loginThrottle) sharedDecision(key throttleKey, st sharedstate.ThrottleState) throttleDecision {
	d := throttleDecision{
		Scope:      key.Scope,
		Generation: st.Generation,
		ShortCount: st.ShortCount,
		LongCount:  st.LongCount,
		NewlyArmed: st.NewlyArmed,
	}
	if st.Blocked {
		d.Outcome = blockOutcome(t.policyFor(key))
		d.RetryAfter = st.RetryAfter
	}
	return d
}

// sharedEvaluate reports the cluster-wide standing of a bucket. ok is false
// outside a cluster and when the shared store cannot answer.
func (t *loginThrottle) sharedEvaluate(key throttleKey, now time.Time) (throttleDecision, bool) {
	store := currentClusterThrottle()
	if store == nil {
		return throttleDecision{}, false
	}
	st, err := store.ThrottlePeek(context.Background(), bucketID(key), t.sharedSpec(key), now)
	if err != nil {
		return throttleDecision{}, false
	}
	return t.sharedDecision(key, st), true
}

// sharedCharge charges a failure cluster-wide.
func (t *loginThrottle) sharedCharge(key throttleKey, now time.Time) (throttleDecision, bool) {
	store := currentClusterThrottle()
	if store == nil {
		return throttleDecision{}, false
	}
	st, err := store.ThrottleCharge(context.Background(), bucketID(key), t.sharedSpec(key), now)
	if err != nil {
		return throttleDecision{}, false
	}
	return t.sharedDecision(key, st), true
}

// sharedClear clears a bucket cluster-wide without holding up the request
// that succeeded.
func (t *loginThrottle) sharedClear(key throttleKey) {
	store := currentClusterThrottle()
	if store == nil {
		return
	}
	bucket := bucketID(key)
	if key.Scope == scopeManagementKey && !claimSharedClear(bucket, time.Now()) {
		return
	}
	go func() {
		if err := store.ThrottleClear(context.Background(), bucket); err != nil {
			sharedClears.Delete(bucket) // retry on the next success
		}
	}()
}

// claimSharedClear reports whether bucket is due for a clear and records it.
func claimSharedClear(bucket string, now time.Time) bool {
	if last, ok := sharedClears.Load(bucket); ok && now.Sub(last.(time.Time)) < sharedClearInterval {
		return false
	}
	sharedClears.Store(bucket, now)
	if sharedClearWrites.Add(1)%sharedClearSweepEvery == 0 {
		sharedClears.Range(func(k, v any) bool {
			if now.Sub(v.(time.Time)) >= sharedClearInterval {
				sharedClears.Delete(k)
			}
			return true
		})
	}
	return true
}
