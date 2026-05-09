package management

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/access"
	configaccess "github.com/router-for-me/CLIProxyAPI/v6/internal/access/config_access"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/usage"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

// refreshAPIKeyCache rebuilds the in-memory access provider cache from SQLite.
// Must be called after every API key write operation.
func (h *Handler) refreshAPIKeyCache() {
	if h == nil || h.cfg == nil {
		return
	}
	// Always update the global provider registry (used during config reload and service bootstrap).
	configaccess.Register(&h.cfg.SDKConfig)
	// Also update the live access manager provider snapshot so changes take effect immediately
	// without waiting for a full config reload.
	if h.accessManager != nil {
		_, _ = access.ApplyAccessProviders(h.accessManager, nil, h.cfg)
	}
}

// Generic helpers for list[string]
func (h *Handler) putStringList(c *gin.Context, set func([]string), after func()) {
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
	set(arr)
	if after != nil {
		after()
	}
	h.persist(c)
}

func (h *Handler) patchStringList(c *gin.Context, target *[]string, after func()) {
	var body struct {
		Old   *string `json:"old"`
		New   *string `json:"new"`
		Index *int    `json:"index"`
		Value *string `json:"value"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(400, gin.H{"error": "invalid body"})
		return
	}
	if body.Index != nil && body.Value != nil && *body.Index >= 0 && *body.Index < len(*target) {
		(*target)[*body.Index] = *body.Value
		if after != nil {
			after()
		}
		h.persist(c)
		return
	}
	if body.Old != nil && body.New != nil {
		for i := range *target {
			if (*target)[i] == *body.Old {
				(*target)[i] = *body.New
				if after != nil {
					after()
				}
				h.persist(c)
				return
			}
		}
		*target = append(*target, *body.New)
		if after != nil {
			after()
		}
		h.persist(c)
		return
	}
	c.JSON(400, gin.H{"error": "missing fields"})
}

func (h *Handler) deleteFromStringList(c *gin.Context, target *[]string, after func()) {
	if idxStr := c.Query("index"); idxStr != "" {
		var idx int
		_, err := fmt.Sscanf(idxStr, "%d", &idx)
		if err == nil && idx >= 0 && idx < len(*target) {
			*target = append((*target)[:idx], (*target)[idx+1:]...)
			if after != nil {
				after()
			}
			h.persist(c)
			return
		}
	}
	if val := strings.TrimSpace(c.Query("value")); val != "" {
		out := make([]string, 0, len(*target))
		for _, v := range *target {
			if strings.TrimSpace(v) != val {
				out = append(out, v)
			}
		}
		*target = out
		if after != nil {
			after()
		}
		h.persist(c)
		return
	}
	c.JSON(400, gin.H{"error": "missing index or value"})
}

// api-keys (legacy simple list — now backed by SQLite)
func (h *Handler) GetAPIKeys(c *gin.Context) {
	rows := usage.ListAPIKeys()
	keys := make([]string, 0, len(rows))
	for _, r := range rows {
		if !r.Disabled {
			keys = append(keys, r.Key)
		}
	}
	c.JSON(200, gin.H{"api-keys": keys})
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
	var rows []usage.APIKeyRow
	for _, k := range arr {
		trimmed := strings.TrimSpace(k)
		if trimmed != "" {
			rows = append(rows, usage.APIKeyRow{Key: trimmed})
		}
	}
	if err := usage.ReplaceAllAPIKeys(rows); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	h.refreshAPIKeyCache()
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
	oldKey := strings.TrimSpace(*body.Old)
	newKey := strings.TrimSpace(*body.New)
	if oldKey != "" {
		_ = usage.DeleteAPIKey(oldKey)
	}
	if newKey != "" {
		if err := usage.UpsertAPIKey(usage.APIKeyRow{Key: newKey}); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
	}
	h.refreshAPIKeyCache()
	c.JSON(200, gin.H{"status": "ok"})
}
func (h *Handler) DeleteAPIKeys(c *gin.Context) {
	if val := strings.TrimSpace(c.Query("value")); val != "" {
		if err := usage.DeleteAPIKey(val); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		h.refreshAPIKeyCache()
		c.JSON(200, gin.H{"status": "ok"})
		return
	}
	c.JSON(400, gin.H{"error": "missing value"})
}

func (h *Handler) GetAPIKeyPermissionProfiles(c *gin.Context) {
	profiles := usage.ListAPIKeyPermissionProfiles()
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

	var profiles []usage.APIKeyPermissionProfileRow
	if err = json.Unmarshal(data, &profiles); err != nil {
		var obj struct {
			Items []usage.APIKeyPermissionProfileRow `json:"items"`
		}
		if err2 := json.Unmarshal(data, &obj); err2 != nil {
			c.JSON(400, gin.H{"error": "invalid body"})
			return
		}
		profiles = obj.Items
	}

	for idx := range profiles {
		profiles[idx].ID = strings.TrimSpace(profiles[idx].ID)
		profiles[idx].Name = strings.TrimSpace(profiles[idx].Name)
		if profiles[idx].ID == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "id is required"})
			return
		}
		if profiles[idx].Name == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "name is required"})
			return
		}
	}

	if err := usage.ReplaceAllAPIKeyPermissionProfiles(profiles); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	c.JSON(200, gin.H{"status": "ok"})
}

// api-key-entries: backed by SQLite api_keys table
func (h *Handler) GetAPIKeyEntries(c *gin.Context) {
	rows := usage.ListAPIKeys()
	entries := make([]config.APIKeyEntry, 0, len(rows))
	for _, r := range rows {
		entries = append(entries, r.ToConfigEntry())
	}
	c.JSON(200, gin.H{"api-key-entries": entries})
}

func (h *Handler) PutAPIKeyEntries(c *gin.Context) {
	data, err := c.GetRawData()
	if err != nil {
		c.JSON(400, gin.H{"error": "failed to read body"})
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
	var rows []usage.APIKeyRow
	var auths []*coreauth.Auth
	if h != nil && h.authManager != nil {
		auths = h.authManager.List()
	}
	routingCfg := config.RoutingConfig{}
	if h != nil && h.cfg != nil {
		routingCfg = h.cfg.Routing
	}
	for _, entry := range arr {
		entry.AllowedChannels = uniqueChannels(entry.AllowedChannels)
		entry.AllowedChannelGroups = uniqueChannelGroups(entry.AllowedChannelGroups)
		validated, errValidate := h.validateAllowedChannels(entry.AllowedChannels)
		if errValidate != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": errValidate.Error()})
			return
		}
		entry.AllowedChannels = validated
		validatedGroups, errValidate := h.validateAllowedChannelGroups(entry.AllowedChannelGroups)
		if errValidate != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": errValidate.Error()})
			return
		}
		entry.AllowedChannelGroups = validatedGroups
		if errValidate := validateRoutingAndAPIKeyRestrictions(&config.Config{
			SDKConfig: config.SDKConfig{
				APIKeyEntries: []config.APIKeyEntry{entry},
			},
			Routing: routingCfg,
		}, auths); errValidate != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": errValidate.Error()})
			return
		}
		rows = append(rows, usage.APIKeyRowFromConfig(entry))
	}
	if err := usage.ReplaceAllAPIKeys(rows); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	h.refreshAPIKeyCache()
	c.JSON(200, gin.H{"status": "ok"})
}

func (h *Handler) PatchAPIKeyEntry(c *gin.Context) {
	type apiKeyEntryPatch struct {
		Key                  *string   `json:"key"`
		Name                 *string   `json:"name"`
		DailyLimit           *int      `json:"daily-limit"`
		TotalQuota           *int      `json:"total-quota"`
		SpendingLimit        *float64  `json:"spending-limit"`
		ConcurrencyLimit     *int      `json:"concurrency-limit"`
		RPMLimit             *int      `json:"rpm-limit"`
		TPMLimit             *int      `json:"tpm-limit"`
		AllowedModels        *[]string `json:"allowed-models"`
		AllowedChannels      *[]string `json:"allowed-channels"`
		AllowedChannelGroups *[]string `json:"allowed-channel-groups"`
		SystemPrompt         *string   `json:"system-prompt"`
		CreatedAt            *string   `json:"created-at"`
	}
	var body struct {
		Index *int              `json:"index"`
		Match *string           `json:"match"`
		Value *apiKeyEntryPatch `json:"value"`
	}
	if err := c.ShouldBindJSON(&body); err != nil || body.Value == nil {
		c.JSON(400, gin.H{"error": "invalid body"})
		return
	}

	// Find existing entry by index or match key
	var targetKey string
	if body.Match != nil {
		targetKey = strings.TrimSpace(*body.Match)
	}
	if targetKey == "" && body.Index != nil {
		rows := usage.ListAPIKeys()
		if *body.Index >= 0 && *body.Index < len(rows) {
			targetKey = rows[*body.Index].Key
		}
	}

	var entry usage.APIKeyRow
	if targetKey != "" {
		if existing := usage.GetAPIKey(targetKey); existing != nil {
			entry = *existing
		} else {
			// If setting a new key via patch, start fresh
			entry.Key = targetKey
		}
	} else {
		c.JSON(404, gin.H{"error": "item not found"})
		return
	}

	// Handle key deletion (empty key)
	if body.Value.Key != nil {
		trimmed := strings.TrimSpace(*body.Value.Key)
		if trimmed == "" {
			if err := usage.DeleteAPIKey(targetKey); err != nil {
				c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
				return
			}
			h.refreshAPIKeyCache()
			c.JSON(200, gin.H{"status": "ok"})
			return
		}
		// Key change: delete old, insert new
		if trimmed != targetKey {
			_ = usage.DeleteAPIKey(targetKey)
		}
		entry.Key = trimmed
	}

	// Apply patches
	if body.Value.Name != nil {
		entry.Name = strings.TrimSpace(*body.Value.Name)
	}
	if body.Value.DailyLimit != nil {
		entry.DailyLimit = *body.Value.DailyLimit
	}
	if body.Value.TotalQuota != nil {
		entry.TotalQuota = *body.Value.TotalQuota
	}
	if body.Value.SpendingLimit != nil {
		entry.SpendingLimit = *body.Value.SpendingLimit
	}
	if body.Value.ConcurrencyLimit != nil {
		entry.ConcurrencyLimit = *body.Value.ConcurrencyLimit
	}
	if body.Value.RPMLimit != nil {
		entry.RPMLimit = *body.Value.RPMLimit
	}
	if body.Value.TPMLimit != nil {
		entry.TPMLimit = *body.Value.TPMLimit
	}
	if body.Value.AllowedModels != nil {
		entry.AllowedModels = append([]string(nil), (*body.Value.AllowedModels)...)
	}
	if body.Value.AllowedChannels != nil {
		validated, errValidate := h.validateAllowedChannels(*body.Value.AllowedChannels)
		if errValidate != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": errValidate.Error()})
			return
		}
		entry.AllowedChannels = validated
	}
	if body.Value.AllowedChannelGroups != nil {
		validated, errValidate := h.validateAllowedChannelGroups(*body.Value.AllowedChannelGroups)
		if errValidate != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": errValidate.Error()})
			return
		}
		entry.AllowedChannelGroups = validated
	}
	var auths []*coreauth.Auth
	if h != nil && h.authManager != nil {
		auths = h.authManager.List()
	}
	routingCfg := config.RoutingConfig{}
	if h != nil && h.cfg != nil {
		routingCfg = h.cfg.Routing
	}
	if errValidate := validateRoutingAndAPIKeyRestrictions(&config.Config{
		SDKConfig: config.SDKConfig{
			APIKeyEntries: []config.APIKeyEntry{entry.ToConfigEntry()},
		},
		Routing: routingCfg,
	}, auths); errValidate != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": errValidate.Error()})
		return
	}
	if body.Value.SystemPrompt != nil {
		entry.SystemPrompt = strings.TrimSpace(*body.Value.SystemPrompt)
	}
	if body.Value.CreatedAt != nil {
		entry.CreatedAt = strings.TrimSpace(*body.Value.CreatedAt)
	}

	if err := usage.UpsertAPIKey(entry); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	h.refreshAPIKeyCache()
	c.JSON(200, gin.H{"status": "ok"})
}

func (h *Handler) DeleteAPIKeyEntry(c *gin.Context) {
	deleteLogs := shouldDeleteAPIKeyLogs(c)
	if val := strings.TrimSpace(c.Query("key")); val != "" {
		if err := usage.DeleteAPIKey(val); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		var logsDeleted int64
		if deleteLogs {
			logsDeleted, _ = usage.DeleteLogsByAPIKey(val)
		}
		h.refreshAPIKeyCache()
		c.JSON(200, gin.H{"status": "ok", "logs_deleted": logsDeleted})
		return
	}
	if idxStr := c.Query("index"); idxStr != "" {
		var idx int
		if _, err := fmt.Sscanf(idxStr, "%d", &idx); err == nil {
			rows := usage.ListAPIKeys()
			if idx >= 0 && idx < len(rows) {
				keyVal := rows[idx].Key
				if err := usage.DeleteAPIKey(keyVal); err != nil {
					c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
					return
				}
				var logsDeleted int64
				if deleteLogs {
					logsDeleted, _ = usage.DeleteLogsByAPIKey(keyVal)
				}
				h.refreshAPIKeyCache()
				c.JSON(200, gin.H{"status": "ok", "logs_deleted": logsDeleted})
				return
			}
		}
	}
	c.JSON(400, gin.H{"error": "missing key or index"})
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

// gemini-api-key: []GeminiKey
func (h *Handler) GetGeminiKeys(c *gin.Context) {
	c.JSON(200, gin.H{"gemini-api-key": h.cfg.GeminiKey})
}
func (h *Handler) PutGeminiKeys(c *gin.Context) {
	data, err := c.GetRawData()
	if err != nil {
		c.JSON(400, gin.H{"error": "failed to read body"})
		return
	}
	var arr []config.GeminiKey
	if err = json.Unmarshal(data, &arr); err != nil {
		var obj struct {
			Items []config.GeminiKey `json:"items"`
		}
		if err2 := json.Unmarshal(data, &obj); err2 != nil || len(obj.Items) == 0 {
			c.JSON(400, gin.H{"error": "invalid body"})
			return
		}
		arr = obj.Items
	}
	prev := append([]config.GeminiKey(nil), h.cfg.GeminiKey...)
	h.cfg.GeminiKey = append([]config.GeminiKey(nil), arr...)
	h.cfg.SanitizeGeminiKeys()
	if err := h.validateChannelNames(); err != nil {
		h.cfg.GeminiKey = prev
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	h.persist(c)
}
func (h *Handler) PatchGeminiKey(c *gin.Context) {
	type geminiKeyPatch struct {
		APIKey         *string            `json:"api-key"`
		Prefix         *string            `json:"prefix"`
		BaseURL        *string            `json:"base-url"`
		ProxyURL       *string            `json:"proxy-url"`
		ProxyID        *string            `json:"proxy-id"`
		Headers        *map[string]string `json:"headers"`
		ExcludedModels *[]string          `json:"excluded-models"`
	}
	var body struct {
		Index *int            `json:"index"`
		Match *string         `json:"match"`
		Value *geminiKeyPatch `json:"value"`
	}
	if err := c.ShouldBindJSON(&body); err != nil || body.Value == nil {
		c.JSON(400, gin.H{"error": "invalid body"})
		return
	}
	targetIndex := -1
	if body.Index != nil && *body.Index >= 0 && *body.Index < len(h.cfg.GeminiKey) {
		targetIndex = *body.Index
	}
	if targetIndex == -1 && body.Match != nil {
		match := strings.TrimSpace(*body.Match)
		if match != "" {
			for i := range h.cfg.GeminiKey {
				if h.cfg.GeminiKey[i].APIKey == match {
					targetIndex = i
					break
				}
			}
		}
	}
	if targetIndex == -1 {
		c.JSON(404, gin.H{"error": "item not found"})
		return
	}

	entry := h.cfg.GeminiKey[targetIndex]
	if body.Value.APIKey != nil {
		trimmed := strings.TrimSpace(*body.Value.APIKey)
		if trimmed == "" {
			h.cfg.GeminiKey = append(h.cfg.GeminiKey[:targetIndex], h.cfg.GeminiKey[targetIndex+1:]...)
			h.cfg.SanitizeGeminiKeys()
			h.persist(c)
			return
		}
		entry.APIKey = trimmed
	}
	if body.Value.Prefix != nil {
		entry.Prefix = strings.TrimSpace(*body.Value.Prefix)
	}
	if body.Value.BaseURL != nil {
		entry.BaseURL = strings.TrimSpace(*body.Value.BaseURL)
	}
	if body.Value.ProxyURL != nil {
		entry.ProxyURL = strings.TrimSpace(*body.Value.ProxyURL)
	}
	if body.Value.ProxyID != nil {
		entry.ProxyID = strings.TrimSpace(*body.Value.ProxyID)
	}
	if body.Value.Headers != nil {
		entry.Headers = config.NormalizeHeaders(*body.Value.Headers)
	}
	if body.Value.ExcludedModels != nil {
		entry.ExcludedModels = config.NormalizeExcludedModels(*body.Value.ExcludedModels)
	}
	prev := append([]config.GeminiKey(nil), h.cfg.GeminiKey...)
	h.cfg.GeminiKey[targetIndex] = entry
	h.cfg.SanitizeGeminiKeys()
	if err := h.validateChannelNames(); err != nil {
		h.cfg.GeminiKey = prev
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	h.persist(c)
}

func (h *Handler) DeleteGeminiKey(c *gin.Context) {
	if val := strings.TrimSpace(c.Query("api-key")); val != "" {
		out := make([]config.GeminiKey, 0, len(h.cfg.GeminiKey))
		for _, v := range h.cfg.GeminiKey {
			if v.APIKey != val {
				out = append(out, v)
			}
		}
		if len(out) != len(h.cfg.GeminiKey) {
			h.cfg.GeminiKey = out
			h.cfg.SanitizeGeminiKeys()
			h.persist(c)
		} else {
			c.JSON(404, gin.H{"error": "item not found"})
		}
		return
	}
	if idxStr := c.Query("index"); idxStr != "" {
		var idx int
		if _, err := fmt.Sscanf(idxStr, "%d", &idx); err == nil && idx >= 0 && idx < len(h.cfg.GeminiKey) {
			h.cfg.GeminiKey = append(h.cfg.GeminiKey[:idx], h.cfg.GeminiKey[idx+1:]...)
			h.cfg.SanitizeGeminiKeys()
			h.persist(c)
			return
		}
	}
	c.JSON(400, gin.H{"error": "missing api-key or index"})
}

// claude-api-key: []ClaudeKey
func (h *Handler) GetClaudeKeys(c *gin.Context) {
	c.JSON(200, gin.H{"claude-api-key": h.cfg.ClaudeKey})
}
func (h *Handler) PutClaudeKeys(c *gin.Context) {
	data, err := c.GetRawData()
	if err != nil {
		c.JSON(400, gin.H{"error": "failed to read body"})
		return
	}
	var arr []config.ClaudeKey
	if err = json.Unmarshal(data, &arr); err != nil {
		var obj struct {
			Items []config.ClaudeKey `json:"items"`
		}
		if err2 := json.Unmarshal(data, &obj); err2 != nil || len(obj.Items) == 0 {
			c.JSON(400, gin.H{"error": "invalid body"})
			return
		}
		arr = obj.Items
	}
	for i := range arr {
		normalizeClaudeKey(&arr[i])
	}
	prev := append([]config.ClaudeKey(nil), h.cfg.ClaudeKey...)
	h.cfg.ClaudeKey = arr
	h.cfg.SanitizeClaudeKeys()
	if err := h.validateChannelNames(); err != nil {
		h.cfg.ClaudeKey = prev
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	h.persist(c)
}
func (h *Handler) PatchClaudeKey(c *gin.Context) {
	type claudeKeyPatch struct {
		Name           *string               `json:"name"`
		APIKey         *string               `json:"api-key"`
		Prefix         *string               `json:"prefix"`
		BaseURL        *string               `json:"base-url"`
		ProxyURL       *string               `json:"proxy-url"`
		ProxyID        *string               `json:"proxy-id"`
		Models         *[]config.ClaudeModel `json:"models"`
		Headers        *map[string]string    `json:"headers"`
		ExcludedModels *[]string             `json:"excluded-models"`
	}
	var body struct {
		Index *int            `json:"index"`
		Match *string         `json:"match"`
		Value *claudeKeyPatch `json:"value"`
	}
	if err := c.ShouldBindJSON(&body); err != nil || body.Value == nil {
		c.JSON(400, gin.H{"error": "invalid body"})
		return
	}
	targetIndex := -1
	if body.Index != nil && *body.Index >= 0 && *body.Index < len(h.cfg.ClaudeKey) {
		targetIndex = *body.Index
	}
	if targetIndex == -1 && body.Match != nil {
		match := strings.TrimSpace(*body.Match)
		for i := range h.cfg.ClaudeKey {
			if h.cfg.ClaudeKey[i].APIKey == match {
				targetIndex = i
				break
			}
		}
	}
	if targetIndex == -1 {
		c.JSON(404, gin.H{"error": "item not found"})
		return
	}

	entry := h.cfg.ClaudeKey[targetIndex]
	if body.Value.Name != nil {
		entry.Name = strings.TrimSpace(*body.Value.Name)
	}
	if body.Value.APIKey != nil {
		entry.APIKey = strings.TrimSpace(*body.Value.APIKey)
	}
	if body.Value.Prefix != nil {
		entry.Prefix = strings.TrimSpace(*body.Value.Prefix)
	}
	if body.Value.BaseURL != nil {
		entry.BaseURL = strings.TrimSpace(*body.Value.BaseURL)
	}
	if body.Value.ProxyURL != nil {
		entry.ProxyURL = strings.TrimSpace(*body.Value.ProxyURL)
	}
	if body.Value.ProxyID != nil {
		entry.ProxyID = strings.TrimSpace(*body.Value.ProxyID)
	}
	if body.Value.Models != nil {
		entry.Models = append([]config.ClaudeModel(nil), (*body.Value.Models)...)
	}
	if body.Value.Headers != nil {
		entry.Headers = config.NormalizeHeaders(*body.Value.Headers)
	}
	if body.Value.ExcludedModels != nil {
		entry.ExcludedModels = config.NormalizeExcludedModels(*body.Value.ExcludedModels)
	}
	normalizeClaudeKey(&entry)
	prev := append([]config.ClaudeKey(nil), h.cfg.ClaudeKey...)
	h.cfg.ClaudeKey[targetIndex] = entry
	h.cfg.SanitizeClaudeKeys()
	if err := h.validateChannelNames(); err != nil {
		h.cfg.ClaudeKey = prev
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	h.persist(c)
}

func (h *Handler) DeleteClaudeKey(c *gin.Context) {
	if val := c.Query("api-key"); val != "" {
		out := make([]config.ClaudeKey, 0, len(h.cfg.ClaudeKey))
		for _, v := range h.cfg.ClaudeKey {
			if v.APIKey != val {
				out = append(out, v)
			}
		}
		h.cfg.ClaudeKey = out
		h.cfg.SanitizeClaudeKeys()
		h.persist(c)
		return
	}
	if idxStr := c.Query("index"); idxStr != "" {
		var idx int
		_, err := fmt.Sscanf(idxStr, "%d", &idx)
		if err == nil && idx >= 0 && idx < len(h.cfg.ClaudeKey) {
			h.cfg.ClaudeKey = append(h.cfg.ClaudeKey[:idx], h.cfg.ClaudeKey[idx+1:]...)
			h.cfg.SanitizeClaudeKeys()
			h.persist(c)
			return
		}
	}
	c.JSON(400, gin.H{"error": "missing api-key or index"})
}

// bedrock-api-key: []BedrockKey
func (h *Handler) GetBedrockKeys(c *gin.Context) {
	c.JSON(200, gin.H{"bedrock-api-key": h.cfg.BedrockKey})
}

func (h *Handler) PutBedrockKeys(c *gin.Context) {
	data, err := c.GetRawData()
	if err != nil {
		c.JSON(400, gin.H{"error": "failed to read body"})
		return
	}
	var arr []config.BedrockKey
	if err = json.Unmarshal(data, &arr); err != nil {
		var obj struct {
			Items []config.BedrockKey `json:"items"`
		}
		if err2 := json.Unmarshal(data, &obj); err2 != nil || len(obj.Items) == 0 {
			c.JSON(400, gin.H{"error": "invalid body"})
			return
		}
		arr = obj.Items
	}
	for i := range arr {
		normalizeBedrockKey(&arr[i])
	}
	prev := append([]config.BedrockKey(nil), h.cfg.BedrockKey...)
	h.cfg.BedrockKey = arr
	h.cfg.SanitizeBedrockKeys()
	if err := h.validateChannelNames(); err != nil {
		h.cfg.BedrockKey = prev
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	h.persist(c)
}

func (h *Handler) PatchBedrockKey(c *gin.Context) {
	type bedrockKeyPatch struct {
		Name            *string                `json:"name"`
		Priority        *int                   `json:"priority"`
		Prefix          *string                `json:"prefix"`
		AuthMode        *string                `json:"auth-mode"`
		APIKey          *string                `json:"api-key"`
		AccessKeyID     *string                `json:"access-key-id"`
		SecretAccessKey *string                `json:"secret-access-key"`
		SessionToken    *string                `json:"session-token"`
		Region          *string                `json:"region"`
		ForceGlobal     *bool                  `json:"force-global"`
		BaseURL         *string                `json:"base-url"`
		ProxyURL        *string                `json:"proxy-url"`
		ProxyID         *string                `json:"proxy-id"`
		Models          *[]config.BedrockModel `json:"models"`
		Headers         *map[string]string     `json:"headers"`
		ExcludedModels  *[]string              `json:"excluded-models"`
	}
	var body struct {
		Index *int             `json:"index"`
		Match *string          `json:"match"`
		Value *bedrockKeyPatch `json:"value"`
	}
	if err := c.ShouldBindJSON(&body); err != nil || body.Value == nil {
		c.JSON(400, gin.H{"error": "invalid body"})
		return
	}
	targetIndex := -1
	if body.Index != nil && *body.Index >= 0 && *body.Index < len(h.cfg.BedrockKey) {
		targetIndex = *body.Index
	}
	if targetIndex == -1 && body.Match != nil {
		match := strings.TrimSpace(*body.Match)
		for i := range h.cfg.BedrockKey {
			entry := h.cfg.BedrockKey[i]
			if entry.APIKey == match || entry.AccessKeyID == match || entry.Name == match {
				targetIndex = i
				break
			}
		}
	}
	if targetIndex == -1 {
		c.JSON(404, gin.H{"error": "item not found"})
		return
	}

	entry := h.cfg.BedrockKey[targetIndex]
	if body.Value.Name != nil {
		entry.Name = strings.TrimSpace(*body.Value.Name)
	}
	if body.Value.Priority != nil {
		entry.Priority = *body.Value.Priority
	}
	if body.Value.Prefix != nil {
		entry.Prefix = strings.TrimSpace(*body.Value.Prefix)
	}
	if body.Value.AuthMode != nil {
		entry.AuthMode = strings.TrimSpace(*body.Value.AuthMode)
	}
	if body.Value.APIKey != nil {
		entry.APIKey = strings.TrimSpace(*body.Value.APIKey)
	}
	if body.Value.AccessKeyID != nil {
		entry.AccessKeyID = strings.TrimSpace(*body.Value.AccessKeyID)
	}
	if body.Value.SecretAccessKey != nil {
		entry.SecretAccessKey = strings.TrimSpace(*body.Value.SecretAccessKey)
	}
	if body.Value.SessionToken != nil {
		entry.SessionToken = strings.TrimSpace(*body.Value.SessionToken)
	}
	if body.Value.Region != nil {
		entry.Region = strings.TrimSpace(*body.Value.Region)
	}
	if body.Value.ForceGlobal != nil {
		entry.ForceGlobal = *body.Value.ForceGlobal
	}
	if body.Value.BaseURL != nil {
		entry.BaseURL = strings.TrimSpace(*body.Value.BaseURL)
	}
	if body.Value.ProxyURL != nil {
		entry.ProxyURL = strings.TrimSpace(*body.Value.ProxyURL)
	}
	if body.Value.ProxyID != nil {
		entry.ProxyID = strings.TrimSpace(*body.Value.ProxyID)
	}
	if body.Value.Models != nil {
		entry.Models = append([]config.BedrockModel(nil), (*body.Value.Models)...)
	}
	if body.Value.Headers != nil {
		entry.Headers = config.NormalizeHeaders(*body.Value.Headers)
	}
	if body.Value.ExcludedModels != nil {
		entry.ExcludedModels = config.NormalizeExcludedModels(*body.Value.ExcludedModels)
	}
	normalizeBedrockKey(&entry)
	prev := append([]config.BedrockKey(nil), h.cfg.BedrockKey...)
	h.cfg.BedrockKey[targetIndex] = entry
	h.cfg.SanitizeBedrockKeys()
	if err := h.validateChannelNames(); err != nil {
		h.cfg.BedrockKey = prev
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	h.persist(c)
}

func (h *Handler) DeleteBedrockKey(c *gin.Context) {
	if val := strings.TrimSpace(c.Query("api-key")); val != "" {
		out := make([]config.BedrockKey, 0, len(h.cfg.BedrockKey))
		for _, v := range h.cfg.BedrockKey {
			if v.APIKey != val {
				out = append(out, v)
			}
		}
		if len(out) != len(h.cfg.BedrockKey) {
			h.cfg.BedrockKey = out
			h.cfg.SanitizeBedrockKeys()
			h.persist(c)
			return
		}
		c.JSON(404, gin.H{"error": "item not found"})
		return
	}
	if val := strings.TrimSpace(c.Query("access-key-id")); val != "" {
		out := make([]config.BedrockKey, 0, len(h.cfg.BedrockKey))
		for _, v := range h.cfg.BedrockKey {
			if v.AccessKeyID != val {
				out = append(out, v)
			}
		}
		if len(out) != len(h.cfg.BedrockKey) {
			h.cfg.BedrockKey = out
			h.cfg.SanitizeBedrockKeys()
			h.persist(c)
			return
		}
		c.JSON(404, gin.H{"error": "item not found"})
		return
	}
	if val := strings.TrimSpace(c.Query("name")); val != "" {
		out := make([]config.BedrockKey, 0, len(h.cfg.BedrockKey))
		for _, v := range h.cfg.BedrockKey {
			if v.Name != val {
				out = append(out, v)
			}
		}
		if len(out) != len(h.cfg.BedrockKey) {
			h.cfg.BedrockKey = out
			h.cfg.SanitizeBedrockKeys()
			h.persist(c)
			return
		}
		c.JSON(404, gin.H{"error": "item not found"})
		return
	}
	if idxStr := c.Query("index"); idxStr != "" {
		var idx int
		if _, err := fmt.Sscanf(idxStr, "%d", &idx); err == nil && idx >= 0 && idx < len(h.cfg.BedrockKey) {
			h.cfg.BedrockKey = append(h.cfg.BedrockKey[:idx], h.cfg.BedrockKey[idx+1:]...)
			h.cfg.SanitizeBedrockKeys()
			h.persist(c)
			return
		}
	}
	c.JSON(400, gin.H{"error": "missing api-key, access-key-id, name, or index"})
}

// opencode-go-api-key: []OpenCodeGoKey
func (h *Handler) GetOpenCodeGoKeys(c *gin.Context) {
	c.JSON(200, gin.H{"opencode-go-api-key": normalizedOpenCodeGoKeyEntries(h.cfg.OpenCodeGoKey)})
}

func (h *Handler) PutOpenCodeGoKeys(c *gin.Context) {
	data, err := c.GetRawData()
	if err != nil {
		c.JSON(400, gin.H{"error": "failed to read body"})
		return
	}
	var arr []config.OpenCodeGoKey
	if err = json.Unmarshal(data, &arr); err != nil {
		var obj struct {
			Items []config.OpenCodeGoKey `json:"items"`
		}
		if err2 := json.Unmarshal(data, &obj); err2 != nil || len(obj.Items) == 0 {
			c.JSON(400, gin.H{"error": "invalid body"})
			return
		}
		arr = obj.Items
	}
	filtered := make([]config.OpenCodeGoKey, 0, len(arr))
	for i := range arr {
		normalizeOpenCodeGoKey(&arr[i])
		if strings.TrimSpace(arr[i].APIKey) != "" {
			filtered = append(filtered, arr[i])
		}
	}
	prev := append([]config.OpenCodeGoKey(nil), h.cfg.OpenCodeGoKey...)
	h.cfg.OpenCodeGoKey = filtered
	h.cfg.SanitizeOpenCodeGoKeys()
	if err := h.validateChannelNames(); err != nil {
		h.cfg.OpenCodeGoKey = prev
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	h.persist(c)
}

func (h *Handler) PatchOpenCodeGoKey(c *gin.Context) {
	type openCodeGoPatch struct {
		APIKey         *string            `json:"api-key"`
		Name           *string            `json:"name"`
		Priority       *int               `json:"priority"`
		Prefix         *string            `json:"prefix"`
		ProxyURL       *string            `json:"proxy-url"`
		ProxyID        *string            `json:"proxy-id"`
		Headers        *map[string]string `json:"headers"`
		ExcludedModels *[]string          `json:"excluded-models"`
	}
	var body struct {
		APIKey *string          `json:"api-key"`
		Name   *string          `json:"name"`
		Index  *int             `json:"index"`
		Value  *openCodeGoPatch `json:"value"`
	}
	if err := c.ShouldBindJSON(&body); err != nil || body.Value == nil {
		c.JSON(400, gin.H{"error": "invalid body"})
		return
	}
	targetIndex := -1
	if body.Index != nil && *body.Index >= 0 && *body.Index < len(h.cfg.OpenCodeGoKey) {
		targetIndex = *body.Index
	}
	if targetIndex == -1 && body.APIKey != nil {
		match := strings.TrimSpace(*body.APIKey)
		for i := range h.cfg.OpenCodeGoKey {
			if h.cfg.OpenCodeGoKey[i].APIKey == match {
				targetIndex = i
				break
			}
		}
	}
	if targetIndex == -1 && body.Name != nil {
		match := strings.TrimSpace(*body.Name)
		for i := range h.cfg.OpenCodeGoKey {
			if h.cfg.OpenCodeGoKey[i].Name == match {
				targetIndex = i
				break
			}
		}
	}
	if targetIndex == -1 {
		c.JSON(404, gin.H{"error": "item not found"})
		return
	}

	entry := h.cfg.OpenCodeGoKey[targetIndex]
	if body.Value.APIKey != nil {
		entry.APIKey = strings.TrimSpace(*body.Value.APIKey)
	}
	if body.Value.Name != nil {
		entry.Name = strings.TrimSpace(*body.Value.Name)
	}
	if body.Value.Priority != nil {
		entry.Priority = *body.Value.Priority
	}
	if body.Value.Prefix != nil {
		entry.Prefix = strings.TrimSpace(*body.Value.Prefix)
	}
	if body.Value.ProxyURL != nil {
		entry.ProxyURL = strings.TrimSpace(*body.Value.ProxyURL)
	}
	if body.Value.ProxyID != nil {
		entry.ProxyID = strings.TrimSpace(*body.Value.ProxyID)
	}
	if body.Value.Headers != nil {
		entry.Headers = config.NormalizeHeaders(*body.Value.Headers)
	}
	if body.Value.ExcludedModels != nil {
		entry.ExcludedModels = config.NormalizeExcludedModels(*body.Value.ExcludedModels)
	}
	normalizeOpenCodeGoKey(&entry)
	if entry.APIKey == "" {
		h.cfg.OpenCodeGoKey = append(h.cfg.OpenCodeGoKey[:targetIndex], h.cfg.OpenCodeGoKey[targetIndex+1:]...)
		h.cfg.SanitizeOpenCodeGoKeys()
		h.persist(c)
		return
	}
	prev := append([]config.OpenCodeGoKey(nil), h.cfg.OpenCodeGoKey...)
	h.cfg.OpenCodeGoKey[targetIndex] = entry
	h.cfg.SanitizeOpenCodeGoKeys()
	if err := h.validateChannelNames(); err != nil {
		h.cfg.OpenCodeGoKey = prev
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	h.persist(c)
}

func (h *Handler) DeleteOpenCodeGoKey(c *gin.Context) {
	if apiKey := strings.TrimSpace(c.Query("api-key")); apiKey != "" {
		out := make([]config.OpenCodeGoKey, 0, len(h.cfg.OpenCodeGoKey))
		for _, v := range h.cfg.OpenCodeGoKey {
			if v.APIKey != apiKey {
				out = append(out, v)
			}
		}
		if len(out) != len(h.cfg.OpenCodeGoKey) {
			h.cfg.OpenCodeGoKey = out
			h.cfg.SanitizeOpenCodeGoKeys()
			h.persist(c)
			return
		}
		c.JSON(404, gin.H{"error": "item not found"})
		return
	}
	if name := strings.TrimSpace(c.Query("name")); name != "" {
		out := make([]config.OpenCodeGoKey, 0, len(h.cfg.OpenCodeGoKey))
		for _, v := range h.cfg.OpenCodeGoKey {
			if v.Name != name {
				out = append(out, v)
			}
		}
		if len(out) != len(h.cfg.OpenCodeGoKey) {
			h.cfg.OpenCodeGoKey = out
			h.cfg.SanitizeOpenCodeGoKeys()
			h.persist(c)
			return
		}
		c.JSON(404, gin.H{"error": "item not found"})
		return
	}
	if idxStr := c.Query("index"); idxStr != "" {
		var idx int
		if _, err := fmt.Sscanf(idxStr, "%d", &idx); err == nil && idx >= 0 && idx < len(h.cfg.OpenCodeGoKey) {
			h.cfg.OpenCodeGoKey = append(h.cfg.OpenCodeGoKey[:idx], h.cfg.OpenCodeGoKey[idx+1:]...)
			h.cfg.SanitizeOpenCodeGoKeys()
			h.persist(c)
			return
		}
	}
	c.JSON(400, gin.H{"error": "missing api-key, name, or index"})
}

// openai-compatibility: []OpenAICompatibility
func (h *Handler) GetOpenAICompat(c *gin.Context) {
	c.JSON(200, gin.H{"openai-compatibility": normalizedOpenAICompatibilityEntries(h.cfg.OpenAICompatibility)})
}
func (h *Handler) PutOpenAICompat(c *gin.Context) {
	data, err := c.GetRawData()
	if err != nil {
		c.JSON(400, gin.H{"error": "failed to read body"})
		return
	}
	var arr []config.OpenAICompatibility
	if err = json.Unmarshal(data, &arr); err != nil {
		var obj struct {
			Items []config.OpenAICompatibility `json:"items"`
		}
		if err2 := json.Unmarshal(data, &obj); err2 != nil || len(obj.Items) == 0 {
			c.JSON(400, gin.H{"error": "invalid body"})
			return
		}
		arr = obj.Items
	}
	filtered := make([]config.OpenAICompatibility, 0, len(arr))
	for i := range arr {
		normalizeOpenAICompatibilityEntry(&arr[i])
		if strings.TrimSpace(arr[i].BaseURL) != "" {
			filtered = append(filtered, arr[i])
		}
	}
	prev := append([]config.OpenAICompatibility(nil), h.cfg.OpenAICompatibility...)
	h.cfg.OpenAICompatibility = filtered
	h.cfg.SanitizeOpenAICompatibility()
	if err := h.validateChannelNames(); err != nil {
		h.cfg.OpenAICompatibility = prev
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	h.persist(c)
}
func (h *Handler) PatchOpenAICompat(c *gin.Context) {
	type openAICompatPatch struct {
		Name           *string                             `json:"name"`
		Prefix         *string                             `json:"prefix"`
		BaseURL        *string                             `json:"base-url"`
		APIKeyEntries  *[]config.OpenAICompatibilityAPIKey `json:"api-key-entries"`
		Models         *[]config.OpenAICompatibilityModel  `json:"models"`
		Headers        *map[string]string                  `json:"headers"`
		Fingerprint    *string                             `json:"identity-fingerprint"`
		ImageEditsMode *string                             `json:"image-edits-mode"`
		ImageField     *string                             `json:"image-generations-image-field"`
	}
	var body struct {
		Name  *string            `json:"name"`
		Index *int               `json:"index"`
		Value *openAICompatPatch `json:"value"`
	}
	if err := c.ShouldBindJSON(&body); err != nil || body.Value == nil {
		c.JSON(400, gin.H{"error": "invalid body"})
		return
	}
	targetIndex := -1
	if body.Index != nil && *body.Index >= 0 && *body.Index < len(h.cfg.OpenAICompatibility) {
		targetIndex = *body.Index
	}
	if targetIndex == -1 && body.Name != nil {
		match := strings.TrimSpace(*body.Name)
		for i := range h.cfg.OpenAICompatibility {
			if h.cfg.OpenAICompatibility[i].Name == match {
				targetIndex = i
				break
			}
		}
	}
	if targetIndex == -1 {
		c.JSON(404, gin.H{"error": "item not found"})
		return
	}

	entry := h.cfg.OpenAICompatibility[targetIndex]
	if body.Value.Name != nil {
		entry.Name = strings.TrimSpace(*body.Value.Name)
	}
	if body.Value.Prefix != nil {
		entry.Prefix = strings.TrimSpace(*body.Value.Prefix)
	}
	if body.Value.BaseURL != nil {
		trimmed := strings.TrimSpace(*body.Value.BaseURL)
		if trimmed == "" {
			h.cfg.OpenAICompatibility = append(h.cfg.OpenAICompatibility[:targetIndex], h.cfg.OpenAICompatibility[targetIndex+1:]...)
			h.cfg.SanitizeOpenAICompatibility()
			h.persist(c)
			return
		}
		entry.BaseURL = trimmed
	}
	if body.Value.APIKeyEntries != nil {
		entry.APIKeyEntries = append([]config.OpenAICompatibilityAPIKey(nil), (*body.Value.APIKeyEntries)...)
	}
	if body.Value.Models != nil {
		entry.Models = append([]config.OpenAICompatibilityModel(nil), (*body.Value.Models)...)
	}
	if body.Value.Headers != nil {
		entry.Headers = config.NormalizeHeaders(*body.Value.Headers)
	}
	if body.Value.Fingerprint != nil {
		entry.IdentityFingerprint = strings.TrimSpace(strings.ToLower(*body.Value.Fingerprint))
	}
	if body.Value.ImageEditsMode != nil {
		entry.ImageEditsMode = normalizeOpenAICompatImageEditsMode(*body.Value.ImageEditsMode)
	}
	if body.Value.ImageField != nil {
		entry.ImageGenerationsImageField = normalizeOpenAICompatImageGenerationsImageField(*body.Value.ImageField)
	}
	normalizeOpenAICompatibilityEntry(&entry)
	prev := append([]config.OpenAICompatibility(nil), h.cfg.OpenAICompatibility...)
	h.cfg.OpenAICompatibility[targetIndex] = entry
	h.cfg.SanitizeOpenAICompatibility()
	if err := h.validateChannelNames(); err != nil {
		h.cfg.OpenAICompatibility = prev
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	h.persist(c)
}

func (h *Handler) DeleteOpenAICompat(c *gin.Context) {
	if name := c.Query("name"); name != "" {
		out := make([]config.OpenAICompatibility, 0, len(h.cfg.OpenAICompatibility))
		for _, v := range h.cfg.OpenAICompatibility {
			if v.Name != name {
				out = append(out, v)
			}
		}
		h.cfg.OpenAICompatibility = out
		h.cfg.SanitizeOpenAICompatibility()
		h.persist(c)
		return
	}
	if idxStr := c.Query("index"); idxStr != "" {
		var idx int
		_, err := fmt.Sscanf(idxStr, "%d", &idx)
		if err == nil && idx >= 0 && idx < len(h.cfg.OpenAICompatibility) {
			h.cfg.OpenAICompatibility = append(h.cfg.OpenAICompatibility[:idx], h.cfg.OpenAICompatibility[idx+1:]...)
			h.cfg.SanitizeOpenAICompatibility()
			h.persist(c)
			return
		}
	}
	c.JSON(400, gin.H{"error": "missing name or index"})
}

// vertex-api-key: []VertexCompatKey
func (h *Handler) GetVertexCompatKeys(c *gin.Context) {
	c.JSON(200, gin.H{"vertex-api-key": h.cfg.VertexCompatAPIKey})
}
func (h *Handler) PutVertexCompatKeys(c *gin.Context) {
	data, err := c.GetRawData()
	if err != nil {
		c.JSON(400, gin.H{"error": "failed to read body"})
		return
	}
	var arr []config.VertexCompatKey
	if err = json.Unmarshal(data, &arr); err != nil {
		var obj struct {
			Items []config.VertexCompatKey `json:"items"`
		}
		if err2 := json.Unmarshal(data, &obj); err2 != nil || len(obj.Items) == 0 {
			c.JSON(400, gin.H{"error": "invalid body"})
			return
		}
		arr = obj.Items
	}
	for i := range arr {
		normalizeVertexCompatKey(&arr[i])
	}
	h.cfg.VertexCompatAPIKey = arr
	h.cfg.SanitizeVertexCompatKeys()
	h.persist(c)
}
func (h *Handler) PatchVertexCompatKey(c *gin.Context) {
	type vertexCompatPatch struct {
		APIKey   *string                     `json:"api-key"`
		Prefix   *string                     `json:"prefix"`
		BaseURL  *string                     `json:"base-url"`
		ProxyURL *string                     `json:"proxy-url"`
		ProxyID  *string                     `json:"proxy-id"`
		Headers  *map[string]string          `json:"headers"`
		Models   *[]config.VertexCompatModel `json:"models"`
	}
	var body struct {
		Index *int               `json:"index"`
		Match *string            `json:"match"`
		Value *vertexCompatPatch `json:"value"`
	}
	if errBindJSON := c.ShouldBindJSON(&body); errBindJSON != nil || body.Value == nil {
		c.JSON(400, gin.H{"error": "invalid body"})
		return
	}
	targetIndex := -1
	if body.Index != nil && *body.Index >= 0 && *body.Index < len(h.cfg.VertexCompatAPIKey) {
		targetIndex = *body.Index
	}
	if targetIndex == -1 && body.Match != nil {
		match := strings.TrimSpace(*body.Match)
		if match != "" {
			for i := range h.cfg.VertexCompatAPIKey {
				if h.cfg.VertexCompatAPIKey[i].APIKey == match {
					targetIndex = i
					break
				}
			}
		}
	}
	if targetIndex == -1 {
		c.JSON(404, gin.H{"error": "item not found"})
		return
	}

	entry := h.cfg.VertexCompatAPIKey[targetIndex]
	if body.Value.APIKey != nil {
		trimmed := strings.TrimSpace(*body.Value.APIKey)
		if trimmed == "" {
			h.cfg.VertexCompatAPIKey = append(h.cfg.VertexCompatAPIKey[:targetIndex], h.cfg.VertexCompatAPIKey[targetIndex+1:]...)
			h.cfg.SanitizeVertexCompatKeys()
			h.persist(c)
			return
		}
		entry.APIKey = trimmed
	}
	if body.Value.Prefix != nil {
		entry.Prefix = strings.TrimSpace(*body.Value.Prefix)
	}
	if body.Value.BaseURL != nil {
		trimmed := strings.TrimSpace(*body.Value.BaseURL)
		if trimmed == "" {
			h.cfg.VertexCompatAPIKey = append(h.cfg.VertexCompatAPIKey[:targetIndex], h.cfg.VertexCompatAPIKey[targetIndex+1:]...)
			h.cfg.SanitizeVertexCompatKeys()
			h.persist(c)
			return
		}
		entry.BaseURL = trimmed
	}
	if body.Value.ProxyURL != nil {
		entry.ProxyURL = strings.TrimSpace(*body.Value.ProxyURL)
	}
	if body.Value.ProxyID != nil {
		entry.ProxyID = strings.TrimSpace(*body.Value.ProxyID)
	}
	if body.Value.Headers != nil {
		entry.Headers = config.NormalizeHeaders(*body.Value.Headers)
	}
	if body.Value.Models != nil {
		entry.Models = append([]config.VertexCompatModel(nil), (*body.Value.Models)...)
	}
	normalizeVertexCompatKey(&entry)
	h.cfg.VertexCompatAPIKey[targetIndex] = entry
	h.cfg.SanitizeVertexCompatKeys()
	h.persist(c)
}

func (h *Handler) DeleteVertexCompatKey(c *gin.Context) {
	if val := strings.TrimSpace(c.Query("api-key")); val != "" {
		out := make([]config.VertexCompatKey, 0, len(h.cfg.VertexCompatAPIKey))
		for _, v := range h.cfg.VertexCompatAPIKey {
			if v.APIKey != val {
				out = append(out, v)
			}
		}
		h.cfg.VertexCompatAPIKey = out
		h.cfg.SanitizeVertexCompatKeys()
		h.persist(c)
		return
	}
	if idxStr := c.Query("index"); idxStr != "" {
		var idx int
		_, errScan := fmt.Sscanf(idxStr, "%d", &idx)
		if errScan == nil && idx >= 0 && idx < len(h.cfg.VertexCompatAPIKey) {
			h.cfg.VertexCompatAPIKey = append(h.cfg.VertexCompatAPIKey[:idx], h.cfg.VertexCompatAPIKey[idx+1:]...)
			h.cfg.SanitizeVertexCompatKeys()
			h.persist(c)
			return
		}
	}
	c.JSON(400, gin.H{"error": "missing api-key or index"})
}

// oauth-excluded-models: map[string][]string
func (h *Handler) GetOAuthExcludedModels(c *gin.Context) {
	c.JSON(200, gin.H{"oauth-excluded-models": config.NormalizeOAuthExcludedModels(h.cfg.OAuthExcludedModels)})
}

func (h *Handler) PutOAuthExcludedModels(c *gin.Context) {
	data, err := c.GetRawData()
	if err != nil {
		c.JSON(400, gin.H{"error": "failed to read body"})
		return
	}
	var entries map[string][]string
	if err = json.Unmarshal(data, &entries); err != nil {
		var wrapper struct {
			Items map[string][]string `json:"items"`
		}
		if err2 := json.Unmarshal(data, &wrapper); err2 != nil {
			c.JSON(400, gin.H{"error": "invalid body"})
			return
		}
		entries = wrapper.Items
	}
	h.cfg.OAuthExcludedModels = config.NormalizeOAuthExcludedModels(entries)
	h.persistRuntimeSetting(c, usage.RuntimeSettingOAuthExcludedModels, h.cfg.OAuthExcludedModels)
}

func (h *Handler) PatchOAuthExcludedModels(c *gin.Context) {
	var body struct {
		Provider *string  `json:"provider"`
		Models   []string `json:"models"`
	}
	if err := c.ShouldBindJSON(&body); err != nil || body.Provider == nil {
		c.JSON(400, gin.H{"error": "invalid body"})
		return
	}
	provider := strings.ToLower(strings.TrimSpace(*body.Provider))
	if provider == "" {
		c.JSON(400, gin.H{"error": "invalid provider"})
		return
	}
	normalized := config.NormalizeExcludedModels(body.Models)
	if len(normalized) == 0 {
		if h.cfg.OAuthExcludedModels == nil {
			c.JSON(404, gin.H{"error": "provider not found"})
			return
		}
		if _, ok := h.cfg.OAuthExcludedModels[provider]; !ok {
			c.JSON(404, gin.H{"error": "provider not found"})
			return
		}
		delete(h.cfg.OAuthExcludedModels, provider)
		if len(h.cfg.OAuthExcludedModels) == 0 {
			h.cfg.OAuthExcludedModels = nil
		}
		h.persistRuntimeSetting(c, usage.RuntimeSettingOAuthExcludedModels, h.cfg.OAuthExcludedModels)
		return
	}
	if h.cfg.OAuthExcludedModels == nil {
		h.cfg.OAuthExcludedModels = make(map[string][]string)
	}
	h.cfg.OAuthExcludedModels[provider] = normalized
	h.persistRuntimeSetting(c, usage.RuntimeSettingOAuthExcludedModels, h.cfg.OAuthExcludedModels)
}

func (h *Handler) DeleteOAuthExcludedModels(c *gin.Context) {
	provider := strings.ToLower(strings.TrimSpace(c.Query("provider")))
	if provider == "" {
		c.JSON(400, gin.H{"error": "missing provider"})
		return
	}
	if h.cfg.OAuthExcludedModels == nil {
		c.JSON(404, gin.H{"error": "provider not found"})
		return
	}
	if _, ok := h.cfg.OAuthExcludedModels[provider]; !ok {
		c.JSON(404, gin.H{"error": "provider not found"})
		return
	}
	delete(h.cfg.OAuthExcludedModels, provider)
	if len(h.cfg.OAuthExcludedModels) == 0 {
		h.cfg.OAuthExcludedModels = nil
	}
	h.persistRuntimeSetting(c, usage.RuntimeSettingOAuthExcludedModels, h.cfg.OAuthExcludedModels)
}

// oauth-model-alias: map[string][]OAuthModelAlias
func (h *Handler) GetOAuthModelAlias(c *gin.Context) {
	c.JSON(200, gin.H{"oauth-model-alias": sanitizedOAuthModelAlias(h.cfg.OAuthModelAlias)})
}

func (h *Handler) PutOAuthModelAlias(c *gin.Context) {
	data, err := c.GetRawData()
	if err != nil {
		c.JSON(400, gin.H{"error": "failed to read body"})
		return
	}
	var entries map[string][]config.OAuthModelAlias
	if err = json.Unmarshal(data, &entries); err != nil {
		var wrapper struct {
			Items map[string][]config.OAuthModelAlias `json:"items"`
		}
		if err2 := json.Unmarshal(data, &wrapper); err2 != nil {
			c.JSON(400, gin.H{"error": "invalid body"})
			return
		}
		entries = wrapper.Items
	}
	h.cfg.OAuthModelAlias = sanitizedOAuthModelAlias(entries)
	h.persistRuntimeSetting(c, usage.RuntimeSettingOAuthModelAlias, h.cfg.OAuthModelAlias)
}

func (h *Handler) PatchOAuthModelAlias(c *gin.Context) {
	var body struct {
		Provider *string                  `json:"provider"`
		Channel  *string                  `json:"channel"`
		Aliases  []config.OAuthModelAlias `json:"aliases"`
	}
	if errBindJSON := c.ShouldBindJSON(&body); errBindJSON != nil {
		c.JSON(400, gin.H{"error": "invalid body"})
		return
	}
	channelRaw := ""
	if body.Channel != nil {
		channelRaw = *body.Channel
	} else if body.Provider != nil {
		channelRaw = *body.Provider
	}
	channel := strings.ToLower(strings.TrimSpace(channelRaw))
	if channel == "" {
		c.JSON(400, gin.H{"error": "invalid channel"})
		return
	}

	normalizedMap := sanitizedOAuthModelAlias(map[string][]config.OAuthModelAlias{channel: body.Aliases})
	normalized := normalizedMap[channel]
	if len(normalized) == 0 {
		if h.cfg.OAuthModelAlias == nil {
			c.JSON(404, gin.H{"error": "channel not found"})
			return
		}
		if _, ok := h.cfg.OAuthModelAlias[channel]; !ok {
			c.JSON(404, gin.H{"error": "channel not found"})
			return
		}
		delete(h.cfg.OAuthModelAlias, channel)
		if len(h.cfg.OAuthModelAlias) == 0 {
			h.cfg.OAuthModelAlias = nil
		}
		h.persistRuntimeSetting(c, usage.RuntimeSettingOAuthModelAlias, h.cfg.OAuthModelAlias)
		return
	}
	if h.cfg.OAuthModelAlias == nil {
		h.cfg.OAuthModelAlias = make(map[string][]config.OAuthModelAlias)
	}
	h.cfg.OAuthModelAlias[channel] = normalized
	h.persistRuntimeSetting(c, usage.RuntimeSettingOAuthModelAlias, h.cfg.OAuthModelAlias)
}

func (h *Handler) DeleteOAuthModelAlias(c *gin.Context) {
	channel := strings.ToLower(strings.TrimSpace(c.Query("channel")))
	if channel == "" {
		channel = strings.ToLower(strings.TrimSpace(c.Query("provider")))
	}
	if channel == "" {
		c.JSON(400, gin.H{"error": "missing channel"})
		return
	}
	if h.cfg.OAuthModelAlias == nil {
		c.JSON(404, gin.H{"error": "channel not found"})
		return
	}
	if _, ok := h.cfg.OAuthModelAlias[channel]; !ok {
		c.JSON(404, gin.H{"error": "channel not found"})
		return
	}
	delete(h.cfg.OAuthModelAlias, channel)
	if len(h.cfg.OAuthModelAlias) == 0 {
		h.cfg.OAuthModelAlias = nil
	}
	h.persistRuntimeSetting(c, usage.RuntimeSettingOAuthModelAlias, h.cfg.OAuthModelAlias)
}

// codex-api-key: []CodexKey
func (h *Handler) GetCodexKeys(c *gin.Context) {
	c.JSON(200, gin.H{"codex-api-key": h.cfg.CodexKey})
}
func (h *Handler) PutCodexKeys(c *gin.Context) {
	data, err := c.GetRawData()
	if err != nil {
		c.JSON(400, gin.H{"error": "failed to read body"})
		return
	}
	var arr []config.CodexKey
	if err = json.Unmarshal(data, &arr); err != nil {
		var obj struct {
			Items []config.CodexKey `json:"items"`
		}
		if err2 := json.Unmarshal(data, &obj); err2 != nil || len(obj.Items) == 0 {
			c.JSON(400, gin.H{"error": "invalid body"})
			return
		}
		arr = obj.Items
	}
	// Filter out codex entries with empty base-url (treat as removed)
	filtered := make([]config.CodexKey, 0, len(arr))
	for i := range arr {
		entry := arr[i]
		normalizeCodexKey(&entry)
		if entry.BaseURL == "" {
			continue
		}
		filtered = append(filtered, entry)
	}
	prev := append([]config.CodexKey(nil), h.cfg.CodexKey...)
	h.cfg.CodexKey = filtered
	h.cfg.SanitizeCodexKeys()
	if err := h.validateChannelNames(); err != nil {
		h.cfg.CodexKey = prev
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	h.persist(c)
}
func (h *Handler) PatchCodexKey(c *gin.Context) {
	type codexKeyPatch struct {
		APIKey         *string              `json:"api-key"`
		Prefix         *string              `json:"prefix"`
		BaseURL        *string              `json:"base-url"`
		ProxyURL       *string              `json:"proxy-url"`
		ProxyID        *string              `json:"proxy-id"`
		Models         *[]config.CodexModel `json:"models"`
		Headers        *map[string]string   `json:"headers"`
		ExcludedModels *[]string            `json:"excluded-models"`
	}
	var body struct {
		Index *int           `json:"index"`
		Match *string        `json:"match"`
		Value *codexKeyPatch `json:"value"`
	}
	if err := c.ShouldBindJSON(&body); err != nil || body.Value == nil {
		c.JSON(400, gin.H{"error": "invalid body"})
		return
	}
	targetIndex := -1
	if body.Index != nil && *body.Index >= 0 && *body.Index < len(h.cfg.CodexKey) {
		targetIndex = *body.Index
	}
	if targetIndex == -1 && body.Match != nil {
		match := strings.TrimSpace(*body.Match)
		for i := range h.cfg.CodexKey {
			if h.cfg.CodexKey[i].APIKey == match {
				targetIndex = i
				break
			}
		}
	}
	if targetIndex == -1 {
		c.JSON(404, gin.H{"error": "item not found"})
		return
	}

	entry := h.cfg.CodexKey[targetIndex]
	if body.Value.APIKey != nil {
		entry.APIKey = strings.TrimSpace(*body.Value.APIKey)
	}
	if body.Value.Prefix != nil {
		entry.Prefix = strings.TrimSpace(*body.Value.Prefix)
	}
	if body.Value.BaseURL != nil {
		trimmed := strings.TrimSpace(*body.Value.BaseURL)
		if trimmed == "" {
			h.cfg.CodexKey = append(h.cfg.CodexKey[:targetIndex], h.cfg.CodexKey[targetIndex+1:]...)
			h.cfg.SanitizeCodexKeys()
			h.persist(c)
			return
		}
		entry.BaseURL = trimmed
	}
	if body.Value.ProxyURL != nil {
		entry.ProxyURL = strings.TrimSpace(*body.Value.ProxyURL)
	}
	if body.Value.ProxyID != nil {
		entry.ProxyID = strings.TrimSpace(*body.Value.ProxyID)
	}
	if body.Value.Models != nil {
		entry.Models = append([]config.CodexModel(nil), (*body.Value.Models)...)
	}
	if body.Value.Headers != nil {
		entry.Headers = config.NormalizeHeaders(*body.Value.Headers)
	}
	if body.Value.ExcludedModels != nil {
		entry.ExcludedModels = config.NormalizeExcludedModels(*body.Value.ExcludedModels)
	}
	normalizeCodexKey(&entry)
	prev := append([]config.CodexKey(nil), h.cfg.CodexKey...)
	h.cfg.CodexKey[targetIndex] = entry
	h.cfg.SanitizeCodexKeys()
	if err := h.validateChannelNames(); err != nil {
		h.cfg.CodexKey = prev
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	h.persist(c)
}

func (h *Handler) DeleteCodexKey(c *gin.Context) {
	if val := c.Query("api-key"); val != "" {
		out := make([]config.CodexKey, 0, len(h.cfg.CodexKey))
		for _, v := range h.cfg.CodexKey {
			if v.APIKey != val {
				out = append(out, v)
			}
		}
		h.cfg.CodexKey = out
		h.cfg.SanitizeCodexKeys()
		h.persist(c)
		return
	}
	if idxStr := c.Query("index"); idxStr != "" {
		var idx int
		_, err := fmt.Sscanf(idxStr, "%d", &idx)
		if err == nil && idx >= 0 && idx < len(h.cfg.CodexKey) {
			h.cfg.CodexKey = append(h.cfg.CodexKey[:idx], h.cfg.CodexKey[idx+1:]...)
			h.cfg.SanitizeCodexKeys()
			h.persist(c)
			return
		}
	}
	c.JSON(400, gin.H{"error": "missing api-key or index"})
}

func normalizeOpenAICompatibilityEntry(entry *config.OpenAICompatibility) {
	if entry == nil {
		return
	}
	// Trim base-url; empty base-url indicates provider should be removed by sanitization
	entry.BaseURL = strings.TrimSpace(entry.BaseURL)
	entry.IdentityFingerprint = strings.TrimSpace(strings.ToLower(entry.IdentityFingerprint))
	entry.ImageEditsMode = normalizeOpenAICompatImageEditsMode(entry.ImageEditsMode)
	entry.Headers = config.NormalizeHeaders(entry.Headers)
	existing := make(map[string]struct{}, len(entry.APIKeyEntries))
	for i := range entry.APIKeyEntries {
		trimmed := strings.TrimSpace(entry.APIKeyEntries[i].APIKey)
		entry.APIKeyEntries[i].APIKey = trimmed
		entry.APIKeyEntries[i].ProxyURL = strings.TrimSpace(entry.APIKeyEntries[i].ProxyURL)
		entry.APIKeyEntries[i].ProxyID = strings.TrimSpace(entry.APIKeyEntries[i].ProxyID)
		if trimmed != "" {
			existing[trimmed] = struct{}{}
		}
	}
}

func normalizeOpenAICompatImageEditsMode(mode string) string {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case "passthrough", "native", "image-edits":
		return "passthrough"
	case "chat-multimodal":
		return "chat-multimodal"
	case "image-generations":
		return "image-generations"
	default:
		return ""
	}
}

func normalizeOpenAICompatImageGenerationsImageField(field string) string {
	switch strings.ToLower(strings.TrimSpace(field)) {
	case "image_url":
		return "image_url"
	case "image":
		return "image"
	default:
		return ""
	}
}

func normalizeOpenCodeGoKey(entry *config.OpenCodeGoKey) {
	if entry == nil {
		return
	}
	entry.Name = strings.TrimSpace(entry.Name)
	entry.APIKey = strings.TrimSpace(entry.APIKey)
	entry.Prefix = strings.TrimSpace(entry.Prefix)
	entry.ProxyURL = strings.TrimSpace(entry.ProxyURL)
	entry.ProxyID = strings.TrimSpace(entry.ProxyID)
	entry.Headers = config.NormalizeHeaders(entry.Headers)
	entry.ExcludedModels = config.NormalizeExcludedModels(entry.ExcludedModels)
}

func normalizedOpenCodeGoKeyEntries(entries []config.OpenCodeGoKey) []config.OpenCodeGoKey {
	if len(entries) == 0 {
		return nil
	}
	out := make([]config.OpenCodeGoKey, len(entries))
	for i := range entries {
		out[i] = entries[i]
		normalizeOpenCodeGoKey(&out[i])
	}
	return out
}

func normalizedOpenAICompatibilityEntries(entries []config.OpenAICompatibility) []config.OpenAICompatibility {
	if len(entries) == 0 {
		return nil
	}
	out := make([]config.OpenAICompatibility, len(entries))
	for i := range entries {
		copyEntry := entries[i]
		if len(copyEntry.APIKeyEntries) > 0 {
			copyEntry.APIKeyEntries = append([]config.OpenAICompatibilityAPIKey(nil), copyEntry.APIKeyEntries...)
		}
		normalizeOpenAICompatibilityEntry(&copyEntry)
		out[i] = copyEntry
	}
	return out
}

func normalizeClaudeKey(entry *config.ClaudeKey) {
	if entry == nil {
		return
	}
	entry.Name = strings.TrimSpace(entry.Name)
	entry.APIKey = strings.TrimSpace(entry.APIKey)
	entry.BaseURL = strings.TrimSpace(entry.BaseURL)
	entry.ProxyURL = strings.TrimSpace(entry.ProxyURL)
	entry.ProxyID = strings.TrimSpace(entry.ProxyID)
	entry.Headers = config.NormalizeHeaders(entry.Headers)
	entry.ExcludedModels = config.NormalizeExcludedModels(entry.ExcludedModels)
	if len(entry.Models) == 0 {
		return
	}
	normalized := make([]config.ClaudeModel, 0, len(entry.Models))
	for i := range entry.Models {
		model := entry.Models[i]
		model.Name = strings.TrimSpace(model.Name)
		model.Alias = strings.TrimSpace(model.Alias)
		if model.Name == "" && model.Alias == "" {
			continue
		}
		normalized = append(normalized, model)
	}
	entry.Models = normalized
}

func normalizeBedrockKey(entry *config.BedrockKey) {
	if entry == nil {
		return
	}
	entry.Name = strings.TrimSpace(entry.Name)
	entry.Prefix = strings.TrimSpace(entry.Prefix)
	entry.AuthMode = strings.TrimSpace(entry.AuthMode)
	entry.APIKey = strings.TrimSpace(entry.APIKey)
	entry.AccessKeyID = strings.TrimSpace(entry.AccessKeyID)
	entry.SecretAccessKey = strings.TrimSpace(entry.SecretAccessKey)
	entry.SessionToken = strings.TrimSpace(entry.SessionToken)
	entry.Region = strings.TrimSpace(entry.Region)
	entry.BaseURL = strings.TrimSpace(entry.BaseURL)
	entry.ProxyURL = strings.TrimSpace(entry.ProxyURL)
	entry.ProxyID = strings.TrimSpace(entry.ProxyID)
	entry.Headers = config.NormalizeHeaders(entry.Headers)
	entry.ExcludedModels = config.NormalizeExcludedModels(entry.ExcludedModels)
	if len(entry.Models) == 0 {
		return
	}
	normalized := make([]config.BedrockModel, 0, len(entry.Models))
	for i := range entry.Models {
		model := entry.Models[i]
		model.Name = strings.TrimSpace(model.Name)
		model.Alias = strings.TrimSpace(model.Alias)
		if model.Name == "" && model.Alias == "" {
			continue
		}
		normalized = append(normalized, model)
	}
	entry.Models = normalized
}

func normalizeCodexKey(entry *config.CodexKey) {
	if entry == nil {
		return
	}
	entry.APIKey = strings.TrimSpace(entry.APIKey)
	entry.Prefix = strings.TrimSpace(entry.Prefix)
	entry.BaseURL = strings.TrimSpace(entry.BaseURL)
	entry.ProxyURL = strings.TrimSpace(entry.ProxyURL)
	entry.ProxyID = strings.TrimSpace(entry.ProxyID)
	entry.Headers = config.NormalizeHeaders(entry.Headers)
	entry.ExcludedModels = config.NormalizeExcludedModels(entry.ExcludedModels)
	if len(entry.Models) == 0 {
		return
	}
	normalized := make([]config.CodexModel, 0, len(entry.Models))
	for i := range entry.Models {
		model := entry.Models[i]
		model.Name = strings.TrimSpace(model.Name)
		model.Alias = strings.TrimSpace(model.Alias)
		if model.Name == "" && model.Alias == "" {
			continue
		}
		normalized = append(normalized, model)
	}
	entry.Models = normalized
}

func normalizeVertexCompatKey(entry *config.VertexCompatKey) {
	if entry == nil {
		return
	}
	entry.APIKey = strings.TrimSpace(entry.APIKey)
	entry.Prefix = strings.TrimSpace(entry.Prefix)
	entry.BaseURL = strings.TrimSpace(entry.BaseURL)
	entry.ProxyURL = strings.TrimSpace(entry.ProxyURL)
	entry.ProxyID = strings.TrimSpace(entry.ProxyID)
	entry.Headers = config.NormalizeHeaders(entry.Headers)
	if len(entry.Models) == 0 {
		return
	}
	normalized := make([]config.VertexCompatModel, 0, len(entry.Models))
	for i := range entry.Models {
		model := entry.Models[i]
		model.Name = strings.TrimSpace(model.Name)
		model.Alias = strings.TrimSpace(model.Alias)
		if model.Name == "" || model.Alias == "" {
			continue
		}
		normalized = append(normalized, model)
	}
	entry.Models = normalized
}

func sanitizedOAuthModelAlias(entries map[string][]config.OAuthModelAlias) map[string][]config.OAuthModelAlias {
	if len(entries) == 0 {
		return nil
	}
	copied := make(map[string][]config.OAuthModelAlias, len(entries))
	for channel, aliases := range entries {
		if len(aliases) == 0 {
			continue
		}
		copied[channel] = append([]config.OAuthModelAlias(nil), aliases...)
	}
	if len(copied) == 0 {
		return nil
	}
	cfg := config.Config{OAuthModelAlias: copied}
	cfg.SanitizeOAuthModelAlias()
	if len(cfg.OAuthModelAlias) == 0 {
		return nil
	}
	return cfg.OAuthModelAlias
}

// GetAmpCode returns the complete ampcode configuration.
func (h *Handler) GetAmpCode(c *gin.Context) {
	if h == nil || h.cfg == nil {
		c.JSON(200, gin.H{"ampcode": config.AmpCode{}})
		return
	}
	c.JSON(200, gin.H{"ampcode": h.cfg.AmpCode})
}

// GetAmpUpstreamURL returns the ampcode upstream URL.
func (h *Handler) GetAmpUpstreamURL(c *gin.Context) {
	if h == nil || h.cfg == nil {
		c.JSON(200, gin.H{"upstream-url": ""})
		return
	}
	c.JSON(200, gin.H{"upstream-url": h.cfg.AmpCode.UpstreamURL})
}

// PutAmpUpstreamURL updates the ampcode upstream URL.
func (h *Handler) PutAmpUpstreamURL(c *gin.Context) {
	h.updateStringField(c, func(v string) { h.cfg.AmpCode.UpstreamURL = strings.TrimSpace(v) })
}

// DeleteAmpUpstreamURL clears the ampcode upstream URL.
func (h *Handler) DeleteAmpUpstreamURL(c *gin.Context) {
	h.cfg.AmpCode.UpstreamURL = ""
	h.persist(c)
}

// GetAmpUpstreamAPIKey returns the ampcode upstream API key.
func (h *Handler) GetAmpUpstreamAPIKey(c *gin.Context) {
	if h == nil || h.cfg == nil {
		c.JSON(200, gin.H{"upstream-api-key": ""})
		return
	}
	c.JSON(200, gin.H{"upstream-api-key": h.cfg.AmpCode.UpstreamAPIKey})
}

// PutAmpUpstreamAPIKey updates the ampcode upstream API key.
func (h *Handler) PutAmpUpstreamAPIKey(c *gin.Context) {
	h.updateStringField(c, func(v string) { h.cfg.AmpCode.UpstreamAPIKey = strings.TrimSpace(v) })
}

// DeleteAmpUpstreamAPIKey clears the ampcode upstream API key.
func (h *Handler) DeleteAmpUpstreamAPIKey(c *gin.Context) {
	h.cfg.AmpCode.UpstreamAPIKey = ""
	h.persist(c)
}

// GetAmpRestrictManagementToLocalhost returns the localhost restriction setting.
func (h *Handler) GetAmpRestrictManagementToLocalhost(c *gin.Context) {
	if h == nil || h.cfg == nil {
		c.JSON(200, gin.H{"restrict-management-to-localhost": true})
		return
	}
	c.JSON(200, gin.H{"restrict-management-to-localhost": h.cfg.AmpCode.RestrictManagementToLocalhost})
}

// PutAmpRestrictManagementToLocalhost updates the localhost restriction setting.
func (h *Handler) PutAmpRestrictManagementToLocalhost(c *gin.Context) {
	h.updateBoolField(c, func(v bool) { h.cfg.AmpCode.RestrictManagementToLocalhost = v })
}

// GetAmpModelMappings returns the ampcode model mappings.
func (h *Handler) GetAmpModelMappings(c *gin.Context) {
	if h == nil || h.cfg == nil {
		c.JSON(200, gin.H{"model-mappings": []config.AmpModelMapping{}})
		return
	}
	c.JSON(200, gin.H{"model-mappings": h.cfg.AmpCode.ModelMappings})
}

// PutAmpModelMappings replaces all ampcode model mappings.
func (h *Handler) PutAmpModelMappings(c *gin.Context) {
	var body struct {
		Value []config.AmpModelMapping `json:"value"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(400, gin.H{"error": "invalid body"})
		return
	}
	h.cfg.AmpCode.ModelMappings = body.Value
	h.persist(c)
}

