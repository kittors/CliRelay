package clusterstate

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// TaskRoute records which credential created an asynchronous upstream task.
// Upstream task ids are scoped to the credential that created them, so every
// poll has to go back to AuthID, whichever node it reaches.
type TaskRoute struct {
	Kind     string
	TaskID   string
	Provider string
	AuthID   string
	TenantID string
	Model    string
}

// TaskRoutes is the async_task_routes table.
type TaskRoutes struct {
	db *sql.DB
}

// NewTaskRoutes returns the store over db.
func NewTaskRoutes(db *sql.DB) *TaskRoutes {
	return &TaskRoutes{db: db}
}

// Remember stores route for ttl, replacing an earlier route of the same id.
func (s *TaskRoutes) Remember(ctx context.Context, route TaskRoute, ttl time.Duration) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO async_task_routes (task_id, kind, provider, auth_id, tenant_id, model, created_at, expires_at)
		VALUES (?, ?, ?, ?, ?, ?, now(), now() + make_interval(secs => ?::double precision))
		ON CONFLICT (task_id) DO UPDATE SET
			kind = EXCLUDED.kind,
			provider = EXCLUDED.provider,
			auth_id = EXCLUDED.auth_id,
			tenant_id = EXCLUDED.tenant_id,
			model = EXCLUDED.model,
			created_at = EXCLUDED.created_at,
			expires_at = EXCLUDED.expires_at
	`, route.TaskID, route.Kind, route.Provider, route.AuthID, route.TenantID, route.Model, seconds(ttl))
	if err != nil {
		return fmt.Errorf("clusterstate: remember task route: %w", err)
	}
	return nil
}

// Lookup returns the live route of taskID.
func (s *TaskRoutes) Lookup(ctx context.Context, kind, taskID string) (TaskRoute, bool, error) {
	route := TaskRoute{Kind: kind, TaskID: taskID}
	err := s.db.QueryRowContext(ctx, `
		SELECT provider, auth_id, tenant_id, model
		  FROM async_task_routes
		 WHERE task_id = ? AND kind = ? AND expires_at > now()
	`, taskID, kind).Scan(&route.Provider, &route.AuthID, &route.TenantID, &route.Model)
	if errors.Is(err, sql.ErrNoRows) {
		return TaskRoute{}, false, nil
	}
	if err != nil {
		return TaskRoute{}, false, fmt.Errorf("clusterstate: read task route: %w", err)
	}
	return route, true, nil
}

// Forget drops the route of a finished task.
func (s *TaskRoutes) Forget(ctx context.Context, kind, taskID string) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM async_task_routes WHERE task_id = ? AND kind = ?`, taskID, kind); err != nil {
		return fmt.Errorf("clusterstate: forget task route: %w", err)
	}
	return nil
}

func sweepTaskRoutes(ctx context.Context, db *sql.DB) error {
	if _, err := db.ExecContext(ctx, `DELETE FROM async_task_routes WHERE expires_at <= now()`); err != nil {
		return fmt.Errorf("clusterstate: delete expired task routes: %w", err)
	}
	return nil
}
