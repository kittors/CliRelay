// Package cluster coordinates multiple CliRelay processes that share one
// PostgreSQL database.
//
// A deployment is either a single node (the default, cluster.enabled=false)
// or a cluster of nodes behind DNS/nginx that share the primary database.
// Every API in this package has a single-node behaviour that keeps the
// pre-cluster semantics exactly: the node is always the leader, the active
// node count is 1 and published events go nowhere. Callers therefore never
// branch on "am I in a cluster"; they ask the coordinator.
//
// Cross-node signalling uses PostgreSQL LISTEN/NOTIFY. Notifications are not
// durable: a node that is disconnected from the database misses everything
// published meanwhile. When the listener (re)connects, the coordinator
// delivers a synthetic event with Resync=true to every subscriber, and a
// subscriber must then reload whatever it caches instead of trusting that it
// saw every change. Payloads carry identifiers and versions, never the data
// itself, so a lost or duplicated notification can only delay a reload.
//
// Leadership is a PostgreSQL session advisory lock held on a dedicated
// connection. It exists for periodic maintenance that must run once per
// cluster (retention cleanup, upstream probes, catalog syncs). It is not a
// correctness primitive for data: anything that must not run twice
// concurrently takes its own advisory lock or version check, because
// leadership can move while a task is still finishing on the old leader.
package cluster
