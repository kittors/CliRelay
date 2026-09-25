package cluster

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Options configures a coordinator.
type Options struct {
	// Enabled turns on cluster mode. When false, Start returns a single-node
	// coordinator and never touches the database.
	Enabled bool
	// NodeID identifies this process in the cluster. Empty means the host
	// name. It must be unique among the nodes sharing a database.
	NodeID string
	// DSN is the PostgreSQL connection string used for the dedicated LISTEN
	// connection. The listener must not come from the shared pool: pooled
	// connections are recycled and a recycled connection silently drops its
	// LISTEN registration.
	DSN string
	// DB is the shared runtime pool, used for NOTIFY, membership heartbeats
	// and the leadership lock connection.
	DB *sql.DB
	// Version is the build version reported in the membership table.
	Version string
	// Advertise is an optional human-readable address shown in status views.
	Advertise string
}

// NodeStatus is one row of the membership view.
type NodeStatus struct {
	NodeID    string    `json:"node_id"`
	Version   string    `json:"version,omitempty"`
	Advertise string    `json:"advertise,omitempty"`
	StartedAt time.Time `json:"started_at"`
	LastSeen  time.Time `json:"last_seen"`
	Leader    bool      `json:"leader"`
	Active    bool      `json:"active"`
	Self      bool      `json:"self"`
}

// ErrPayloadTooLarge is returned by Publish when the encoded payload would
// exceed the NOTIFY size limit. Publish identifiers, not data.
var ErrPayloadTooLarge = errors.New("cluster: event payload too large")

// transport moves events between nodes. The PostgreSQL implementation lives
// in pgbus.go; the in-memory one in memory.go serves tests.
type transport interface {
	publish(ctx context.Context, ev Event) error
	publishTx(ctx context.Context, tx *sql.Tx, ev Event) error
	close()
}

// Coordinator is the per-process handle on cluster state. The zero value is
// not usable; use Start, Default or a MemoryHub.
type Coordinator struct {
	enabled bool
	nodeID  string

	transport transport

	leader        atomic.Bool
	activeNodes   atomic.Int64
	subsMu        sync.RWMutex
	nextSubID     int
	subs          map[string]map[int]func(Event)
	leaderSubs    map[int]func(bool)
	nodesProvider func(ctx context.Context) ([]NodeStatus, error)

	closeOnce sync.Once
	closeFns  []func()

	// prepared marks a coordinator installed by Prepare; started is set once
	// Start has adopted it, and closed once Close ran.
	prepared bool
	started  atomic.Bool
	closed   atomic.Bool
}

func newCoordinator(enabled bool, nodeID string) *Coordinator {
	c := &Coordinator{
		enabled:    enabled,
		nodeID:     nodeID,
		subs:       make(map[string]map[int]func(Event)),
		leaderSubs: make(map[int]func(bool)),
	}
	c.activeNodes.Store(1)
	if !enabled {
		c.leader.Store(true)
	}
	return c
}

var defaultCoordinator atomic.Pointer[Coordinator]

// Default returns the process-wide coordinator installed by Start, or a
// single-node coordinator when none was started. It never returns nil.
func Default() *Coordinator {
	if c := defaultCoordinator.Load(); c != nil {
		return c
	}
	return singleNode
}

// SetDefault installs c as the process-wide coordinator. Passing nil resets
// to single-node behaviour. Tests use this to install a MemoryHub node.
func SetDefault(c *Coordinator) {
	defaultCoordinator.Store(c)
}

var singleNode = newCoordinator(false, DefaultNodeID())

// DefaultNodeID returns the host name, or "local" when it is unavailable.
func DefaultNodeID() string {
	if id := strings.TrimSpace(os.Getenv(EnvNodeID)); id != "" {
		return id
	}
	host, err := os.Hostname()
	if err != nil || strings.TrimSpace(host) == "" {
		return "local"
	}
	return host
}

// EnvNodeID overrides the node ID; config.ClusterConfig.NodeID takes
// precedence when set.
const EnvNodeID = "CLIRELAY_CLUSTER_NODE_ID"

// Start builds the coordinator for opts, installs it as Default and returns
// it. With opts.Enabled false it returns a single-node coordinator.
func Start(ctx context.Context, opts Options) (*Coordinator, error) {
	nodeID := strings.TrimSpace(opts.NodeID)
	if nodeID == "" {
		nodeID = DefaultNodeID()
	}
	if !opts.Enabled {
		c := newCoordinator(false, nodeID)
		SetDefault(c)
		return c, nil
	}
	c, err := startPostgres(ctx, nodeID, opts)
	if err != nil {
		return nil, fmt.Errorf("cluster: start node %s: %w", nodeID, err)
	}
	SetDefault(c)
	return c, nil
}

// Prepare installs a cluster-mode coordinator as Default before the database
// is up, and returns it. Until Start adopts it the node is a follower that
// publishes nothing, so leader-only maintenance started during startup stays
// idle instead of running on every node that boots at the same time, and
// subscriptions made before Start keep working afterwards because Start
// reuses this coordinator. With opts.Enabled false it changes nothing.
func Prepare(opts Options) *Coordinator {
	if !opts.Enabled {
		return Default()
	}
	nodeID := strings.TrimSpace(opts.NodeID)
	if nodeID == "" {
		nodeID = DefaultNodeID()
	}
	c := newCoordinator(true, nodeID)
	c.prepared = true
	newBackendSlot(c)
	SetDefault(c)
	return c
}

// adoptPrepared hands Start the coordinator Prepare installed for nodeID, or
// nil when there is none to take over.
func adoptPrepared(nodeID string) *Coordinator {
	c := defaultCoordinator.Load()
	if c == nil || !c.prepared || c.nodeID != nodeID || c.closed.Load() {
		return nil
	}
	if !c.started.CompareAndSwap(false, true) {
		return nil
	}
	return c
}

