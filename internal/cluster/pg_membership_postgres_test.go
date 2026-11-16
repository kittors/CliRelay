package cluster

import (
	"context"
	"database/sql"
	"encoding/json"
	"sync/atomic"
	"testing"
	"time"
)

func pgNodeOf(t *testing.T, c *Coordinator) *pgNode {
	t.Helper()
	slot, ok := c.transport.(*backendSlot)
	if !ok {
		t.Fatalf("coordinator transport is %T, want *backendSlot", c.transport)
	}
	node := slot.node.Load()
	if node == nil {
		t.Fatal("coordinator has no postgres backend")
	}
	return node
}

func (pc *pgCluster) nodeRow(nodeID string) (started time.Time, stopped bool) {
	pc.t.Helper()
	var stoppedAt sql.NullTime
	if err := pc.db.QueryRow(`SELECT started_at, stopped_at FROM cluster_nodes WHERE node_id = $1`, nodeID).Scan(&started, &stoppedAt); err != nil {
		pc.t.Fatalf("read row of %s: %v", nodeID, err)
	}
	return started, stoppedAt.Valid
}

// A blue-green deploy runs the new slot next to the draining old one, both
// with the node's ID. The row must settle on the newer process instead of
// flipping between the two on every heartbeat, and must go back to the old
// process when the new slot is rolled back.
func TestPostgresNewerProcessKeepsTheNodeRow(t *testing.T) {
	pc := newPGCluster(t)
	old := pc.start("slot")
	oldNode := pgNodeOf(t, old)
	newer := pc.start("slot")
	newNode := pgNodeOf(t, newer)
	if !newNode.startedAt.After(oldNode.startedAt) {
		t.Fatalf("new slot registered at %s, not after the old slot at %s", newNode.startedAt, oldNode.startedAt)
	}

	// Twelve heartbeats of each process: an old process that fought for the
	// row would flip it back at least once.
	for deadline := time.Now().Add(12 * testPGTimings.heartbeat); time.Now().Before(deadline); time.Sleep(40 * time.Millisecond) {
		if started, _ := pc.nodeRow(newNode.nodeID); !started.Equal(newNode.startedAt) {
			t.Fatalf("row flipped back to the process started at %s", started)
		}
	}

	newer.Close()
	waitFor(t, 8*testPGTimings.heartbeat, "the old slot to take the row back", func() bool {
		started, stopped := pc.nodeRow(oldNode.nodeID)
		return started.Equal(oldNode.startedAt) && !stopped
	})
}

// A live row claiming an earlier start than this process is not a blue-green
// successor, so this process takes it back instead of yielding.
func TestPostgresNodeReclaimsRowFromOlderProcess(t *testing.T) {
	pc := newPGCluster(t)
	c := pc.start("solo")
	node := pgNodeOf(t, c)
	if _, err := pc.db.Exec(`UPDATE cluster_nodes SET started_at = started_at - interval '1 hour', last_seen = now() WHERE node_id = $1`, node.nodeID); err != nil {
		t.Fatalf("simulate an older process: %v", err)
	}
	waitFor(t, 8*testPGTimings.heartbeat, "the process to reclaim its row", func() bool {
		started, stopped := pc.nodeRow(node.nodeID)
		return started.Equal(node.startedAt) && !stopped
	})
}

