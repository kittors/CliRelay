package usage

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/quota"
	log "github.com/sirupsen/logrus"
)

// EndUserQuota is the account-level limit/permission snapshot used by auth + quota.
type EndUserQuota struct {
	ID          string
	TenantID    string
	DisplayName string
	// Status is end_users.status (active/disabled/locked). Auth must refuse non-active accounts.
	Status               string
	PermissionProfileID  string
	DailyLimit           int
	TotalQuota           int
	SpendingLimit        float64
	DailySpendingLimit   float64
	PeriodSpendingLimits quota.PeriodSpendingLimits
	ConcurrencyLimit     int
	RPMLimit             int
	TPMLimit             int
	AllowedModels        []string
	AllowedChannels      []string
	AllowedChannelGroups []string
	SystemPrompt         string
	// AccessPolicy 是 access_policy 列：固定列之外的额度格子、按模型规则与模型路由。
	AccessPolicy quota.AccessPolicy
}

// QuotaMatrix 返回账号的完整总额度矩阵：固定列与策略格子合成，模板覆盖后调用即为生效值。
func (q EndUserQuota) QuotaMatrix() quota.Matrix {
	period := q.PeriodSpendingLimits
	period.Day = q.DailySpendingLimit
	return quota.AccountColumnLayout.MatrixFromColumns(quota.ColumnValues{
		Period: period, LifetimeCost: q.SpendingLimit, DailyRequests: q.DailyLimit, TotalRequests: q.TotalQuota,
	}, q.AccessPolicy.Limits)
}

func decodeEndUserAccessPolicy(endUserID, raw string) quota.AccessPolicy {
	policy, err := quota.DecodeAccessPolicy(raw)
	if err != nil {
		// 写入路径一律先校验再序列化，读到坏数据只可能是手工改库；记下来，按空策略继续，
		// 固定列上的额度仍然生效。
		log.WithError(err).Errorf("usage: end user %s has an unreadable access_policy", endUserID)
	}
	return policy
}

func decodeQuotaStringList(raw string) []string {
	raw = strings.TrimSpace(raw)
	if raw == "" || raw == "[]" {
		return nil
	}
	var out []string
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return nil
	}
	return out
}

// GetEndUserQuota loads account-level quota for an end user. nil when missing.
func GetEndUserQuota(endUserID string) *EndUserQuota {
	db := getReadDB()
	if db == nil {
		return nil
	}
	endUserID = strings.TrimSpace(endUserID)
	if endUserID == "" {
		return nil
	}
	var q EndUserQuota
	var modelsJSON, channelsJSON, groupsJSON string
	policyJSON := "{}"
	err := db.QueryRow(`
		SELECT id, tenant_id, display_name, COALESCE(status, 'active'),
			COALESCE(permission_profile_id, ''), COALESCE(daily_limit, 0), COALESCE(total_quota, 0),
			COALESCE(spending_limit, 0), COALESCE(daily_spending_limit, 0),
			COALESCE(five_hour_spending_limit, 0), COALESCE(weekly_spending_limit, 0), COALESCE(monthly_spending_limit, 0),
			COALESCE(concurrency_limit, 0), COALESCE(rpm_limit, 0), COALESCE(tpm_limit, 0),
			COALESCE(allowed_models, '[]'), COALESCE(allowed_channels, '[]'), COALESCE(allowed_channel_groups, '[]'),
			COALESCE(system_prompt, ''), COALESCE(access_policy, '{}')
		FROM end_users WHERE id = ?
	`, endUserID).Scan(
		&q.ID, &q.TenantID, &q.DisplayName, &q.Status,
		&q.PermissionProfileID, &q.DailyLimit, &q.TotalQuota,
		&q.SpendingLimit, &q.DailySpendingLimit,
		&q.PeriodSpendingLimits.FiveHour, &q.PeriodSpendingLimits.Week, &q.PeriodSpendingLimits.Month, &q.ConcurrencyLimit, &q.RPMLimit, &q.TPMLimit,
		&modelsJSON, &channelsJSON, &groupsJSON, &q.SystemPrompt, &policyJSON,
	)
	if err != nil && strings.Contains(strings.ToLower(err.Error()), "no such column") {
		err = db.QueryRow(`
			SELECT id, tenant_id, display_name, COALESCE(status, 'active'),
				COALESCE(permission_profile_id, ''), COALESCE(daily_limit, 0), COALESCE(total_quota, 0),
				COALESCE(spending_limit, 0), COALESCE(daily_spending_limit, 0),
				COALESCE(concurrency_limit, 0), COALESCE(rpm_limit, 0), COALESCE(tpm_limit, 0),
				COALESCE(allowed_models, '[]'), COALESCE(allowed_channels, '[]'), COALESCE(allowed_channel_groups, '[]'),
				COALESCE(system_prompt, '')
			FROM end_users WHERE id = ?
		`, endUserID).Scan(
			&q.ID, &q.TenantID, &q.DisplayName, &q.Status,
			&q.PermissionProfileID, &q.DailyLimit, &q.TotalQuota,
			&q.SpendingLimit, &q.DailySpendingLimit,
			&q.ConcurrencyLimit, &q.RPMLimit, &q.TPMLimit,
			&modelsJSON, &channelsJSON, &groupsJSON, &q.SystemPrompt,
		)
	}
	if err != nil {
		return nil
	}
	q.PeriodSpendingLimits.Day = q.DailySpendingLimit
	q.AllowedModels = decodeQuotaStringList(modelsJSON)
	q.AllowedChannels = decodeQuotaStringList(channelsJSON)
	q.AllowedChannelGroups = decodeQuotaStringList(groupsJSON)
	q.AccessPolicy = decodeEndUserAccessPolicy(q.ID, policyJSON)
	return &q
}

