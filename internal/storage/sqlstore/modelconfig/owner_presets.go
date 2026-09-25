package modelconfig

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/configsync"
)

// ReplaceModelOwnerPresets replaces the tenant's owner presets whatever the
// stored collection version is, as clients without versions expect.
func (s Store) ReplaceModelOwnerPresets(rows []ModelOwnerPresetRow) error {
	_, err := s.ReplaceModelOwnerPresetsExpect(context.Background(), rows, configsync.AnyVersion)
	return err
}

// ReplaceModelOwnerPresetsExpect replaces the tenant's owner presets if the
// collection is still at expected (configsync.AnyVersion: unchecked) and
// returns the new collection version.
func (s Store) ReplaceModelOwnerPresetsExpect(ctx context.Context, rows []ModelOwnerPresetRow, expected int64) (int64, error) {
	if s.db == nil {
		return 0, fmt.Errorf("database not initialised")
	}
	return configsync.WriteCollection(ctx, s.db, configsync.DomainModelOwnerPresets, s.tenantID, expected, func(tx *sql.Tx) error {
		if _, err := tx.Exec("DELETE FROM model_owner_presets WHERE tenant_id = ?", s.tenantID); err != nil {
			return fmt.Errorf("clear owner presets: %w", err)
		}
		now := nowRFC3339()
		for _, row := range rows {
			row.Value = NormalizeModelOwnerValue(row.Value)
			if row.Value == "" {
				continue
			}
			if strings.TrimSpace(row.Label) == "" {
				row.Label = OwnerLabelForValue(row.Value)
			}
			if _, err := tx.Exec(
				`INSERT INTO model_owner_presets (tenant_id, value, label, description, enabled, updated_at)
				 VALUES (?, ?, ?, ?, ?, ?)`,
				s.tenantID, row.Value, row.Label, row.Description, boolToInt(row.Enabled), now,
			); err != nil {
				return fmt.Errorf("insert owner preset: %w", err)
			}
		}
		return nil
	})
}
