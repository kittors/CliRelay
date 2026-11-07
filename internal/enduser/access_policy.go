package enduser

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/quota"
	sqlapikey "github.com/router-for-me/CLIProxyAPI/v6/internal/storage/sqlstore/apikey"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/usage"
	log "github.com/sirupsen/logrus"
)

// 账号与账号下 Key 的访问策略读写。账号的总额度矩阵由固定列（美元四周期、累计消费、
// 每日/总请求）与 access_policy 中其余格子合成，见 quota.AccountColumnLayout；Key 只有
// 美元四周期列，其余子额度格子都在 Key 自己的 access_policy 里。

func decodeAccountPolicy(endUserID, raw string) quota.AccessPolicy {
	policy, err := quota.DecodeAccessPolicy(raw)
	if err != nil {
		log.WithError(err).Errorf("enduser: account %s has an unreadable access_policy", endUserID)
	}
	return policy
}

// setUserAccessPolicy 用账号行自己的值填充 QuotaLimits / ModelRules / ModelRoutes。
func setUserAccessPolicy(u *User, policy quota.AccessPolicy) {
	period := u.PeriodSpendingLimits
	period.Day = u.DailySpendingLimit
	u.QuotaLimits = quota.AccountColumnLayout.MatrixFromColumns(quota.ColumnValues{
		Period: period, LifetimeCost: u.SpendingLimit, DailyRequests: u.DailyLimit, TotalRequests: u.TotalQuota,
	}, policy.Limits)
	u.ModelRules = nonNilRules(policy.ModelRules)
	u.ModelRoutes = nonNilRoutes(policy.ModelRoutes)
}

func nonNilRules(rules []quota.ModelRule) []quota.ModelRule {
	if rules == nil {
		return []quota.ModelRule{}
	}
	return rules
}

func nonNilRoutes(routes []quota.ModelRoute) []quota.ModelRoute {
	if routes == nil {
		return []quota.ModelRoute{}
	}
	return routes
}

// ApplyEffectiveQuota 用模板覆盖后的生效值改写 User 的全部限额字段。列表、详情与
// 修改后的响应都经过它，面板看到的就是网关实际执行的那一套。
func ApplyEffectiveQuota(u *User, effective usage.EndUserQuota) {
	if u == nil {
		return
	}
	u.DailyLimit = effective.DailyLimit
	u.TotalQuota = effective.TotalQuota
	u.SpendingLimit = effective.SpendingLimit
	u.DailySpendingLimit = effective.DailySpendingLimit
	u.PeriodSpendingLimits = effective.PeriodSpendingLimits
	u.PeriodSpendingLimits.Day = effective.DailySpendingLimit
	u.ConcurrencyLimit = effective.ConcurrencyLimit
	u.RPMLimit = effective.RPMLimit
	u.TPMLimit = effective.TPMLimit
	u.AllowedModels = append([]string(nil), effective.AllowedModels...)
	u.AllowedChannels = append([]string(nil), effective.AllowedChannels...)
	u.AllowedChannelGroups = append([]string(nil), effective.AllowedChannelGroups...)
	u.SystemPrompt = effective.SystemPrompt
	u.QuotaLimits = effective.QuotaMatrix()
	u.ModelRules = nonNilRules(effective.AccessPolicy.ModelRules)
	u.ModelRoutes = nonNilRoutes(effective.AccessPolicy.ModelRoutes)
}

// accountQuotaRow 是账号行上与额度/权限相关的部分。
type accountQuotaRow struct {
	Status        string
	ProfileID     string
	Columns       quota.ColumnValues
	Policy        quota.AccessPolicy
	AllowedModels []string
}

func (r accountQuotaRow) matrix() quota.Matrix {
	return quota.AccountColumnLayout.MatrixFromColumns(r.Columns, r.Policy.Limits)
}

const accountQuotaSelect = `SELECT status, COALESCE(permission_profile_id,''),
	COALESCE(five_hour_spending_limit,0), COALESCE(daily_spending_limit,0), COALESCE(weekly_spending_limit,0), COALESCE(monthly_spending_limit,0),
	COALESCE(spending_limit,0), COALESCE(daily_limit,0), COALESCE(total_quota,0),
	COALESCE(access_policy,'{}'), COALESCE(allowed_models,'[]')
	FROM end_users WHERE id = ? AND tenant_id = ?`

