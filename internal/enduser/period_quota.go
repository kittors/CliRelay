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
	sqlapikey "github.com/router-for-me/CLIProxyAPI/v6/internal/storage/sqlstore/apikey"
)

// 账号下 Key 的创建与修改。Key 的子额度矩阵每一格都不能超过账号同一格的生效额度
// （模板覆盖后），校验与写入在同一事务中、在锁住账号行之后进行，账号同时降额也不会
// 留下超出天花板的 Key。

func (s *Service) CreateKeyWithPeriodLimits(ctx context.Context, tenantID, endUserID, name string, patch *quota.PeriodSpendingLimitsPatch) (CreateKeyResult, error) {
	return s.CreateKeyWithPatch(ctx, tenantID, endUserID, KeyPatch{Name: &name, Period: patch})
}

// CreateKeyWithPatch 为账号创建一把 Key，可同时设置子额度与按模型规则。
func (s *Service) CreateKeyWithPatch(ctx context.Context, tenantID, endUserID string, patch KeyPatch) (CreateKeyResult, error) {
	var result CreateKeyResult
	if err := requireUUID(tenantID); err != nil {
		return result, err
	}
	if err := requireUUID(endUserID); err != nil {
		return result, err
	}
	name := ""
	if patch.Name != nil {
		name = strings.TrimSpace(*patch.Name)
	}
	if name == "" {
		return result, fmt.Errorf("%w: key name is required", ErrValidation)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return result, err
	}
	defer func() { _ = tx.Rollback() }()
	account, err := effectiveAccountTx(ctx, tx, tenantID, endUserID, true)
	if err != nil {
		return result, err
	}
	if account.Status != "active" {
		return result, fmt.Errorf("%w: cannot create api key for non-active end user", ErrValidation)
	}
	matrix, policy, err := planKeyQuota(quota.Matrix{}, quota.AccessPolicy{}, patch, account)
	if err != nil {
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
	if err = ensureUniqueKeyName(ctx, tx, tenantID, endUserID, name, ""); err != nil {
		return result, err
	}
	cols := quota.OwnedKeyColumnLayout.ColumnsFromMatrix(matrix)
	if _, err = tx.ExecContext(ctx, `
		INSERT INTO api_keys (key, id, name, disabled, end_user_id, is_default, tenant_id, created_at, updated_at,
			permission_profile_id, daily_limit, total_quota, spending_limit, daily_spending_limit, five_hour_spending_limit, weekly_spending_limit, monthly_spending_limit,
			concurrency_limit, rpm_limit, tpm_limit, allowed_models, allowed_channels, allowed_channel_groups, system_prompt, access_policy)
		VALUES (?, ?, ?, 0, ?, ?, ?, ?, ?,
			'', 0, 0, 0, ?, ?, ?, ?, 0, 0, 0, '[]', '[]', '[]', '', ?)
	`, plain, id, name, endUserID, isDefault, tenantID, now, now,
		cols.Period.Day, cols.Period.FiveHour, cols.Period.Week, cols.Period.Month, quota.EncodeAccessPolicy(policy)); err != nil {
		return result, err
	}
	if err = commitKeyChange(ctx, tx, tenantID); err != nil {
		return result, err
	}
	result.APIKey = APIKey{
		ID: id, TenantID: tenantID, EndUserID: endUserID, Name: name, IsDefault: isDefault,
		KeyMasked: MaskAPIKey(plain), CreatedAt: now, UpdatedAt: now,
		DailySpendingLimit: cols.Period.Day, PeriodSpendingLimits: cols.Period,
		QuotaLimits: matrix, ModelRules: nonNilRules(policy.ModelRules),
	}
	result.PlaintextKey = plain
	return result, nil
}

func (s *Service) UpdateKey(ctx context.Context, tenantID, endUserID, keyID string, name *string, patch *quota.PeriodSpendingLimitsPatch) error {
	return s.UpdateKeyWithPatch(ctx, tenantID, endUserID, keyID, KeyPatch{Name: name, Period: patch})
}

// UpdateKeyWithPatch 修改账号下一把 Key 的名称、子额度或按模型规则。
func (s *Service) UpdateKeyWithPatch(ctx context.Context, tenantID, endUserID, keyID string, patch KeyPatch) error {
	if err := requireUUID(tenantID); err != nil {
		return err
	}
	if err := requireUUID(endUserID); err != nil {
		return err
	}
	if err := requireUUID(keyID); err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	account, err := effectiveAccountTx(ctx, tx, tenantID, endUserID, true)
	if err != nil {
		return err
	}
	var currentName, policyJSON string
	var disabledInt int
	var period quota.PeriodSpendingLimits
	// 已暂停的 Key 也能改名、调额度与恢复；已删除（密钥被作废成 sk-deleted-…）的不行。
	if err := tx.QueryRowContext(ctx, `
		SELECT name, disabled, COALESCE(daily_spending_limit,0), COALESCE(five_hour_spending_limit,0),
		       COALESCE(weekly_spending_limit,0), COALESCE(monthly_spending_limit,0), COALESCE(access_policy,'{}')
		FROM api_keys WHERE tenant_id = ? AND end_user_id = ? AND id = ? AND key NOT LIKE 'sk-deleted-%'
	`, tenantID, endUserID, keyID).Scan(&currentName, &disabledInt, &period.Day, &period.FiveHour, &period.Week, &period.Month, &policyJSON); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		return err
	}
	currentPolicy := decodeAccountPolicy(keyID, policyJSON)
	matrix, policy, err := planKeyQuota(sqlapikey.OwnedKeyQuotaMatrix(period, currentPolicy), currentPolicy, patch, account)
	if err != nil {
		return err
	}
	if patch.Name != nil {
		currentName = strings.TrimSpace(*patch.Name)
		if currentName == "" {
			return fmt.Errorf("%w: name required", ErrValidation)
		}
		if err := ensureUniqueKeyName(ctx, tx, tenantID, endUserID, currentName, keyID); err != nil {
			return err
		}
	}
	cols := quota.OwnedKeyColumnLayout.ColumnsFromMatrix(matrix)
	now := time.Now().UTC().Format(time.RFC3339)
	disabled := disabledInt != 0
	if patch.Disabled != nil {
		disabled = *patch.Disabled
	}
	if disabled && disabledInt == 0 {
		var active int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM api_keys WHERE tenant_id = ? AND end_user_id = ? AND disabled = 0`, tenantID, endUserID).Scan(&active); err != nil {
			return err
		}
		if active <= 1 {
			return ErrLastKey
		}
	}
	nextDisabled := 0
	if disabled {
		nextDisabled = 1
	}
	res, err := tx.ExecContext(ctx, `
		UPDATE api_keys SET name = ?, disabled = ?, daily_spending_limit = ?, five_hour_spending_limit = ?,
		       weekly_spending_limit = ?, monthly_spending_limit = ?, access_policy = ?, updated_at = ?
		WHERE tenant_id = ? AND end_user_id = ? AND id = ? AND key NOT LIKE 'sk-deleted-%'
	`, currentName, nextDisabled, cols.Period.Day, cols.Period.FiveHour, cols.Period.Week, cols.Period.Month, quota.EncodeAccessPolicy(policy),
		now, tenantID, endUserID, keyID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	if (disabledInt != 0) != disabled {
		// 暂停的 Key 不能是默认 Key；暂停或恢复后重新确定默认 Key。
		if err := sqlapikey.EnsureOwnedActiveKeyAndDefault(tx, tenantID, endUserID, now); err != nil {
			return err
		}
	}
	return commitKeyChange(ctx, tx, tenantID)
}

// planKeyQuota 计算一把 Key 修改后的子额度矩阵与策略，并校验不超过账号天花板。
func planKeyQuota(current quota.Matrix, policy quota.AccessPolicy, patch KeyPatch, account effectiveAccount) (quota.Matrix, quota.AccessPolicy, error) {
	normalizedPeriod, err := quota.NormalizePatch(patch.Period)
	if err != nil {
		return quota.Matrix{}, quota.AccessPolicy{}, fmt.Errorf("%w: %v", ErrValidation, err)
	}
	matrixPatch := patch.QuotaLimits.Clone()
	if err := mergeLegacyPeriod(matrixPatch, normalizedPeriod); err != nil {
		return quota.Matrix{}, quota.AccessPolicy{}, err
	}
	next, err := quota.ApplyMatrixPatch(current, matrixPatch)
	if err != nil {
		return quota.Matrix{}, quota.AccessPolicy{}, fmt.Errorf("%w: quota-limits.%v", ErrValidation, err)
	}
	if err := quota.ValidateKeyMatrixWithinAccount(next, account.Matrix); err != nil {
		return quota.Matrix{}, quota.AccessPolicy{}, err
	}
	nextPolicy := policy.Clone()
	nextPolicy.Limits = quota.OwnedKeyColumnLayout.StripColumnCells(next)
	if patch.ModelRules != nil {
		nextPolicy.ModelRules = *patch.ModelRules
	}
	nextPolicy.ModelRoutes = nil
	nextPolicy, err = quota.NormalizeAccessPolicy(nextPolicy, quota.OwnedKeyColumnLayout)
	if err != nil {
		return quota.Matrix{}, quota.AccessPolicy{}, err
	}
	if err := validatePolicyTargetsAllowed(nextPolicy, account.AllowedModels); err != nil {
		return quota.Matrix{}, quota.AccessPolicy{}, err
	}
	return next, nextPolicy, nil
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