// EffectiveEndUserQuota merges a permission profile over the end-user row when set.
func EffectiveEndUserQuota(q EndUserQuota) EndUserQuota {
	return EffectiveEndUserQuotaWithProfiles(q, ListAPIKeyPermissionProfilesForTenant(q.TenantID))
}

// ListAPIKeyIDsForEndUser returns stable key ids owned by the end user.
func ListAPIKeyIDsForEndUser(endUserID string) []string {
	return listAPIKeyValuesForEndUser("", endUserID, "id")
}

func ListAPIKeyIDsForEndUserForTenant(tenantID, endUserID string) []string {
	return listAPIKeyValuesForEndUser(tenantID, endUserID, "id")
}

func listAPIKeyValuesForEndUser(tenantID, endUserID, column string) []string {
	db := getReadDB()
	if db == nil {
		return nil
	}
	endUserID = strings.TrimSpace(endUserID)
	if endUserID == "" {
		return nil
	}
	query := `SELECT ` + column + ` FROM api_keys WHERE end_user_id = ? AND ` + column + ` != ''`
	args := []interface{}{endUserID}
	if strings.TrimSpace(tenantID) != "" {
		query += ` AND tenant_id = ?`
		args = append(args, normalizeTenantID(tenantID))
	}
	rows, err := db.Query(query, args...)
	if err != nil {
		return nil
	}
	defer rows.Close()
	out := make([]string, 0)
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return out
		}
		id = strings.TrimSpace(id)
		if id != "" {
			out = append(out, id)
		}
	}
	return out
}

// ListAPIKeySecretsForEndUser returns raw key secrets owned by the end user (legacy log rows).
func ListAPIKeySecretsForEndUser(endUserID string) []string {
	return listAPIKeyValuesForEndUser("", endUserID, "key")
}

func ListAPIKeySecretsForEndUserForTenant(tenantID, endUserID string) []string {
	return listAPIKeyValuesForEndUser(tenantID, endUserID, "key")
}

// ExpandPublicLookupAPIKeys returns the presented API key only.
// Account-wide multi-key views use portal EndUserID auth, not raw-secret expansion.
func ExpandPublicLookupAPIKeys(apiKey string) []string {
	trimmed := strings.TrimSpace(apiKey)
	if trimmed == "" {
		return nil
	}
	row := GetAPIKey(trimmed)
	if row != nil {
		if k := strings.TrimSpace(row.Key); k != "" {
			return []string{k}
		}
	}
	return []string{trimmed}
}