// loadAccountQuotaTx 读取账号行；lock 时加行锁（SQLite 不支持 FOR UPDATE，退回普通读，
// 它本身按库串行写）。
func loadAccountQuotaTx(ctx context.Context, tx *sql.Tx, tenantID, endUserID string, lock bool) (accountQuotaRow, error) {
	var row accountQuotaRow
	var policyJSON, modelsJSON string
	scan := func(query string) error {
		return tx.QueryRowContext(ctx, query, endUserID, tenantID).Scan(
			&row.Status, &row.ProfileID,
			&row.Columns.Period.FiveHour, &row.Columns.Period.Day, &row.Columns.Period.Week, &row.Columns.Period.Month,
			&row.Columns.LifetimeCost, &row.Columns.DailyRequests, &row.Columns.TotalRequests,
			&policyJSON, &modelsJSON,
		)
	}
	var err error
	if lock {
		err = scan(accountQuotaSelect + " FOR UPDATE")
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			err = scan(accountQuotaSelect)
		}
	} else {
		err = scan(accountQuotaSelect)
	}
	if errors.Is(err, sql.ErrNoRows) {
		return row, ErrNotFound
	}
	if err != nil {
		return row, err
	}
	row.ProfileID = strings.TrimSpace(row.ProfileID)
	row.Policy = decodeAccountPolicy(endUserID, policyJSON)
	row.AllowedModels = decodeJSONStringList(modelsJSON)
	return row, nil
}

// effectiveAccount 是账号在模板覆盖后的生效额度与可用模型，Key 子额度以它为天花板。
type effectiveAccount struct {
	Status        string
	Matrix        quota.Matrix
	AllowedModels []string
}

func effectiveAccountTx(ctx context.Context, tx *sql.Tx, tenantID, endUserID string, lock bool) (effectiveAccount, error) {
	row, err := loadAccountQuotaTx(ctx, tx, tenantID, endUserID, lock)
	if err != nil {
		return effectiveAccount{}, err
	}
	out := effectiveAccount{Status: row.Status, Matrix: row.matrix(), AllowedModels: row.AllowedModels}
	if row.ProfileID == "" {
		return out, nil
	}
	profile, err := sqlapikey.LoadPermissionProfileTx(ctx, tx, tenantID, row.ProfileID)
	if err != nil {
		return effectiveAccount{}, err
	}
	if profile == nil {
		// 模板被删后账号行保留着最后一次同步的快照（见模板整表替换的解绑逻辑），
		// 运行时同样退回账号行，这里保持一致。
		return out, nil
	}
	matrix := sqlapikey.ProfileQuotaMatrix(*profile)
	// 累计消费是发给账号的额度，不归模板管。
	matrix.Cost.Lifetime = out.Matrix.Cost.Lifetime
	out.Matrix = matrix
	out.AllowedModels = append([]string(nil), profile.AllowedModels...)
	return out, nil
}

// accountColumn 把账号的一个固定列映射到矩阵中的一格。
type accountColumn struct {
	metric quota.Metric
	window quota.Period
	column string
}

var accountColumns = []accountColumn{
	{quota.MetricCost, quota.PeriodFiveHour, "five_hour_spending_limit"},
	{quota.MetricCost, quota.PeriodDay, "daily_spending_limit"},
	{quota.MetricCost, quota.PeriodWeek, "weekly_spending_limit"},
	{quota.MetricCost, quota.PeriodMonth, "monthly_spending_limit"},
	{quota.MetricCost, quota.PeriodLifetime, "spending_limit"},
	{quota.MetricRequests, quota.PeriodDay, "daily_limit"},
	{quota.MetricRequests, quota.PeriodLifetime, "total_quota"},
}

// mergeMatrixPatchCell 把旧字段的一格并进补丁；同一格已有不同的值即冲突。
func mergeMatrixPatchCell(patch *quota.MatrixPatch, metric quota.Metric, window quota.Period, value float64) error {
	normalized, err := quota.NormalizeQuotaValue(metric, value)
	if err != nil {
		return fmt.Errorf("%w: %s.%s: %v", ErrValidation, metric, window, err)
	}
	if existing := patch.Get(metric, window); existing != nil {
		other, err := quota.NormalizeQuotaValue(metric, *existing)
		if err != nil {
			return fmt.Errorf("%w: quota-limits.%s.%s: %v", ErrValidation, metric, window, err)
		}
		if other != normalized {
			return fmt.Errorf("%w: %s.%s", ErrQuotaLegacyConflict, metric, window)
		}
	}
	patch.Set(metric, window, normalized)
	return nil
}

