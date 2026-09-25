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
