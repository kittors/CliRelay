package management

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/access"
	configaccess "github.com/router-for-me/CLIProxyAPI/v6/internal/access/config_access"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/configsync"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/contentmoderation"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/identity"
	apikeysettings "github.com/router-for-me/CLIProxyAPI/v6/internal/management/settings/apikey"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/quota"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/usage"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

// refreshAPIKeyCache rebuilds the in-memory access provider cache from the database.
// Must be called after every API key write operation.
// Returns error when live access manager update fails (fail-closed for callers that care).
func (h *Handler) refreshAPIKeyCache() error {
	if h == nil || h.cfg == nil {
		return nil
	}
	// Always update the global provider registry (used during config reload and service bootstrap).
	configaccess.Register(&h.cfg.SDKConfig)
	// Also update the live access manager provider snapshot so changes take effect immediately
	// without waiting for a full config reload.
	if h.accessManager != nil {
		if _, err := access.ApplyAccessProviders(h.accessManager, nil, h.cfg); err != nil {
			return err
		}
	}
	return nil
}

func (h *Handler) apiKeySettings(c *gin.Context) *apikeysettings.Service {
	// A full replacement is checked against the collection version the client
	// read, when it sent one.
	return h.apiKeySettingsForTenant(effectiveTenantID(c), apikeysettings.WithExpectedVersion(requestExpectedVersion(c)))
}

func (h *Handler) apiKeySettingsForTenant(tenantID string, extra ...apikeysettings.Option) *apikeysettings.Service {
	if h == nil {
		return apikeysettings.NewService(nil, append([]apikeysettings.Option{apikeysettings.WithTenantID(tenantID)}, extra...)...)
	}

	var auths []*coreauth.Auth
	if h.authManager != nil {
		// A key may be limited to a shared platform credential's channel.
		auths = h.authManager.ListUsableForTenant(tenantID)
	}

	routingCfg := currentRoutingConfigForTenant(h.cfg, tenantID)

	validateEntry := func(entry config.APIKeyEntry) error {
		return validateRoutingAndAPIKeyRestrictions(&config.Config{
			SDKConfig: config.SDKConfig{
				APIKeyEntries: []config.APIKeyEntry{entry},
			},
			Routing: routingCfg,
		}, auths)
	}

	return apikeysettings.NewService(
		func(values []string) ([]string, error) {
			return h.sanitizeAllowedChannelsForTenant(tenantID, values)
		},
		append([]apikeysettings.Option{apikeysettings.WithTenantID(tenantID),
			apikeysettings.WithChannelGroupValidator(func(values []string) ([]string, error) {
				return h.validateAllowedChannelGroupsForTenant(tenantID, values)
			}),
			apikeysettings.WithEntryValidator(validateEntry),
			apikeysettings.WithLogsDeleter(func(apiKeyID string) (int64, error) {
				return usage.DeleteLogsByAPIKeyIDForTenant(tenantID, apiKeyID)
			}),
			// The word guard's override of a deleted key goes with it, so that a key
			// created later under the same id does not inherit it.
			apikeysettings.WithKeyPolicyDeleter(func(apiKeyIDs []string) error {
				_, err := h.contentModerationStore().DeleteWordPoliciesForKeys(context.Background(), tenantID, apiKeyIDs)
				if errors.Is(err, contentmoderation.ErrUnavailable) {
					return nil // no database, no policies
				}
				return err
			}),
			apikeysettings.WithExpectedVersion(configsync.AnyVersion),
		}, extra...)...,
	)
}

// api-keys (legacy simple list — now backed by the database)
func (h *Handler) GetAPIKeys(c *gin.Context) {
	setVersionHeader(c, usage.ConfigCollectionVersion(configsync.DomainAPIKeys, effectiveTenantID(c)))
	c.JSON(200, gin.H{"api-keys": h.apiKeySettings(c).EnabledKeys()})
}