// ResolveAPIKeyOwnName returns the key's own name (not end-user account display name).
func ResolveAPIKeyOwnName(apiKey string) string {
	trimmed := strings.TrimSpace(apiKey)
	if trimmed == "" {
		return ""
	}
	row := GetAPIKey(trimmed)
	if row == nil {
		return ""
	}
	return strings.TrimSpace(row.Name)
}

// DisplayNameForEndUser returns display_name for request-log labeling.
func DisplayNameForEndUser(endUserID string) string {
	q := GetEndUserQuota(endUserID)
	if q == nil {
		return ""
	}
	return strings.TrimSpace(q.DisplayName)
}

// ResolveAPIKeyDisplayName prefers end-user display name when the key is owned.
func ResolveAPIKeyDisplayName(row *APIKeyRow, fallback string) string {
	if row != nil {
		if name := DisplayNameForEndUser(row.EndUserID); name != "" {
			return name
		}
		if n := strings.TrimSpace(row.Name); n != "" {
			return n
		}
	}
	return strings.TrimSpace(fallback)
}

// buildEndUserAPIKeySelectorPredicate matches every row of the account's keys.
// Rows are attributed by api_key_id; the api_key column holds only a mask, so
// there is no secret to match a legacy row against.
func buildEndUserAPIKeySelectorPredicate(tenantID, endUserID string) (string, []interface{}) {
	ids := ListAPIKeyIDsForEndUserForTenant(tenantID, endUserID)
	if len(ids) == 0 {
		// No keys yet: match nothing.
		return "1 = 0", nil
	}
	args := make([]interface{}, 0, len(ids))
	ph := make([]string, len(ids))
	for i, id := range ids {
		ph[i] = "?"
		args = append(args, id)
	}
	return "api_key_id IN (" + strings.Join(ph, ",") + ")", args
}

func buildEndUserAPIKeySelectorClause(endUserID string) (string, []interface{}) {
	quota := GetEndUserQuota(endUserID)
	tenantID := ""
	if quota != nil {
		tenantID = quota.TenantID
	}
	predicate, args := buildEndUserAPIKeySelectorPredicate(tenantID, endUserID)
	return " WHERE " + predicate, args
}

// CountTodayByEndUser counts requests across all keys of the end user today.
func CountTodayByEndUser(endUserID string) (int64, error) {
	quota := GetEndUserQuota(endUserID)
	tenantID := systemTenantID
	if quota != nil && strings.TrimSpace(quota.TenantID) != "" {
		tenantID = quota.TenantID
	}
	return queryTodayCountByEndUserFromRollup(tenantID, endUserID)
}

// CountTotalByEndUser counts lifetime requests across all keys of the end user.
func CountTotalByEndUser(endUserID string) (int64, error) {
	quota := GetEndUserQuota(endUserID)
	tenantID := systemTenantID
	if quota != nil && strings.TrimSpace(quota.TenantID) != "" {
		tenantID = quota.TenantID
	}
	return queryLifetimeCountByEndUserFromRollup(tenantID, endUserID)
}

// QueryTotalCostByEndUser returns spend counted against the account's current
// lifetime allowance: cumulative cost minus the baseline recorded by the last
// allowance grant. Without the baseline the limit would be a permanent ceiling
// on the account's entire history, so granting a fresh allowance would require
// the operator to add past spend into the new limit by hand.
func QueryTotalCostByEndUser(endUserID string) (float64, error) {
	quota := GetEndUserQuota(endUserID)
	tenantID := systemTenantID
	if quota != nil && strings.TrimSpace(quota.TenantID) != "" {
		tenantID = quota.TenantID
	}
	agg, err := queryRollupAgg(rollupFilter{
		TenantID:   tenantID,
		BucketKind: rollupBucketLifetime,
		EndUserID:  endUserID,
	})
	if err != nil {
		return 0, fmt.Errorf("usage: total cost by end user: %w", err)
	}
	baseline, err := lifetimeSpendingBaseline(tenantID, periodSubjectEndUser, endUserID)
	if err != nil {
		return 0, fmt.Errorf("usage: total cost by end user: %w", err)
	}
	if remaining := agg.CostTotal - baseline; remaining > 0 {
		return remaining, nil
	}
	return 0, nil
}

