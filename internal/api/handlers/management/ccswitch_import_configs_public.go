package management

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	apikeysettings "github.com/router-for-me/CLIProxyAPI/v6/internal/management/settings/apikey"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/usage"
)

func normalizeLowerStringList(values []string) []string {
	if len(values) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(values))
	out := make([]string, 0, len(values))
	for _, raw := range values {
		text := strings.ToLower(strings.TrimSpace(raw))
		if text == "" {
			continue
		}
		if _, ok := seen[text]; ok {
			continue
		}
		seen[text] = struct{}{}
		out = append(out, text)
	}
	return out
}

func normalizeExactStringList(values []string) []string {
	if len(values) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(values))
	out := make([]string, 0, len(values))
	for _, raw := range values {
		text := strings.TrimSpace(raw)
		if text == "" {
			continue
		}
		if _, ok := seen[text]; ok {
			continue
		}
		seen[text] = struct{}{}
		out = append(out, text)
	}
	return out
}

func ccSwitchImportConfigTargetModels(config usage.CcSwitchImportConfigRow) []string {
	seen := map[string]struct{}{}
	var targets []string

	for _, mapping := range config.ModelMappings {
		target := strings.TrimSpace(mapping.TargetModel)
		if target == "" {
			continue
		}
		if _, ok := seen[target]; ok {
			continue
		}
		seen[target] = struct{}{}
		targets = append(targets, target)
	}

	if len(targets) > 0 {
		return targets
	}

	if model := strings.TrimSpace(config.DefaultModel); model != "" {
		return []string{model}
	}

	return nil
}

func ccSwitchImportConfigMatchesAllowedModels(config usage.CcSwitchImportConfigRow, allowedModels []string) bool {
	if len(allowedModels) == 0 {
		return true
	}
	targets := ccSwitchImportConfigTargetModels(config)
	if len(targets) == 0 {
		return true
	}
	allowed := make(map[string]struct{}, len(allowedModels))
	for _, model := range allowedModels {
		allowed[strings.TrimSpace(model)] = struct{}{}
	}
	for _, model := range targets {
		if _, ok := allowed[model]; !ok {
			return false
		}
	}
	return true
}

func ccSwitchImportConfigMatchesAPIKeyPermissions(config usage.CcSwitchImportConfigRow, entry *usage.APIKeyRow) bool {
	if entry == nil {
		return true
	}

	entryGroups := normalizeLowerStringList(entry.AllowedChannelGroups)
	configGroups := normalizeLowerStringList(config.AllowedChannelGroups)
	matchesGroups := len(entryGroups) == 0 || len(configGroups) == 0
	if !matchesGroups {
		set := make(map[string]struct{}, len(entryGroups))
		for _, group := range entryGroups {
			set[group] = struct{}{}
		}
		for _, group := range configGroups {
			if _, ok := set[group]; ok {
				matchesGroups = true
				break
			}
		}
	}

	return matchesGroups && ccSwitchImportConfigMatchesAllowedModels(config, normalizeExactStringList(entry.AllowedModels))
}

// GetPublicCcSwitchImportConfigs returns CC Switch import presets filtered by the API key's
// permission restrictions. This is a public endpoint (no management key required).
//
// SECURITY:
// - Requires api_key in POST body; does not accept query params to avoid leaking secrets in URLs.
// - Returns an empty list when the key does not exist or is disabled.
func (h *Handler) GetPublicCcSwitchImportConfigs(c *gin.Context) {
	req, status, message := readPublicLookupRequest(c)
	if message != "" {
		c.JSON(status, gin.H{"error": message})
		return
	}

	apiKey := strings.TrimSpace(req.APIKey)
	if apiKey == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "api_key parameter is required"})
		return
	}

	row := apikeysettings.NewService(nil).GetRow(apiKey)
	maskedAPIKey := maskKey(apiKey)
	if row == nil {
		items := []usage.CcSwitchImportConfigRow{}
		c.JSON(http.StatusOK, gin.H{
			"ccswitch-import-configs": items,
			"items":                   items,
			"api_key":                 maskedAPIKey,
			"api_key_masked":          maskedAPIKey,
			"found":                   false,
		})
		return
	}

	profiles := usage.ListAPIKeyPermissionProfiles()
	effective := usage.EffectiveAPIKeyRowWithProfiles(*row, profiles)
	if effective.Disabled {
		items := []usage.CcSwitchImportConfigRow{}
		c.JSON(http.StatusOK, gin.H{
			"ccswitch-import-configs": items,
			"items":                   items,
			"api_key":                 maskedAPIKey,
			"api_key_masked":          maskedAPIKey,
			"found":                   false,
		})
		return
	}

	configs := usage.ListCcSwitchImportConfigs()
	if configs == nil {
		configs = []usage.CcSwitchImportConfigRow{}
	}

	filtered := make([]usage.CcSwitchImportConfigRow, 0, len(configs))
	for _, cfg := range configs {
		if ccSwitchImportConfigMatchesAPIKeyPermissions(cfg, &effective) {
			filtered = append(filtered, usage.AttachCcSwitchCodexModelCatalog(cfg))
		}
	}

	c.JSON(http.StatusOK, gin.H{
		"ccswitch-import-configs": filtered,
		"items":                   filtered,
		"api_key":                 maskedAPIKey,
		"api_key_masked":          maskedAPIKey,
		"found":                   true,
	})
}
