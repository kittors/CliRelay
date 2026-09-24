// Package sharedstate keeps the counters, leases and bindings that must be
// global across the nodes of a cluster in the shared cluster Redis.
//
// Every operation is one atomic Lua script (or a background pipeline), so two
// nodes can never both observe "one slot left" and both take it. Nothing here
// decides what to do when Redis is unavailable: each method reports the error
// and the caller falls back to its node-local behaviour, which is the
// pre-cluster code path.
//
// Time is passed from the calling node rather than read with Redis TIME. The
// windows and leases only need agreement to within a fraction of a second,
// which NTP-synchronised nodes provide, and it keeps the scripts deterministic
// under test.
package sharedstate

import (
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/cluster/sharedredis"
)

// Default lease timing. A lease is renewed every LeaseTTL/3 while the request
// holding it runs, so a crashed node leaks each of its slots for at most one
// lease TTL.
const (
	DefaultLeaseTTL = 90 * time.Second
)

// Options tunes a Store. Zero values take the defaults.
type Options struct {
	// NodeID prefixes lease members, so a stuck slot can be traced to a node.
	NodeID string
	// LeaseTTL is how long a concurrency slot survives without renewal.
	LeaseTTL time.Duration
	// FlushInterval is how often asynchronously counted requests and tokens
	// are written. Defaults to one second.
	FlushInterval time.Duration
	// Now overrides the clock, for tests.
	Now func() time.Time
}

// Store is the process handle on shared cluster state. A nil *Store, or one
// whose client is nil, is permanently unavailable.
type Store struct {
	client *sharedredis.Client
	nodeID string
	now    func() time.Time

	leaseTTL time.Duration
	keeper   *leaseKeeper
	counts   *countBatcher
	slots    *accountSlotPool

	closeOnce sync.Once
}

// New builds a store on client. client may be nil when the cluster has no
// shared Redis configured; every method then reports unavailability.
func New(client *sharedredis.Client, opts Options) *Store {
	if opts.LeaseTTL <= 0 {
		opts.LeaseTTL = DefaultLeaseTTL
	}
	if opts.FlushInterval <= 0 {
		opts.FlushInterval = time.Second
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.NodeID == "" {
		opts.NodeID = client.NodeID()
	}
	if opts.NodeID == "" {
		opts.NodeID = "local"
	}
	s := &Store{
		client:   client,
		nodeID:   opts.NodeID,
		now:      opts.Now,
		leaseTTL: opts.LeaseTTL,
	}
	s.keeper = newLeaseKeeper(s)
	s.counts = newCountBatcher(s, opts.FlushInterval)
	s.slots = newAccountSlotPool(s)
	return s
}

// Available reports whether shared state can be used right now.
func (s *Store) Available() bool {
	return s != nil && s.client.Available()
}

// Client returns the underlying Redis client.
func (s *Store) Client() *sharedredis.Client {
	if s == nil {
		return nil
	}
	return s.client
}

// Close flushes pending counters, stops renewing leases and gives back the
// ones still held, so a node that shuts down cleanly frees its slots at once
// instead of after a lease TTL.
func (s *Store) Close() {
	if s == nil {
		return
	}
	s.closeOnce.Do(func() {
		s.counts.close()
		s.keeper.close()
	})
}
