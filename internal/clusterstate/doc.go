// Package clusterstate stores the short-lived state that cluster nodes must
// share so that a request may land on any of them: OAuth login sessions,
// routes of asynchronous upstream tasks, management job snapshots and warmup
// policies.
//
// Every table here is PostgreSQL-only and lives in the runtime database. The
// domain packages define the interfaces (oauth/session.Repo,
// jobsnapshot.Store, warmup.PolicyStore); this package implements them, and
// Janitor expires their rows on the elected leader.
//
// Time comparisons are made in SQL against now(), never against the calling
// node's clock: nodes on different machines drift, the database is the one
// clock they share.
package clusterstate
