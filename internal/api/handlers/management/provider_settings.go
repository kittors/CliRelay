package management

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	serviceapp "github.com/router-for-me/CLIProxyAPI/v6/internal/app/service"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/identity"
	providersettings "github.com/router-for-me/CLIProxyAPI/v6/internal/management/settings/providers"
	settingsstore "github.com/router-for-me/CLIProxyAPI/v6/internal/management/settings/store"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

type ProviderKeysHandler struct {
	*Handler
}

func (h *Handler) ProviderKeys() *ProviderKeysHandler {
	if h == nil {
		return nil
	}
	return &ProviderKeysHandler{Handler: h}
}

const providerTenantConfigKey = "management.provider_tenant_config"

func providerSettingsService(h *ProviderKeysHandler, c *gin.Context) *providersettings.Service {
	if h == nil {
		return providersettings.NewService(nil, nil)
	}
	cfg := h.providerConfigForTenant(c)
	captureProviderModerationBindingSnapshot(c, cfg)
	tenantID := effectiveTenantID(c)
	return providersettings.NewService(cfg, func() error {
		var auths []*coreauth.Auth
		if h.authManager != nil {
			auths = h.authManager.ListForTenant(tenantID)
		}
		_, err := collectKnownChannels(cfg, auths, "")
		return err
	})
}

func (h *Handler) providerConfigForTenant(c *gin.Context) *config.Config {
	if c != nil {
		if existing, ok := c.Get(providerTenantConfigKey); ok {
			if cfg, valid := existing.(*config.Config); valid {
				return cfg
			}
		}
	}
	// Every tenant, the system one included, edits a copy freshly read from
	// the database: a write computed from this node's in-memory copy could
	// silently revert a change another node made in the meantime.
	cfg := h.freshConfig(effectiveTenantID(c))
	if c != nil {
		c.Set(providerTenantConfigKey, cfg)
	}
	return cfg
}

func (h *ProviderKeysHandler) persistProviderSettings(c *gin.Context) bool {
	tenantID := effectiveTenantID(c)
	cfg := h.providerConfigForTenant(c)
	if !settingsstore.StoreAvailable() {
		// Without a database the live config is the store (see mutateSystemConfig).
		return h.persistProviderSettingsToYAML(c, cfg)
	}
	keys, err := settingsstore.CommitTenantConfig(c.Request.Context(), tenantID, cfg, requestVersion(c), h.configFilePath)
	if err != nil {
		writeConfigSaveError(c, "failed to save provider setting", err)
		return false
	}
	payload := gin.H{"status": "ok"}
	if len(keys) == 1 {
		setVersionHeader(c, cfg.RuntimeSettingState().Version(keys[0]))
	}
	var mutated func(*config.Config)
	var live *config.Config
	if tenantID == identity.SystemTenantID {
		h.mu.Lock()
		live = h.cfg
		settingsstore.AdoptKeys(live, cfg, keys...)
		mutated = h.onConfigMutated
		h.mu.Unlock()
	} else if h.authManager != nil {
		serviceapp.SyncConfigDerivedAuthsForTenant(h.cfg, h.authManager, tenantID)
	}
	if errCleanup := h.cleanupRemovedProviderModerationBindings(c.Request.Context(), c, tenantID, cfg); errCleanup != nil {
		payload["warning"] = fmt.Sprintf("provider saved but content moderation binding cleanup failed: %v", errCleanup)
	}
	c.JSON(http.StatusOK, payload)
	if mutated != nil {
		mutated(live)
	}
	return true
}

// gemini-api-key: []GeminiKey
func (h *ProviderKeysHandler) GetGeminiKeys(c *gin.Context) {
	h.jsonWithRequestVersion(c, settingsstore.RuntimeSettingGeminiKeys, gin.H{"gemini-api-key": providerSettingsService(h, c).GeminiKeys()})
}

func (h *ProviderKeysHandler) PutGeminiKeys(c *gin.Context) {
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
	if err := providerSettingsService(h, c).ReplaceGeminiKeys(arr); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	h.persistProviderSettings(c)
}

