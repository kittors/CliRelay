package enduser

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/quota"
)

func (s *Service) CreateKeyWithPeriodLimits(ctx context.Context, tenantID, endUserID, name string, patch *quota.PeriodSpendingLimitsPatch) (CreateKeyResult, error) {
	var result CreateKeyResult
	if err := requireUUID(tenantID); err != nil {
		return result, err
	}
	if err := requireUUID(endUserID); err != nil {
		return result, err
	}
	patch, err := quota.NormalizePatch(patch)
	if err != nil {
		return result, fmt.Errorf("%w: %v", ErrValidation, err)
	}
	limits := quota.ApplyPatch(quota.PeriodSpendingLimits{}, patch)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return result, err
	}
	defer func() { _ = tx.Rollback() }()
	status, accountLimits, err := effectiveAccountPeriodLimitsTx(ctx, tx, tenantID, endUserID)
	if err != nil {
		return result, err
	}
	if status != "active" {
		return result, fmt.Errorf("%w: cannot create api key for non-active end user", ErrValidation)
	}
	if err := quota.ValidateKeyWithinAccount(limits, accountLimits); err != nil {
		return result, err
	}
	var count int
	_ = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM api_keys WHERE tenant_id = ? AND end_user_id = ? AND disabled = 0`, tenantID, endUserID).Scan(&count)
	isDefault := count == 0
	var plain string
	for attempt := 0; attempt < 8; attempt++ {
		plain, err = GenerateAPIKey()
		if err != nil {
			return result, err
		}
		var exists int
		if err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM api_keys WHERE key = ?`, plain).Scan(&exists); err != nil {
			return result, err
		}
		if exists == 0 {
			break
		}
	}
	id := uuid.NewString()
	now := time.Now().UTC().Format(time.RFC3339)
	name = strings.TrimSpace(name)
	if name == "" {
		return result, fmt.Errorf("%w: key name is required", ErrValidation)
	}
	if err = ensureUniqueKeyName(ctx, tx, tenantID, endUserID, name, ""); err != nil {
		return result, err
	}
	if _, err = tx.ExecContext(ctx, `
		INSERT INTO api_keys (key, id, name, disabled, end_user_id, is_default, tenant_id, created_at, updated_at,
			permission_profile_id, daily_limit, total_quota, spending_limit, daily_spending_limit, five_hour_spending_limit, weekly_spending_limit, monthly_spending_limit,
			concurrency_limit, rpm_limit, tpm_limit, allowed_models, allowed_channels, allowed_channel_groups, system_prompt)
		VALUES (?, ?, ?, 0, ?, ?, ?, ?, ?,
			'', 0, 0, 0, ?, ?, ?, ?, 0, 0, 0, '[]', '[]', '[]', '')
	`, plain, id, name, endUserID, isDefault, tenantID, now, now,
		limits.Day, limits.FiveHour, limits.Week, limits.Month); err != nil {
		return result, err
	}
	if err = tx.Commit(); err != nil {
		return result, err
	}
	result.APIKey = APIKey{
		ID: id, TenantID: tenantID, EndUserID: endUserID, Name: name, IsDefault: isDefault,
		KeyMasked: MaskAPIKey(plain), CreatedAt: now, UpdatedAt: now,
		DailySpendingLimit: limits.Day, PeriodSpendingLimits: limits,
	}
	result.PlaintextKey = plain
	return result, nil
}

func (s *Service) UpdateKey(ctx context.Context, tenantID, endUserID, keyID string, name *string, patch *quota.PeriodSpendingLimitsPatch) error {
	if err := requireUUID(tenantID); err != nil {
		return err
	}
	if err := requireUUID(endUserID); err != nil {
		return err
	}
	if err := requireUUID(keyID); err != nil {
		return err
	}
	patch, err := quota.NormalizePatch(patch)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrValidation, err)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	_, accountLimits, err := effectiveAccountPeriodLimitsTx(ctx, tx, tenantID, endUserID)
	if err != nil {
		return err
	}
	var currentName string
	var current quota.PeriodSpendingLimits
	if err := tx.QueryRowContext(ctx, `
		SELECT name, COALESCE(daily_spending_limit,0), COALESCE(five_hour_spending_limit,0),
		       COALESCE(weekly_spending_limit,0), COALESCE(monthly_spending_limit,0)
		FROM api_keys WHERE tenant_id = ? AND end_user_id = ? AND id = ? AND disabled = 0
	`, tenantID, endUserID, keyID).Scan(&currentName, &current.Day, &current.FiveHour, &current.Week, &current.Month); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		return err
	}
	updated := quota.ApplyPatch(current, patch)
	if err := quota.ValidateKeyWithinAccount(updated, accountLimits); err != nil {
		return err
	}
	if name != nil {
		currentName = strings.TrimSpace(*name)
		if currentName == "" {
			return fmt.Errorf("%w: name required", ErrValidation)
		}
		if err := ensureUniqueKeyName(ctx, tx, tenantID, endUserID, currentName, keyID); err != nil {
			return err
		}
	}
	res, err := tx.ExecContext(ctx, `
		UPDATE api_keys SET name = ?, daily_spending_limit = ?, five_hour_spending_limit = ?,
		       weekly_spending_limit = ?, monthly_spending_limit = ?, updated_at = ?
		WHERE tenant_id = ? AND end_user_id = ? AND id = ? AND disabled = 0
	`, currentName, updated.Day, updated.FiveHour, updated.Week, updated.Month,
		time.Now().UTC().Format(time.RFC3339), tenantID, endUserID, keyID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return tx.Commit()
}

