package management

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	providersettings "github.com/router-for-me/CLIProxyAPI/v6/internal/management/settings/providers"
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

func providerSettingsService(h *ProviderKeysHandler) *providersettings.Service {
	if h == nil {
		return providersettings.NewService(nil, nil)
	}
	return providersettings.NewService(h.cfg, h.validateChannelNames)
}

// gemini-api-key: []GeminiKey
func (h *ProviderKeysHandler) GetGeminiKeys(c *gin.Context) {
	c.JSON(200, gin.H{"gemini-api-key": providerSettingsService(h).GeminiKeys()})
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
	if err := providerSettingsService(h).ReplaceGeminiKeys(arr); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	h.persist(c)
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
	if err := providerSettingsService(h).PatchGeminiKey(body.Index, body.Match, *body.Value); err != nil {
		if errors.Is(err, providersettings.ErrItemNotFound) {
			c.JSON(404, gin.H{"error": "item not found"})
			return
		}
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	h.persist(c)
}

func (h *ProviderKeysHandler) DeleteGeminiKey(c *gin.Context) {
	if val := strings.TrimSpace(c.Query("api-key")); val != "" {
		if providerSettingsService(h).DeleteGeminiKeyByAPIKey(val) {
			h.persist(c)
		} else {
			c.JSON(404, gin.H{"error": "item not found"})
		}
		return
	}
	if idxStr := c.Query("index"); idxStr != "" {
		var idx int
		if _, err := fmt.Sscanf(idxStr, "%d", &idx); err == nil && providerSettingsService(h).DeleteGeminiKeyByIndex(idx) {
			h.persist(c)
			return
		}
	}
	c.JSON(400, gin.H{"error": "missing api-key or index"})
}

// claude-api-key: []ClaudeKey
func (h *ProviderKeysHandler) GetClaudeKeys(c *gin.Context) {
	c.JSON(200, gin.H{"claude-api-key": providerSettingsService(h).ClaudeKeys()})
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
	if err := providerSettingsService(h).ReplaceClaudeKeys(arr); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	h.persist(c)
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
	if err := providerSettingsService(h).PatchClaudeKey(body.Index, body.Match, *body.Value); err != nil {
		if errors.Is(err, providersettings.ErrItemNotFound) {
			c.JSON(404, gin.H{"error": "item not found"})
			return
		}
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	h.persist(c)
}

func (h *ProviderKeysHandler) DeleteClaudeKey(c *gin.Context) {
	if val := c.Query("api-key"); val != "" {
		providerSettingsService(h).DeleteClaudeKeyByAPIKey(val)
		h.persist(c)
		return
	}
	if idxStr := c.Query("index"); idxStr != "" {
		var idx int
		_, err := fmt.Sscanf(idxStr, "%d", &idx)
		if err == nil && providerSettingsService(h).DeleteClaudeKeyByIndex(idx) {
			h.persist(c)
			return
		}
	}
	c.JSON(400, gin.H{"error": "missing api-key or index"})
}

// bedrock-api-key: []BedrockKey
func (h *ProviderKeysHandler) GetBedrockKeys(c *gin.Context) {
	c.JSON(200, gin.H{"bedrock-api-key": providerSettingsService(h).BedrockKeys()})
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
	if err := providerSettingsService(h).ReplaceBedrockKeys(arr); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	h.persist(c)
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
	if err := providerSettingsService(h).PatchBedrockKey(body.Index, body.Match, *body.Value); err != nil {
		if errors.Is(err, providersettings.ErrItemNotFound) {
			c.JSON(404, gin.H{"error": "item not found"})
			return
		}
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	h.persist(c)
}

func (h *ProviderKeysHandler) DeleteBedrockKey(c *gin.Context) {
	if val := strings.TrimSpace(c.Query("api-key")); val != "" {
		if providerSettingsService(h).DeleteBedrockKeyByAPIKey(val) {
			h.persist(c)
			return
		}
		c.JSON(404, gin.H{"error": "item not found"})
		return
	}
	if val := strings.TrimSpace(c.Query("access-key-id")); val != "" {
		if providerSettingsService(h).DeleteBedrockKeyByAccessKeyID(val) {
			h.persist(c)
			return
		}
		c.JSON(404, gin.H{"error": "item not found"})
		return
	}
	if val := strings.TrimSpace(c.Query("name")); val != "" {
		if providerSettingsService(h).DeleteBedrockKeyByName(val) {
			h.persist(c)
			return
		}
		c.JSON(404, gin.H{"error": "item not found"})
		return
	}
	if idxStr := c.Query("index"); idxStr != "" {
		var idx int
		if _, err := fmt.Sscanf(idxStr, "%d", &idx); err == nil && providerSettingsService(h).DeleteBedrockKeyByIndex(idx) {
			h.persist(c)
			return
		}
	}
	c.JSON(400, gin.H{"error": "missing api-key, access-key-id, name, or index"})
}

// opencode-go-api-key: []OpenCodeGoKey
func (h *ProviderKeysHandler) GetOpenCodeGoKeys(c *gin.Context) {
	c.JSON(200, gin.H{"opencode-go-api-key": providerSettingsService(h).OpenCodeGoKeys()})
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
	if err := providerSettingsService(h).ReplaceOpenCodeGoKeys(arr); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	h.persist(c)
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
	if err := providerSettingsService(h).PatchOpenCodeGoKey(body.Index, body.APIKey, body.Name, *body.Value); err != nil {
		if errors.Is(err, providersettings.ErrItemNotFound) {
			c.JSON(404, gin.H{"error": "item not found"})
			return
		}
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	h.persist(c)
}

func (h *ProviderKeysHandler) DeleteOpenCodeGoKey(c *gin.Context) {
	if apiKey := strings.TrimSpace(c.Query("api-key")); apiKey != "" {
		if providerSettingsService(h).DeleteOpenCodeGoKeyByAPIKey(apiKey) {
			h.persist(c)
			return
		}
		c.JSON(404, gin.H{"error": "item not found"})
		return
	}
	if name := strings.TrimSpace(c.Query("name")); name != "" {
		if providerSettingsService(h).DeleteOpenCodeGoKeyByName(name) {
			h.persist(c)
			return
		}
		c.JSON(404, gin.H{"error": "item not found"})
		return
	}
	if idxStr := c.Query("index"); idxStr != "" {
		var idx int
		if _, err := fmt.Sscanf(idxStr, "%d", &idx); err == nil && providerSettingsService(h).DeleteOpenCodeGoKeyByIndex(idx) {
			h.persist(c)
			return
		}
	}
	c.JSON(400, gin.H{"error": "missing api-key, name, or index"})
}

// cline-api-key: []ClineKey
func (h *ProviderKeysHandler) GetClineKeys(c *gin.Context) {
	c.JSON(200, gin.H{"cline-api-key": providerSettingsService(h).ClineKeys()})
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
	if err := providerSettingsService(h).ReplaceClineKeys(arr); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	h.persist(c)
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
	if err := providerSettingsService(h).PatchClineKey(body.Index, body.APIKey, body.Name, *body.Value); err != nil {
		if errors.Is(err, providersettings.ErrItemNotFound) {
			c.JSON(404, gin.H{"error": "item not found"})
			return
		}
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	h.persist(c)
}

func (h *ProviderKeysHandler) DeleteClineKey(c *gin.Context) {
	if apiKey := strings.TrimSpace(c.Query("api-key")); apiKey != "" {
		if providerSettingsService(h).DeleteClineKeyByAPIKey(apiKey) {
			h.persist(c)
			return
		}
		c.JSON(404, gin.H{"error": "item not found"})
		return
	}
	if name := strings.TrimSpace(c.Query("name")); name != "" {
		if providerSettingsService(h).DeleteClineKeyByName(name) {
			h.persist(c)
			return
		}
		c.JSON(404, gin.H{"error": "item not found"})
		return
	}
	if idxStr := c.Query("index"); idxStr != "" {
		var idx int
		if _, err := fmt.Sscanf(idxStr, "%d", &idx); err == nil && providerSettingsService(h).DeleteClineKeyByIndex(idx) {
			h.persist(c)
			return
		}
	}
	c.JSON(400, gin.H{"error": "missing api-key, name, or index"})
}

// openai-compatibility: []OpenAICompatibility
func (h *ProviderKeysHandler) GetOpenAICompat(c *gin.Context) {
	c.JSON(200, gin.H{"openai-compatibility": providerSettingsService(h).OpenAICompatibility()})
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
	if err := providerSettingsService(h).ReplaceOpenAICompatibility(arr); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	h.persist(c)
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
	if err := providerSettingsService(h).PatchOpenAICompatibility(body.Index, body.Name, *body.Value); err != nil {
		if errors.Is(err, providersettings.ErrItemNotFound) {
			c.JSON(404, gin.H{"error": "item not found"})
			return
		}
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	h.persist(c)
}

func (h *ProviderKeysHandler) DeleteOpenAICompat(c *gin.Context) {
	if name := c.Query("name"); name != "" {
		providerSettingsService(h).DeleteOpenAICompatibilityByName(name)
		h.persist(c)
		return
	}
	if idxStr := c.Query("index"); idxStr != "" {
		var idx int
		_, err := fmt.Sscanf(idxStr, "%d", &idx)
		if err == nil && providerSettingsService(h).DeleteOpenAICompatibilityByIndex(idx) {
			h.persist(c)
			return
		}
	}
	c.JSON(400, gin.H{"error": "missing name or index"})
}

// vertex-api-key: []VertexCompatKey
func (h *ProviderKeysHandler) GetVertexCompatKeys(c *gin.Context) {
	c.JSON(200, gin.H{"vertex-api-key": providerSettingsService(h).VertexCompatKeys()})
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
	providerSettingsService(h).ReplaceVertexCompatKeys(arr)
	h.persist(c)
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
	if err := providerSettingsService(h).PatchVertexCompatKey(body.Index, body.Match, *body.Value); err != nil {
		c.JSON(404, gin.H{"error": "item not found"})
		return
	}
	h.persist(c)
}

func (h *ProviderKeysHandler) DeleteVertexCompatKey(c *gin.Context) {
	if val := strings.TrimSpace(c.Query("api-key")); val != "" {
		providerSettingsService(h).DeleteVertexCompatKeyByAPIKey(val)
		h.persist(c)
		return
	}
	if idxStr := c.Query("index"); idxStr != "" {
		var idx int
		_, errScan := fmt.Sscanf(idxStr, "%d", &idx)
		if errScan == nil && providerSettingsService(h).DeleteVertexCompatKeyByIndex(idx) {
			h.persist(c)
			return
		}
	}
	c.JSON(400, gin.H{"error": "missing api-key or index"})
}

// codex-api-key: []CodexKey
func (h *ProviderKeysHandler) GetCodexKeys(c *gin.Context) {
	c.JSON(200, gin.H{"codex-api-key": providerSettingsService(h).CodexKeys()})
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
	if err := providerSettingsService(h).ReplaceCodexKeys(arr); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	h.persist(c)
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
	if err := providerSettingsService(h).PatchCodexKey(body.Index, body.Match, *body.Value); err != nil {
		if errors.Is(err, providersettings.ErrItemNotFound) {
			c.JSON(404, gin.H{"error": "item not found"})
			return
		}
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	h.persist(c)
}

func (h *ProviderKeysHandler) DeleteCodexKey(c *gin.Context) {
	if val := c.Query("api-key"); val != "" {
		providerSettingsService(h).DeleteCodexKeyByAPIKey(val)
		h.persist(c)
		return
	}
	if idxStr := c.Query("index"); idxStr != "" {
		var idx int
		_, err := fmt.Sscanf(idxStr, "%d", &idx)
		if err == nil && providerSettingsService(h).DeleteCodexKeyByIndex(idx) {
			h.persist(c)
			return
		}
	}
	c.JSON(400, gin.H{"error": "missing api-key or index"})
}