func (h *Handler) PutAPIKeys(c *gin.Context) {
	data, err := c.GetRawData()
	if err != nil {
		writeReadBodyError(c, err, 0)
		return
	}
	var arr []string
	if err = json.Unmarshal(data, &arr); err != nil {
		var obj struct {
			Items []string `json:"items"`
		}
		if err2 := json.Unmarshal(data, &obj); err2 != nil || len(obj.Items) == 0 {
			writeInvalidBodyError(c, err)
			return
		}
		arr = obj.Items
	}
	if err := h.apiKeySettings(c).ReplaceKeys(arr); err != nil {
		writeConfigSaveError(c, "保存 API Key 列表失败", err)
		return
	}
	if err := h.refreshAPIKeyCache(); err != nil {
		writeInternalError(c, "刷新 API Key 运行时缓存", err)
		return
	}
	c.JSON(200, gin.H{"status": "ok"})
}

func (h *Handler) PatchAPIKeys(c *gin.Context) {
	var body struct {
		Old *string `json:"old"`
		New *string `json:"new"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		writeInvalidBodyError(c, err)
		return
	}
	if body.Old == nil || body.New == nil {
		writeMissingFieldError(c, "old/new", "更新 API Key 必须同时提供 old（原密钥）和 new（新密钥）字段")
		return
	}
	if err := h.apiKeySettings(c).PatchKey(*body.Old, *body.New); err != nil {
		writeInternalError(c, "更新 API Key", err)
		return
	}
	if err := h.refreshAPIKeyCache(); err != nil {
		writeInternalError(c, "刷新 API Key 运行时缓存", err)
		return
	}
	c.JSON(200, gin.H{"status": "ok"})
}

func (h *Handler) DeleteAPIKeys(c *gin.Context) {
	if err := h.apiKeySettings(c).DeleteKey(c.Query("value")); err != nil {
		if errors.Is(err, apikeysettings.ErrMissingValue) {
			writeMissingFieldError(c, "value", "请通过查询参数 value 指定要删除的 API Key")
			return
		}
		writeInternalError(c, "删除 API Key", err)
		return
	}
	if err := h.refreshAPIKeyCache(); err != nil {
		writeInternalError(c, "刷新 API Key 运行时缓存", err)
		return
	}
	c.JSON(200, gin.H{"status": "ok"})
}

func (h *Handler) GetAPIKeyPermissionProfiles(c *gin.Context) {
	profiles := permissionProfileDTOs(h.apiKeySettings(c).PermissionProfiles())
	setVersionHeader(c, usage.ConfigCollectionVersion(configsync.DomainPermissionProfiles, effectiveTenantID(c)))
	c.JSON(200, gin.H{
		"api-key-permission-profiles": profiles,
		"items":                       profiles,
		"capabilities":                accessPolicyCapabilities(),
	})
}

func (h *Handler) PutAPIKeyPermissionProfiles(c *gin.Context) {
	data, err := c.GetRawData()
	if err != nil {
		writeReadBodyError(c, err, 0)
		return
	}
	if err := validatePeriodPayloadJSON(data); err != nil {
		if errors.Is(err, quota.ErrPeriodDayLegacyConflict) {
			writeError(c, http.StatusBadRequest, "period_day_legacy_conflict", "每日消费限额（daily-spending-limit）与周期限额中的日限额（period-spending-limits.day）存在冲突，请保持一致或仅配置其一")
		} else {
			writeError(c, http.StatusBadRequest, "invalid_period_spending_limit", "周期消费限额配置无效："+err.Error())
		}
		return
	}

	items, syncAccounts, err := decodePermissionProfilePayload(data)
	if err != nil {
		writeInvalidBodyError(c, err)
		return
	}
	settings := h.apiKeySettings(c)
	profiles, err := permissionProfilesFromDTOs(items, settings.PermissionProfiles())
	if err != nil {
		if !writeAccessPolicyError(c, err) {
			writeError(c, http.StatusBadRequest, "invalid_permission_profile", "权限模板配置校验失败："+err.Error())
		}
		return
	}

	result, err := settings.ReplacePermissionProfilesWithCaps(profiles, syncAccounts)
	if err != nil {
		if writeConfigConflict(c, err) || writeAccessPolicyError(c, err) {
			return
		}
		if errors.Is(err, quota.ErrPeriodDayLegacyConflict) {
			writeError(c, http.StatusBadRequest, "period_day_legacy_conflict", "每日消费限额（daily-spending-limit）与周期限额中的日限额（period-spending-limits.day）存在冲突，请保持一致或仅配置其一")
		} else {
			writeError(c, http.StatusBadRequest, "save_permission_profiles_failed", "保存权限模板失败："+err.Error())
		}
		return
	}
	if err := h.refreshAPIKeyCache(); err != nil {
		writeInternalError(c, "刷新 API Key 运行时缓存", err)
		return
	}
	if len(result.CappedCells) > 0 {
		principal, _ := principalFromContext(c)
		if audit := identity.Default(); audit != nil {
			audit.RecordAudit(c.Request.Context(), identity.AuditEvent{
				TenantID: effectiveTenantID(c), ActorKind: principal.Kind, ActorUserID: principal.User.ID, ActorSessionID: principal.SessionID,
				Action: "permission_profile.period_quota.cap_keys", ResourceType: "api_key_permission_profiles", Result: "success",
				Changes: map[string]any{"capped-keys": result.CappedKeys, "capped-cells": result.CappedCells},
			})
		}
	}
	c.JSON(200, gin.H{"status": "ok", "applied_count": result.AppliedCount, "capped-keys": result.CappedKeys, "capped-cells": result.CappedCells})
}

// api-key-entries: backed by the api_keys table
func (h *Handler) GetAPIKeyEntries(c *gin.Context) {
	version := usage.ConfigCollectionVersion(configsync.DomainAPIKeys, effectiveTenantID(c))
	entries, err := h.apiKeySettings(c).ListEntriesWithDailySpending()
	if err != nil {
		writeInternalError(c, "查询 API Key 条目列表", err)
		return
	}
	setVersionHeader(c, version)
	c.JSON(200, gin.H{"api-key-entries": entries})
}

// ResetAPIKeyDailySpending sets today's spending baseline so effective used becomes 0.
// POST /v0/management/api-key-entries/daily-spending/reset
func (h *Handler) ResetAPIKeyDailySpending(c *gin.Context) {
	var body struct {
		ID  *string `json:"id"`
		Key *string `json:"key"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		writeInvalidBodyError(c, err)
		return
	}
	actor := apikeysettings.DailySpendingResetActor{Kind: "service_credential"}
	if principal, ok := principalFromContext(c); ok {
		actor.Kind = strings.TrimSpace(principal.Kind)
		if actor.Kind == "" {
			actor.Kind = "service_credential"
		}
		actor.UserID = strings.TrimSpace(principal.User.ID)
		actor.Username = strings.TrimSpace(principal.User.Username)
		if actor.Username == "" {
			actor.Username = strings.TrimSpace(principal.User.DisplayName)
		}
	}
	result, err := h.apiKeySettings(c).ResetDailySpending(body.ID, body.Key, actor)
	if err != nil {
		switch {
		case errors.Is(err, apikeysettings.ErrItemNotFound):
			writeNotFoundError(c, "API Key 条目", "")
			return
		case errors.Is(err, apikeysettings.ErrDailySpendingLimitMissing):
			writeError(c, http.StatusBadRequest, "daily_spending_limit_missing", "该 API Key 尚未配置每日消费限额，无需重置今日消费统计")
			return
		}
		writeInternalError(c, "重置 API Key 今日消费统计", err)
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"status":                     "ok",
		"id":                         result.ID,
		"key":                        result.Key,
		"daily-spending-limit":       result.DailySpendingLimit,
		"daily-spending-used":        result.DailySpendingUsed,
		"daily-spending-remaining":   result.DailySpendingRemaining,
		"daily-spending-reset-count": result.DailySpendingResetCount,
	})
}

