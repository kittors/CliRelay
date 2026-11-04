package enduser

import "context"

// UserBrief names a portal account and nothing more: what another part of the
// panel needs to list a tenant's accounts next to its own settings for them
// (the sensitive-word permissions, for one) without reading their quotas.
type UserBrief struct {
	ID          string `json:"id"`
	Username    string `json:"username"`
	DisplayName string `json:"display_name"`
	Status      string `json:"status"`
}

// ListUserBriefs lists the portal accounts of a tenant, newest first. It checks
// no permission of its own: the caller's route already decided the caller may
// manage something about these accounts, and names are all it hands out.
func (s *Service) ListUserBriefs(ctx context.Context, tenantID string) ([]UserBrief, error) {
	if err := requireUUID(tenantID); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id, username, COALESCE(display_name, ''), COALESCE(status, 'active') FROM end_users WHERE tenant_id = ? ORDER BY created_at DESC`, tenantID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]UserBrief, 0)
	for rows.Next() {
		var brief UserBrief
		if err := rows.Scan(&brief.ID, &brief.Username, &brief.DisplayName, &brief.Status); err != nil {
			return nil, err
		}
		out = append(out, brief)
	}
	return out, rows.Err()
}
