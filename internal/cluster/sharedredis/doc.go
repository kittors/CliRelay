// Package sharedredis is the handle on the Redis instance shared by every node
// of a CliRelay cluster.
//
// The cluster Redis holds state that must be global rather than per process:
// rate-limit windows, concurrency leases, session affinity bindings and
// synthetic session identifiers. It is a different instance from the
// top-level redis block, which stays node-local. In production it sits on a
// third machine reached over the public network with mutual TLS, so this client
// is built for a link that can be slow or absent:
//
//   - every request-path command is bounded by a short timeout (200ms by
//     default) and is never retried;
//   - a connectivity failure marks the client unavailable at once, so later
//     requests skip Redis entirely instead of each paying the timeout;
//   - a background probe brings it back, backing off while the server stays
//     down and while it keeps flapping.
//
// Callers check Available and fall back to their node-local behaviour whenever
// it is false or a command fails. Redis being slow or down must never make a
// request wait longer than the timeout, and must never fail it.
package sharedredis
