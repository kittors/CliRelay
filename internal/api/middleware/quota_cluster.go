package middleware

import (
	"context"
	"math"
	"sync"
	"sync/atomic"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/cluster/sharedstate"
)

// ─── Cluster-wide counters ──────────────────────────────────────────────────
// In a cluster every node sees only part of a key's traffic, so the in-memory
// windows above would let each node admit the full limit: N nodes, N times the
// limit. A cluster installs ClusterQuota and the RPM, TPM and concurrency
// checks then run against counters shared by all nodes. When the shared
// counters are unavailable, or the cluster has none configured, each node keeps
// counting locally and enforces its share of the limit, ceil(limit/N).
//
// A single-node deployment never installs one, and every path below then
// reduces to the pre-cluster code.

// ClusterQuota shares per-key request, token and concurrency counters between
// the nodes of a cluster.
type ClusterQuota interface {
	// Available reports whether the shared counters can be used right now.
	Available() bool
	// ActiveNodes is the number of live nodes, at least 1. Limits are split
	// across them while the shared counters are unavailable.
	ActiveNodes() int
	// AdmitRequest counts one request and, when concurrencyLimit is positive,
	// tries to take a concurrency slot, atomically across the cluster.
	AdmitRequest(ctx context.Context, subject string, concurrencyLimit int) (sharedstate.Admission, error)
	// CountRequest counts a request without waiting, for subjects with no
	// rate or concurrency limit (dashboard only).
	CountRequest(subject string)
	// AddTokens adds token usage without waiting.
	AddTokens(subject string, tokens int64)
	// Rates reads cluster-wide windows for the dashboard.
	Rates(ctx context.Context, subjects []string) (map[string]sharedstate.Rate, error)
}

type clusterQuotaHolder struct{ quota ClusterQuota }

var clusterQuota atomic.Pointer[clusterQuotaHolder]

// SetClusterQuota installs the cluster-wide counters. nil restores the
// single-node behaviour.
func SetClusterQuota(q ClusterQuota) {
	if q == nil {
		clusterQuota.Store(nil)
		return
	}
	clusterQuota.Store(&clusterQuotaHolder{quota: q})
}

func currentClusterQuota() ClusterQuota {
	if holder := clusterQuota.Load(); holder != nil {
		return holder.quota
	}
	return nil
}

// dashboardRatesTimeout bounds the shared read behind the system stats view.
const dashboardRatesTimeout = time.Second

// needsSharedAdmission reports whether the key has a limit that reads the
// per-minute or concurrency counters. Other limits (daily, total, spending)
// read the usage database, which every node already shares.
func (p *quotaPolicy) needsSharedAdmission() bool {
	return p.rpmLimit > 0 || p.tpmLimit > 0 || p.concurrencyLimit > 0
}

// countRequest records one request in the RPM window. Every authenticated POST
// and every WebSocket turn is counted, admitted or not, so the dashboard sees
// all traffic.
//
// The node-local window is always kept, even in a cluster: it is what the node
// falls back to when the shared counters go away, and falling back to an empty
// window would briefly admit a burst. Subjects whose limits need the shared
// window are counted there by admitShared; the rest are counted
// asynchronously so they never wait for Redis.
func (p *quotaPolicy) countRequest() {
	getRPMTracker(p.subject).add()
	if q := currentClusterQuota(); q != nil && !p.needsSharedAdmission() && q.Available() {
		q.CountRequest(p.subject)
	}
}

