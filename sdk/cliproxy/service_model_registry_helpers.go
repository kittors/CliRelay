package cliproxy

import (
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
)

func providerModelAccessExcludedModels(excluded []string) []string {
	for _, model := range excluded {
		if strings.TrimSpace(model) == "*" {
			return []string{"*"}
		}
	}
	return nil
}

type legacyModelEntry interface {
	GetName() string
	GetAlias() string
}

type staticThinkingResolver func(name string) *ModelThinkingSupport

func buildLegacyConfigModels[T legacyModelEntry](
	models []T,
	ownedBy string,
	modelType string,
	resolveThinking staticThinkingResolver,
) []*ModelInfo {
	if len(models) == 0 {
		return nil
	}
	now := time.Now().Unix()
	out := make([]*ModelInfo, 0, len(models))
	seen := make(map[string]struct{}, len(models))
	for i := range models {
		model := models[i]
		name := strings.TrimSpace(model.GetName())
		alias := strings.TrimSpace(model.GetAlias())
		if alias == "" {
			alias = name
		}
		if alias == "" {
			continue
		}
		key := strings.ToLower(alias)
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		display := name
		if display == "" {
			display = alias
		}
		info := &ModelInfo{
			ID:          alias,
			Object:      "model",
			Created:     now,
			OwnedBy:     ownedBy,
			Type:        modelType,
			DisplayName: display,
			UserDefined: true,
		}
		if name != "" && !strings.EqualFold(name, alias) {
			info.UpstreamModelID = name
		}
		if resolveThinking != nil && name != "" {
			info.Thinking = resolveThinking(name)
		}
		out = append(out, info)
	}
	return out
}

func filterConfigModels[T legacyModelEntry](models []T, keep func(string) bool) []T {
	if len(models) == 0 {
		return nil
	}
	out := make([]T, 0, len(models))
	for _, model := range models {
		if keep(model.GetName()) {
			out = append(out, model)
		}
	}
	return out
}

func isClinePassConfigModelID(model string) bool {
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(model)), "cline-pass/")
}

type oauthProviderModelConfigRow struct {
	ModelID     string
	OwnedBy     string
	Description string
	Source      string
	Enabled     bool
}

// appendOAuthProviderModelConfigs makes catalog rows routable for an OAuth
// credential. mappedOwners carries the tenant's own auth-group→owner mappings so
// operators can attach a model to a channel without the owner name having to
// match a built-in provider alias.
func appendOAuthProviderModelConfigs(
	models []*ModelInfo,
	provider string,
	authKind string,
	rows []oauthProviderModelConfigRow,
	mappedOwners []string,
) []*ModelInfo {
	if !strings.EqualFold(strings.TrimSpace(authKind), "oauth") {
		return models
	}
	provider = strings.ToLower(strings.TrimSpace(provider))
	if provider == "" {
		return models
	}
	owners := modelConfigOwnerAliases(provider)
	for _, owner := range mappedOwners {
		if key := normalizeModelConfigOwner(owner); key != "" {
			if owners == nil {
				owners = make(map[string]struct{}, len(mappedOwners))
			}
			owners[key] = struct{}{}
		}
	}
	if len(owners) == 0 {
		return models
	}

	out := append([]*ModelInfo(nil), models...)
	seen := make(map[string]struct{}, len(out))
	for _, model := range out {
		if model == nil {
			continue
		}
		id := strings.ToLower(strings.TrimSpace(model.ID))
		if id != "" {
			seen[id] = struct{}{}
		}
	}

	now := time.Now().Unix()
	for _, row := range rows {
		modelID := strings.TrimSpace(row.ModelID)
		if modelID == "" || !row.Enabled || !oauthProviderModelConfigSourceAllowed(row.Source) {
			continue
		}
		if _, ok := owners[normalizeModelConfigOwner(row.OwnedBy)]; !ok {
			continue
		}
		key := strings.ToLower(modelID)
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, &ModelInfo{
			ID:          modelID,
			Object:      "model",
			Created:     now,
			OwnedBy:     strings.TrimSpace(row.OwnedBy),
			Type:        provider,
			DisplayName: strings.TrimSpace(row.Description),
			Description: strings.TrimSpace(row.Description),
			UserDefined: true,
		})
	}
	return out
}

func oauthProviderModelConfigSourceAllowed(source string) bool {
	switch strings.ToLower(strings.TrimSpace(source)) {
	case "user", "seed", "openrouter":
		return true
	default:
		return false
	}
}

func modelConfigOwnerAliases(provider string) map[string]struct{} {
	provider = normalizeModelConfigOwner(provider)
	if provider == "" {
		return nil
	}
	values := []string{provider}
	switch provider {
	case "claude":
		values = append(values, "anthropic", "claude-code")
	case "gemini", "gemini-cli", "vertex":
		values = append(values, "google")
	case "kimi":
		// Kimi models are owned by Moonshot in the catalog, so a library row added
		// under the vendor name has to resolve back to the kimi channel.
		values = append(values, "moonshot", "moonshotai")
	}
	out := make(map[string]struct{}, len(values))
	for _, value := range values {
		if value = normalizeModelConfigOwner(value); value != "" {
			out[value] = struct{}{}
		}
	}
	return out
}

func normalizeModelConfigOwner(value string) string {
	return strings.ToLower(strings.Join(strings.Fields(strings.TrimSpace(value)), "-"))
}

func buildBedrockConfigModels(entry *config.BedrockKey, resolveThinking staticThinkingResolver) []*ModelInfo {
	if entry == nil {
		return nil
	}
	return buildLegacyConfigModels(entry.Models, "aws", "bedrock", resolveThinking)
}

func buildOpenCodeGoConfigModels(entry *config.OpenCodeGoKey) []*ModelInfo {
	if entry == nil || len(entry.Models) == 0 {
		return nil
	}
	models := filterConfigModels(entry.Models, func(name string) bool {
		return !isClinePassConfigModelID(name)
	})
	return buildLegacyConfigModels(models, "opencode", "opencode-go", nil)
}

func buildClineConfigModels(entry *config.ClineKey) []*ModelInfo {
	if entry == nil || len(entry.Models) == 0 {
		return nil
	}
	models := filterConfigModels(entry.Models, isClinePassConfigModelID)
	return buildLegacyConfigModels(models, "cline", "cline", nil)
}

func buildCommandCodeConfigModels(entry *config.CommandCodeKey) []*ModelInfo {
	if entry == nil || len(entry.Models) == 0 {
		return nil
	}
	models := filterConfigModels(entry.Models, func(name string) bool {
		return !isClinePassConfigModelID(name)
	})
	return buildLegacyConfigModels(models, "command-code", "commandcode", nil)
}

func buildOllamaCloudConfigModels(entry *config.OllamaCloudKey) []*ModelInfo {
	if entry == nil || len(entry.Models) == 0 {
		return nil
	}
	models := filterConfigModels(entry.Models, func(name string) bool {
		return !isClinePassConfigModelID(name)
	})
	return buildLegacyConfigModels(models, "ollama", "ollama-cloud", nil)
}