// PatchAmpModelMappings adds or updates model mappings.
func (h *Handler) PatchAmpModelMappings(c *gin.Context) {
	var body struct {
		Value []config.AmpModelMapping `json:"value"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(400, gin.H{"error": "invalid body"})
		return
	}

	existing := make(map[string]int)
	for i, m := range h.cfg.AmpCode.ModelMappings {
		existing[strings.TrimSpace(m.From)] = i
	}

	for _, newMapping := range body.Value {
		from := strings.TrimSpace(newMapping.From)
		if idx, ok := existing[from]; ok {
			h.cfg.AmpCode.ModelMappings[idx] = newMapping
		} else {
			h.cfg.AmpCode.ModelMappings = append(h.cfg.AmpCode.ModelMappings, newMapping)
			existing[from] = len(h.cfg.AmpCode.ModelMappings) - 1
		}
	}
	h.persist(c)
}

// DeleteAmpModelMappings removes specified model mappings by "from" field.
func (h *Handler) DeleteAmpModelMappings(c *gin.Context) {
	var body struct {
		Value []string `json:"value"`
	}
	if err := c.ShouldBindJSON(&body); err != nil || len(body.Value) == 0 {
		h.cfg.AmpCode.ModelMappings = nil
		h.persist(c)
		return
	}

	toRemove := make(map[string]bool)
	for _, from := range body.Value {
		toRemove[strings.TrimSpace(from)] = true
	}

	newMappings := make([]config.AmpModelMapping, 0, len(h.cfg.AmpCode.ModelMappings))
	for _, m := range h.cfg.AmpCode.ModelMappings {
		if !toRemove[strings.TrimSpace(m.From)] {
			newMappings = append(newMappings, m)
		}
	}
	h.cfg.AmpCode.ModelMappings = newMappings
	h.persist(c)
}

// GetAmpForceModelMappings returns whether model mappings are forced.
func (h *Handler) GetAmpForceModelMappings(c *gin.Context) {
	if h == nil || h.cfg == nil {
		c.JSON(200, gin.H{"force-model-mappings": false})
		return
	}
	c.JSON(200, gin.H{"force-model-mappings": h.cfg.AmpCode.ForceModelMappings})
}

// PutAmpForceModelMappings updates the force model mappings setting.
func (h *Handler) PutAmpForceModelMappings(c *gin.Context) {
	h.updateBoolField(c, func(v bool) { h.cfg.AmpCode.ForceModelMappings = v })
}

// GetAmpUpstreamAPIKeys returns the ampcode upstream API keys mapping.
func (h *Handler) GetAmpUpstreamAPIKeys(c *gin.Context) {
	if h == nil || h.cfg == nil {
		c.JSON(200, gin.H{"upstream-api-keys": []config.AmpUpstreamAPIKeyEntry{}})
		return
	}
	c.JSON(200, gin.H{"upstream-api-keys": h.cfg.AmpCode.UpstreamAPIKeys})
}

// PutAmpUpstreamAPIKeys replaces all ampcode upstream API keys mappings.
func (h *Handler) PutAmpUpstreamAPIKeys(c *gin.Context) {
	var body struct {
		Value []config.AmpUpstreamAPIKeyEntry `json:"value"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(400, gin.H{"error": "invalid body"})
		return
	}
	// Normalize entries: trim whitespace, filter empty
	normalized := normalizeAmpUpstreamAPIKeyEntries(body.Value)
	h.cfg.AmpCode.UpstreamAPIKeys = normalized
	h.persist(c)
}