// lifetimeSpendingBaseline returns the cumulative-spend baseline stored by the
// last allowance grant, or 0 when the subject was never granted one.
func lifetimeSpendingBaseline(tenantID string, subject periodSubject, subjectID string) (float64, error) {
	subjectType, err := periodResetSubjectType(subject)
	if err != nil {
		return 0, err
	}
	db := getReadDB()
	if db == nil {
		return 0, ErrQuotaUsageUnavailable
	}
	var baseline float64
	err = db.QueryRow(`SELECT cost_baseline FROM period_spending_resets
		WHERE tenant_id = ? AND subject_type = ? AND subject_id = ? AND period = ?`,
		normalizeTenantID(tenantID), subjectType, strings.TrimSpace(subjectID), string(quota.PeriodLifetime),
	).Scan(&baseline)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	return baseline, nil
}

// QueryTodayCostByEndUser returns account-level project-day spend after the latest same-day reset baseline.
func QueryTodayCostByEndUser(endUserID string) (float64, error) {
	quota := GetEndUserQuota(endUserID)
	if quota == nil {
		return 0, nil
	}
	return QueryTodayEffectiveCostByEndUserForTenant(quota.TenantID, endUserID)
}

// EnsureEndUserQuotaColumns adds account quota columns on SQLite bootstraps / tests.
func EnsureEndUserQuotaColumns(db *sql.DB) error {
	if db == nil {
		return nil
	}
	for _, col := range []struct {
		name string
		def  string
	}{
		{"permission_profile_id", "TEXT NOT NULL DEFAULT ''"},
		{"daily_limit", "INTEGER NOT NULL DEFAULT 0"},
		{"total_quota", "INTEGER NOT NULL DEFAULT 0"},
		{"spending_limit", "REAL NOT NULL DEFAULT 0"},
		{"daily_spending_limit", "REAL NOT NULL DEFAULT 0"},
		{"five_hour_spending_limit", "REAL NOT NULL DEFAULT 0"},
		{"weekly_spending_limit", "REAL NOT NULL DEFAULT 0"},
		{"monthly_spending_limit", "REAL NOT NULL DEFAULT 0"},
		{"concurrency_limit", "INTEGER NOT NULL DEFAULT 0"},
		{"rpm_limit", "INTEGER NOT NULL DEFAULT 0"},
		{"tpm_limit", "INTEGER NOT NULL DEFAULT 0"},
		{"allowed_models", "TEXT NOT NULL DEFAULT '[]'"},
		{"allowed_channels", "TEXT NOT NULL DEFAULT '[]'"},
		{"allowed_channel_groups", "TEXT NOT NULL DEFAULT '[]'"},
		{"system_prompt", "TEXT NOT NULL DEFAULT ''"},
		{"access_policy", "TEXT NOT NULL DEFAULT '{}'"},
	} {
		if _, err := db.Exec("ALTER TABLE end_users ADD COLUMN " + col.name + " " + col.def); err != nil {
			if !strings.Contains(strings.ToLower(err.Error()), "duplicate") {
				// table may not exist yet in pure-key tests
				msg := strings.ToLower(err.Error())
				if strings.Contains(msg, "no such table") {
					return nil
				}
				return err
			}
		}
	}
	return nil
}