func effectiveAccountPeriodLimitsTx(ctx context.Context, tx *sql.Tx, tenantID, endUserID string) (string, quota.PeriodSpendingLimits, error) {
	var status, profileID string
	var limits quota.PeriodSpendingLimits
	query := `SELECT status, COALESCE(permission_profile_id,''), COALESCE(daily_spending_limit,0),
	                 COALESCE(five_hour_spending_limit,0), COALESCE(weekly_spending_limit,0), COALESCE(monthly_spending_limit,0)
	          FROM end_users WHERE id = ? AND tenant_id = ? FOR UPDATE`
	err := tx.QueryRowContext(ctx, query, endUserID, tenantID).Scan(&status, &profileID, &limits.Day, &limits.FiveHour, &limits.Week, &limits.Month)
	if err != nil {
		err = tx.QueryRowContext(ctx, strings.TrimSuffix(query, " FOR UPDATE"), endUserID, tenantID).Scan(&status, &profileID, &limits.Day, &limits.FiveHour, &limits.Week, &limits.Month)
	}
	if errors.Is(err, sql.ErrNoRows) {
		return "", quota.PeriodSpendingLimits{}, ErrNotFound
	}
	if err != nil {
		return "", quota.PeriodSpendingLimits{}, err
	}
	if profileID != "" {
		err = tx.QueryRowContext(ctx, `SELECT COALESCE(daily_spending_limit,0), COALESCE(five_hour_spending_limit,0),
			COALESCE(weekly_spending_limit,0), COALESCE(monthly_spending_limit,0)
			FROM api_key_permission_profiles WHERE tenant_id = ? AND id = ?`, tenantID, profileID).
			Scan(&limits.Day, &limits.FiveHour, &limits.Week, &limits.Month)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return "", quota.PeriodSpendingLimits{}, err
		}
	}
	return status, limits, nil
}

func capOwnedKeyPeriodLimitsTx(ctx context.Context, tx *sql.Tx, tenantID, endUserID string) ([]quota.CappedKey, error) {
	_, account, err := effectiveAccountPeriodLimitsTx(ctx, tx, tenantID, endUserID)
	if err != nil {
		return nil, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT id, COALESCE(daily_spending_limit,0), COALESCE(five_hour_spending_limit,0),
		COALESCE(weekly_spending_limit,0), COALESCE(monthly_spending_limit,0)
		FROM api_keys WHERE tenant_id = ? AND end_user_id = ? ORDER BY id ASC`, tenantID, endUserID)
	if err != nil {
		return nil, err
	}
	type keyLimits struct {
		id     string
		limits quota.PeriodSpendingLimits
	}
	keys := make([]keyLimits, 0)
	for rows.Next() {
		var item keyLimits
		if err := rows.Scan(&item.id, &item.limits.Day, &item.limits.FiveHour, &item.limits.Week, &item.limits.Month); err != nil {
			_ = rows.Close()
			return nil, err
		}
		keys = append(keys, item)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	capped := make([]quota.CappedKey, 0)
	for _, item := range keys {
		updated := item.limits
		for _, period := range quota.OrderedPeriods {
			from, ceiling := updated.Value(period), account.Value(period)
			if from <= 0 || ceiling <= 0 || from <= ceiling {
				continue
			}
			switch period {
			case quota.PeriodFiveHour:
				updated.FiveHour = ceiling
			case quota.PeriodDay:
				updated.Day = ceiling
			case quota.PeriodWeek:
				updated.Week = ceiling
			case quota.PeriodMonth:
				updated.Month = ceiling
			}
			capped = append(capped, quota.CappedKey{ID: item.id, Period: period, From: from, To: ceiling})
		}
		if updated != item.limits {
			if _, err := tx.ExecContext(ctx, `UPDATE api_keys SET daily_spending_limit=?, five_hour_spending_limit=?, weekly_spending_limit=?, monthly_spending_limit=?, updated_at=? WHERE tenant_id=? AND id=?`,
				updated.Day, updated.FiveHour, updated.Week, updated.Month, time.Now().UTC().Format(time.RFC3339), tenantID, item.id); err != nil {
				return nil, err
			}
		}
	}
	return capped, nil
}

func quotaPkgResolveLegacyDay(legacy *float64, patch *quota.PeriodSpendingLimitsPatch) (*quota.PeriodSpendingLimitsPatch, error) {
	resolved, err := quota.ResolveLegacyDay(legacy, patch)
	if errors.Is(err, quota.ErrPeriodDayLegacyConflict) {
		return nil, ErrPeriodDayLegacyConflict
	}
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrValidation, err)
	}
	return resolved, nil
}
