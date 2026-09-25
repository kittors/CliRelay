package usage

import "github.com/router-for-me/CLIProxyAPI/v6/internal/cluster"

// redisUsageSnapshotKey is the key the in-memory statistics snapshot is saved
// under in the top-level Redis.
//
// That Redis is meant to be node-local, but nothing stops the nodes of a
// cluster from pointing at one instance, and with a single fixed key each
// node's periodic save would overwrite the others' snapshots. In cluster mode
// the key therefore carries the node ID. It is resolved on every use:
// StartService installs the cluster coordinator (cluster.Prepare) before the
// snapshot is first loaded. A single node keeps the historical key, so the
// snapshot it saved before this change still loads.
func redisUsageSnapshotKey() string {
	if c := cluster.Default(); c.Enabled() {
		return redisUsageKey + ":" + c.NodeID()
	}
	return redisUsageKey
}
