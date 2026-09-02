package modelconfig

import (
	"fmt"
	"strings"
)

type AuthGroupOwnerMappingRow struct {
	AuthGroup string `json:"auth_group"`
	Owner     string `json:"owner"`
	UpdatedAt string `json:"updated_at"`
}

func NormalizeAuthGroupKey(value string) string {
	return strings.ToLower(strings.Join(strings.Fields(strings.TrimSpace(value)), "-"))
}

func (s Store) ListAuthGroupOwnerMappings() []AuthGroupOwnerMappingRow {
	if s.db == nil {
		return nil
	}
	rows, err := s.db.Query(`SELECT auth_group,owner,updated_at FROM auth_group_model_owner_mappings WHERE tenant_id = ? ORDER BY auth_group`, s.tenantID)
	if err != nil {
		return nil
	}
	defer rows.Close()
	result := make([]AuthGroupOwnerMappingRow, 0)
	for rows.Next() {
		var row AuthGroupOwnerMappingRow
		if rows.Scan(&row.AuthGroup, &row.Owner, &row.UpdatedAt) == nil {
			result = append(result, row)
		}
	}
	return result
}

func (s Store) GetAuthGroupOwnerMapping(authGroup string) (AuthGroupOwnerMappingRow, bool) {
	var row AuthGroupOwnerMappingRow
	if s.db == nil {
		return row, false
	}
	err := s.db.QueryRow(`SELECT auth_group,owner,updated_at FROM auth_group_model_owner_mappings WHERE tenant_id = ? AND auth_group = ?`, s.tenantID, NormalizeAuthGroupKey(authGroup)).Scan(&row.AuthGroup, &row.Owner, &row.UpdatedAt)
	return row, err == nil
}

func (s Store) UpsertAuthGroupOwnerMapping(row AuthGroupOwnerMappingRow) error {
	if s.db == nil {
		return fmt.Errorf("database not initialised")
	}
	row.AuthGroup = NormalizeAuthGroupKey(row.AuthGroup)
	row.Owner = NormalizeModelOwnerValue(row.Owner)
	row.UpdatedAt = nowRFC3339()
	if row.AuthGroup == "" || row.Owner == "" {
		return fmt.Errorf("auth group and owner are required")
	}
	_, err := s.db.Exec(`INSERT INTO auth_group_model_owner_mappings(tenant_id,auth_group,owner,updated_at) VALUES(?,?,?,?) ON CONFLICT(tenant_id,auth_group) DO UPDATE SET owner=excluded.owner,updated_at=excluded.updated_at`, s.tenantID, row.AuthGroup, row.Owner, row.UpdatedAt)
	if err != nil {
		return fmt.Errorf("upsert auth group owner mapping: %w", err)
	}
	return nil
}

func (s Store) DeleteAuthGroupOwnerMapping(authGroup string) error {
	if s.db == nil {
		return fmt.Errorf("database not initialised")
	}
	authGroup = NormalizeAuthGroupKey(authGroup)
	if authGroup == "" {
		return fmt.Errorf("auth group is required")
	}
	_, err := s.db.Exec("DELETE FROM auth_group_model_owner_mappings WHERE tenant_id = ? AND auth_group = ?", s.tenantID, authGroup)
	return err
}
