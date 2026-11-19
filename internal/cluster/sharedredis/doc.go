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
//   - a command lost to a timeout or a broken connection falls back on its
//     own. A lossy link loses a share of the commands, one dropped packet
//     being enough to miss the timeout, and that must not take the whole
//     node off the shared state;
//   - the client is marked unavailable, so that later requests skip Redis
//     entirely instead of each paying the timeout, when the server cannot be
//     used (a refused dial, a reply such as LOADING or NOAUTH), or when the
//     lost commands make up a large share of a short window or keep failing
//     with nothing succeeding;
//   - a background probe, given a second try when one attempt is lost, brings
//     it back, backing off while the server stays down and while it keeps
//     flapping.
//
// Callers check Available and fall back to their node-local behaviour whenever
// it is false or a command fails. Redis being slow or down must never make a
// request wait longer than the timeout, and must never fail it.
package sharedredis
