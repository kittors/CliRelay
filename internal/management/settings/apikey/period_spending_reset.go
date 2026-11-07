package apikey

import (
	"errors"
	"fmt"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/quota"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/usage"
)

type PeriodLimitMissingError struct {
	Period quota.Period
}

func (e *PeriodLimitMissingError) Error() string {
	return fmt.Sprintf("%s spending limit is not set", e.Period)
}

type DailySpendingResetActor struct {
	UserID   string
	Username string
	Kind     string
}

type DailySpendingResetResult struct {
	ID                      string   `json:"id"`
	Key                     string   `json:"key,omitempty"`
	DailySpendingLimit      float64  `json:"daily-spending-limit"`
	DailySpendingUsed       float64  `json:"daily-spending-used"`
	DailySpendingRemaining  *float64 `json:"daily-spending-remaining"`
	DailySpendingResetCount int      `json:"daily-spending-reset-count"`
}

type PeriodSpendingResetResult struct {
	ID                  string                    `json:"id"`
	Key                 string                    `json:"key,omitempty"`
	Periods             []quota.Period            `json:"periods"`
	PeriodSpending      []quota.PeriodSpending    `json:"period-spending"`
	ResetCount          int                       `json:"reset-count"`
	EffectiveUsedBefore quota.PeriodSpendingUsage `json:"effective-used-before"`
	Raw                 quota.PeriodSpendingUsage `json:"raw"`
}

func (s *Service) ResetPeriodSpending(id *string, match *string, periods []quota.Period, actor DailySpendingResetActor) (PeriodSpendingResetResult, error) {
	periods, err := quota.NormalizePeriods(periods)
	if err != nil {
		return PeriodSpendingResetResult{}, err
	}
	row := s.resolvePatchTargetRow(id, nil, match)
	if row == nil {
		return PeriodSpendingResetResult{}, ErrItemNotFound
	}
	effective := usage.EffectiveAPIKeyRowForTenant(s.tenantID, *row)
	effective.PeriodSpendingLimits.Day = effective.DailySpendingLimit
	matrix := keyQuotaMatrix(effective)
	for _, period := range periods {
		if !quota.WindowHasLimit(matrix, effective.AccessPolicy.ModelRules, period) {
			return PeriodSpendingResetResult{}, &PeriodLimitMissingError{Period: period}
		}
	}
	reset, err := usage.ResetPeriodSpendingByAPIKeyIDForTenant(s.tenantID, effective.ID, periods, usage.PeriodSpendingResetActor{
		UserID: actor.UserID, Username: actor.Username, Kind: actor.Kind,
	})
	if err != nil {
		return PeriodSpendingResetResult{}, err
	}
	count, err := usage.CountAPIKeyPeriodSpendingResetEvents(s.tenantID, effective.ID)
	if err != nil {
		return PeriodSpendingResetResult{}, err
	}
	return PeriodSpendingResetResult{
		ID: effective.ID, Key: effective.Key, Periods: reset.Periods,
		PeriodSpending: quota.BuildPeriodSpending(effective.PeriodSpendingLimits, reset.EffectiveAfter),
		ResetCount:     count, EffectiveUsedBefore: reset.EffectiveUsedBefore, Raw: reset.Raw,
	}, nil
}

// ResetDailySpending sets today's cost baseline to the current raw today cost so effective used becomes 0.
func (s *Service) ResetDailySpending(id *string, match *string, actor DailySpendingResetActor) (DailySpendingResetResult, error) {
	reset, err := s.ResetPeriodSpending(id, match, []quota.Period{quota.PeriodDay}, actor)
	if err != nil {
		var missing *PeriodLimitMissingError
		if errors.As(err, &missing) && missing.Period == quota.PeriodDay {
			return DailySpendingResetResult{}, ErrDailySpendingLimitMissing
		}
		return DailySpendingResetResult{}, err
	}
	row := s.resolvePatchTargetRow(id, nil, match)
	effective := usage.EffectiveAPIKeyRowForTenant(s.tenantID, *row)
	raw := reset.Raw.Day
	usedBefore := reset.EffectiveUsedBefore.Day
	if err := usage.UpsertDailySpendingReset(s.tenantID, effective.ID, raw); err != nil {
		return DailySpendingResetResult{}, err
	}
	_ = usage.InsertDailySpendingResetEvent(usage.APIKeyDailySpendingResetEvent{
		TenantID:            s.tenantID,
		APIKeyID:            effective.ID,
		CostBaseline:        raw,
		EffectiveUsedBefore: usedBefore,
		RawTodayCost:        raw,
		ActorUserID:         actor.UserID,
		ActorUsername:       actor.Username,
		ActorKind:           actor.Kind,
	})
	count, _ := usage.CountDailySpendingResetEvents(s.tenantID, effective.ID)
	used := 0.0
	return DailySpendingResetResult{
		ID:                      effective.ID,
		Key:                     effective.Key,
		DailySpendingLimit:      effective.DailySpendingLimit,
		DailySpendingUsed:       used,
		DailySpendingRemaining:  usage.DailySpendingRemaining(effective.DailySpendingLimit, used),
		DailySpendingResetCount: count,
	}, nil
}

// ListDailySpendingResetHistory returns newest-first reset events for a key.
func (s *Service) ListDailySpendingResetHistory(id *string, match *string, limit int) ([]usage.APIKeyDailySpendingResetEvent, error) {
	row := s.resolvePatchTargetRow(id, nil, match)
	if row == nil {
		return nil, ErrItemNotFound
	}
	return usage.ListDailySpendingResetEvents(s.tenantID, row.ID, limit)
}

// KeyQuotaMatrix 返回 Key 的完整额度矩阵，供包外按同一口径判断用量（告警规则）。
// 传入的行必须已叠加权限模板（usage.EffectiveAPIKeyRowWithProfiles）。
func KeyQuotaMatrix(row usage.APIKeyRow) quota.Matrix { return keyQuotaMatrix(row) }

// keyQuotaMatrix 是 Key 的完整额度矩阵：账号下的 Key 只有美元四周期列，其余子额度在
// 访问策略里；独立 Key 拥有全部固定列。
func keyQuotaMatrix(row usage.APIKeyRow) quota.Matrix {
	period := row.PeriodSpendingLimits
	period.Day = row.DailySpendingLimit
	if row.EndUserID != "" {
		return quota.OwnedKeyColumnLayout.MatrixFromColumns(quota.ColumnValues{Period: period}, row.AccessPolicy.Limits)
	}
	return quota.StandaloneKeyColumnLayout.MatrixFromColumns(quota.ColumnValues{
		Period: period, LifetimeCost: row.SpendingLimit, DailyRequests: row.DailyLimit, TotalRequests: row.TotalQuota,
	}, row.AccessPolicy.Limits)
}
