package cluster

import (
	"context"
	"database/sql"
	"sort"
	"sync"
	"time"
)

// MemoryHub connects coordinators inside one process. Tests use it to
// exercise cross-node behaviour (cache invalidation, cooldown propagation,
// leader-only tasks) without PostgreSQL. Delivery is synchronous: Publish
// returns after every connected peer's subscribers have run.
type MemoryHub struct {
	mu     sync.Mutex
	nodes  map[string]*Coordinator
	down   map[string]bool
	leader string
}

// NewMemoryHub returns an empty hub.
func NewMemoryHub() *MemoryHub {
	return &MemoryHub{nodes: make(map[string]*Coordinator), down: make(map[string]bool)}
}

// Join attaches a new enabled coordinator with nodeID. The first node to
// join becomes the leader.
func (h *MemoryHub) Join(nodeID string) *Coordinator {
	c := newCoordinator(true, nodeID)
	c.transport = &memoryTransport{hub: h, nodeID: nodeID}
	c.nodesProvider = func(context.Context) ([]NodeStatus, error) { return h.statuses(nodeID), nil }
	h.mu.Lock()
	h.nodes[nodeID] = c
	first := h.leader == ""
	if first {
		h.leader = nodeID
	}
	h.mu.Unlock()
	if first {
		c.setLeader(true)
	}
	h.refreshCounts()
	return c
}

// SetLeader moves leadership to nodeID ("" leaves the cluster leaderless).
func (h *MemoryHub) SetLeader(nodeID string) {
	h.mu.Lock()
	h.leader = nodeID
	nodes := h.snapshot()
	h.mu.Unlock()
	for id, c := range nodes {
		c.setLeader(id == nodeID)
	}
}

// Disconnect simulates a node losing its bus connection: it neither sends
// nor receives until Reconnect, and events published meanwhile are lost.
func (h *MemoryHub) Disconnect(nodeID string) {
	h.mu.Lock()
	h.down[nodeID] = true
	h.mu.Unlock()
	h.refreshCounts()
}

// Reconnect restores delivery for nodeID and hands it a Resync event, the
// same contract the PostgreSQL listener has after reconnecting.
func (h *MemoryHub) Reconnect(nodeID string) {
	h.mu.Lock()
	delete(h.down, nodeID)
	c := h.nodes[nodeID]
	h.mu.Unlock()
	h.refreshCounts()
	if c != nil {
		c.deliver(Event{Resync: true, Origin: nodeID})
	}
}

// Leave detaches nodeID, as if the process exited.
func (h *MemoryHub) Leave(nodeID string) {
	h.mu.Lock()
	c := h.nodes[nodeID]
	delete(h.nodes, nodeID)
	delete(h.down, nodeID)
	if h.leader == nodeID {
		h.leader = ""
	}
	h.mu.Unlock()
	if c != nil {
		c.setLeader(false)
	}
	h.refreshCounts()
}

func (h *MemoryHub) snapshot() map[string]*Coordinator {
	out := make(map[string]*Coordinator, len(h.nodes))
	for id, c := range h.nodes {
		out[id] = c
	}
	return out
}

func (h *MemoryHub) refreshCounts() {
	h.mu.Lock()
	active := int64(0)
	for id := range h.nodes {
		if !h.down[id] {
			active++
		}
	}
	nodes := h.snapshot()
	h.mu.Unlock()
	for _, c := range nodes {
		c.activeNodes.Store(active)
	}
}

func (h *MemoryHub) statuses(self string) []NodeStatus {
	h.mu.Lock()
	defer h.mu.Unlock()
	now := time.Now()
	out := make([]NodeStatus, 0, len(h.nodes))
	for id := range h.nodes {
		out = append(out, NodeStatus{
			NodeID: id, StartedAt: now, LastSeen: now,
			Leader: id == h.leader, Active: !h.down[id], Self: id == self,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].NodeID < out[j].NodeID })
	return out
}

type memoryTransport struct {
	hub    *MemoryHub
	nodeID string
}

func (t *memoryTransport) publish(_ context.Context, ev Event) error {
	t.hub.mu.Lock()
	if t.hub.down[t.nodeID] {
		t.hub.mu.Unlock()
		return nil
	}
	targets := make([]*Coordinator, 0, len(t.hub.nodes))
	for id, c := range t.hub.nodes {
		if !t.hub.down[id] {
			targets = append(targets, c)
		}
	}
	t.hub.mu.Unlock()
	for _, c := range targets {
		c.deliver(ev)
	}
	return nil
}

// publishTx delivers immediately: the in-memory hub has no transactions, so
// tests that care about rollback must assert on PostgreSQL.
func (t *memoryTransport) publishTx(ctx context.Context, _ *sql.Tx, ev Event) error {
	return t.publish(ctx, ev)
}

func (t *memoryTransport) close() {}
