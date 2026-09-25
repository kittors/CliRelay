package clusterstate

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/management/jobsnapshot"
)

// ManagementJobs is the management_jobs table, the shared jobsnapshot.Store.
type ManagementJobs struct {
	db        *sql.DB
	lostAfter time.Duration
}

var _ jobsnapshot.Store = (*ManagementJobs)(nil)

// NewManagementJobs returns the store over db.
func NewManagementJobs(db *sql.DB) *ManagementJobs {
	return &ManagementJobs{db: db, lostAfter: jobsnapshot.OwnerLostAfter}
}

// Save upserts snap. The version guard in the conflict clause is what makes
// out-of-order writes harmless: a snapshot only ever replaces an older one.
func (s *ManagementJobs) Save(ctx context.Context, snap jobsnapshot.Snapshot, ttl time.Duration) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO management_jobs (
			id, kind, tenant_id, status, phase, terminal, result, error, owner_node, version,
			created_at, updated_at, expires_at, heartbeat_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, now() + make_interval(secs => ?::double precision), now())
		ON CONFLICT (id) DO UPDATE SET
			status = EXCLUDED.status,
			phase = EXCLUDED.phase,
			terminal = EXCLUDED.terminal,
			result = EXCLUDED.result,
			error = EXCLUDED.error,
			owner_node = EXCLUDED.owner_node,
			version = EXCLUDED.version,
			updated_at = EXCLUDED.updated_at,
			expires_at = EXCLUDED.expires_at,
			heartbeat_at = EXCLUDED.heartbeat_at
		WHERE management_jobs.version < EXCLUDED.version
		  AND management_jobs.kind = EXCLUDED.kind
	`,
		snap.ID, snap.Kind, snap.TenantID, snap.Status, snap.Phase, snap.Terminal,
		nullableBytes(snap.Result), nullableBytes(snap.Error), snap.OwnerNode, snap.Version,
		snap.CreatedAt.UTC(), snap.UpdatedAt.UTC(), seconds(ttl),
	)
	if err != nil {
		return fmt.Errorf("clusterstate: save management job: %w", err)
	}
	return nil
}

func (s *ManagementJobs) Get(ctx context.Context, kind, id string) (jobsnapshot.Snapshot, bool, error) {
	var snap jobsnapshot.Snapshot
	err := s.db.QueryRowContext(ctx, `
		SELECT id, kind, tenant_id, status, phase, terminal, result, error, owner_node, version,
		       created_at, updated_at,
		       NOT terminal AND heartbeat_at < now() - make_interval(secs => ?::double precision)
		  FROM management_jobs
		 WHERE id = ? AND kind = ? AND expires_at > now()
	`, seconds(s.lostAfter), id, kind).Scan(
		&snap.ID, &snap.Kind, &snap.TenantID, &snap.Status, &snap.Phase, &snap.Terminal,
		&snap.Result, &snap.Error, &snap.OwnerNode, &snap.Version,
		&snap.CreatedAt, &snap.UpdatedAt, &snap.OwnerLost,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return jobsnapshot.Snapshot{}, false, nil
	}
	if err != nil {
		return jobsnapshot.Snapshot{}, false, fmt.Errorf("clusterstate: read management job: %w", err)
	}
	return snap, true, nil
}

func (s *ManagementJobs) Touch(ctx context.Context, owner string, ids []string) error {
	for _, id := range ids {
		if _, err := s.db.ExecContext(ctx, `
			UPDATE management_jobs SET heartbeat_at = now()
			 WHERE id = ? AND owner_node = ? AND NOT terminal
		`, id, owner); err != nil {
			return fmt.Errorf("clusterstate: heartbeat management job: %w", err)
		}
	}
	return nil
}

// sweepManagementJobs deletes snapshots past their retention window, the
// shared counterpart of each service dropping jobs 30 minutes after their
// last update.
func sweepManagementJobs(ctx context.Context, db *sql.DB) error {
	if _, err := db.ExecContext(ctx, `DELETE FROM management_jobs WHERE expires_at <= now()`); err != nil {
		return fmt.Errorf("clusterstate: delete expired management jobs: %w", err)
	}
	return nil
}

// nullableBytes stores an absent result as NULL rather than an empty value.
// It also passes plain []byte: a json.RawMessage argument could otherwise be
// encoded as JSON text instead of bytea.
func nullableBytes(b []byte) any {
	if len(b) == 0 {
		return nil
	}
	return []byte(b)
}
