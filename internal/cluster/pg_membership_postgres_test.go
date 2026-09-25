package cluster

import (
	"database/sql"
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
