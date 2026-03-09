package management

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/usage"
)

// GetUsageLogs returns paginated, filterable request log entries from SQLite.
// It enriches each log item with resolved api_key_name and channel_name
// from the in-memory config, eliminating the need for multiple frontend API calls.
func (h *Handler) GetUsageLogs(c *gin.Context) {
	params := usage.LogQueryParams{
		Page:   intQueryDefault(c, "page", 1),
		Size:   intQueryDefault(c, "size", 50),
		Days:   intQueryDefault(c, "days", 7),
		APIKey: strings.TrimSpace(c.Query("api_key")),
		Model:  strings.TrimSpace(c.Query("model")),
		Status: strings.TrimSpace(c.Query("status")),
	}

	result, err := usage.QueryLogs(params)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	filters, err := usage.QueryFilters(params.Days)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	stats, err := usage.QueryStats(params)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	// Build name maps from config
	keyNameMap, channelNameMap := h.buildNameMaps()

	// Enrich log items with resolved names
	for i := range result.Items {
		item := &result.Items[i]
		if name, ok := keyNameMap[item.APIKey]; ok {
			item.APIKeyName = name
		}
		// Fill in channel_name from config if not already set in the log
		if item.ChannelName == "" {
			if name, ok := channelNameMap[item.Source]; ok {
				item.ChannelName = name
			}
		}
	}

	// Enrich filter options with key names
	filters.APIKeyNames = make(map[string]string, len(filters.APIKeys))
	for _, key := range filters.APIKeys {
		if name, ok := keyNameMap[key]; ok {
			filters.APIKeyNames[key] = name
		}
	}

	c.JSON(http.StatusOK, gin.H{
		"items":   result.Items,
		"total":   result.Total,
		"page":    result.Page,
		"size":    result.Size,
		"filters": filters,
		"stats":   stats,
	})
}

// buildNameMaps builds two maps from the current config:
//  1. keyNameMap:     api_key → display name (from api-key-entries with names, or provider configs)
//  2. channelNameMap: provider_api_key → channel name (from provider config Name fields)
func (h *Handler) buildNameMaps() (keyNameMap, channelNameMap map[string]string) {
	keyNameMap = make(map[string]string)
	channelNameMap = make(map[string]string)

	cfg := h.cfg
	if cfg == nil {
		return
	}

	// Gemini keys: apiKey → name (as channel name)
	for _, k := range cfg.GeminiKey {
		if k.APIKey != "" && k.Name != "" {
			channelNameMap[k.APIKey] = k.Name
		}
	}

	// Claude keys: apiKey → name
	for _, k := range cfg.ClaudeKey {
		if k.APIKey != "" && k.Name != "" {
			channelNameMap[k.APIKey] = k.Name
		}
	}

	// Codex keys: apiKey → name
	for _, k := range cfg.CodexKey {
		if k.APIKey != "" && k.Name != "" {
			channelNameMap[k.APIKey] = k.Name
		}
	}

	// Vertex keys: no Name field, skip

	// OpenAI compatibility: provider name applies to all its API keys
	for _, provider := range cfg.OpenAICompatibility {
		if provider.Name == "" {
			continue
		}
		for _, entry := range provider.APIKeyEntries {
			if entry.APIKey != "" {
				channelNameMap[entry.APIKey] = provider.Name
			}
		}
	}

	// Build key name map from api-key-entries in OpenAI compatibility configs
	// (these are the user-facing API keys exposed by the proxy)
	buildKeyNameMapFromConfig(cfg, keyNameMap)

	return
}

// buildKeyNameMapFromConfig populates keyNameMap with api_key → name mappings
// from the config's various provider sources.
func buildKeyNameMapFromConfig(cfg *config.Config, keyNameMap map[string]string) {
	if cfg == nil {
		return
	}

	// The user-facing API keys are typically in openai-compatibility entries
	// and the direct provider keys (gemini/claude/codex/vertex).
	// The "name" for a user-facing key comes from the OpenAICompatibility.Name
	// or from the provider key's own Name field.

	for _, k := range cfg.GeminiKey {
		if k.APIKey != "" && k.Name != "" {
			keyNameMap[k.APIKey] = k.Name
		}
	}
	for _, k := range cfg.ClaudeKey {
		if k.APIKey != "" && k.Name != "" {
			keyNameMap[k.APIKey] = k.Name
		}
	}
	for _, k := range cfg.CodexKey {
		if k.APIKey != "" && k.Name != "" {
			keyNameMap[k.APIKey] = k.Name
		}
	}
	for _, provider := range cfg.OpenAICompatibility {
		for _, entry := range provider.APIKeyEntries {
			if entry.APIKey != "" && provider.Name != "" {
				keyNameMap[entry.APIKey] = provider.Name
			}
		}
	}
}

func intQueryDefault(c *gin.Context, key string, def int) int {
	v := strings.TrimSpace(c.Query(key))
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 1 {
		return def
	}
	return n
}