// ListEndUserQuotasByIDs batch-loads account quota rows for provider cache rebuilds.
func ListEndUserQuotasByIDs(endUserIDs []string) (map[string]EndUserQuota, error) {
	db := getReadDB()
	if db == nil {
		return nil, fmt.Errorf("usage: end user quota database unavailable")
	}
	ids := dedupeExactStrings(endUserIDs)
	out := make(map[string]EndUserQuota, len(ids))
	const chunkSize = 400
	for start := 0; start < len(ids); start += chunkSize {
		end := start + chunkSize
		if end > len(ids) {
			end = len(ids)
		}
		chunk := ids[start:end]
		rows, err := db.Query(`
			SELECT id, tenant_id, display_name, COALESCE(status, 'active'),
				COALESCE(permission_profile_id, ''), COALESCE(daily_limit, 0), COALESCE(total_quota, 0),
				COALESCE(spending_limit, 0), COALESCE(daily_spending_limit, 0),
				COALESCE(five_hour_spending_limit, 0), COALESCE(weekly_spending_limit, 0), COALESCE(monthly_spending_limit, 0),
				COALESCE(concurrency_limit, 0), COALESCE(rpm_limit, 0), COALESCE(tpm_limit, 0),
				COALESCE(allowed_models, '[]'), COALESCE(allowed_channels, '[]'), COALESCE(allowed_channel_groups, '[]'),
				COALESCE(system_prompt, ''), COALESCE(access_policy, '{}')
			FROM end_users WHERE id IN (`+placeholders(len(chunk))+`)`, stringsToAny(chunk)...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var q EndUserQuota
			var modelsJSON, channelsJSON, groupsJSON, policyJSON string
			if err := rows.Scan(&q.ID, &q.TenantID, &q.DisplayName, &q.Status,
				&q.PermissionProfileID, &q.DailyLimit, &q.TotalQuota, &q.SpendingLimit, &q.DailySpendingLimit,
				&q.PeriodSpendingLimits.FiveHour, &q.PeriodSpendingLimits.Week, &q.PeriodSpendingLimits.Month,
				&q.ConcurrencyLimit, &q.RPMLimit, &q.TPMLimit,
				&modelsJSON, &channelsJSON, &groupsJSON, &q.SystemPrompt, &policyJSON); err != nil {
				_ = rows.Close()
				return nil, err
			}
			q.PeriodSpendingLimits.Day = q.DailySpendingLimit
			q.AllowedModels = decodeQuotaStringList(modelsJSON)
			q.AllowedChannels = decodeQuotaStringList(channelsJSON)
			q.AllowedChannelGroups = decodeQuotaStringList(groupsJSON)
			q.AccessPolicy = decodeEndUserAccessPolicy(q.ID, policyJSON)
			out[q.ID] = q
		}
		if err := rows.Close(); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func stringsToAny(values []string) []any {
	out := make([]any, len(values))
	for i := range values {
		out[i] = values[i]
	}
	return out
}

func EffectiveEndUserQuotaWithProfiles(q EndUserQuota, profiles []APIKeyPermissionProfileRow) EndUserQuota {
	profileID := strings.TrimSpace(q.PermissionProfileID)
	if profileID == "" {
		return q
	}
	for _, profile := range profiles {
		if strings.TrimSpace(profile.ID) != profileID {
			continue
		}
		q.PermissionProfileID = profileID
		q.DailyLimit = profile.DailyLimit
		q.TotalQuota = profile.TotalQuota
		q.DailySpendingLimit = profile.DailySpendingLimit
		q.PeriodSpendingLimits = profile.PeriodSpendingLimits
		q.PeriodSpendingLimits.Day = q.DailySpendingLimit
		q.ConcurrencyLimit = profile.ConcurrencyLimit
		q.RPMLimit = profile.RPMLimit
		q.TPMLimit = profile.TPMLimit
		q.AllowedModels = append([]string(nil), profile.AllowedModels...)
		q.AllowedChannels = append([]string(nil), profile.AllowedChannels...)
		q.AllowedChannelGroups = append([]string(nil), profile.AllowedChannelGroups...)
		q.SystemPrompt = profile.SystemPrompt
		// 模板的策略整体生效（额度格子、规则、路由）。累计消费不在模板里，仍取账号的
		// spending_limit 列，所以这里不需要特殊处理。
		q.AccessPolicy = profile.AccessPolicy.Clone()
		return q
	}
	return q
}
