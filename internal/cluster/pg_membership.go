package cluster

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	log "github.com/sirupsen/logrus"
)

// Membership lives in cluster_nodes, one row per node ID. Every timestamp is
// the database's now(), so nodes whose clocks drift still agree on who is
// alive. A row is active while it was refreshed within the active window and
// has not been marked stopped.

const registerNodeSQL = `
WITH previous AS (
	SELECT stopped_at IS NULL AND last_seen > now() - interval '20 seconds' AS active
	  FROM cluster_nodes
	 WHERE node_id = $1
), registered AS (
	INSERT INTO cluster_nodes (node_id, version, advertise, started_at, last_seen, leader, stopped_at)
	VALUES ($1, $2, $3, now(), now(), false, NULL)
	ON CONFLICT (node_id) DO UPDATE SET
		version    = EXCLUDED.version,
		advertise  = EXCLUDED.advertise,
		started_at = EXCLUDED.started_at,
		last_seen  = EXCLUDED.last_seen,
		leader     = false,
		stopped_at = NULL
	RETURNING started_at
)
SELECT started_at, COALESCE((SELECT active FROM previous), false) FROM registered`

const heartbeatSQL = `
UPDATE cluster_nodes
   SET last_seen = now(), leader = $3, version = $4, advertise = $5, stopped_at = NULL
 WHERE node_id = $1 AND started_at = $2`

const reclaimNodeSQL = `
INSERT INTO cluster_nodes (node_id, version, advertise, started_at, last_seen, leader, stopped_at)
VALUES ($1, $3, $4, $2, now(), $5, NULL)
ON CONFLICT (node_id) DO UPDATE SET
	version    = EXCLUDED.version,
	advertise  = EXCLUDED.advertise,
	started_at = EXCLUDED.started_at,
	last_seen  = EXCLUDED.last_seen,
	leader     = EXCLUDED.leader,
	stopped_at = NULL`

// inspectNodeSQL reads the row of a node ID whose heartbeat matched nothing,
// to tell a newer process of the same node (a blue-green deploy) from a row
// that was stopped, pruned or left behind.
const inspectNodeSQL = `
SELECT started_at, stopped_at IS NULL AND last_seen > now() - interval '20 seconds'
  FROM cluster_nodes
 WHERE node_id = $1`

const activeNodeCountSQL = `
SELECT count(*) FROM cluster_nodes
 WHERE stopped_at IS NULL AND last_seen > now() - interval '20 seconds'`

// Only the lock holder is the leader, so a leader flag left on another row
// belongs to a node that died or was cut off before it could clear it.
const clearStaleLeaderFlagsSQL = `UPDATE cluster_nodes SET leader = false WHERE leader AND node_id <> $1`

const pruneNodesSQL = `DELETE FROM cluster_nodes WHERE last_seen < now() - interval '7 days'`

const markStoppedSQL = `
UPDATE cluster_nodes SET stopped_at = now(), leader = false
 WHERE node_id = $1 AND started_at = $2`

const listNodesSQL = `
SELECT node_id, version, advertise, started_at, last_seen, leader,
       stopped_at IS NULL AND last_seen > now() - interval '20 seconds' AS active
  FROM cluster_nodes
 ORDER BY node_id`

// warnEvery throttles warnings that would otherwise repeat on every
// heartbeat while a condition lasts.
const warnEvery = time.Minute

func (p *pgNode) register(ctx context.Context) error {
	var stillActive bool
	if err := p.db.QueryRowContext(ctx, registerNodeSQL, p.nodeID, p.version, p.advertise).Scan(&p.startedAt, &stillActive); err != nil {
		return fmt.Errorf("register node %s: %w", p.nodeID, err)
	}
	if stillActive {
		// A blue-green deploy starts the new slot while the old one drains,
		// and both carry the node's ID. The older process yields the row
		// (see reclaim) and complains only if it keeps running afterwards.
		log.Infof("cluster: node id %q was active until this process started; expected during a "+
			"blue-green deploy or right after a crash, and the older process yields the membership row", p.nodeID)
	}
	return nil
}

func (p *pgNode) runHeartbeat(ctx context.Context) {
	defer close(p.heartbeatDone)
	ticker := time.NewTicker(p.t.heartbeat)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case <-p.kick:
		}
		beatCtx, cancel := context.WithTimeout(ctx, p.t.heartbeat)
		p.heartbeat(beatCtx)
		cancel()
	}
}

