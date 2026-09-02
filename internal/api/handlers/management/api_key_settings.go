package management

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/access"
	configaccess "github.com/router-for-me/CLIProxyAPI/v6/internal/access/config_access"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/enduser"
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
	return h.apiKeySettingsForTenant(effectiveTenantID(c))
}

func (h *Handler) apiKeySettingsForTenant(tenantID string) *apikeysettings.Service {
	if h == nil {
		return apikeysettings.NewService(nil, apikeysettings.WithTenantID(tenantID))
	}

	var auths []*coreauth.Auth
	if h.authManager != nil {
		auths = h.authManager.ListForTenant(tenantID)
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
		apikeysettings.WithTenantID(tenantID),
		apikeysettings.WithChannelGroupValidator(func(values []string) ([]string, error) {
			return h.validateAllowedChannelGroupsForTenant(tenantID, values)
		}),
		apikeysettings.WithEntryValidator(validateEntry),
		apikeysettings.WithLogsDeleter(func(apiKey string) (int64, error) {
			return usage.DeleteLogsByAPIKeyForTenant(tenantID, apiKey)
		}),
	)
}

// api-keys (legacy simple list — now backed by the database)
func (h *Handler) GetAPIKeys(c *gin.Context) {
	c.JSON(200, gin.H{"api-keys": h.apiKeySettings(c).EnabledKeys()})
}

func (h *Handler) PutAPIKeys(c *gin.Context) {
	data, err := c.GetRawData()
	if err != nil {
		c.JSON(400, gin.H{"error": "failed to read body"})
		return
	}
	var arr []string
	if err = json.Unmarshal(data, &arr); err != nil {
		var obj struct {
			Items []string `json:"items"`
		}
		if err2 := json.Unmarshal(data, &obj); err2 != nil || len(obj.Items) == 0 {
			c.JSON(400, gin.H{"error": "invalid body"})
			return
		}
		arr = obj.Items
	}
	if err := h.apiKeySettings(c).ReplaceKeys(arr); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	if err := h.refreshAPIKeyCache(); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(200, gin.H{"status": "ok"})
}

func (h *Handler) PatchAPIKeys(c *gin.Context) {
	var body struct {
		Old *string `json:"old"`
		New *string `json:"new"`
	}
	if err := c.ShouldBindJSON(&body); err != nil || body.Old == nil || body.New == nil {
		c.JSON(400, gin.H{"error": "invalid body"})
		return
	}
	if err := h.apiKeySettings(c).PatchKey(*body.Old, *body.New); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	if err := h.refreshAPIKeyCache(); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(200, gin.H{"status": "ok"})
}

func (h *Handler) DeleteAPIKeys(c *gin.Context) {
	if err := h.apiKeySettings(c).DeleteKey(c.Query("value")); err != nil {
		if errors.Is(err, apikeysettings.ErrMissingValue) {
			c.JSON(400, gin.H{"error": "missing value"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	if err := h.refreshAPIKeyCache(); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(200, gin.H{"status": "ok"})
}

func (h *Handler) GetAPIKeyPermissionProfiles(c *gin.Context) {
	profiles := h.apiKeySettings(c).PermissionProfiles()
	c.JSON(200, gin.H{
		"api-key-permission-profiles": profiles,
		"items":                       profiles,
	})
}

func (h *Handler) PutAPIKeyPermissionProfiles(c *gin.Context) {
	data, err := c.GetRawData()
	if err != nil {
		c.JSON(400, gin.H{"error": "failed to read body"})
		return
	}
	if err := validatePeriodPayloadJSON(data); err != nil {
		if errors.Is(err, quota.ErrPeriodDayLegacyConflict) {
			c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"code": "period_day_legacy_conflict", "message": "daily-spending-limit conflicts with period-spending-limits.day"}})
		} else {
			c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"code": "invalid_period_spending_limit", "message": err.Error()}})
		}
		return
	}

	var profiles []usage.APIKeyPermissionProfileRow
	syncAccounts := false
	if err = json.Unmarshal(data, &profiles); err != nil {
		var obj struct {
			Items        []usage.APIKeyPermissionProfileRow `json:"items"`
			SyncAccounts bool                               `json:"sync-accounts"`
		}
		if err2 := json.Unmarshal(data, &obj); err2 != nil {
			c.JSON(400, gin.H{"error": "invalid body"})
			return
		}
		profiles = obj.Items
		syncAccounts = obj.SyncAccounts
	}

	result, err := h.apiKeySettings(c).ReplacePermissionProfilesWithCaps(profiles, syncAccounts)
	if err != nil {
		if errors.Is(err, enduser.ErrFiveHourProjectionWarming) {
			c.JSON(http.StatusConflict, gin.H{"error": gin.H{"code": "five_hour_quota_projection_warming", "message": "5-hour quota projection is still warming"}})
		} else if errors.Is(err, quota.ErrPeriodDayLegacyConflict) {
			c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"code": "period_day_legacy_conflict", "message": "daily-spending-limit conflicts with period-spending-limits.day"}})
		} else {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		}
		return
	}
	if err := h.refreshAPIKeyCache(); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	if len(result.CappedKeys) > 0 {
		principal, _ := principalFromContext(c)
		if audit := identity.Default(); audit != nil {
			audit.RecordAudit(c.Request.Context(), identity.AuditEvent{
				TenantID: effectiveTenantID(c), ActorKind: principal.Kind, ActorUserID: principal.User.ID, ActorSessionID: principal.SessionID,
				Action: "permission_profile.period_quota.cap_keys", ResourceType: "api_key_permission_profiles", Result: "success",
				Changes: map[string]any{"capped-keys": result.CappedKeys},
			})
		}
	}
	c.JSON(200, gin.H{"status": "ok", "applied_count": result.AppliedCount, "capped-keys": result.CappedKeys})
}

