package management

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/enduser"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/quota"
	sqlapikey "github.com/router-for-me/CLIProxyAPI/v6/internal/storage/sqlstore/apikey"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/usage"
)

// permissionProfileDTO 是权限模板在管理接口上的形态：既有字段之外，以 quota-limits /
// model-rules / model-routes 收发模板的访问策略。三个新字段在请求里缺失（旧版面板）
// 时保留模板原有的值，整表替换不会把新面板配置的规则清空。
type permissionProfileDTO struct {
	usage.APIKeyPermissionProfileRow
	QuotaLimits *quota.Matrix       `json:"quota-limits"`
	ModelRules  *[]quota.ModelRule  `json:"model-rules"`
	ModelRoutes *[]quota.ModelRoute `json:"model-routes"`
}

func permissionProfileDTOs(profiles []usage.APIKeyPermissionProfileRow) []permissionProfileDTO {
	out := make([]permissionProfileDTO, 0, len(profiles))
	for _, profile := range profiles {
		limits := sqlapikey.ProfileQuotaMatrix(profile)
		rules := profile.AccessPolicy.ModelRules
		if rules == nil {
			rules = []quota.ModelRule{}
		}
		routes := profile.AccessPolicy.ModelRoutes
		if routes == nil {
			routes = []quota.ModelRoute{}
		}
		out = append(out, permissionProfileDTO{APIKeyPermissionProfileRow: profile, QuotaLimits: &limits, ModelRules: &rules, ModelRoutes: &routes})
	}
	return out
}

// decodePermissionProfilePayload 解析整表替换请求（数组或 {items, sync-accounts}）。
func decodePermissionProfilePayload(data []byte) ([]permissionProfileDTO, bool, error) {
	var items []permissionProfileDTO
	if err := json.Unmarshal(data, &items); err == nil {
		return items, false, nil
	}
	var obj struct {
		Items        []permissionProfileDTO `json:"items"`
		SyncAccounts bool                   `json:"sync-accounts"`
	}
	if err := json.Unmarshal(data, &obj); err != nil {
		return nil, false, err
	}
	return obj.Items, obj.SyncAccounts, nil
}

// permissionProfilesFromDTOs 把请求转换成模板行。quota-limits 给出时它是模板额度的
// 唯一来源（覆盖旧的扁平字段）；规则与路由的目标模型必须在模板的可用模型之内。
func permissionProfilesFromDTOs(items []permissionProfileDTO, current []usage.APIKeyPermissionProfileRow) ([]usage.APIKeyPermissionProfileRow, error) {
	currentByID := make(map[string]usage.APIKeyPermissionProfileRow, len(current))
	for _, profile := range current {
		currentByID[strings.TrimSpace(profile.ID)] = profile
	}
	out := make([]usage.APIKeyPermissionProfileRow, 0, len(items))
	for i, item := range items {
		row := item.APIKeyPermissionProfileRow
		policy := quota.AccessPolicy{}
		if prev, ok := currentByID[strings.TrimSpace(row.ID)]; ok {
			policy = prev.AccessPolicy.Clone()
		}
		if item.QuotaLimits != nil {
			limits, err := quota.NormalizeMatrix(*item.QuotaLimits)
			if err != nil {
				return nil, &sqlapikey.ProfilePolicyError{Index: i, ProfileID: row.ID, Err: &quota.PolicyValidationError{Field: "quota-limits", Message: err.Error()}}
			}
			cols := quota.ProfileColumnLayout.ColumnsFromMatrix(limits)
			row.PeriodSpendingLimits = cols.Period
			row.DailySpendingLimit = cols.Period.Day
			row.DailyLimit = cols.DailyRequests
			row.TotalQuota = cols.TotalRequests
			policy.Limits = limits
		}
		if item.ModelRules != nil {
			policy.ModelRules = *item.ModelRules
		}
		if item.ModelRoutes != nil {
			policy.ModelRoutes = *item.ModelRoutes
		}
		normalized, err := quota.NormalizeAccessPolicy(policy, quota.ProfileColumnLayout)
		if err != nil {
			return nil, &sqlapikey.ProfilePolicyError{Index: i, ProfileID: row.ID, Err: err}
		}
		if err := enduser.ValidatePolicyTargetsAllowed(normalized, row.AllowedModels); err != nil {
			return nil, fmt.Errorf("permission profile %q: %w", row.ID, err)
		}
		row.AccessPolicy = normalized
		out = append(out, row)
	}
	return out, nil
}
