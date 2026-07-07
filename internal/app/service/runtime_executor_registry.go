package serviceapp

import (
	"context"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/runtime/executor"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/watcher/synthesizer"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/wsrelay"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v6/sdk/config"
	sdkmodelcatalog "github.com/router-for-me/CLIProxyAPI/v6/sdk/modelcatalog"
	log "github.com/sirupsen/logrus"
)

func SyncConfigDerivedAuths(cfg *config.Config, coreManager *coreauth.Manager) {
	if cfg == nil || coreManager == nil {
		return
	}

	ctx := coreauth.WithSkipPersist(context.Background())
	synth := synthesizer.NewConfigSynthesizer()
	auths, err := synth.Synthesize(&synthesizer.SynthesisContext{
		Config:      cfg,
		AuthDir:     cfg.AuthDir,
		Now:         time.Now(),
		IDGenerator: synthesizer.NewStableIDGenerator(),
	})
	if err != nil {
		log.WithError(err).Warn("failed to synthesize config auths during service config reload")
		return
	}

	desiredIDs := make(map[string]struct{}, len(auths))
	for _, next := range auths {
		if next == nil || strings.TrimSpace(next.ID) == "" {
			continue
		}
		desiredIDs[next.ID] = struct{}{}
		if existing, ok := coreManager.GetByID(next.ID); ok && existing != nil {
			next.CreatedAt = existing.CreatedAt
			next.LastRefreshedAt = existing.LastRefreshedAt
			next.NextRefreshAfter = existing.NextRefreshAfter
			_, err = coreManager.Update(ctx, next)
		} else {
			_, err = coreManager.Register(ctx, next)
		}
		if err != nil {
			log.WithError(err).Warnf("failed to apply config auth %s", next.ID)
			continue
		}
		syncConfigDerivedAuthModels(cfg, next)
	}

	for _, existing := range coreManager.List() {
		if existing == nil || strings.TrimSpace(existing.ID) == "" {
			continue
		}
		if !isConfigDerivedAuth(existing) {
			continue
		}
		if _, stillConfigured := desiredIDs[existing.ID]; stillConfigured {
			continue
		}
		if existing.Disabled && existing.Status == coreauth.StatusDisabled {
			continue
		}
		disabled := existing.Clone()
		disabled.Disabled = true
		disabled.Status = coreauth.StatusDisabled
		disabled.StatusMessage = "removed via config update"
		disabled.UpdatedAt = time.Now()
		if _, err := coreManager.Update(ctx, disabled); err != nil {
			log.WithError(err).Warnf("failed to disable removed config auth %s", disabled.ID)
			continue
		}
		syncConfigDerivedAuthModels(cfg, disabled)
	}
}

func syncConfigDerivedAuthModels(cfg *config.Config, auth *coreauth.Auth) {
	if cfg == nil || auth == nil || strings.TrimSpace(auth.ID) == "" {
		return
	}
	reg := sdkmodelcatalog.GlobalRegistry()
	if reg == nil {
		return
	}
	if auth.Disabled {
		reg.UnregisterClient(auth.ID)
		return
	}
	switch strings.ToLower(strings.TrimSpace(auth.Provider)) {
	case "opencode-go", "cline", "ollama-cloud":
		syncDynamicConfigAuthModels(reg, cfg, auth)
	}
}

