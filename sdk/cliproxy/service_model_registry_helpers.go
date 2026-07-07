package cliproxy

import (
	"strings"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v6/sdk/config"
)

func applyExcludedModels(models []*ModelInfo, excluded []string) []*ModelInfo {
	if len(models) == 0 || len(excluded) == 0 {
		return models
	}

	patterns := make([]string, 0, len(excluded))
	for _, item := range excluded {
		if trimmed := strings.TrimSpace(item); trimmed != "" {
			patterns = append(patterns, strings.ToLower(trimmed))
		}
	}
	if len(patterns) == 0 {
		return models
	}

	filtered := make([]*ModelInfo, 0, len(models))
	for _, model := range models {
		if model == nil {
			continue
		}
		modelID := strings.ToLower(strings.TrimSpace(model.ID))
		blocked := false
		for _, pattern := range patterns {
			if matchWildcard(pattern, modelID) {
				blocked = true
				break
			}
		}
		if !blocked {
			filtered = append(filtered, model)
		}
	}
	return filtered
}

func providerModelAccessExcludedModels(excluded []string) []string {
	for _, model := range excluded {
		if strings.TrimSpace(model) == "*" {
			return []string{"*"}
		}
	}
	return nil
}

func applyModelPrefixes(models []*ModelInfo, prefix string, forceModelPrefix bool) []*ModelInfo {
	trimmedPrefix := strings.TrimSpace(prefix)
	if trimmedPrefix == "" || len(models) == 0 {
		return models
	}

	out := make([]*ModelInfo, 0, len(models)*2)
	seen := make(map[string]struct{}, len(models)*2)

	addModel := func(model *ModelInfo) {
		if model == nil {
			return
		}
		id := strings.TrimSpace(model.ID)
		if id == "" {
			return
		}
		if _, exists := seen[id]; exists {
			return
		}
		seen[id] = struct{}{}
		out = append(out, model)
	}

	for _, model := range models {
		if model == nil {
			continue
		}
		baseID := strings.TrimSpace(model.ID)
		if baseID == "" {
			continue
		}
		if !forceModelPrefix || trimmedPrefix == baseID {
			addModel(model)
		}
		clone := *model
		clone.ID = trimmedPrefix + "/" + baseID
		addModel(&clone)
	}
	return out
}

// matchWildcard performs case-insensitive wildcard matching where '*' matches any substring.
func matchWildcard(pattern, value string) bool {
	if pattern == "" {
		return false
	}

	// Fast path for exact match (no wildcard present).
	if !strings.Contains(pattern, "*") {
		return pattern == value
	}

	parts := strings.Split(pattern, "*")
	// Handle prefix.
	if prefix := parts[0]; prefix != "" {
		if !strings.HasPrefix(value, prefix) {
			return false
		}
		value = value[len(prefix):]
	}

	// Handle suffix.
	if suffix := parts[len(parts)-1]; suffix != "" {
		if !strings.HasSuffix(value, suffix) {
			return false
		}
		value = value[:len(value)-len(suffix)]
	}

	// Handle middle segments in order.
	for i := 1; i < len(parts)-1; i++ {
		segment := parts[i]
		if segment == "" {
			continue
		}
		idx := strings.Index(value, segment)
		if idx < 0 {
			return false
		}
		value = value[idx+len(segment):]
	}

	return true
}

type modelEntry interface {
	GetName() string
	GetAlias() string
}

type staticThinkingResolver func(name string) *ModelThinkingSupport