// api-key-entries: backed by the api_keys table
func (h *Handler) GetAPIKeyEntries(c *gin.Context) {
	entries, err := h.apiKeySettings(c).ListEntriesWithDailySpending()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
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
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body"})
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
			c.JSON(http.StatusNotFound, gin.H{"error": "item not found"})
			return
		case errors.Is(err, apikeysettings.ErrDailySpendingLimitMissing):
			c.JSON(http.StatusBadRequest, gin.H{"error": "daily spending limit is not set"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
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
		c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"code": "invalid_body", "message": "Request body must be valid JSON"}})
		return
	}
	periods, err := quota.NormalizePeriods(body.Periods)
	if err != nil {
		var invalid *quota.InvalidPeriodError
		switch {
		case errors.Is(err, quota.ErrPeriodsRequired):
			c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"code": "periods_required", "message": "periods must be a non-empty array"}})
		case errors.As(err, &invalid):
			c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"code": "invalid_period", "message": "Invalid period " + string(invalid.Period)}})
		default:
			c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"code": "invalid_period", "message": err.Error()}})
		}
		return
	}
	result, err := h.apiKeySettings(c).ResetPeriodSpending(body.ID, body.Key, periods, periodResetActorFromContext(c))
	if err != nil {
		switch {
		case errors.Is(err, apikeysettings.ErrItemNotFound):
			c.JSON(http.StatusNotFound, gin.H{"error": gin.H{"code": "item_not_found", "message": "item not found"}})
		case writeKeyPeriodResetError(c, err, "Key"):
		default:
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
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
			c.JSON(http.StatusNotFound, gin.H{"error": "item not found"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
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
		c.JSON(400, gin.H{"error": "failed to read body"})
		return
	}
	if err := validatePeriodPayloadJSON(data); err != nil {
		if errors.Is(err, quota.ErrPeriodDayLegacyConflict) {
			c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"code": "period_day_legacy_conflict", "message": "daily-spending-limit conflicts with period-spending-limits.day"}})
		} else {
			c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"code": "invalid_period_spending_limit", "message": err.Error()}})
		}
		return
	}
	var arr []config.APIKeyEntry
	if err = json.Unmarshal(data, &arr); err != nil {
		var obj struct {
			Items []config.APIKeyEntry `json:"items"`
		}
		if err2 := json.Unmarshal(data, &obj); err2 != nil || len(obj.Items) == 0 {
			c.JSON(400, gin.H{"error": "invalid body"})
			return
		}
		arr = obj.Items
	}
	if err := h.apiKeySettings(c).ReplaceEntries(arr); err != nil {
		if errors.Is(err, apikeysettings.ErrInvalidEntry) || errors.Is(err, apikeysettings.ErrKeyRequired) {
			c.JSON(http.StatusBadRequest, gin.H{"error": apiKeyEntryErrorMessage(err)})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	if err := h.refreshAPIKeyCache(); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
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
	if err := c.ShouldBindJSON(&body); err != nil || body.Value == nil {
		c.JSON(400, gin.H{"error": "invalid body"})
		return
	}
	if err := h.apiKeySettings(c).PatchEntry(body.ID, body.Index, body.Match, *body.Value); err != nil {
		var exceeds *quota.LimitExceedsAccountError
		switch {
		case errors.Is(err, quota.ErrPeriodDayLegacyConflict):
			c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"code": "period_day_legacy_conflict", "message": "daily-spending-limit conflicts with period-spending-limits.day"}})
			return
		case errors.Is(err, enduser.ErrFiveHourProjectionWarming):
			c.JSON(http.StatusConflict, gin.H{"error": gin.H{"code": "five_hour_quota_projection_warming", "message": "5-hour quota projection is still warming"}})
			return
		case errors.As(err, &exceeds):
			c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"code": "key_period_limit_exceeds_account", "message": exceeds.Error(), "details": gin.H{"period": exceeds.Period, "key_limit": exceeds.KeyLimit, "account_limit": exceeds.AccountLimit}}})
			return
		case errors.Is(err, apikeysettings.ErrOwnedKeyAccountManagedField):
			c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"code": "owned_key_account_managed_field", "message": err.Error()}})
			return
		case errors.Is(err, apikeysettings.ErrItemNotFound):
			c.JSON(http.StatusNotFound, gin.H{"error": "item not found"})
			return
		case errors.Is(err, apikeysettings.ErrDuplicateKey):
			c.JSON(http.StatusConflict, gin.H{"error": "api key already exists"})
			return
		case errors.Is(err, apikeysettings.ErrInvalidEntry), errors.Is(err, apikeysettings.ErrKeyRequired):
			c.JSON(http.StatusBadRequest, gin.H{"error": apiKeyEntryErrorMessage(err)})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	if err := h.refreshAPIKeyCache(); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
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
			c.JSON(400, gin.H{"error": "missing key or index"})
			return
		}
		index = &parsed
	}

	result, err := h.apiKeySettings(c).DeleteEntry(c.Query("key"), id, index, shouldDeleteAPIKeyLogs(c))
	if err != nil {
		if errors.Is(err, apikeysettings.ErrMissingKeyOrIndex) {
			c.JSON(400, gin.H{"error": "missing key or index"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	if err := h.refreshAPIKeyCache(); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
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