func (h *ProviderKeysHandler) PatchGeminiKey(c *gin.Context) {
	var body struct {
		Index *int                             `json:"index"`
		Match *string                          `json:"match"`
		Value *providersettings.GeminiKeyPatch `json:"value"`
	}
	if err := c.ShouldBindJSON(&body); err != nil || body.Value == nil {
		c.JSON(400, gin.H{"error": "invalid body"})
		return
	}
	if err := providerSettingsService(h, c).PatchGeminiKey(body.Index, body.Match, *body.Value); err != nil {
		if errors.Is(err, providersettings.ErrItemNotFound) {
			c.JSON(404, gin.H{"error": "item not found"})
			return
		}
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	h.persistProviderSettings(c)
}

func (h *ProviderKeysHandler) DeleteGeminiKey(c *gin.Context) {
	if val := strings.TrimSpace(c.Query("api-key")); val != "" {
		if providerSettingsService(h, c).DeleteGeminiKeyByAPIKey(val) {
			h.persistProviderSettings(c)
		} else {
			c.JSON(404, gin.H{"error": "item not found"})
		}
		return
	}
	if idxStr := c.Query("index"); idxStr != "" {
		var idx int
		if _, err := fmt.Sscanf(idxStr, "%d", &idx); err == nil && providerSettingsService(h, c).DeleteGeminiKeyByIndex(idx) {
			h.persistProviderSettings(c)
			return
		}
	}
	c.JSON(400, gin.H{"error": "missing api-key or index"})
}

// claude-api-key: []ClaudeKey
func (h *ProviderKeysHandler) GetClaudeKeys(c *gin.Context) {
	h.jsonWithRequestVersion(c, settingsstore.RuntimeSettingClaudeKeys, gin.H{"claude-api-key": providerSettingsService(h, c).ClaudeKeys()})
}

func (h *ProviderKeysHandler) PutClaudeKeys(c *gin.Context) {
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
	if err := providerSettingsService(h, c).ReplaceClaudeKeys(arr); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	h.persistProviderSettings(c)
}

func (h *ProviderKeysHandler) PatchClaudeKey(c *gin.Context) {
	var body struct {
		Index *int                             `json:"index"`
		Match *string                          `json:"match"`
		Value *providersettings.ClaudeKeyPatch `json:"value"`
	}
	if err := c.ShouldBindJSON(&body); err != nil || body.Value == nil {
		c.JSON(400, gin.H{"error": "invalid body"})
		return
	}
	if err := providerSettingsService(h, c).PatchClaudeKey(body.Index, body.Match, *body.Value); err != nil {
		if errors.Is(err, providersettings.ErrItemNotFound) {
			c.JSON(404, gin.H{"error": "item not found"})
			return
		}
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	h.persistProviderSettings(c)
}

func (h *ProviderKeysHandler) DeleteClaudeKey(c *gin.Context) {
	if val := c.Query("api-key"); val != "" {
		providerSettingsService(h, c).DeleteClaudeKeyByAPIKey(val)
		h.persistProviderSettings(c)
		return
	}
	if idxStr := c.Query("index"); idxStr != "" {
		var idx int
		_, err := fmt.Sscanf(idxStr, "%d", &idx)
		if err == nil && providerSettingsService(h, c).DeleteClaudeKeyByIndex(idx) {
			h.persistProviderSettings(c)
			return
		}
	}
	c.JSON(400, gin.H{"error": "missing api-key or index"})
}

// bedrock-api-key: []BedrockKey
func (h *ProviderKeysHandler) GetBedrockKeys(c *gin.Context) {
	h.jsonWithRequestVersion(c, settingsstore.RuntimeSettingBedrockKeys, gin.H{"bedrock-api-key": providerSettingsService(h, c).BedrockKeys()})
}

func (h *ProviderKeysHandler) PutBedrockKeys(c *gin.Context) {
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
	if err := providerSettingsService(h, c).ReplaceBedrockKeys(arr); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	h.persistProviderSettings(c)
}

func (h *ProviderKeysHandler) PatchBedrockKey(c *gin.Context) {
	var body struct {
		Index *int                              `json:"index"`
		Match *string                           `json:"match"`
		Value *providersettings.BedrockKeyPatch `json:"value"`
	}
	if err := c.ShouldBindJSON(&body); err != nil || body.Value == nil {
		c.JSON(400, gin.H{"error": "invalid body"})
		return
	}
	if err := providerSettingsService(h, c).PatchBedrockKey(body.Index, body.Match, *body.Value); err != nil {
		if errors.Is(err, providersettings.ErrItemNotFound) {
			c.JSON(404, gin.H{"error": "item not found"})
			return
		}
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	h.persistProviderSettings(c)
}

func (h *ProviderKeysHandler) DeleteBedrockKey(c *gin.Context) {
	if val := strings.TrimSpace(c.Query("api-key")); val != "" {
		if providerSettingsService(h, c).DeleteBedrockKeyByAPIKey(val) {
			h.persistProviderSettings(c)
			return
		}
		c.JSON(404, gin.H{"error": "item not found"})
		return
	}
	if val := strings.TrimSpace(c.Query("access-key-id")); val != "" {
		if providerSettingsService(h, c).DeleteBedrockKeyByAccessKeyID(val) {
			h.persistProviderSettings(c)
			return
		}
		c.JSON(404, gin.H{"error": "item not found"})
		return
	}
	if val := strings.TrimSpace(c.Query("name")); val != "" {
		if providerSettingsService(h, c).DeleteBedrockKeyByName(val) {
			h.persistProviderSettings(c)
			return
		}
		c.JSON(404, gin.H{"error": "item not found"})
		return
	}
	if idxStr := c.Query("index"); idxStr != "" {
		var idx int
		if _, err := fmt.Sscanf(idxStr, "%d", &idx); err == nil && providerSettingsService(h, c).DeleteBedrockKeyByIndex(idx) {
			h.persistProviderSettings(c)
			return
		}
	}
	c.JSON(400, gin.H{"error": "missing api-key, access-key-id, name, or index"})
}

// opencode-go-api-key: []OpenCodeGoKey
func (h *ProviderKeysHandler) GetOpenCodeGoKeys(c *gin.Context) {
	h.jsonWithRequestVersion(c, settingsstore.RuntimeSettingOpenCodeGoKeys, gin.H{"opencode-go-api-key": providerSettingsService(h, c).OpenCodeGoKeys()})
}

func (h *ProviderKeysHandler) PutOpenCodeGoKeys(c *gin.Context) {
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
	if err := providerSettingsService(h, c).ReplaceOpenCodeGoKeys(arr); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	h.persistProviderSettings(c)
}

func (h *ProviderKeysHandler) PatchOpenCodeGoKey(c *gin.Context) {
	var body struct {
		APIKey *string                           `json:"api-key"`
		Name   *string                           `json:"name"`
		Index  *int                              `json:"index"`
		Value  *providersettings.OpenCodeGoPatch `json:"value"`
	}
	if err := c.ShouldBindJSON(&body); err != nil || body.Value == nil {
		c.JSON(400, gin.H{"error": "invalid body"})
		return
	}
	if err := providerSettingsService(h, c).PatchOpenCodeGoKey(body.Index, body.APIKey, body.Name, *body.Value); err != nil {
		if errors.Is(err, providersettings.ErrItemNotFound) {
			c.JSON(404, gin.H{"error": "item not found"})
			return
		}
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	h.persistProviderSettings(c)
}

func (h *ProviderKeysHandler) DeleteOpenCodeGoKey(c *gin.Context) {
	if apiKey := strings.TrimSpace(c.Query("api-key")); apiKey != "" {
		if providerSettingsService(h, c).DeleteOpenCodeGoKeyByAPIKey(apiKey) {
			h.persistProviderSettings(c)
			return
		}
		c.JSON(404, gin.H{"error": "item not found"})
		return
	}
	if name := strings.TrimSpace(c.Query("name")); name != "" {
		if providerSettingsService(h, c).DeleteOpenCodeGoKeyByName(name) {
			h.persistProviderSettings(c)
			return
		}
		c.JSON(404, gin.H{"error": "item not found"})
		return
	}
	if idxStr := c.Query("index"); idxStr != "" {
		var idx int
		if _, err := fmt.Sscanf(idxStr, "%d", &idx); err == nil && providerSettingsService(h, c).DeleteOpenCodeGoKeyByIndex(idx) {
			h.persistProviderSettings(c)
			return
		}
	}
	c.JSON(400, gin.H{"error": "missing api-key, name, or index"})
}

// cline-api-key: []ClineKey
func (h *ProviderKeysHandler) GetClineKeys(c *gin.Context) {
	h.jsonWithRequestVersion(c, settingsstore.RuntimeSettingClineKeys, gin.H{"cline-api-key": providerSettingsService(h, c).ClineKeys()})
}

func (h *ProviderKeysHandler) PutClineKeys(c *gin.Context) {
	data, err := c.GetRawData()
	if err != nil {
		c.JSON(400, gin.H{"error": "failed to read body"})
		return
	}
	var arr []config.ClineKey
	if err = json.Unmarshal(data, &arr); err != nil {
		var obj struct {
			Items []config.ClineKey `json:"items"`
		}
		if err2 := json.Unmarshal(data, &obj); err2 != nil || len(obj.Items) == 0 {
			c.JSON(400, gin.H{"error": "invalid body"})
			return
		}
		arr = obj.Items
	}
	if err := providerSettingsService(h, c).ReplaceClineKeys(arr); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	h.persistProviderSettings(c)
}

func (h *ProviderKeysHandler) PatchClineKey(c *gin.Context) {
	var body struct {
		APIKey *string                      `json:"api-key"`
		Name   *string                      `json:"name"`
		Index  *int                         `json:"index"`
		Value  *providersettings.ClinePatch `json:"value"`
	}
	if err := c.ShouldBindJSON(&body); err != nil || body.Value == nil {
		c.JSON(400, gin.H{"error": "invalid body"})
		return
	}
	if err := providerSettingsService(h, c).PatchClineKey(body.Index, body.APIKey, body.Name, *body.Value); err != nil {
		if errors.Is(err, providersettings.ErrItemNotFound) {
			c.JSON(404, gin.H{"error": "item not found"})
			return
		}
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	h.persistProviderSettings(c)
}

func (h *ProviderKeysHandler) DeleteClineKey(c *gin.Context) {
	if apiKey := strings.TrimSpace(c.Query("api-key")); apiKey != "" {
		if providerSettingsService(h, c).DeleteClineKeyByAPIKey(apiKey) {
			h.persistProviderSettings(c)
			return
		}
		c.JSON(404, gin.H{"error": "item not found"})
		return
	}
	if name := strings.TrimSpace(c.Query("name")); name != "" {
		if providerSettingsService(h, c).DeleteClineKeyByName(name) {
			h.persistProviderSettings(c)
			return
		}
		c.JSON(404, gin.H{"error": "item not found"})
		return
	}
	if idxStr := c.Query("index"); idxStr != "" {
		var idx int
		if _, err := fmt.Sscanf(idxStr, "%d", &idx); err == nil && providerSettingsService(h, c).DeleteClineKeyByIndex(idx) {
			h.persistProviderSettings(c)
			return
		}
	}
	c.JSON(400, gin.H{"error": "missing api-key, name, or index"})
}

// ollama-cloud-api-key: []OllamaCloudKey
func (h *ProviderKeysHandler) GetOllamaCloudKeys(c *gin.Context) {
	h.jsonWithRequestVersion(c, settingsstore.RuntimeSettingOllamaCloudKeys, gin.H{"ollama-cloud-api-key": providerSettingsService(h, c).OllamaCloudKeys()})
}

func (h *ProviderKeysHandler) PutOllamaCloudKeys(c *gin.Context) {
	data, err := c.GetRawData()
	if err != nil {
		c.JSON(400, gin.H{"error": "failed to read body"})
		return
	}
	var arr []config.OllamaCloudKey
	if err = json.Unmarshal(data, &arr); err != nil {
		var obj struct {
			Items []config.OllamaCloudKey `json:"items"`
		}
		if err2 := json.Unmarshal(data, &obj); err2 != nil || len(obj.Items) == 0 {
			c.JSON(400, gin.H{"error": "invalid body"})
			return
		}
		arr = obj.Items
	}
	if err := providerSettingsService(h, c).ReplaceOllamaCloudKeys(arr); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	h.persistProviderSettings(c)
}

func (h *ProviderKeysHandler) PatchOllamaCloudKey(c *gin.Context) {
	var body struct {
		APIKey *string                            `json:"api-key"`
		Name   *string                            `json:"name"`
		Index  *int                               `json:"index"`
		Value  *providersettings.OllamaCloudPatch `json:"value"`
	}
	if err := c.ShouldBindJSON(&body); err != nil || body.Value == nil {
		c.JSON(400, gin.H{"error": "invalid body"})
		return
	}
	if err := providerSettingsService(h, c).PatchOllamaCloudKey(body.Index, body.APIKey, body.Name, *body.Value); err != nil {
		if errors.Is(err, providersettings.ErrItemNotFound) {
			c.JSON(404, gin.H{"error": "item not found"})
			return
		}
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	h.persistProviderSettings(c)
}

func (h *ProviderKeysHandler) DeleteOllamaCloudKey(c *gin.Context) {
	if apiKey := strings.TrimSpace(c.Query("api-key")); apiKey != "" {
		if providerSettingsService(h, c).DeleteOllamaCloudKeyByAPIKey(apiKey) {
			h.persistProviderSettings(c)
			return
		}
		c.JSON(404, gin.H{"error": "item not found"})
		return
	}
	if name := strings.TrimSpace(c.Query("name")); name != "" {
		if providerSettingsService(h, c).DeleteOllamaCloudKeyByName(name) {
			h.persistProviderSettings(c)
			return
		}
		c.JSON(404, gin.H{"error": "item not found"})
		return
	}
	if idxStr := c.Query("index"); idxStr != "" {
		var idx int
		if _, err := fmt.Sscanf(idxStr, "%d", &idx); err == nil && providerSettingsService(h, c).DeleteOllamaCloudKeyByIndex(idx) {
			h.persistProviderSettings(c)
			return
		}
	}
	c.JSON(400, gin.H{"error": "missing api-key, name, or index"})
}

// openai-compatibility: []OpenAICompatibility
func (h *ProviderKeysHandler) GetOpenAICompat(c *gin.Context) {
	h.jsonWithRequestVersion(c, settingsstore.RuntimeSettingOpenAICompatibility, gin.H{"openai-compatibility": providerSettingsService(h, c).OpenAICompatibility()})
}

func (h *ProviderKeysHandler) PutOpenAICompat(c *gin.Context) {
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
	if err := providerSettingsService(h, c).ReplaceOpenAICompatibility(arr); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	h.persistProviderSettings(c)
}

func (h *ProviderKeysHandler) PatchOpenAICompat(c *gin.Context) {
	var body struct {
		Name  *string                                    `json:"name"`
		Index *int                                       `json:"index"`
		Value *providersettings.OpenAICompatibilityPatch `json:"value"`
	}
	if err := c.ShouldBindJSON(&body); err != nil || body.Value == nil {
		c.JSON(400, gin.H{"error": "invalid body"})
		return
	}
	if err := providerSettingsService(h, c).PatchOpenAICompatibility(body.Index, body.Name, *body.Value); err != nil {
		if errors.Is(err, providersettings.ErrItemNotFound) {
			c.JSON(404, gin.H{"error": "item not found"})
			return
		}
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	h.persistProviderSettings(c)
}

func (h *ProviderKeysHandler) DeleteOpenAICompat(c *gin.Context) {
	if name := c.Query("name"); name != "" {
		providerSettingsService(h, c).DeleteOpenAICompatibilityByName(name)
		h.persistProviderSettings(c)
		return
	}
	if idxStr := c.Query("index"); idxStr != "" {
		var idx int
		_, err := fmt.Sscanf(idxStr, "%d", &idx)
		if err == nil && providerSettingsService(h, c).DeleteOpenAICompatibilityByIndex(idx) {
			h.persistProviderSettings(c)
			return
		}
	}
	c.JSON(400, gin.H{"error": "missing name or index"})
}

// vertex-api-key: []VertexCompatKey
func (h *ProviderKeysHandler) GetVertexCompatKeys(c *gin.Context) {
	h.jsonWithRequestVersion(c, settingsstore.RuntimeSettingVertexCompatKeys, gin.H{"vertex-api-key": providerSettingsService(h, c).VertexCompatKeys()})
}

func (h *ProviderKeysHandler) PutVertexCompatKeys(c *gin.Context) {
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
	providerSettingsService(h, c).ReplaceVertexCompatKeys(arr)
	h.persistProviderSettings(c)
}

func (h *ProviderKeysHandler) PatchVertexCompatKey(c *gin.Context) {
	var body struct {
		Index *int                                `json:"index"`
		Match *string                             `json:"match"`
		Value *providersettings.VertexCompatPatch `json:"value"`
	}
	if errBindJSON := c.ShouldBindJSON(&body); errBindJSON != nil || body.Value == nil {
		c.JSON(400, gin.H{"error": "invalid body"})
		return
	}
	if err := providerSettingsService(h, c).PatchVertexCompatKey(body.Index, body.Match, *body.Value); err != nil {
		c.JSON(404, gin.H{"error": "item not found"})
		return
	}
	h.persistProviderSettings(c)
}

func (h *ProviderKeysHandler) DeleteVertexCompatKey(c *gin.Context) {
	if val := strings.TrimSpace(c.Query("api-key")); val != "" {
		providerSettingsService(h, c).DeleteVertexCompatKeyByAPIKey(val)
		h.persistProviderSettings(c)
		return
	}
	if idxStr := c.Query("index"); idxStr != "" {
		var idx int
		_, errScan := fmt.Sscanf(idxStr, "%d", &idx)
		if errScan == nil && providerSettingsService(h, c).DeleteVertexCompatKeyByIndex(idx) {
			h.persistProviderSettings(c)
			return
		}
	}
	c.JSON(400, gin.H{"error": "missing api-key or index"})
}

// codex-api-key: []CodexKey
func (h *ProviderKeysHandler) GetCodexKeys(c *gin.Context) {
	h.jsonWithRequestVersion(c, settingsstore.RuntimeSettingCodexKeys, gin.H{"codex-api-key": providerSettingsService(h, c).CodexKeys()})
}

func (h *ProviderKeysHandler) PutCodexKeys(c *gin.Context) {
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
	if err := providerSettingsService(h, c).ReplaceCodexKeys(arr); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	h.persistProviderSettings(c)
}

func (h *ProviderKeysHandler) PatchCodexKey(c *gin.Context) {
	var body struct {
		Index *int                            `json:"index"`
		Match *string                         `json:"match"`
		Value *providersettings.CodexKeyPatch `json:"value"`
	}
	if err := c.ShouldBindJSON(&body); err != nil || body.Value == nil {
		c.JSON(400, gin.H{"error": "invalid body"})
		return
	}
	if err := providerSettingsService(h, c).PatchCodexKey(body.Index, body.Match, *body.Value); err != nil {
		if errors.Is(err, providersettings.ErrItemNotFound) {
			c.JSON(404, gin.H{"error": "item not found"})
			return
		}
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	h.persistProviderSettings(c)
}

func (h *ProviderKeysHandler) DeleteCodexKey(c *gin.Context) {
	if val := c.Query("api-key"); val != "" {
		providerSettingsService(h, c).DeleteCodexKeyByAPIKey(val)
		h.persistProviderSettings(c)
		return
	}
	if idxStr := c.Query("index"); idxStr != "" {
		var idx int
		_, err := fmt.Sscanf(idxStr, "%d", &idx)
		if err == nil && providerSettingsService(h, c).DeleteCodexKeyByIndex(idx) {
			h.persistProviderSettings(c)
			return
		}
	}
	c.JSON(400, gin.H{"error": "missing api-key or index"})
}