func buildConfigModels[T modelEntry](
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

func filterConfigModels[T modelEntry](models []T, keep func(string) bool) []T {
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

func buildVertexCompatConfigModels(
	entry *config.VertexCompatKey,
	resolveThinking staticThinkingResolver,
) []*ModelInfo {
	if entry == nil {
		return nil
	}
	return buildConfigModels(entry.Models, "google", "vertex", resolveThinking)
}

func buildGeminiConfigModels(entry *config.GeminiKey, resolveThinking staticThinkingResolver) []*ModelInfo {
	if entry == nil {
		return nil
	}
	return buildConfigModels(entry.Models, "google", "gemini", resolveThinking)
}

func buildClaudeConfigModels(entry *config.ClaudeKey, resolveThinking staticThinkingResolver) []*ModelInfo {
	if entry == nil {
		return nil
	}
	return buildConfigModels(entry.Models, "anthropic", "claude", resolveThinking)
}

type oauthProviderModelConfigRow struct {
	ModelID     string
	OwnedBy     string
	Description string
	Source      string
	Enabled     bool
}

func appendOAuthProviderModelConfigs(
	models []*ModelInfo,
	provider string,
	authKind string,
	rows []oauthProviderModelConfigRow,
) []*ModelInfo {
	if !strings.EqualFold(strings.TrimSpace(authKind), "oauth") {
		return models
	}
	provider = strings.ToLower(strings.TrimSpace(provider))
	if provider == "" {
		return models
	}
	owners := modelConfigOwnerAliases(provider)
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
	case "codex":
		values = append(values, "openai")
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
	return buildConfigModels(entry.Models, "aws", "bedrock", resolveThinking)
}

func buildCodexConfigModels(entry *config.CodexKey, resolveThinking staticThinkingResolver) []*ModelInfo {
	if entry == nil {
		return nil
	}
	return buildConfigModels(entry.Models, "openai", "openai", resolveThinking)
}

func buildOpenCodeGoConfigModels(entry *config.OpenCodeGoKey) []*ModelInfo {
	if entry == nil || len(entry.Models) == 0 {
		return nil
	}
	models := filterConfigModels(entry.Models, func(name string) bool {
		return !isClinePassConfigModelID(name)
	})
	return buildConfigModels(models, "opencode", "opencode-go", nil)
}

func buildClineConfigModels(entry *config.ClineKey) []*ModelInfo {
	if entry == nil || len(entry.Models) == 0 {
		return nil
	}
	models := filterConfigModels(entry.Models, isClinePassConfigModelID)
	return buildConfigModels(models, "cline", "cline", nil)
}

func buildOllamaCloudConfigModels(entry *config.OllamaCloudKey) []*ModelInfo {
	if entry == nil || len(entry.Models) == 0 {
		return nil
	}
	models := filterConfigModels(entry.Models, func(name string) bool {
		return !isClinePassConfigModelID(name)
	})
	return buildConfigModels(models, "ollama", "ollama-cloud", nil)
}

func rewriteModelInfoName(name, oldID, newID string) string {
	trimmed := strings.TrimSpace(name)
	if trimmed == "" {
		return name
	}
	oldID = strings.TrimSpace(oldID)
	newID = strings.TrimSpace(newID)
	if oldID == "" || newID == "" {
		return name
	}
	if strings.EqualFold(oldID, newID) {
		return name
	}
	if strings.EqualFold(trimmed, oldID) {
		return newID
	}
	if strings.HasSuffix(trimmed, "/"+oldID) {
		prefix := strings.TrimSuffix(trimmed, oldID)
		return prefix + newID
	}
	if trimmed == "models/"+oldID {
		return "models/" + newID
	}
	return name
}

func applyOAuthModelAlias(cfg *config.Config, provider, authKind string, models []*ModelInfo) []*ModelInfo {
	if cfg == nil || len(models) == 0 {
		return models
	}
	channel := coreauth.OAuthModelAliasChannel(provider, authKind)
	if channel == "" || len(cfg.OAuthModelAlias) == 0 {
		return models
	}
	aliases := cfg.OAuthModelAlias[channel]
	if len(aliases) == 0 {
		return models
	}

	type aliasEntry struct {
		alias string
		fork  bool
	}

	forward := make(map[string][]aliasEntry, len(aliases))
	for i := range aliases {
		name := strings.TrimSpace(aliases[i].Name)
		alias := strings.TrimSpace(aliases[i].Alias)
		if name == "" || alias == "" {
			continue
		}
		if strings.EqualFold(name, alias) {
			continue
		}
		key := strings.ToLower(name)
		forward[key] = append(forward[key], aliasEntry{alias: alias, fork: aliases[i].Fork})
	}
	if len(forward) == 0 {
		return models
	}

	out := make([]*ModelInfo, 0, len(models))
	seen := make(map[string]struct{}, len(models))
	for _, model := range models {
		if model == nil {
			continue
		}
		id := strings.TrimSpace(model.ID)
		if id == "" {
			continue
		}
		key := strings.ToLower(id)
		entries := forward[key]
		if len(entries) == 0 {
			if _, exists := seen[key]; exists {
				continue
			}
			seen[key] = struct{}{}
			out = append(out, model)
			continue
		}

		keepOriginal := false
		for _, entry := range entries {
			if entry.fork {
				keepOriginal = true
				break
			}
		}
		if keepOriginal {
			if _, exists := seen[key]; !exists {
				seen[key] = struct{}{}
				out = append(out, model)
			}
		}

		addedAlias := false
		for _, entry := range entries {
			mappedID := strings.TrimSpace(entry.alias)
			if mappedID == "" {
				continue
			}
			if strings.EqualFold(mappedID, id) {
				continue
			}
			aliasKey := strings.ToLower(mappedID)
			if _, exists := seen[aliasKey]; exists {
				continue
			}
			seen[aliasKey] = struct{}{}
			clone := *model
			clone.ID = mappedID
			if clone.Name != "" {
				clone.Name = rewriteModelInfoName(clone.Name, id, mappedID)
			}
			out = append(out, &clone)
			addedAlias = true
		}

		if !keepOriginal && !addedAlias {
			if _, exists := seen[key]; exists {
				continue
			}
			seen[key] = struct{}{}
			out = append(out, model)
		}
	}
	return out
}