// ResetAPIKeyPeriodSpending resets configured period baselines for one key.
// POST /v0/management/api-key-entries/period-spending/reset
func (h *Handler) ResetAPIKeyPeriodSpending(c *gin.Context) {
	var body struct {
		ID      *string        `json:"id"`
		Key     *string        `json:"key"`
		Periods []quota.Period `json:"periods"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		writeError(c, http.StatusBadRequest, "invalid_body", "请求体 JSON 格式无效，请检查传入的密钥标识和 periods 数组")
		return
	}
	periods, err := quota.NormalizePeriods(body.Periods)
	if err != nil {
		var invalid *quota.InvalidPeriodError
		switch {
		case errors.Is(err, quota.ErrPeriodsRequired):
			writeError(c, http.StatusBadRequest, "periods_required", "缺少要重置的周期列表（periods），请至少选择一个周期（5h、day、week 或 month）")
		case errors.As(err, &invalid):
			writeError(c, http.StatusBadRequest, "invalid_period", "不支持的周期类型 "+string(invalid.Period)+"，仅允许 5h、day、week 或 month")
		default:
			writeError(c, http.StatusBadRequest, "invalid_period", "周期参数无效："+err.Error())
		}
		return
	}
	result, err := h.apiKeySettings(c).ResetPeriodSpending(body.ID, body.Key, periods, periodResetActorFromContext(c))
	if err != nil {
		switch {
		case errors.Is(err, apikeysettings.ErrItemNotFound):
			writeError(c, http.StatusNotFound, "item_not_found", "未找到指定的 API Key 条目，可能已被删除或不属于当前租户")
		case writeKeyPeriodResetError(c, err, "Key"):
		default:
			writeInternalError(c, "重置 API Key 周期消费额度", err)
		}
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"status":                "ok",
		"id":                    result.ID,
		"key":                   result.Key,
		"periods":               result.Periods,
		"period-spending":       result.PeriodSpending,
		"reset-count":           result.ResetCount,
		"effective-used-before": result.EffectiveUsedBefore,
		"raw":                   result.Raw,
	})
}

// GetAPIKeyDailySpendingResetHistory lists manual reset events for a key.
// GET /v0/management/api-key-entries/daily-spending/reset-history?id=... or ?key=...
func (h *Handler) GetAPIKeyDailySpendingResetHistory(c *gin.Context) {
	id := strings.TrimSpace(c.Query("id"))
	key := strings.TrimSpace(c.Query("key"))
	var idPtr, keyPtr *string
	if id != "" {
		idPtr = &id
	}
	if key != "" {
		keyPtr = &key
	}
	limit := 100
	if raw := strings.TrimSpace(c.Query("limit")); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil {
			limit = n
		}
	}
	events, err := h.apiKeySettings(c).ListDailySpendingResetHistory(idPtr, keyPtr, limit)
	if err != nil {
		if errors.Is(err, apikeysettings.ErrItemNotFound) {
			writeNotFoundError(c, "API Key 条目", id)
			return
		}
		writeInternalError(c, "查询 API Key 消费重置历史", err)
		return
	}
	if events == nil {
		events = []usage.APIKeyDailySpendingResetEvent{}
	}
	c.JSON(http.StatusOK, gin.H{"items": events, "total": len(events)})
}

func (h *Handler) PutAPIKeyEntries(c *gin.Context) {
	data, err := c.GetRawData()
	if err != nil {
		writeReadBodyError(c, err, 0)
		return
	}
	if err := validatePeriodPayloadJSON(data); err != nil {
		if errors.Is(err, quota.ErrPeriodDayLegacyConflict) {
			writeError(c, http.StatusBadRequest, "period_day_legacy_conflict", "每日消费限额（daily-spending-limit）与周期限额中的日限额（period-spending-limits.day）存在冲突，请保持一致或仅配置其一")
		} else {
			writeError(c, http.StatusBadRequest, "invalid_period_spending_limit", "周期消费限额配置无效："+err.Error())
		}
		return
	}
	var arr []config.APIKeyEntry
	if err = json.Unmarshal(data, &arr); err != nil {
		var obj struct {
			Items []config.APIKeyEntry `json:"items"`
		}
		if err2 := json.Unmarshal(data, &obj); err2 != nil || len(obj.Items) == 0 {
			writeInvalidBodyError(c, err)
			return
		}
		arr = obj.Items
	}
	if err := h.apiKeySettings(c).ReplaceEntries(arr); err != nil {
		if writeConfigConflict(c, err) {
			return
		}
		if errors.Is(err, apikeysettings.ErrInvalidEntry) || errors.Is(err, apikeysettings.ErrKeyRequired) {
			writeError(c, http.StatusBadRequest, "invalid_api_key_entry", "API Key 配置校验失败："+apiKeyEntryErrorMessage(err))
			return
		}
		writeInternalError(c, "保存 API Key 条目列表", err)
		return
	}
	if err := h.refreshAPIKeyCache(); err != nil {
		writeInternalError(c, "刷新 API Key 运行时缓存", err)
		return
	}
	c.JSON(200, gin.H{"status": "ok"})
}

func (h *Handler) PatchAPIKeyEntry(c *gin.Context) {
	var body struct {
		ID    *string                    `json:"id"`
		Index *int                       `json:"index"`
		Match *string                    `json:"match"`
		Value *apikeysettings.EntryPatch `json:"value"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		writeInvalidBodyError(c, err)
		return
	}
	if body.Value == nil {
		writeMissingFieldError(c, "value", "请提供包含要修改字段的 value 对象")
		return
	}
	if err := h.apiKeySettings(c).PatchEntry(body.ID, body.Index, body.Match, *body.Value); err != nil {
		var exceeds *quota.LimitExceedsAccountError
		switch {
		case errors.Is(err, quota.ErrPeriodDayLegacyConflict):
			writeError(c, http.StatusBadRequest, "period_day_legacy_conflict", "每日消费限额（daily-spending-limit）与周期限额中的日限额（period-spending-limits.day）存在冲突，请保持一致或仅配置其一")
			return
		case errors.As(err, &exceeds):
			writeError(c, http.StatusBadRequest, "key_period_limit_exceeds_account", exceeds.Error(), map[string]any{"period": exceeds.Period, "key_limit": exceeds.KeyLimit, "account_limit": exceeds.AccountLimit})
			return
		case errors.Is(err, apikeysettings.ErrOwnedKeyAccountManagedField):
			writeError(c, http.StatusBadRequest, "owned_key_account_managed_field", err.Error())
			return
		case errors.Is(err, apikeysettings.ErrItemNotFound):
			writeNotFoundError(c, "API Key 条目", "")
			return
		case errors.Is(err, apikeysettings.ErrDuplicateKey):
			writeError(c, http.StatusConflict, "duplicate_api_key", "该 API Key 已存在，请勿重复创建或修改为已占用的密钥值")
			return
		case errors.Is(err, apikeysettings.ErrInvalidEntry), errors.Is(err, apikeysettings.ErrKeyRequired):
			writeError(c, http.StatusBadRequest, "invalid_api_key_entry", "API Key 配置校验失败："+apiKeyEntryErrorMessage(err))
			return
		}
		writeInternalError(c, "更新 API Key 条目", err)
		return
	}
	if err := h.refreshAPIKeyCache(); err != nil {
		writeInternalError(c, "刷新 API Key 运行时缓存", err)
		return
	}
	c.JSON(200, gin.H{"status": "ok"})
}