// admitShared runs the rate and concurrency checks against the cluster-wide
// counters. ok is false when they cannot be used, and the caller then applies
// this node's share of the limits to its local counters.
func (p *quotaPolicy) admitShared() (release func(), verdict *quotaVerdict, ok bool) {
	q := currentClusterQuota()
	if q == nil || !p.needsSharedAdmission() || !q.Available() {
		return nil, nil, false
	}
	adm, err := q.AdmitRequest(context.Background(), p.subject, p.concurrencyLimit)
	if err != nil {
		return nil, nil, false
	}
	if p.concurrencyLimit > 0 && !adm.SlotTaken {
		return nil, concurrencyLimitVerdict(adm.Held, p.concurrencyLimit), true
	}
	// Mirror the slot in the node-local count as well, so that if this node
	// falls back mid-request its local count already includes the requests it
	// is running.
	localRelease := func() {}
	if p.concurrencyLimit > 0 {
		localRelease = trackKeyConcurrency(p.subject)
	}
	var once sync.Once
	release = func() {
		once.Do(func() {
			adm.Release()
			localRelease()
		})
	}

	if p.rpmLimit > 0 && adm.RPM > float64(p.rpmLimit) {
		release()
		return nil, rpmLimitVerdict(int(math.Ceil(adm.RPM)), p.rpmLimit), true
	}
	if p.tpmLimit > 0 && adm.TPM >= float64(p.tpmLimit) {
		release()
		return nil, tpmLimitVerdict(int64(math.Floor(adm.TPM)), p.tpmLimit), true
	}
	if verdict = p.checkBudgets(); verdict != nil {
		release()
		return nil, verdict, true
	}
	return release, nil, true
}

// nodeShare returns the limits this node enforces on its own counters. Outside
// a cluster, or with one live node, they are the configured limits. Otherwise
// the per-minute and concurrency limits are split evenly across the live nodes;
// budget limits read the shared database and stay whole.
func (p *quotaPolicy) nodeShare() quotaPolicy {
	share := *p
	q := currentClusterQuota()
	if q == nil {
		return share
	}
	nodes := q.ActiveNodes()
	share.rpmLimit = splitLimit(p.rpmLimit, nodes)
	share.tpmLimit = splitLimit(p.tpmLimit, nodes)
	share.concurrencyLimit = splitLimit(p.concurrencyLimit, nodes)
	return share
}

// splitLimit is ceil(limit/nodes). Rounding up keeps every node able to admit
// at least one request, at the cost of the cluster total slightly exceeding the
// configured limit when it does not divide evenly.
func splitLimit(limit, nodes int) int {
	if limit <= 0 || nodes <= 1 {
		return limit
	}
	return (limit + nodes - 1) / nodes
}

// trackKeyConcurrency counts an in-flight request against subject without
// checking any limit; the returned function undoes it once.
func trackKeyConcurrency(subject string) func() {
	if subject == "" {
		return func() {}
	}
	inFlightMu.Lock()
	inFlightByKey[subject]++
	inFlightMu.Unlock()
	var once sync.Once
	return func() { once.Do(func() { releaseKeyConcurrency(subject) }) }
}

func recordClusterTokens(subject string, tokens int64) {
	if q := currentClusterQuota(); q != nil && q.Available() {
		q.AddTokens(subject, tokens)
	}
}

// applyClusterRates replaces each subject's node-local RPM/TPM with the
// cluster-wide values, so every node's dashboard shows the same numbers. The
// subject list is still this node's: a key that has not reached this node in
// the last minute is not listed, because the shared counters are keyed by
// digests and cannot name the key.
func applyClusterRates(snapshots []ConcurrencySnapshot, total int64) ([]ConcurrencySnapshot, int64) {
	q := currentClusterQuota()
	if q == nil || len(snapshots) == 0 || !q.Available() {
		return snapshots, total
	}
	subjects := make([]string, 0, len(snapshots))
	for _, snap := range snapshots {
		subjects = append(subjects, snap.APIKey)
	}
	ctx, cancel := context.WithTimeout(context.Background(), dashboardRatesTimeout)
	defer cancel()
	rates, err := q.Rates(ctx, subjects)
	if err != nil {
		return snapshots, total
	}
	total = 0
	for i := range snapshots {
		if rate, ok := rates[snapshots[i].APIKey]; ok {
			snapshots[i].RPM = int(math.Round(rate.RPM))
			snapshots[i].TPM = int64(math.Round(rate.TPM))
		}
		total += int64(snapshots[i].RPM)
	}
	return snapshots, total
}