// Every heartbeat stores the node's snapshot, registering clears the one an
// earlier process left, and a snapshot the heartbeating process no longer
// refreshes (an older build after a rollback) is not reported.
func TestPostgresHeartbeatStoresTheNodeSnapshot(t *testing.T) {
	pc := newPGCluster(t)
	var current atomic.Pointer[json.RawMessage]
	setSnapshot := func(raw string) {
		value := json.RawMessage(raw)
		current.Store(&value)
	}
	setSnapshot(`{"schema":1,"traffic":{"in_flight":3}}`)
	snapshot := func() json.RawMessage {
		if raw := current.Load(); raw != nil {
			return *raw
		}
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	c, err := startPostgresWith(ctx, pc.prefix+"snap", Options{Enabled: true, DSN: pc.dsn, DB: pc.db, Version: "test", Snapshot: snapshot}, testPGTimings)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(c.Close)
	nodeID := pc.prefix + "snap"

	stored := func() NodeStatus {
		t.Helper()
		nodes, err := c.Nodes(context.Background())
		if err != nil {
			t.Fatalf("nodes: %v", err)
		}
		for _, node := range nodes {
			if node.NodeID == nodeID {
				return node
			}
		}
		t.Fatalf("node %s missing from %+v", nodeID, nodes)
		return NodeStatus{}
	}
	waitFor(t, 8*testPGTimings.heartbeat, "the heartbeat to store the snapshot", func() bool {
		return len(stored().Snapshot) > 0
	})
	node := stored()
	var decoded map[string]any
	if err := json.Unmarshal(node.Snapshot, &decoded); err != nil || decoded["schema"] != float64(1) || node.SnapshotAt == nil {
		t.Fatalf("stored snapshot = %s at %v (%v)", node.Snapshot, node.SnapshotAt, err)
	}

	setSnapshot(`{"schema":1,"traffic":{"in_flight":9}}`)
	waitFor(t, 8*testPGTimings.heartbeat, "the next heartbeat to replace it", func() bool {
		var got struct {
			Traffic struct {
				InFlight int `json:"in_flight"`
			} `json:"traffic"`
		}
		return json.Unmarshal(stored().Snapshot, &got) == nil && got.Traffic.InFlight == 9
	})

	// An older build heartbeats without touching the snapshot: last_seen
	// moves on and the snapshot falls behind it.
	if _, err := pc.db.Exec(`UPDATE cluster_nodes SET snapshot_at = now() - interval '5 minutes' WHERE node_id = $1`, nodeID); err != nil {
		t.Fatal(err)
	}
	current.Store(nil)
	waitFor(t, 8*testPGTimings.heartbeat, "a heartbeat without a snapshot", func() bool {
		var hasSnapshot bool
		if err := pc.db.QueryRow(`SELECT snapshot IS NOT NULL FROM cluster_nodes WHERE node_id = $1`, nodeID).Scan(&hasSnapshot); err != nil {
			t.Fatal(err)
		}
		return !hasSnapshot
	})
	if node := stored(); len(node.Snapshot) != 0 || node.SnapshotAt != nil {
		t.Fatalf("no snapshot should be reported, got %s", node.Snapshot)
	}

	// A stale snapshot left in the row is not reported either.
	if _, err := pc.db.Exec(`UPDATE cluster_nodes SET snapshot = '{"schema":1}', snapshot_at = last_seen - interval '5 minutes' WHERE node_id = $1`, nodeID); err != nil {
		t.Fatal(err)
	}
	if node := stored(); len(node.Snapshot) != 0 {
		t.Fatalf("a snapshot older than the heartbeat must be dropped, got %s", node.Snapshot)
	}

	// A new process of the node starts with no snapshot of its own yet.
	if _, err := pc.db.Exec(`UPDATE cluster_nodes SET snapshot = '{"schema":1}', snapshot_at = now() WHERE node_id = $1`, nodeID); err != nil {
		t.Fatal(err)
	}
	next := pc.start("snap")
	if node := pgNodeOf(t, next); node.startedAt.IsZero() {
		t.Fatal("the new process did not register")
	}
	var cleared bool
	if err := pc.db.QueryRow(`SELECT snapshot IS NULL AND snapshot_at IS NULL FROM cluster_nodes WHERE node_id = $1`, nodeID).Scan(&cleared); err != nil || !cleared {
		t.Fatalf("registering must clear the earlier snapshot: cleared=%v err=%v", cleared, err)
	}
}