// PatchAmpUpstreamAPIKeys adds or updates upstream API keys entries.
// Matching is done by upstream-api-key value.
func (h *Handler) PatchAmpUpstreamAPIKeys(c *gin.Context) {
	var body struct {
		Value []config.AmpUpstreamAPIKeyEntry `json:"value"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(400, gin.H{"error": "invalid body"})
		return
	}

	existing := make(map[string]int)
	for i, entry := range h.cfg.AmpCode.UpstreamAPIKeys {
		existing[strings.TrimSpace(entry.UpstreamAPIKey)] = i
	}

	for _, newEntry := range body.Value {
		upstreamKey := strings.TrimSpace(newEntry.UpstreamAPIKey)
		if upstreamKey == "" {
			continue
		}
		normalizedEntry := config.AmpUpstreamAPIKeyEntry{
			UpstreamAPIKey: upstreamKey,
			APIKeys:        normalizeAPIKeysList(newEntry.APIKeys),
		}
		if idx, ok := existing[upstreamKey]; ok {
			h.cfg.AmpCode.UpstreamAPIKeys[idx] = normalizedEntry
		} else {
			h.cfg.AmpCode.UpstreamAPIKeys = append(h.cfg.AmpCode.UpstreamAPIKeys, normalizedEntry)
			existing[upstreamKey] = len(h.cfg.AmpCode.UpstreamAPIKeys) - 1
		}
	}
	h.persist(c)
}

// DeleteAmpUpstreamAPIKeys removes specified upstream API keys entries.
// Body must be JSON: {"value": ["<upstream-api-key>", ...]}.
// If "value" is an empty array, clears all entries.
// If JSON is invalid or "value" is missing/null, returns 400 and does not persist any change.
func (h *Handler) DeleteAmpUpstreamAPIKeys(c *gin.Context) {
	var body struct {
		Value []string `json:"value"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(400, gin.H{"error": "invalid body"})
		return
	}

	if body.Value == nil {
		c.JSON(400, gin.H{"error": "missing value"})
		return
	}

	// Empty array means clear all
	if len(body.Value) == 0 {
		h.cfg.AmpCode.UpstreamAPIKeys = nil
		h.persist(c)
		return
	}

	toRemove := make(map[string]bool)
	for _, key := range body.Value {
		trimmed := strings.TrimSpace(key)
		if trimmed == "" {
			continue
		}
		toRemove[trimmed] = true
	}
	if len(toRemove) == 0 {
		c.JSON(400, gin.H{"error": "empty value"})
		return
	}

	newEntries := make([]config.AmpUpstreamAPIKeyEntry, 0, len(h.cfg.AmpCode.UpstreamAPIKeys))
	for _, entry := range h.cfg.AmpCode.UpstreamAPIKeys {
		if !toRemove[strings.TrimSpace(entry.UpstreamAPIKey)] {
			newEntries = append(newEntries, entry)
		}
	}
	h.cfg.AmpCode.UpstreamAPIKeys = newEntries
	h.persist(c)
}

// normalizeAmpUpstreamAPIKeyEntries normalizes a list of upstream API key entries.
func normalizeAmpUpstreamAPIKeyEntries(entries []config.AmpUpstreamAPIKeyEntry) []config.AmpUpstreamAPIKeyEntry {
	if len(entries) == 0 {
		return nil
	}
	out := make([]config.AmpUpstreamAPIKeyEntry, 0, len(entries))
	for _, entry := range entries {
		upstreamKey := strings.TrimSpace(entry.UpstreamAPIKey)
		if upstreamKey == "" {
			continue
		}
		apiKeys := normalizeAPIKeysList(entry.APIKeys)
		out = append(out, config.AmpUpstreamAPIKeyEntry{
			UpstreamAPIKey: upstreamKey,
			APIKeys:        apiKeys,
		})
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// normalizeAPIKeysList trims and filters empty strings from a list of API keys.
func normalizeAPIKeysList(keys []string) []string {
	if len(keys) == 0 {
		return nil
	}
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		trimmed := strings.TrimSpace(k)
		if trimmed != "" {
			out = append(out, trimmed)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