func (h *Handler) DeleteAPIKeyEntry(c *gin.Context) {
	var id *string
	if value := strings.TrimSpace(c.Query("id")); value != "" {
		id = &value
	}
	var index *int
	if idxStr := strings.TrimSpace(c.Query("index")); idxStr != "" {
		parsed, err := strconv.Atoi(idxStr)
		if err != nil {
			writeInvalidFieldError(c, "index", "index 必须是合法的整数序号")
			return
		}
		index = &parsed
	}

	result, err := h.apiKeySettings(c).DeleteEntry(c.Query("key"), id, index, shouldDeleteAPIKeyLogs(c))
	if err != nil {
		if errors.Is(err, apikeysettings.ErrMissingKeyOrIndex) {
			writeMissingFieldError(c, "id/key/index", "删除 API Key 时必须提供 id、key 或 index 参数之一")
			return
		}
		writeInternalError(c, "删除 API Key 条目", err)
		return
	}
	if err := h.refreshAPIKeyCache(); err != nil {
		writeInternalError(c, "刷新 API Key 运行时缓存", err)
		return
	}
	c.JSON(200, gin.H{"status": "ok", "logs_deleted": result.LogsDeleted})
}

func shouldDeleteAPIKeyLogs(c *gin.Context) bool {
	raw := strings.TrimSpace(c.Query("delete_logs"))
	if raw == "" {
		return true
	}
	value, err := strconv.ParseBool(raw)
	if err != nil {
		return true
	}
	return value
}

func apiKeyEntryErrorMessage(err error) string {
	if err == nil {
		return ""
	}
	if errors.Is(err, apikeysettings.ErrInvalidEntry) {
		prefix := apikeysettings.ErrInvalidEntry.Error() + ": "
		return strings.TrimPrefix(err.Error(), prefix)
	}
	return err.Error()
}