// Enabled reports whether this process runs in cluster mode.
func (c *Coordinator) Enabled() bool { return c != nil && c.enabled }

// NodeID returns this node's identifier.
func (c *Coordinator) NodeID() string {
	if c == nil {
		return DefaultNodeID()
	}
	return c.nodeID
}

// IsLeader reports whether this node currently holds cluster leadership.
// A single node is always the leader, so leader-only tasks keep running
// exactly as they did before clustering existed.
func (c *Coordinator) IsLeader() bool {
	if c == nil {
		return true
	}
	return c.leader.Load()
}

// ActiveNodeCount returns how many nodes have heartbeated recently,
// including this one. It is at least 1.
func (c *Coordinator) ActiveNodeCount() int {
	if c == nil {
		return 1
	}
	if n := c.activeNodes.Load(); n > 1 {
		return int(n)
	}
	return 1
}

// Nodes returns the membership view. A single node reports only itself.
func (c *Coordinator) Nodes(ctx context.Context) ([]NodeStatus, error) {
	if c == nil || c.nodesProvider == nil {
		now := time.Now()
		return []NodeStatus{{NodeID: c.NodeID(), StartedAt: now, LastSeen: now, Leader: true, Active: true, Self: true}}, nil
	}
	return c.nodesProvider(ctx)
}

// OnLeadershipChange registers fn to be called whenever this node gains or
// loses leadership. It returns a function that removes the registration.
// fn runs on the coordinator's goroutine and must not block.
func (c *Coordinator) OnLeadershipChange(fn func(isLeader bool)) func() {
	if c == nil || fn == nil {
		return func() {}
	}
	c.subsMu.Lock()
	id := c.nextSubID
	c.nextSubID++
	c.leaderSubs[id] = fn
	c.subsMu.Unlock()
	return func() {
		c.subsMu.Lock()
		delete(c.leaderSubs, id)
		c.subsMu.Unlock()
	}
}

// Subscribe registers fn for events on topic published by other nodes, plus
// the Resync events of every topic. Events a node publishes are not echoed
// back to it: the publisher has already applied its own change. fn runs on
// the bus goroutine; it must not block, so hand long work to a goroutine.
func (c *Coordinator) Subscribe(topic string, fn func(Event)) func() {
	if c == nil || fn == nil {
		return func() {}
	}
	c.subsMu.Lock()
	id := c.nextSubID
	c.nextSubID++
	if c.subs[topic] == nil {
		c.subs[topic] = make(map[int]func(Event))
	}
	c.subs[topic][id] = fn
	c.subsMu.Unlock()
	return func() {
		c.subsMu.Lock()
		delete(c.subs[topic], id)
		c.subsMu.Unlock()
	}
}

// Publish sends payload on topic to the other nodes. It is a no-op in
// single-node mode. Delivery is best effort; see the package comment.
func (c *Coordinator) Publish(ctx context.Context, topic string, payload any) error {
	if !c.Enabled() || c.transport == nil {
		return nil
	}
	ev, err := c.envelope(topic, payload)
	if err != nil {
		return err
	}
	return c.transport.publish(ctx, ev)
}

// PublishTx queues payload on topic inside tx. PostgreSQL delivers it only
// if tx commits, so receivers never observe a change that was rolled back.
func (c *Coordinator) PublishTx(ctx context.Context, tx *sql.Tx, topic string, payload any) error {
	if !c.Enabled() || c.transport == nil {
		return nil
	}
	if tx == nil {
		return c.Publish(ctx, topic, payload)
	}
	ev, err := c.envelope(topic, payload)
	if err != nil {
		return err
	}
	return c.transport.publishTx(ctx, tx, ev)
}

func (c *Coordinator) envelope(topic string, payload any) (Event, error) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return Event{}, fmt.Errorf("cluster: encode %s payload: %w", topic, err)
	}
	if len(raw) > maxPayloadBytes {
		return Event{}, fmt.Errorf("%w: %s payload is %d bytes", ErrPayloadTooLarge, topic, len(raw))
	}
	return Event{Topic: topic, Origin: c.nodeID, Payload: raw}, nil
}

// deliver dispatches an incoming event to subscribers. Self-originated
// events are dropped; Resync events go to every subscriber of every topic.
func (c *Coordinator) deliver(ev Event) {
	if !ev.Resync && ev.Origin == c.nodeID {
		return
	}
	c.subsMu.RLock()
	var fns []func(Event)
	if ev.Resync {
		for _, byID := range c.subs {
			for _, fn := range byID {
				fns = append(fns, fn)
			}
		}
	} else {
		for _, fn := range c.subs[ev.Topic] {
			fns = append(fns, fn)
		}
	}
	c.subsMu.RUnlock()
	for _, fn := range fns {
		fn(ev)
	}
}

// setLeader records a leadership transition and notifies subscribers.
func (c *Coordinator) setLeader(isLeader bool) {
	if c.leader.Swap(isLeader) == isLeader {
		return
	}
	c.subsMu.RLock()
	fns := make([]func(bool), 0, len(c.leaderSubs))
	for _, fn := range c.leaderSubs {
		fns = append(fns, fn)
	}
	c.subsMu.RUnlock()
	for _, fn := range fns {
		fn(isLeader)
	}
}

// Close stops background work and releases leadership. It is safe to call
// more than once.
func (c *Coordinator) Close() {
	if c == nil {
		return
	}
	c.closeOnce.Do(func() {
		c.closed.Store(true)
		for i := len(c.closeFns) - 1; i >= 0; i-- {
			c.closeFns[i]()
		}
		if c.transport != nil {
			c.transport.close()
		}
		if c.enabled {
			c.setLeader(false)
		}
	})
}
