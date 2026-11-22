package billing

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/configsync"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/quota"
	sqlapikey "github.com/router-for-me/CLIProxyAPI/v6/internal/storage/sqlstore/apikey"
)

// DailyResetFunc resets today's spending baseline for either an end-user or an API key.
// Supplied by the management layer so package billing does not import package usage.
type DailyResetFunc func(ctx context.Context, tenantID string, kind TargetKind, targetID, orderNo string) error

// fulfillOrder applies the purchased plan snapshot to the target account or API key
// inside a transaction and bumps the API key / end-user config sync versions.
func (s *Service) fulfillOrder(ctx context.Context, o Order) (string, error) {
	db := s.store.DB()
	if db == nil {
		return "", ErrUnavailable
	}
	plan := o.PlanSnapshot
	now := time.Now().UTC()

	if plan.Kind == PlanKindDailyReset {
		if s.dailyReset == nil {
			return "", fmt.Errorf("daily reset handler unavailable")
		}
		if err := s.dailyReset(ctx, o.TenantID, o.TargetKind, o.TargetID, o.OrderNo); err != nil {
			return "", err
		}
		return "今日消费额度已重置", nil
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer func() { _ = tx.Rollback() }()

	parts := make([]string, 0, 4)

	switch plan.Kind {
	case PlanKindSubscription:
		days := plan.DurationDays
		if days <= 0 {
			return "", fmt.Errorf("subscription plan missing duration_days")
		}
		var nextExp time.Time
		if o.Source == "admin_manual_from_paid_at" && o.PaidAt != nil {
			// Historical offline payment: compute expiry starting from paid_at + days,
			// preserving any already-longer expiry so backfilling never shortens a customer.
			candidate := o.PaidAt.UTC().Add(time.Duration(days) * 24 * time.Hour)
			var raw any
			if qErr := tx.QueryRowContext(ctx, `
				SELECT expires_at FROM billing_entitlements
				 WHERE tenant_id = ? AND target_kind = ? AND target_id = ?
			`, o.TenantID, string(o.TargetKind), o.TargetID).Scan(&raw); qErr != nil && !errors.Is(qErr, sql.ErrNoRows) {
				return "", qErr
			}
			nextExp = candidate
			if current, ok := parseAnyTime(raw); ok && current.After(nextExp) {
				nextExp = current
			}
			if err := setEntitlementExpiryTx(ctx, tx, o.TenantID, o.TargetKind, o.TargetID, &nextExp, now); err != nil {
				return "", err
			}
		} else {
			var extErr error
			nextExp, extErr = extendEntitlementExpiryTx(ctx, tx, o.TenantID, o.TargetKind, o.TargetID, days, now, now)
			if extErr != nil {
				return "", extErr
			}
		}
		parts = append(parts, fmt.Sprintf("有效期顺延 +%d 天（至 %s）", days, nextExp.In(time.Local).Format("2006-01-02 15:04")))

		if o.TargetKind == TargetKindEndUser && strings.TrimSpace(plan.PermissionProfileID) != "" {
			if err := applyProfileToEndUserTx(ctx, tx, o.TenantID, o.TargetID, strings.TrimSpace(plan.PermissionProfileID)); err != nil {
				return "", err
			}
			parts = append(parts, "已应用权限模板")
		}
		if plan.QuotaAmountUSD > 0 {
			if err := addQuotaTx(ctx, tx, o.TenantID, o.TargetKind, o.TargetID, plan.QuotaAmountUSD, 0, 0, now); err != nil {
				return "", err
			}
			parts = append(parts, fmt.Sprintf("累计额度 +$%.2f", plan.QuotaAmountUSD))
		}

	case PlanKindQuotaPack:
		if err := addQuotaTx(ctx, tx, o.TenantID, o.TargetKind, o.TargetID, plan.QuotaAmountUSD, plan.QuotaRequests, plan.QuotaTokens, now); err != nil {
			return "", err
		}
		if plan.QuotaAmountUSD > 0 {
			parts = append(parts, fmt.Sprintf("累计额度 +$%.2f", plan.QuotaAmountUSD))
		}
		if plan.QuotaRequests > 0 {
			parts = append(parts, fmt.Sprintf("请求次数 +%d", plan.QuotaRequests))
		}
		if plan.QuotaTokens > 0 {
			parts = append(parts, fmt.Sprintf("Token 额度 +%d", plan.QuotaTokens))
		}

	case PlanKindCustomQuota:
		usd := math.Round((float64(o.AmountCents)/100.0)*plan.RateUSDPerCNY*100) / 100
		if usd <= 0 {
			return "", fmt.Errorf("computed quota USD must be positive")
		}
		if err := addQuotaTx(ctx, tx, o.TenantID, o.TargetKind, o.TargetID, usd, 0, 0, now); err != nil {
			return "", err
		}
		parts = append(parts, fmt.Sprintf("累计额度 +$%.2f", usd))

	default:
		return "", fmt.Errorf("unsupported plan kind %q", plan.Kind)
	}

	if err := configsync.BumpAndCommit(ctx, tx, configsync.DomainAPIKeys, o.TenantID,
		configsync.Event(configsync.DomainEndUsers, o.TenantID)); err != nil {
		// In unit tests without config_versions table, fall back to direct commit cleanly.
		if !isMissingTableErr(err) {
			return "", err
		}
		if commitErr := tx.Commit(); commitErr != nil {
			return "", commitErr
		}
	}

	if s.onQuotaChanged != nil {
		_ = s.onQuotaChanged()
	}
	return strings.Join(parts, " · "), nil
}

// addQuotaTx adds USD spending, request count, and/or token lifetime allowance to
// either an end_user or an api_key. For an owned API key, it updates the key's
// access_policy lifetime cell and also raises the owning account's ceiling when
// the account has a finite ceiling, so the paid allowance is actually usable.
func addQuotaTx(ctx context.Context, tx *sql.Tx, tenantID string, kind TargetKind, targetID string, deltaUSD float64, deltaReqs, deltaTokens int64, now time.Time) error {
	switch kind {
	case TargetKindEndUser:
		return addEndUserQuotaTx(ctx, tx, tenantID, targetID, deltaUSD, deltaReqs, deltaTokens, now)
	case TargetKindAPIKey:
		return addAPIKeyQuotaTx(ctx, tx, tenantID, targetID, deltaUSD, deltaReqs, deltaTokens, now)
	default:
		return fmt.Errorf("invalid target_kind %q", kind)
	}
}

func addEndUserQuotaTx(ctx context.Context, tx *sql.Tx, tenantID, userID string, deltaUSD float64, deltaReqs, deltaTokens int64, now time.Time) error {
	var spend float64
	var totalQuota int64
	var policyJSON string
	err := tx.QueryRowContext(ctx, `
		SELECT COALESCE(spending_limit, 0), COALESCE(total_quota, 0), COALESCE(access_policy, '{}')
		  FROM end_users
		 WHERE tenant_id = ? AND id = ?
	`, tenantID, userID).Scan(&spend, &totalQuota, &policyJSON)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	nextSpend := math.Round((spend+deltaUSD)*100) / 100
	nextTotal := totalQuota + deltaReqs
	nextPolicyJSON := policyJSON
	if deltaTokens > 0 {
		policy, _ := quota.DecodeAccessPolicy(policyJSON)
		policy.Limits.Tokens.Lifetime += float64(deltaTokens)
		nextPolicyJSON = quota.EncodeAccessPolicy(policy)
	}
	res, err := tx.ExecContext(ctx, `
		UPDATE end_users
		   SET spending_limit = ?, total_quota = ?, access_policy = ?, updated_at = ?, version = version + 1
		 WHERE tenant_id = ? AND id = ?
	`, nextSpend, nextTotal, nextPolicyJSON, now.UTC(), tenantID, userID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func addAPIKeyQuotaTx(ctx context.Context, tx *sql.Tx, tenantID, keyID string, deltaUSD float64, deltaReqs, deltaTokens int64, now time.Time) error {
	var endUserID sql.NullString
	var spend float64
	var totalQuota int64
	var policyJSON string
	err := tx.QueryRowContext(ctx, `
		SELECT end_user_id, COALESCE(spending_limit, 0), COALESCE(total_quota, 0), COALESCE(access_policy, '{}')
		  FROM api_keys
		 WHERE tenant_id = ? AND id = ?
	`, tenantID, keyID).Scan(&endUserID, &spend, &totalQuota, &policyJSON)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	ownerID := ""
	if endUserID.Valid {
		ownerID = strings.TrimSpace(endUserID.String)
	}
	nowStr := now.UTC().Format(time.RFC3339)
	if ownerID == "" {
		// Standalone key stores lifetime cost/requests directly on its columns.
		nextSpend := math.Round((spend+deltaUSD)*100) / 100
		nextTotal := totalQuota + deltaReqs
		nextPolicyJSON := policyJSON
		if deltaTokens > 0 {
			policy, _ := quota.DecodeAccessPolicy(policyJSON)
			policy.Limits.Tokens.Lifetime += float64(deltaTokens)
			nextPolicyJSON = quota.EncodeAccessPolicy(policy)
		}
		_, err = tx.ExecContext(ctx, `
			UPDATE api_keys
			   SET spending_limit = ?, total_quota = ?, access_policy = ?, updated_at = ?
			 WHERE tenant_id = ? AND id = ?
		`, nextSpend, nextTotal, nextPolicyJSON, nowStr, tenantID, keyID)
		return err
	}

	// Owned key stores lifetime sub-quotas in api_keys.access_policy (see quota.OwnedKeyColumnLayout).
	policy, _ := quota.DecodeAccessPolicy(policyJSON)
	if deltaUSD > 0 {
		policy.Limits.Cost.Lifetime = math.Round((policy.Limits.Cost.Lifetime+deltaUSD)*100) / 100
	}
	if deltaReqs > 0 {
		policy.Limits.Requests.Lifetime += float64(deltaReqs)
	}
	if deltaTokens > 0 {
		policy.Limits.Tokens.Lifetime += float64(deltaTokens)
	}
	encoded := quota.EncodeAccessPolicy(policy)
	if _, err := tx.ExecContext(ctx, `
		UPDATE api_keys
		   SET access_policy = ?, updated_at = ?
		 WHERE tenant_id = ? AND id = ?
	`, encoded, nowStr, tenantID, keyID); err != nil {
		return err
	}

	// If the owning account has a finite lifetime ceiling, raise it by the same
	// delta so the account ceiling never blocks the quota just bought for this key.
	var acctSpend float64
	var acctTotal int64
	var acctPolicyJSON string
	if err := tx.QueryRowContext(ctx, `
		SELECT COALESCE(spending_limit, 0), COALESCE(total_quota, 0), COALESCE(access_policy, '{}')
		  FROM end_users
		 WHERE tenant_id = ? AND id = ?
	`, tenantID, ownerID).Scan(&acctSpend, &acctTotal, &acctPolicyJSON); err == nil {
		needUpdate := false
		nextAcctSpend := acctSpend
		nextAcctTotal := acctTotal
		acctPolicy, _ := quota.DecodeAccessPolicy(acctPolicyJSON)
		if deltaUSD > 0 && acctSpend > 0 {
			nextAcctSpend = math.Round((acctSpend+deltaUSD)*100) / 100
			needUpdate = true
		}
		if deltaReqs > 0 && acctTotal > 0 {
			nextAcctTotal = acctTotal + deltaReqs
			needUpdate = true
		}
		if deltaTokens > 0 && acctPolicy.Limits.Tokens.Lifetime > 0 {
			acctPolicy.Limits.Tokens.Lifetime += float64(deltaTokens)
			needUpdate = true
		}
		if needUpdate {
			nextAcctPolicyJSON := quota.EncodeAccessPolicy(acctPolicy)
			if _, err := tx.ExecContext(ctx, `
				UPDATE end_users
				   SET spending_limit = ?, total_quota = ?, access_policy = ?, updated_at = ?, version = version + 1
				 WHERE tenant_id = ? AND id = ?
			`, nextAcctSpend, nextAcctTotal, nextAcctPolicyJSON, now.UTC(), tenantID, ownerID); err != nil {
				return err
			}
		}
	}
	return nil
}

func applyProfileToEndUserTx(ctx context.Context, tx *sql.Tx, tenantID, userID, profileID string) error {
	profile, err := sqlapikey.LoadPermissionProfileTx(ctx, tx, tenantID, profileID)
	if err != nil {
		return err
	}
	if profile == nil {
		return fmt.Errorf("permission profile %s not found", profileID)
	}
	modelsJSON, _ := json.Marshal(profile.AllowedModels)
	channelsJSON, _ := json.Marshal(profile.AllowedChannels)
	groupsJSON, _ := json.Marshal(profile.AllowedChannelGroups)
	policyJSON := quota.EncodeAccessPolicy(profile.AccessPolicy)
	_, err = tx.ExecContext(ctx, `
		UPDATE end_users
		   SET permission_profile_id = ?,
		       daily_limit = ?, total_quota = ?,
		       daily_spending_limit = ?, five_hour_spending_limit = ?,
		       weekly_spending_limit = ?, monthly_spending_limit = ?,
		       concurrency_limit = ?, rpm_limit = ?, tpm_limit = ?,
		       allowed_models = ?, allowed_channels = ?, allowed_channel_groups = ?,
		       system_prompt = ?, access_policy = ?, updated_at = ?, version = version + 1
		 WHERE tenant_id = ? AND id = ?
	`,
		profile.ID,
		profile.DailyLimit, profile.TotalQuota,
		profile.DailySpendingLimit, profile.PeriodSpendingLimits.FiveHour,
		profile.PeriodSpendingLimits.Week, profile.PeriodSpendingLimits.Month,
		profile.ConcurrencyLimit, profile.RPMLimit, profile.TPMLimit,
		string(modelsJSON), string(channelsJSON), string(groupsJSON),
		profile.SystemPrompt, policyJSON, time.Now().UTC(),
		tenantID, userID,
	)
	return err
}

func isMissingTableErr(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "no such table") || strings.Contains(msg, "does not exist")
}