func syncDynamicConfigAuthModels(reg sdkmodelcatalog.Registry, cfg *config.Config, auth *coreauth.Auth) {
	provider := strings.ToLower(strings.TrimSpace(auth.Provider))
	staticModels := sdkmodelcatalog.StaticModelDefinitionsByChannel(provider)
	models := staticModels
	excluded := []string(nil)
	ownedBy := provider
	switch provider {
	case "opencode-go":
		ownedBy = "opencode"
		entry := resolveConfigOpenCodeGoKey(cfg, auth)
		if entry == nil {
			reg.UnregisterClient(auth.ID)
			return
		}
		if len(entry.Models) > 0 {
			models = buildNamedConfigModels(filterNamedConfigModels(entry.Models, isNotClinePassConfigModelID), staticModels, ownedBy, provider)
		}
		excluded = providerModelAccessExcludedModels(entry.ExcludedModels)
	case "cline":
		entry := resolveConfigClineKey(cfg, auth)
		if entry == nil {
			reg.UnregisterClient(auth.ID)
			return
		}
		if len(entry.Models) > 0 {
			models = buildNamedConfigModels(filterNamedConfigModels(entry.Models, isClinePassConfigModelID), staticModels, ownedBy, provider)
		}
		excluded = providerModelAccessExcludedModels(entry.ExcludedModels)
	case "ollama-cloud":
		ownedBy = "ollama"
		entry := resolveConfigOllamaCloudKey(cfg, auth)
		if entry == nil {
			reg.UnregisterClient(auth.ID)
			return
		}
		if len(entry.Models) > 0 {
			models = buildNamedConfigModels(filterNamedConfigModels(entry.Models, isNotClinePassConfigModelID), staticModels, ownedBy, provider)
		}
		excluded = providerModelAccessExcludedModels(entry.ExcludedModels)
	default:
		reg.UnregisterClient(auth.ID)
		return
	}
	models = applyConfigModelExclusions(models, excluded)
	if len(models) == 0 {
		reg.UnregisterClient(auth.ID)
		return
	}
	reg.RegisterClient(auth.ID, provider, applyConfigModelPrefixes(models, auth.Prefix, cfg.ForceModelPrefix))
}

func providerModelAccessExcludedModels(excluded []string) []string {
	for _, model := range excluded {
		if strings.TrimSpace(model) == "*" {
			return []string{"*"}
		}
	}
	return nil
}

func resolveConfigOpenCodeGoKey(cfg *config.Config, auth *coreauth.Auth) *config.OpenCodeGoKey {
	if cfg == nil || auth == nil || auth.Attributes == nil {
		return nil
	}
	attrKey := strings.TrimSpace(auth.Attributes["api_key"])
	if attrKey == "" {
		return nil
	}
	for i := range cfg.OpenCodeGoKey {
		if strings.EqualFold(strings.TrimSpace(cfg.OpenCodeGoKey[i].APIKey), attrKey) {
			return &cfg.OpenCodeGoKey[i]
		}
	}
	return nil
}

func resolveConfigClineKey(cfg *config.Config, auth *coreauth.Auth) *config.ClineKey {
	if cfg == nil || auth == nil || auth.Attributes == nil {
		return nil
	}
	attrKey := strings.TrimSpace(auth.Attributes["api_key"])
	attrBase := strings.TrimSpace(auth.Attributes["base_url"])
	for i := range cfg.ClineKey {
		entry := &cfg.ClineKey[i]
		cfgBase := strings.TrimSpace(entry.BaseURL)
		if cfgBase == "" {
			cfgBase = config.DefaultClineBaseURL
		}
		if attrKey != "" && strings.EqualFold(strings.TrimSpace(entry.APIKey), attrKey) {
			if attrBase == "" || strings.EqualFold(cfgBase, attrBase) {
				return entry
			}
		}
	}
	return nil
}

func resolveConfigOllamaCloudKey(cfg *config.Config, auth *coreauth.Auth) *config.OllamaCloudKey {
	if cfg == nil || auth == nil || auth.Attributes == nil {
		return nil
	}
	attrKey := strings.TrimSpace(auth.Attributes["api_key"])
	attrBase := strings.TrimSpace(auth.Attributes["base_url"])
	for i := range cfg.OllamaCloudKey {
		entry := &cfg.OllamaCloudKey[i]
		cfgBase := strings.TrimSpace(entry.BaseURL)
		if cfgBase == "" {
			cfgBase = config.DefaultOllamaCloudBaseURL
		}
		if attrKey != "" && strings.EqualFold(strings.TrimSpace(entry.APIKey), attrKey) {
			if attrBase == "" || strings.EqualFold(cfgBase, attrBase) {
				return entry
			}
		}
	}
	return nil
}

type namedConfigModel interface {
	GetName() string
	GetAlias() string
}

