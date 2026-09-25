// Package configsync keeps management-side configuration consistent across
// the nodes of a cluster.
//
// Every configuration write runs in a database transaction that also queues a
// cluster.ConfigEvent on cluster.TopicConfig naming the domain, tenant and key
// that changed. PostgreSQL delivers the event only when the transaction
// commits, and each receiving node reloads just that domain from the database
// (see Dispatcher). Events carry identifiers and versions, never data, so a
// lost or duplicated event can only delay a reload.
//
// Optimistic versions make a write that was computed from stale data fail
// instead of silently overwriting a newer change:
//
//   - runtime_settings and routing_config carry a version per row;
//   - collections that the management API replaces as a whole (api keys,
//     permission profiles, proxy pool, model owner presets, cc-switch import
//     configs) carry one counter per tenant in config_versions, bumped by every
//     write to the collection.
//
// In single-node mode publishing is a no-op and no dispatcher runs; the only
// visible effect is the version bookkeeping.
package configsync
