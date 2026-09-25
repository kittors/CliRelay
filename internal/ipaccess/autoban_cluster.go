package ipaccess

import (
	"context"
	"sync/atomic"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/cluster/sharedstate"
)

// Auto-ban counting across a cluster.
//
// Each node's engine only counts the failures that reach it, so a source
// spreading its attempts over N nodes needed N times the threshold before any
// node banned it. With a ClusterAutoBanStore installed failures are counted in
// the shared Redis, and a crossing is claimed by exactly one node, which
// writes the rule and sends the alert; the others treat the source as already
// banned meanwhile. The deny rule itself lives in the shared database.
//
// The in-memory counters keep running underneath and are what a node falls
// back to while the shared store is unavailable, with the full threshold: a
// per-node count is the pre-cluster behaviour.

// ClusterAutoBanStore shares auto-ban failure counters between nodes.
type ClusterAutoBanStore interface {
	Available() bool
	AutoBanCharge(ctx context.Context, cidr string, spec sharedstate.AutoBanSpec, now time.Time) (sharedstate.AutoBanCharge, error)
	AutoBanMark(ctx context.Context, cidr string, until, now time.Time, retention time.Duration) error
}

type clusterAutoBanRef struct{ store ClusterAutoBanStore }

var clusterAutoBan atomic.Pointer[clusterAutoBanRef]

// SetClusterAutoBanStore installs the shared counter store. nil restores
// per-node counting.
func SetClusterAutoBanStore(store ClusterAutoBanStore) {
	if store == nil {
		clusterAutoBan.Store(nil)
		return
	}
	clusterAutoBan.Store(&clusterAutoBanRef{store: store})
}

func currentClusterAutoBan() ClusterAutoBanStore {
	if ref := clusterAutoBan.Load(); ref != nil && ref.store.Available() {
		return ref.store
	}
	return nil
}

const (
	// autoBanClaimTTL covers the rule write (bounded by the handler's 5s
	// context) with margin; a claim that lapses lets the next failure retry.
	autoBanClaimTTL = 10 * time.Second
	// autoBanSharedRetention keeps a quiet source's ban history, which
	// doubles the next ban, for a day.
	autoBanSharedRetention = 24 * time.Hour
)

// chargeShared charges a failure cluster-wide. ok is false outside a cluster
// and when the shared store cannot answer.
func chargeShared(cidr string, policy AutoBanPolicy, now time.Time) (failures, bans int, alreadyBanned, ok bool) {
	store := currentClusterAutoBan()
	if store == nil {
		return 0, 0, false, false
	}
	retention := autoBanSharedRetention
	if window := policy.Window(); window > retention {
		retention = window
	}
	res, err := store.AutoBanCharge(context.Background(), cidr, sharedstate.AutoBanSpec{
		Window:    policy.Window(),
		Slots:     autoBanSlotCount,
		Threshold: policy.FailureThreshold,
		ClaimTTL:  autoBanClaimTTL,
		Retention: retention,
	}, now)
	if err != nil {
		return 0, 0, false, false
	}
	return res.Failures, res.Bans, res.AlreadyBanned, true
}

// markBannedShared records a ban cluster-wide, which also releases the claim.
func markBannedShared(cidr string, until, now time.Time) {
	if store := currentClusterAutoBan(); store != nil {
		_ = store.AutoBanMark(context.Background(), cidr, until, now, autoBanSharedRetention)
	}
}
