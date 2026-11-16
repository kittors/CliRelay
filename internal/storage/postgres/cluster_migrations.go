package postgres

// clusterNodesSQL adds the membership table of multi-instance deployments.
//
// Each node upserts its own row every few seconds; a row counts as active
// while last_seen is recent and stopped_at is empty. Timestamps come from the
// database clock so nodes with drifting clocks still agree on who is alive.
// The leader flag is informational (the leadership lock is an advisory lock),
// and single-node deployments never write here.
const clusterNodesSQL = `
CREATE TABLE IF NOT EXISTS cluster_nodes (
  node_id    TEXT PRIMARY KEY,
  version    TEXT NOT NULL DEFAULT '',
  advertise  TEXT NOT NULL DEFAULT '',
  started_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  last_seen  TIMESTAMPTZ NOT NULL DEFAULT now(),
  leader     BOOLEAN NOT NULL DEFAULT false,
  stopped_at TIMESTAMPTZ
);
`

// clusterNodeSnapshotSQL adds each node's latest resource snapshot (package
// nodemetrics) to its membership row, for the console's node view. Nodes
// write it with every heartbeat. Builds from before this migration leave the
// columns alone, so readers ignore a snapshot much older than the row's
// last_seen. Both columns are nullable without a default: adding them only
// changes the catalog.
const clusterNodeSnapshotSQL = `
ALTER TABLE cluster_nodes ADD COLUMN IF NOT EXISTS snapshot JSONB;
ALTER TABLE cluster_nodes ADD COLUMN IF NOT EXISTS snapshot_at TIMESTAMPTZ;
`
