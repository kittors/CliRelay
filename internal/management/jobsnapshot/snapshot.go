// Package jobsnapshot shares the progress of console jobs between cluster
// nodes.
//
// Image, video and model tests and AI account status refreshes run as
// background jobs on the node that accepted them, and the panel polls for
// their progress. Behind a load balancer the poll can reach a different node,
// which has never seen the job. The running node therefore publishes a
// snapshot of the job when it starts, as it progresses and when it ends, and
// any node answers a poll from the latest snapshot.
//
// Publishing is only wired up in cluster mode. A single node keeps serving
// polls from memory exactly as before.
package jobsnapshot

import (
	"context"
	"time"
)

const (
	// HeartbeatInterval is how often a node records that its unfinished jobs
	// are still running.
	HeartbeatInterval = 15 * time.Second
	// OwnerLostAfter is how long an unfinished job may go without a heartbeat
	// before readers report that the node running it is gone.
	OwnerLostAfter = 60 * time.Second
	// FlushInterval bounds how often progress of one job is written. The
	// first and the final snapshot are always written at once.
	FlushInterval = time.Second
	// MaxResultBytes caps a shared result. Image results carry base64 image
	// data; a result over the cap is not copied into the database, and nodes
	// other than the one that ran the job report it as unavailable.
	MaxResultBytes = 32 << 20
)

// Snapshot is the shared view of one job. Status, Phase, Result and Error
// are the owning service's own vocabulary and encoding.
type Snapshot struct {
	ID        string
	Kind      string
	TenantID  string
	Status    string
	Phase     string
	Terminal  bool
	Result    []byte
	Error     []byte
	OwnerNode string
	// Version orders snapshots of one job. A store never replaces a snapshot
	// with one of a lower or equal version, so a delayed progress write
	// cannot overwrite the final state.
	Version   int64
	CreatedAt time.Time
	UpdatedAt time.Time
	// OwnerLost is set on read when the job is unfinished and the node
	// running it has stopped heartbeating. The job can no longer finish.
	OwnerLost bool
}

// Store persists snapshots. The PostgreSQL implementation lives in
// internal/clusterstate; MemoryStore serves tests.
type Store interface {
	// Save writes snap unless the stored snapshot has an equal or higher
	// version. A save also refreshes the owner heartbeat and restarts the
	// retention window of ttl.
	Save(ctx context.Context, snap Snapshot, ttl time.Duration) error
	// Get returns the snapshot of job id of kind, unless it is missing or
	// past its retention window.
	Get(ctx context.Context, kind, id string) (Snapshot, bool, error)
	// Touch refreshes the heartbeat of owner's unfinished jobs ids.
	Touch(ctx context.Context, owner string, ids []string) error
}
