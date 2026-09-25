package clusterstate

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/management/warmup"
)

// WarmupPolicies is the warmup_policies table.
type WarmupPolicies struct {
	db *sql.DB
}

var _ warmup.PolicyStore = (*WarmupPolicies)(nil)

// NewWarmupPolicies returns the store over db.
func NewWarmupPolicies(db *sql.DB) *WarmupPolicies {
	return &WarmupPolicies{db: db}
}

func (s *WarmupPolicies) ListPolicies(ctx context.Context, tenantID string) ([]warmup.StoredPolicy, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT policy::text, revision
		  FROM warmup_policies
		 WHERE (? = '' OR tenant_id = ?)
		 ORDER BY tenant_id, id
	`, tenantID, tenantID)
	if err != nil {
		return nil, fmt.Errorf("clusterstate: list warmup policies: %w", err)
	}
	defer rows.Close()
	var out []warmup.StoredPolicy
	for rows.Next() {
		var raw string
		var sp warmup.StoredPolicy
		if err := rows.Scan(&raw, &sp.Revision); err != nil {
			return nil, fmt.Errorf("clusterstate: list warmup policies: %w", err)
		}
		if err := json.Unmarshal([]byte(raw), &sp.Policy); err != nil {
			return nil, fmt.Errorf("clusterstate: decode warmup policy: %w", err)
		}
		out = append(out, sp)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("clusterstate: list warmup policies: %w", err)
	}
	return out, nil
}

// SavePolicy stores an operator edit. Every edit moves the revision on, which
// is what turns a concurrent run-state write-back into a no-op.
func (s *WarmupPolicies) SavePolicy(ctx context.Context, p warmup.Policy) (int64, error) {
	raw, err := json.Marshal(p)
	if err != nil {
		return 0, fmt.Errorf("clusterstate: encode warmup policy: %w", err)
	}
	var revision int64
	err = s.db.QueryRowContext(ctx, `
		INSERT INTO warmup_policies (tenant_id, id, policy, revision, created_at, updated_at)
		VALUES (?, ?, ?::jsonb, 1, now(), now())
		ON CONFLICT (tenant_id, id) DO UPDATE SET
			policy = EXCLUDED.policy,
			revision = warmup_policies.revision + 1,
			updated_at = now()
		RETURNING revision
	`, p.TenantID, p.ID, string(raw)).Scan(&revision)
	if err != nil {
		return 0, fmt.Errorf("clusterstate: save warmup policy: %w", err)
	}
	return revision, nil
}

func (s *WarmupPolicies) SaveRunState(ctx context.Context, p warmup.Policy, revision int64) (bool, error) {
	raw, err := json.Marshal(p)
	if err != nil {
		return false, fmt.Errorf("clusterstate: encode warmup policy: %w", err)
	}
	res, err := s.db.ExecContext(ctx, `
		UPDATE warmup_policies
		   SET policy = ?::jsonb, updated_at = now()
		 WHERE tenant_id = ? AND id = ? AND revision = ?
	`, string(raw), p.TenantID, p.ID, revision)
	if err != nil {
		return false, fmt.Errorf("clusterstate: save warmup run state: %w", err)
	}
	return affected(res)
}
