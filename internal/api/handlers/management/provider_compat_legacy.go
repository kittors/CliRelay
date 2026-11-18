package management

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/MoeForge/moego/json"
	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/management/patchfield"
)

func parseLegacyCredentialWeight(raw json.RawMessage) (*int, error) {
	if len(raw) == 0 {
		return nil, fmt.Errorf("weight is missing")
	}
	if strings.TrimSpace(string(raw)) == "null" {
		return nil, nil
	}
	var weight int
	if err := json.Unmarshal(raw, &weight); err != nil {
		return nil, fmt.Errorf("weight must be an integer")
	}
	if err := config.ValidateCredentialWeight(&weight); err != nil {
		return nil, err
	}
	return &weight, nil
}

type legacyCompatKeyPatch struct {
	APIKey              *string                          `json:"api-key"`
	Priority            *int                             `json:"priority"`
	Weight              json.RawMessage                  `json:"weight"`
	Prefix              *string                          `json:"prefix"`
	BaseURL             *string                          `json:"base-url"`
	Websockets          *bool                            `json:"websockets"`
	ProxyURL            *string                          `json:"proxy-url"`
	Models              *[]config.CodexModel             `json:"models"`
	Headers             *map[string]string               `json:"headers"`
	ExcludedModels      *[]string                        `json:"excluded-models"`
	DisableCooling      patchfield.NullableBool          `json:"disable-cooling"`
	RequestRetry        *int                             `json:"request-retry"`
	RequestScopedErrors *[]config.RequestScopedErrorRule `json:"request-scoped-errors"`
}

func decodeLegacyCompatKeys(c *gin.Context) ([]config.CodexKey, error) {
	data, err := c.GetRawData()
	if err != nil {
		return nil, fmt.Errorf("failed to read body")
	}
	var entries []config.CodexKey
	if err = json.Unmarshal(data, &entries); err != nil {
		var body struct {
			Items []config.CodexKey `json:"items"`
		}
		if errItems := json.Unmarshal(data, &body); errItems != nil || len(body.Items) == 0 {
			return nil, fmt.Errorf("invalid body")
		}
		entries = body.Items
	}
	return entries, nil
}

func applyLegacyCompatPatch(entry *config.CodexKey, patch legacyCompatKeyPatch, allowWebsockets bool) error {
	if patch.APIKey != nil {
		entry.APIKey = strings.TrimSpace(*patch.APIKey)
	}
	if patch.Priority != nil {
		entry.Priority = *patch.Priority
	}
	if len(patch.Weight) > 0 {
		weight, err := parseLegacyCredentialWeight(patch.Weight)
		if err != nil {
			return err
		}
		entry.Weight = weight
	}
	if patch.Prefix != nil {
		entry.Prefix = strings.TrimSpace(*patch.Prefix)
	}
	if patch.BaseURL != nil {
		entry.BaseURL = strings.TrimSpace(*patch.BaseURL)
	}
	if allowWebsockets && patch.Websockets != nil {
		entry.Websockets = *patch.Websockets
	}
	if patch.ProxyURL != nil {
		entry.ProxyURL = strings.TrimSpace(*patch.ProxyURL)
	}
	if patch.Models != nil {
		entry.Models = append([]config.CodexModel(nil), (*patch.Models)...)
	}
	if patch.Headers != nil {
		entry.Headers = config.NormalizeHeaders(*patch.Headers)
	}
	if patch.ExcludedModels != nil {
		entry.ExcludedModels = config.NormalizeExcludedModels(*patch.ExcludedModels)
	}
	patch.DisableCooling.Apply(&entry.DisableCooling)
	if patch.RequestRetry != nil {
		entry.RequestRetry = patch.RequestRetry
	}
	if patch.RequestScopedErrors != nil {
		entry.RequestScopedErrors = append([]config.RequestScopedErrorRule(nil), (*patch.RequestScopedErrors)...)
	}
	return nil
}

func legacyCompatTargetIndex(entries []config.CodexKey, index *int, match *string) int {
	if index != nil && *index >= 0 && *index < len(entries) {
		return *index
	}
	if match != nil {
		value := strings.TrimSpace(*match)
		for i := range entries {
			if strings.TrimSpace(entries[i].APIKey) == value {
				return i
			}
		}
	}
	return -1
}

func (h *Handler) getLegacyCompatKeys(c *gin.Context, key string, entries []config.CodexKey) {
	h.mu.Lock()
	out := append([]config.CodexKey(nil), entries...)
	h.mu.Unlock()
	c.JSON(http.StatusOK, gin.H{key: out})
}

func (h *Handler) putLegacyCompatKeys(c *gin.Context, entries []config.CodexKey, set func(*config.Config, []config.CodexKey)) {
	h.mu.Lock()
	if h.cfg == nil {
		h.cfg = &config.Config{}
	}
	set(h.cfg, entries)
	h.cfg.SanitizeXAIKeys()
	h.cfg.SanitizeMetaKeys()
	h.persistLocked(c)
	h.mu.Unlock()
}