func filterNamedConfigModels[T namedConfigModel](models []T, keep func(string) bool) []T {
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

func isNotClinePassConfigModelID(model string) bool {
	model = strings.TrimSpace(model)
	return model != "" && !isClinePassConfigModelID(model)
}

func buildNamedConfigModels[T namedConfigModel](models []T, staticModels []*sdkmodelcatalog.ModelInfo, ownedBy, modelType string) []*sdkmodelcatalog.ModelInfo {
	staticByID := make(map[string]*sdkmodelcatalog.ModelInfo, len(staticModels))
	for _, model := range staticModels {
		if model == nil {
			continue
		}
		if id := strings.ToLower(strings.TrimSpace(model.ID)); id != "" {
			staticByID[id] = model
		}
	}

	now := time.Now().Unix()
	seen := make(map[string]struct{}, len(models))
	out := make([]*sdkmodelcatalog.ModelInfo, 0, len(models))
	for i := range models {
		name := strings.TrimSpace(models[i].GetName())
		alias := strings.TrimSpace(models[i].GetAlias())
		if name == "" {
			continue
		}
		id := name
		if alias != "" {
			id = alias
		}
		key := strings.ToLower(id)
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		if model := staticByID[strings.ToLower(name)]; model != nil {
			clone := *model
			clone.ID = id
			clone.DisplayName = name
			if alias != "" && !strings.EqualFold(alias, name) {
				clone.UpstreamModelID = name
			}
			clone.UserDefined = true
			out = append(out, &clone)
			continue
		}
		info := &sdkmodelcatalog.ModelInfo{
			ID:          id,
			Object:      "model",
			Created:     now,
			OwnedBy:     ownedBy,
			Type:        modelType,
			DisplayName: name,
			UserDefined: true,
		}
		if alias != "" && !strings.EqualFold(alias, name) {
			info.UpstreamModelID = name
		}
		out = append(out, info)
	}
	return out
}

func applyConfigModelExclusions(models []*sdkmodelcatalog.ModelInfo, excluded []string) []*sdkmodelcatalog.ModelInfo {
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
	out := make([]*sdkmodelcatalog.ModelInfo, 0, len(models))
	for _, model := range models {
		if model == nil {
			continue
		}
		id := strings.ToLower(strings.TrimSpace(model.ID))
		blocked := false
		for _, pattern := range patterns {
			if matchConfigModelPattern(pattern, id) {
				blocked = true
				break
			}
		}
		if !blocked {
			out = append(out, model)
		}
	}
	return out
}

func applyConfigModelPrefixes(models []*sdkmodelcatalog.ModelInfo, prefix string, forceModelPrefix bool) []*sdkmodelcatalog.ModelInfo {
	prefix = strings.TrimSpace(prefix)
	if prefix == "" || len(models) == 0 {
		return models
	}
	out := make([]*sdkmodelcatalog.ModelInfo, 0, len(models)*2)
	seen := make(map[string]struct{}, len(models)*2)
	add := func(model *sdkmodelcatalog.ModelInfo) {
		if model == nil || strings.TrimSpace(model.ID) == "" {
			return
		}
		if _, exists := seen[model.ID]; exists {
			return
		}
		seen[model.ID] = struct{}{}
		out = append(out, model)
	}
	for _, model := range models {
		if !forceModelPrefix {
			add(model)
		}
		if model == nil || strings.TrimSpace(model.ID) == "" {
			continue
		}
		clone := *model
		clone.ID = prefix + "/" + strings.TrimSpace(model.ID)
		add(&clone)
	}
	return out
}

func matchConfigModelPattern(pattern, value string) bool {
	if pattern == "" {
		return false
	}
	if !strings.Contains(pattern, "*") {
		return pattern == value
	}
	parts := strings.Split(pattern, "*")
	if parts[0] != "" && !strings.HasPrefix(value, parts[0]) {
		return false
	}
	if parts[len(parts)-1] != "" && !strings.HasSuffix(value, parts[len(parts)-1]) {
		return false
	}
	offset := len(parts[0])
	for i := 1; i < len(parts)-1; i++ {
		part := parts[i]
		if part == "" {
			continue
		}
		idx := strings.Index(value[offset:], part)
		if idx < 0 {
			return false
		}
		offset += idx + len(part)
	}
	return true
}

func isConfigDerivedAuth(auth *coreauth.Auth) bool {
	if auth == nil || auth.Attributes == nil {
		return false
	}
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(auth.Attributes["source"])), "config:")
}