// mergeLegacyAccountLimits 把账号补丁中的旧扁平字段折进矩阵补丁。旧字段沿用原来的
// 宽松处理：负数按 0（不限制）计，而不是报错，以免旧客户端的请求突然开始失败。
func mergeLegacyAccountLimits(patch *QuotaPatch, period *quota.PeriodSpendingLimitsPatch) (*quota.MatrixPatch, error) {
	out := patch.QuotaLimits.Clone()
	if err := mergeLegacyPeriod(out, period); err != nil {
		return nil, err
	}
	if patch.SpendingLimit != nil {
		if err := mergeMatrixPatchCell(out, quota.MetricCost, quota.PeriodLifetime, clampNonNegFloat(*patch.SpendingLimit)); err != nil {
			return nil, err
		}
	}
	if patch.DailyLimit != nil {
		if err := mergeMatrixPatchCell(out, quota.MetricRequests, quota.PeriodDay, float64(clampNonNegInt(*patch.DailyLimit))); err != nil {
			return nil, err
		}
	}
	if patch.TotalQuota != nil {
		if err := mergeMatrixPatchCell(out, quota.MetricRequests, quota.PeriodLifetime, float64(clampNonNegInt(*patch.TotalQuota))); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func mergeLegacyPeriod(out *quota.MatrixPatch, period *quota.PeriodSpendingLimitsPatch) error {
	if period == nil {
		return nil
	}
	for window, value := range map[quota.Period]*float64{
		quota.PeriodFiveHour: period.FiveHour, quota.PeriodDay: period.Day,
		quota.PeriodWeek: period.Week, quota.PeriodMonth: period.Month,
	} {
		if value == nil {
			continue
		}
		if err := mergeMatrixPatchCell(out, quota.MetricCost, window, *value); err != nil {
			return err
		}
	}
	return nil
}

// touchesProfileOwnedCells 报告补丁是否修改了模板拥有的格子。
func touchesProfileOwnedCells(patch *quota.MatrixPatch) bool {
	for _, metric := range quota.OrderedMetrics {
		for _, window := range quota.OrderedWindows {
			if patch.Get(metric, window) != nil && quota.ProfileOwnsCell(metric, window) {
				return true
			}
		}
	}
	return false
}

// PolicyTargetNotAllowedError：路由目标或降级模型不在可用模型白名单里。改写后的请求
// 会被模型门控以 model_not_allowed 拒绝，而客户端只看到一个它没请求过的模型名，
// 所以在保存配置时就拦下。
type PolicyTargetNotAllowedError struct {
	Field string
	Model string
}

func (e *PolicyTargetNotAllowedError) Error() string {
	return fmt.Sprintf("%s: model %q is not in the allowed models", e.Field, e.Model)
}

func (e *PolicyTargetNotAllowedError) Unwrap() error { return ErrValidation }

// validatePolicyTargetsAllowed 要求白名单非空时，路由目标与降级模型都在白名单内。
// 与网关的 allowed-models 判定一致，按名称精确匹配。
func validatePolicyTargetsAllowed(policy quota.AccessPolicy, allowed []string) error {
	if len(allowed) == 0 {
		return nil
	}
	set := make(map[string]struct{}, len(allowed))
	for _, model := range allowed {
		set[strings.TrimSpace(model)] = struct{}{}
	}
	for i, route := range policy.ModelRoutes {
		if _, ok := set[route.To]; !ok {
			return &PolicyTargetNotAllowedError{Field: fmt.Sprintf("model-routes[%d].to", i), Model: route.To}
		}
	}
	for i, rule := range policy.ModelRules {
		if rule.FallbackModel == "" {
			continue
		}
		if _, ok := set[rule.FallbackModel]; !ok {
			return &PolicyTargetNotAllowedError{Field: fmt.Sprintf("model-rules[%d].fallback-model", i), Model: rule.FallbackModel}
		}
	}
	return nil
}

// copyProfileToAccountTx 把模板的全部值写进账号行。绑定后运行时以模板为准，这份
// 快照只在解绑时生效：解绑的账号保留模板最后的值并转为可编辑，而不是瞬间失去限制。
func copyProfileToAccountTx(ctx context.Context, tx *sql.Tx, tenantID, endUserID string, profile sqlapikey.PermissionProfileRow) error {
	period := profile.PeriodSpendingLimits
	period.Day = profile.DailySpendingLimit
	_, err := tx.ExecContext(ctx, `UPDATE end_users SET
		daily_limit = ?, total_quota = ?,
		daily_spending_limit = ?, five_hour_spending_limit = ?, weekly_spending_limit = ?, monthly_spending_limit = ?,
		concurrency_limit = ?, rpm_limit = ?, tpm_limit = ?,
		allowed_models = ?, allowed_channels = ?, allowed_channel_groups = ?, system_prompt = ?, access_policy = ?
		WHERE id = ? AND tenant_id = ?`,
		profile.DailyLimit, profile.TotalQuota,
		period.Day, period.FiveHour, period.Week, period.Month,
		profile.ConcurrencyLimit, profile.RPMLimit, profile.TPMLimit,
		encodeJSONStringList(profile.AllowedModels), encodeJSONStringList(profile.AllowedChannels),
		encodeJSONStringList(profile.AllowedChannelGroups), profile.SystemPrompt,
		quota.EncodeAccessPolicy(profile.AccessPolicy), endUserID, tenantID)
	return err
}

// capOwnedKeysToAccountTx 在账号额度变化后把名下 Key 的子额度压到账号生效额度之内。
func capOwnedKeysToAccountTx(ctx context.Context, tx *sql.Tx, tenantID, endUserID string) ([]quota.CappedCell, error) {
	account, err := effectiveAccountTx(ctx, tx, tenantID, endUserID, false)
	if err != nil {
		return nil, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT id, COALESCE(daily_spending_limit,0), COALESCE(five_hour_spending_limit,0),
		COALESCE(weekly_spending_limit,0), COALESCE(monthly_spending_limit,0), COALESCE(access_policy,'{}')
		FROM api_keys WHERE tenant_id = ? AND end_user_id = ? ORDER BY id ASC`, tenantID, endUserID)
	if err != nil {
		return nil, err
	}
	type keyRow struct {
		id     string
		period quota.PeriodSpendingLimits
		policy quota.AccessPolicy
	}
	keys := make([]keyRow, 0)
	for rows.Next() {
		var item keyRow
		var policyJSON string
		if err := rows.Scan(&item.id, &item.period.Day, &item.period.FiveHour, &item.period.Week, &item.period.Month, &policyJSON); err != nil {
			_ = rows.Close()
			return nil, err
		}
		item.policy = decodeAccountPolicy(item.id, policyJSON)
		keys = append(keys, item)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	now := time.Now().UTC().Format(time.RFC3339)
	capped := make([]quota.CappedCell, 0)
	for _, item := range keys {
		cells, err := sqlapikey.CapOwnedKeyMatrixTx(tx, tenantID, item.id, item.period, item.policy, account.Matrix, now)
		if err != nil {
			return nil, err
		}
		capped = append(capped, cells...)
	}
	return capped, nil
}

// AccountQuota 把账号行转换成网关使用的额度结构。列表批量计算生效值时用它，
// 免得为同一批账号再查一次 end_users。
func (u User) AccountQuota() usage.EndUserQuota {
	policy := quota.AccessPolicy{
		Limits:      quota.AccountColumnLayout.StripColumnCells(u.QuotaLimits),
		ModelRules:  u.ModelRules,
		ModelRoutes: u.ModelRoutes,
	}
	return usage.EndUserQuota{
		ID: u.ID, TenantID: u.TenantID, DisplayName: u.DisplayName, Status: u.Status,
		PermissionProfileID: u.PermissionProfileID, DailyLimit: u.DailyLimit, TotalQuota: u.TotalQuota,
		SpendingLimit: u.SpendingLimit, DailySpendingLimit: u.DailySpendingLimit, PeriodSpendingLimits: u.PeriodSpendingLimits,
		ConcurrencyLimit: u.ConcurrencyLimit, RPMLimit: u.RPMLimit, TPMLimit: u.TPMLimit,
		AllowedModels:        append([]string(nil), u.AllowedModels...),
		AllowedChannels:      append([]string(nil), u.AllowedChannels...),
		AllowedChannelGroups: append([]string(nil), u.AllowedChannelGroups...),
		SystemPrompt:         u.SystemPrompt, AccessPolicy: policy.Clone(),
	}
}

// ValidatePolicyTargetsAllowed 供权限模板保存时复用：白名单非空时，路由目标与降级模型
// 都必须在白名单内。
func ValidatePolicyTargetsAllowed(policy quota.AccessPolicy, allowed []string) error {
	return validatePolicyTargetsAllowed(policy, allowed)
}