func (h *Handler) patchLegacyCompatKey(c *gin.Context, entries []config.CodexKey, set func(*config.Config, []config.CodexKey), allowWebsockets bool) {
	var body struct {
		Index *int                  `json:"index"`
		Match *string               `json:"match"`
		Value *legacyCompatKeyPatch `json:"value"`
	}
	if err := c.ShouldBindJSON(&body); err != nil || body.Value == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body"})
		return
	}

	index := legacyCompatTargetIndex(entries, body.Index, body.Match)
	if index < 0 {
		c.JSON(http.StatusNotFound, gin.H{"error": "item not found"})
		return
	}
	if err := applyLegacyCompatPatch(&entries[index], *body.Value, allowWebsockets); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if entries[index].APIKey == "" || entries[index].BaseURL == "" {
		entries = append(entries[:index], entries[index+1:]...)
	}

	h.mu.Lock()
	if h.cfg == nil {
		h.cfg = &config.Config{}
	}
	set(h.cfg, entries)
	h.cfg.SanitizeXAIKeys()
	h.cfg.SanitizeMetaKeys()
	h.persistLocked(c)
	h.mu.Unlock()
}

func (h *Handler) deleteLegacyCompatKey(c *gin.Context, entries []config.CodexKey, set func(*config.Config, []config.CodexKey), sanitize func(*config.Config)) {
	index := -1
	if value := strings.TrimSpace(c.Query("api-key")); value != "" {
		base, hasBase := c.GetQuery("base-url")
		matches := 0
		for i := range entries {
			if strings.TrimSpace(entries[i].APIKey) != value {
				continue
			}
			if hasBase && strings.TrimSpace(entries[i].BaseURL) != strings.TrimSpace(base) {
				continue
			}
			index = i
			matches++
		}
		if matches > 1 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "multiple items match api-key; base-url is required"})
			return
		}
	} else if value := strings.TrimSpace(c.Query("index")); value != "" {
		_, _ = fmt.Sscanf(value, "%d", &index)
	}
	if index < 0 || index >= len(entries) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "missing api-key or index"})
		return
	}
	entries = append(entries[:index], entries[index+1:]...)

	h.mu.Lock()
	if h.cfg == nil {
		h.cfg = &config.Config{}
	}
	set(h.cfg, entries)
	sanitize(h.cfg)
	h.persistLocked(c)
	h.mu.Unlock()
}

func (h *Handler) GetXAIKeys(c *gin.Context) {
	h.mu.Lock()
	entries := append([]config.CodexKey(nil), h.cfg.XAIKey...)
	h.mu.Unlock()
	h.getLegacyCompatKeys(c, "xai-api-key", entries)
}

func (h *Handler) PutXAIKeys(c *gin.Context) {
	entries, err := decodeLegacyCompatKeys(c)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	h.putLegacyCompatKeys(c, entries, func(cfg *config.Config, value []config.CodexKey) { cfg.XAIKey = value })
}

func (h *Handler) PatchXAIKey(c *gin.Context) {
	h.mu.Lock()
	entries := append([]config.CodexKey(nil), h.cfg.XAIKey...)
	h.mu.Unlock()
	h.patchLegacyCompatKey(c, entries, func(cfg *config.Config, value []config.CodexKey) { cfg.XAIKey = value }, true)
}

func (h *Handler) DeleteXAIKey(c *gin.Context) {
	h.mu.Lock()
	entries := append([]config.CodexKey(nil), h.cfg.XAIKey...)
	h.mu.Unlock()
	h.deleteLegacyCompatKey(c, entries, func(cfg *config.Config, value []config.CodexKey) { cfg.XAIKey = value }, func(cfg *config.Config) { cfg.SanitizeXAIKeys() })
}

func (h *Handler) GetMetaKeys(c *gin.Context) {
	h.mu.Lock()
	entries := append([]config.CodexKey(nil), h.cfg.MetaKey...)
	h.mu.Unlock()
	h.getLegacyCompatKeys(c, "meta-api-key", entries)
}

func (h *Handler) PutMetaKeys(c *gin.Context) {
	entries, err := decodeLegacyCompatKeys(c)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	h.putLegacyCompatKeys(c, entries, func(cfg *config.Config, value []config.CodexKey) { cfg.MetaKey = value })
}

func (h *Handler) PatchMetaKey(c *gin.Context) {
	h.mu.Lock()
	entries := append([]config.CodexKey(nil), h.cfg.MetaKey...)
	h.mu.Unlock()
	h.patchLegacyCompatKey(c, entries, func(cfg *config.Config, value []config.CodexKey) { cfg.MetaKey = value }, false)
}

func (h *Handler) DeleteMetaKey(c *gin.Context) {
	h.mu.Lock()
	entries := append([]config.CodexKey(nil), h.cfg.MetaKey...)
	h.mu.Unlock()
	h.deleteLegacyCompatKey(c, entries, func(cfg *config.Config, value []config.CodexKey) { cfg.MetaKey = value }, func(cfg *config.Config) { cfg.SanitizeMetaKeys() })
}