// heartbeat refreshes this node's row and the cached active node count.
func (p *pgNode) heartbeat(ctx context.Context) {
	leader := p.c.IsLeader()
	result, err := p.db.ExecContext(ctx, heartbeatSQL, p.nodeID, p.startedAt, leader, p.version, p.advertise)
	if err != nil {
		if ctx.Err() == nil && time.Since(p.lastHeartbeatWarn) >= warnEvery {
			p.lastHeartbeatWarn = time.Now()
			log.Warnf("cluster: heartbeat of node %s failed: %v", p.nodeID, err)
		}
		return
	}
	if updated, errRows := result.RowsAffected(); errRows == nil && updated == 0 {
		p.reclaim(ctx, leader)
	}
	if leader {
		if _, err := p.db.ExecContext(ctx, clearStaleLeaderFlagsSQL, p.nodeID); err != nil {
			log.Debugf("cluster: clear stale leader flags: %v", err)
		}
		if _, err := p.db.ExecContext(ctx, pruneNodesSQL); err != nil {
			log.Debugf("cluster: prune departed nodes: %v", err)
		}
	}
	p.refreshActiveCount(ctx)
}

// reclaim handles a heartbeat that matched no row. A newer live process with
// the same node ID keeps the row: that is the new slot of a blue-green deploy
// while this one drains, and fighting over the row would only flip it back
// and forth. Any other row (stopped by a rolled-back slot, pruned after an
// outage, left by a crashed process, or held by an older live process) is
// taken back.
func (p *pgNode) reclaim(ctx context.Context, leader bool) {
	var rowStarted time.Time
	var rowActive bool
	err := p.db.QueryRowContext(ctx, inspectNodeSQL, p.nodeID).Scan(&rowStarted, &rowActive)
	switch {
	case err == nil && rowActive && rowStarted.After(p.startedAt):
		p.yieldTo(rowStarted)
		return
	case err != nil && !errors.Is(err, sql.ErrNoRows):
		log.Debugf("cluster: inspect membership row: %v", err)
		return
	}
	olderLive := err == nil && rowActive
	p.supersededAt = time.Time{}
	if time.Since(p.lastConflictWarn) >= warnEvery {
		p.lastConflictWarn = time.Now()
		if olderLive {
			log.Errorf("cluster: an older process keeps rewriting the membership row of node %q; "+
				"if another process uses the same node id, give each process a unique cluster.node-id", p.nodeID)
		} else {
			log.Warnf("cluster: took back the membership row of node %q after it was stopped or removed "+
				"(expected after a rolled-back deploy or a database outage)", p.nodeID)
		}
	}
	if _, err := p.db.ExecContext(ctx, reclaimNodeSQL, p.nodeID, p.startedAt, p.version, p.advertise, leader); err != nil {
		log.Debugf("cluster: reclaim membership row: %v", err)
	}
}

// yieldTo leaves the membership row to a newer process of the same node ID.
// A draining slot exits within its shutdown grace, so one still running long
// after that is a second process misconfigured with this node ID.
func (p *pgNode) yieldTo(newerStarted time.Time) {
	now := time.Now()
	if p.supersededAt.IsZero() {
		p.supersededAt = now
		log.Infof("cluster: a newer process registered node id %q at %s; this process leaves the "+
			"membership row to it while it drains", p.nodeID, newerStarted.Format(time.RFC3339))
		return
	}
	if now.Sub(p.supersededAt) >= p.t.supersededGrace && now.Sub(p.lastConflictWarn) >= warnEvery {
		p.lastConflictWarn = now
		log.Errorf("cluster: node id %q has belonged to a newer process for %s and this process is still "+
			"running; unless it is a slot being drained, give each process a unique cluster.node-id",
			p.nodeID, now.Sub(p.supersededAt).Round(time.Second))
	}
}

func (p *pgNode) refreshActiveCount(ctx context.Context) {
	var active int64
	if err := p.db.QueryRowContext(ctx, activeNodeCountSQL).Scan(&active); err != nil {
		// Keep the last known count: while the database is unreachable this
		// node cannot see its peers, and assuming it is alone would loosen
		// every limit that is divided by the node count.
		log.Debugf("cluster: count active nodes: %v", err)
		return
	}
	p.c.activeNodes.Store(active)
}

func (p *pgNode) markStopped(ctx context.Context) {
	if _, err := p.db.ExecContext(ctx, markStoppedSQL, p.nodeID, p.startedAt); err != nil {
		log.Warnf("cluster: mark node %s stopped: %v", p.nodeID, err)
	}
}

func (p *pgNode) nodes(ctx context.Context) ([]NodeStatus, error) {
	rows, err := p.db.QueryContext(ctx, listNodesSQL)
	if err != nil {
		return nil, fmt.Errorf("cluster: list nodes: %w", err)
	}
	defer rows.Close()
	out := make([]NodeStatus, 0, 2)
	for rows.Next() {
		var node NodeStatus
		if err := rows.Scan(&node.NodeID, &node.Version, &node.Advertise, &node.StartedAt, &node.LastSeen, &node.Leader, &node.Active); err != nil {
			return nil, fmt.Errorf("cluster: scan node: %w", err)
		}
		// The flag of a node that stopped heartbeating is stale.
		node.Leader = node.Leader && node.Active
		node.Self = node.NodeID == p.nodeID
		out = append(out, node)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("cluster: list nodes: %w", err)
	}
	return out, nil
}
