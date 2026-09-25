package apikey

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/configsync"
)

// ReplaceAll replaces the tenant's API keys with entries whatever the stored
// collection version is (last writer wins), as clients without versions expect.
func (s Store) ReplaceAll(entries []APIKeyRow) error {
	_, err := s.ReplaceAllExpect(context.Background(), entries, configsync.AnyVersion)
	return err
}

// ReplaceAllExpect replaces the tenant's API keys with entries if the api_keys
// collection is still at expected (configsync.AnyVersion: unchecked) and
// returns the new version. The version bump comes first and locks the
// collection, so two replacements run one after the other instead of mixing
// their deletes and inserts; the current rows are read inside the same
// transaction for the same reason.
func (s Store) ReplaceAllExpect(ctx context.Context, entries []APIKeyRow, expected int64) (int64, error) {
	if s.db == nil {
		return 0, fmt.Errorf("database not initialised")
	}
	return configsync.WriteCollection(ctx, s.db, configsync.DomainAPIKeys, s.tenantID, expected, func(tx *sql.Tx) error {
		return s.replaceAllTx(tx, entries)
	})
}

func (s Store) replaceAllTx(tx *sql.Tx, entries []APIKeyRow) error {
	// Preserve end-user ownership across full replace.
	// Prefer stable id, then key text (admin may rename key secret while keeping id).
	type ownership struct {
		id        string
		key       string
		endUserID string
		isDefault bool
	}
	byID := make(map[string]ownership)
	byKey := make(map[string]ownership)
	current, err := s.listTx(tx)
	if err != nil {
		return err
	}
	for _, row := range current {
		key := strings.TrimSpace(row.Key)
		id := strings.TrimSpace(row.ID)
		own := ownership{
			id:        id,
			key:       key,
			endUserID: strings.TrimSpace(row.EndUserID),
			isDefault: row.IsDefault,
		}
		if id != "" {
			byID[id] = own
		}
		if key != "" {
			byKey[key] = own
		}
	}

	// Resolve ownership for each incoming row first, then verify every previously
	// owned end user still ends up with >=1 key. Counting by ID and key separately
	// is unsafe: id=A+key=B would mark both A and B as "kept" while only A survives.
	type resolvedEntry struct {
		row APIKeyRow
	}
	resolved := make([]resolvedEntry, 0, len(entries))
	seenIDs := make(map[string]struct{}, len(entries))
	seenKeys := make(map[string]struct{}, len(entries))
	for _, entry := range entries {
		entry = normalizeRow(entry)
		if entry.Key == "" {
			continue
		}
		if _, dup := seenKeys[entry.Key]; dup {
			return fmt.Errorf("duplicate api key in replace payload")
		}
		seenKeys[entry.Key] = struct{}{}
		prev, ok := byID[entry.ID]
		if !ok {
			prev = byKey[entry.Key]
		}
		if entry.ID == "" {
			if prev.id != "" {
				entry.ID = prev.id
			} else {
				entry.ID = uuid.NewString()
			}
		}
		if _, dup := seenIDs[entry.ID]; dup {
			return fmt.Errorf("duplicate api key id in replace payload")
		}
		seenIDs[entry.ID] = struct{}{}
		// Ownership is not client-authoritative on full replace for existing keys.
		if prev.endUserID != "" {
			entry.EndUserID = prev.endUserID
			entry.IsDefault = prev.isDefault
		} else {
			// Brand-new keys may not carry ownership through generic replace.
			entry.EndUserID = ""
			entry.IsDefault = false
		}
		resolved = append(resolved, resolvedEntry{row: entry})
	}
	ownedBefore := make(map[string]struct{})
	for _, prev := range byID {
		if prev.endUserID != "" {
			ownedBefore[prev.endUserID] = struct{}{}
		}
	}
	for _, prev := range byKey {
		if prev.endUserID != "" {
			ownedBefore[prev.endUserID] = struct{}{}
		}
	}
	if _, err := tx.Exec("DELETE FROM api_keys WHERE tenant_id = ?", s.tenantID); err != nil {
		return err
	}

	stmt, err := tx.Prepare(`INSERT INTO api_keys
		(tenant_id, key, id, name, disabled, permission_profile_id, daily_limit, total_quota, spending_limit, daily_spending_limit, five_hour_spending_limit, weekly_spending_limit, monthly_spending_limit,
		 concurrency_limit, rpm_limit, tpm_limit, allowed_models, allowed_channels, allowed_channel_groups, system_prompt, created_at, updated_at,
		 end_user_id, is_default)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return err
	}
	defer stmt.Close()

	now := time.Now().UTC().Format(time.RFC3339)
	for _, item := range resolved {
		entry := item.row
		if entry.CreatedAt == "" {
			entry.CreatedAt = now
		}
		disabledInt := 0
		if entry.Disabled {
			disabledInt = 1
		}
		isDefault := entry.IsDefault
		var endUserID any
		if entry.EndUserID != "" {
			endUserID = entry.EndUserID
		}
		if _, err := stmt.Exec(
			s.tenantID, entry.Key, entry.ID, entry.Name, disabledInt, entry.PermissionProfileID,
			entry.DailyLimit, entry.TotalQuota, entry.SpendingLimit, entry.DailySpendingLimit,
			entry.PeriodSpendingLimits.FiveHour, entry.PeriodSpendingLimits.Week, entry.PeriodSpendingLimits.Month,
			entry.ConcurrencyLimit, entry.RPMLimit, entry.TPMLimit,
			mustJSONStringList(entry.AllowedModels), mustJSONStringList(entry.AllowedChannels),
			mustJSONStringList(entry.AllowedChannelGroups), entry.SystemPrompt,
			entry.CreatedAt, now, endUserID, isDefault,
		); err != nil {
			return err
		}
	}

	for endUserID := range ownedBefore {
		if err := ensureOwnedActiveKeyAndDefault(tx, s.tenantID, endUserID, now); err != nil {
			return err
		}
	}

	return nil
}

// listTx is List inside a transaction.
func (s Store) listTx(tx *sql.Tx) ([]APIKeyRow, error) {
	rows, err := tx.Query(`SELECT key, name, disabled, id, daily_limit, total_quota,
		permission_profile_id, spending_limit, daily_spending_limit, five_hour_spending_limit, weekly_spending_limit, monthly_spending_limit, concurrency_limit, rpm_limit, tpm_limit,
		allowed_models, allowed_channels, allowed_channel_groups, system_prompt, created_at, updated_at,
		end_user_id, is_default
		FROM api_keys WHERE tenant_id = ? ORDER BY created_at ASC`, s.tenantID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := scanAPIKeyRows(rows)
	for i := range items {
		items[i].TenantID = s.tenantID
	}
	return items, rows.Err()
}

// execKeyWrite runs one statement that writes API keys, bumping the api_keys
// collection version and announcing the change in the same transaction. A
// statement that matched no row commits nothing.
func (s Store) execKeyWrite(query string, args ...any) (sql.Result, error) {
	ctx := context.Background()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	result, err := tx.ExecContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	if affected, _ := result.RowsAffected(); affected == 0 {
		return result, nil
	}
	return result, configsync.BumpAndCommit(ctx, tx, configsync.DomainAPIKeys, s.tenantID)
}