func FetchAntigravityModels(ctx context.Context, auth *coreauth.Auth, cfg *config.Config) []*sdkmodelcatalog.ModelInfo {
	return executor.FetchAntigravityModels(ctx, auth, cfg)
}

func RegisterExecutorForAuth(coreManager *coreauth.Manager, cfg *config.Config, auth *coreauth.Auth, forceReplace bool, gateway WebsocketGateway) {
	if coreManager == nil || auth == nil {
		return
	}
	if strings.EqualFold(strings.TrimSpace(auth.Provider), "codex") {
		if !forceReplace {
			existingExecutor, hasExecutor := coreManager.Executor("codex")
			if hasExecutor {
				if _, isCodexAutoExecutor := existingExecutor.(*executor.CodexAutoExecutor); isCodexAutoExecutor {
					return
				}
			}
		}
		coreManager.RegisterExecutor(executor.NewCodexAutoExecutor(cfg))
		return
	}
	if auth.Disabled {
		return
	}
	if compatProviderKey, _, isCompat := openAICompatInfoFromAuth(auth); isCompat {
		if compatProviderKey == "" {
			compatProviderKey = strings.ToLower(strings.TrimSpace(auth.Provider))
		}
		if compatProviderKey == "" {
			compatProviderKey = "openai-compatibility"
		}
		coreManager.RegisterExecutor(executor.NewOpenAICompatExecutor(compatProviderKey, cfg))
		return
	}
	switch strings.ToLower(auth.Provider) {
	case "gemini":
		coreManager.RegisterExecutor(executor.NewGeminiExecutor(cfg))
	case "vertex":
		coreManager.RegisterExecutor(executor.NewGeminiVertexExecutor(cfg))
	case "gemini-cli":
		coreManager.RegisterExecutor(executor.NewGeminiCLIExecutor(cfg))
	case "aistudio":
		if gateway != nil {
			relay, _ := gateway.RelayValue().(*wsrelay.Manager)
			if relay != nil {
				coreManager.RegisterExecutor(executor.NewAIStudioExecutor(cfg, auth.ID, relay))
			}
		}
		return
	case "antigravity":
		coreManager.RegisterExecutor(executor.NewAntigravityExecutor(cfg))
	case "claude":
		coreManager.RegisterExecutor(executor.NewClaudeExecutor(cfg))
	case "bedrock":
		coreManager.RegisterExecutor(executor.NewBedrockExecutor(cfg))
	case "opencode-go":
		coreManager.RegisterExecutor(executor.NewOpenCodeGoExecutor(cfg))
	case "ollama-cloud":
		coreManager.RegisterExecutor(executor.NewOllamaCloudExecutor(cfg))
	case "qwen":
		coreManager.RegisterExecutor(executor.NewQwenExecutor(cfg))
	case "iflow":
		coreManager.RegisterExecutor(executor.NewIFlowExecutor(cfg))
	case "kimi":
		coreManager.RegisterExecutor(executor.NewKimiExecutor(cfg))
	default:
		providerKey := strings.ToLower(strings.TrimSpace(auth.Provider))
		if providerKey == "" {
			providerKey = "openai-compatibility"
		}
		coreManager.RegisterExecutor(executor.NewOpenAICompatExecutor(providerKey, cfg))
	}
}

func openAICompatInfoFromAuth(auth *coreauth.Auth) (providerKey string, compatName string, ok bool) {
	if auth == nil {
		return "", "", false
	}
	if len(auth.Attributes) > 0 {
		providerKey = strings.TrimSpace(auth.Attributes["provider_key"])
		compatName = strings.TrimSpace(auth.Attributes["compat_name"])
		if compatName != "" {
			if providerKey == "" {
				providerKey = compatName
			}
			return strings.ToLower(providerKey), compatName, true
		}
	}
	if strings.EqualFold(strings.TrimSpace(auth.Provider), "openai-compatibility") {
		return "openai-compatibility", strings.TrimSpace(auth.Label), true
	}
	return "", "", false
}
