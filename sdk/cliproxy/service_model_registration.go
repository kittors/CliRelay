package cliproxy

import (
	"context"
	serviceapp "github.com/router-for-me/CLIProxyAPI/v6/sdkbridge/service"
	"strings"

	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	sdkmodelcatalog "github.com/router-for-me/CLIProxyAPI/v6/sdk/modelcatalog"
)

// ModelInfo re-exports the SDK-visible model info structure.
type ModelInfo = sdkmodelcatalog.ModelInfo

// ModelThinkingSupport re-exports the SDK-visible thinking metadata used by helpers.
type ModelThinkingSupport = sdkmodelcatalog.ThinkingSupport

// ModelRegistryHook re-exports the SDK-visible registry hook interface.
type ModelRegistryHook = sdkmodelcatalog.RegistryHook

// ModelRegistry describes registry operations consumed by external callers.
type ModelRegistry = sdkmodelcatalog.Registry

// GlobalModelRegistry returns the shared registry instance.
func GlobalModelRegistry() ModelRegistry {
	return sdkmodelcatalog.GlobalRegistry()
}

func init() {
	coreauth.SetDefaultModelRegistryProvider(func() coreauth.ModelRegistry {
		return GlobalModelRegistry()
	})
}

func lookupStaticModelThinking(name string) *ModelThinkingSupport {
	if strings.TrimSpace(name) == "" {
		return nil
	}
	upstream := sdkmodelcatalog.LookupStaticModelInfo(name)
	if upstream == nil {
		return nil
	}
	return upstream.Thinking
}

// SetGlobalModelRegistryHook registers an optional hook on the shared global registry instance.
func SetGlobalModelRegistryHook(hook ModelRegistryHook) {
	reg := GlobalModelRegistry()
	if reg == nil {
		return
	}
	reg.SetHook(hook)
}

// oauthCatalogScope returns the catalog rows and mapped model owners that make a
// tenant's model-library entries routable for this credential. Both are scoped to
// the credential's own tenant: registration must not leak (or depend on) another
// tenant's library.
func oauthCatalogScope(a *coreauth.Auth) ([]oauthProviderModelConfigRow, []string) {
	if a == nil {
		return nil, nil
	}
	rows := listOAuthProviderModelConfigRowsForTenant(a.TenantID)
	if len(rows) == 0 {
		return nil, nil
	}
	groups := append([]string{a.Provider, a.ChannelName()}, a.ChannelIdentifiers()...)
	return rows, serviceapp.ListModelOwnersForAuthGroupsForTenant(a.TenantID, groups)
}

// registerModelsForAuth (re)binds provider models in the global registry using the core auth ID as client identifier.
func (s *Service) registerModelsForAuth(ctx context.Context, a *coreauth.Auth) {
	if a == nil || a.ID == "" {
		return
	}
	if a.Disabled {
		GlobalModelRegistry().UnregisterClient(a.ID)
		return
	}
	authKind := strings.ToLower(strings.TrimSpace(a.Attributes["auth_kind"]))
	if authKind == "" {
		if kind, _ := a.AccountInfo(); strings.EqualFold(kind, "api_key") {
			authKind = "apikey"
		} else if strings.EqualFold(kind, "oauth") {
			authKind = "oauth"
		}
	}
	if a.Attributes != nil {
		if v := strings.TrimSpace(a.Attributes["gemini_virtual_primary"]); strings.EqualFold(v, "true") {
			GlobalModelRegistry().UnregisterClient(a.ID)
			return
		}
	}
	// Unregister legacy client ID (if present) to avoid double counting
	if a.Runtime != nil {
		if idGetter, ok := a.Runtime.(interface{ GetClientID() string }); ok {
			if rid := idGetter.GetClientID(); rid != "" && rid != a.ID {
				GlobalModelRegistry().UnregisterClient(rid)
			}
		}
	}
	provider := strings.ToLower(strings.TrimSpace(a.Provider))
	compatProviderKey, compatDisplayName, compatDetected := openAICompatInfoFromAuth(a)
	// Cline, Ollama Cloud and Command Code use OpenAI-compatible transport metadata
	// to pick executors, but their model lists are owned by their native config blocks.
	if compatDetected && provider != "cline" && provider != "ollama-cloud" && provider != "commandcode" {
		provider = "openai-compatibility"
	}
	excluded := s.oauthExcludedModels(provider, authKind)
	// The synthesizer pre-merges per-account and global exclusions into the "excluded_models" attribute.
	// If this attribute is present, it represents the complete list of exclusions and overrides the global config.
	if a.Attributes != nil {
		if val, ok := a.Attributes["excluded_models"]; ok && strings.TrimSpace(val) != "" {
			excluded = strings.Split(val, ",")
		}
	}
	var models []*ModelInfo
	switch provider {
	case "gemini":
		models = sdkmodelcatalog.StaticModelDefinitionsByChannel("gemini")
		if entry := s.resolveConfigGeminiKey(a); entry != nil {
			if len(entry.Models) > 0 {
				models = buildGeminiConfigModels(entry, lookupStaticModelThinking)
			}
			if authKind == "apikey" {
				excluded = entry.ExcludedModels
			}
		}
		models = applyExcludedModels(models, excluded)
	case "vertex":
		// Vertex AI Gemini supports the same model identifiers as Gemini.
		models = sdkmodelcatalog.StaticModelDefinitionsByChannel("vertex")
		if authKind == "apikey" {
			if entry := s.resolveConfigVertexCompatKey(a); entry != nil && len(entry.Models) > 0 {
				models = buildVertexCompatConfigModels(entry, lookupStaticModelThinking)
			}
		}
		models = applyExcludedModels(models, excluded)
	case "gemini-cli":
		models = sdkmodelcatalog.StaticModelDefinitionsByChannel("gemini-cli")
		models = applyExcludedModels(models, excluded)
	case "aistudio":
		models = sdkmodelcatalog.StaticModelDefinitionsByChannel("aistudio")
		models = applyExcludedModels(models, excluded)
	case "antigravity":
		models = s.fetchAntigravityRegistryModels(ctx, a, excluded)
	case "claude":
		// Always use the static Claude catalog (+ optional config / OAuth model
		// configs). Live Anthropic /v1/models can return a subset of models and
		// must not replace the full registry list (same regression class as #674 codex).
		models = sdkmodelcatalog.StaticModelDefinitionsByChannel("claude")
		if entry := s.resolveConfigClaudeKey(a); entry != nil {
			if len(entry.Models) > 0 {
				// Explicit config models still win for API-key channels that pin a list.
				models = buildClaudeConfigModels(entry, lookupStaticModelThinking)
			}
			if authKind == "apikey" {
				excluded = entry.ExcludedModels
			}
		}
		catalogRows, mappedOwners := oauthCatalogScope(a)
		models = appendOAuthProviderModelConfigs(models, provider, authKind, catalogRows, mappedOwners)
		models = applyExcludedModels(models, excluded)
	case "bedrock":
		models = sdkmodelcatalog.StaticModelDefinitionsByChannel("bedrock")
		if entry := s.resolveConfigBedrockKey(a); entry != nil {
			if len(entry.Models) > 0 {
				models = buildBedrockConfigModels(entry, lookupStaticModelThinking)
			}
			if authKind == "apikey" {
				excluded = entry.ExcludedModels
			}
		}
		models = applyExcludedModels(models, excluded)
	case "opencode-go":
		models = sdkmodelcatalog.StaticModelDefinitionsByChannel("opencode-go")
		if entry := s.resolveConfigOpenCodeGoKey(a); entry != nil && authKind == "apikey" {
			if len(entry.Models) > 0 {
				models = buildOpenCodeGoConfigModels(entry)
			}
			excluded = providerModelAccessExcludedModels(entry.ExcludedModels)
		}
		models = applyExcludedModels(models, excluded)
	case "cline":
		models = sdkmodelcatalog.StaticModelDefinitionsByChannel("cline")
		if entry := s.resolveConfigClineKey(a); entry != nil && authKind == "apikey" {
			if len(entry.Models) > 0 {
				models = buildClineConfigModels(entry)
			}
			excluded = providerModelAccessExcludedModels(entry.ExcludedModels)
		}
		models = applyExcludedModels(models, excluded)
	case "commandcode":
		models = sdkmodelcatalog.StaticModelDefinitionsByChannel("commandcode")
		if entry := s.resolveConfigCommandCodeKey(a); entry != nil && authKind == "apikey" {
			if len(entry.Models) > 0 {
				models = buildCommandCodeConfigModels(entry)
			}
			excluded = providerModelAccessExcludedModels(entry.ExcludedModels)
		}
		models = applyExcludedModels(models, excluded)
	case "ollama-cloud":
		models = sdkmodelcatalog.StaticModelDefinitionsByChannel("ollama-cloud")
		if entry := s.resolveConfigOllamaCloudKey(a); entry != nil && authKind == "apikey" {
			if len(entry.Models) > 0 {
				models = buildOllamaCloudConfigModels(entry)
			}
			excluded = providerModelAccessExcludedModels(entry.ExcludedModels)
		}
		models = applyExcludedModels(models, excluded)
	case "codex":
		// Always use the static Codex catalog (+ optional config / OAuth model
		// configs). Live ChatGPT manifest returns only a subset of models and
		// must not replace the full registry list (regression from #673).
		models = sdkmodelcatalog.StaticModelDefinitionsByChannel("codex")
		if entry := s.resolveConfigCodexKey(a); entry != nil {
			if len(entry.Models) > 0 {
				models = buildCodexConfigModels(entry, lookupStaticModelThinking)
			}
			if authKind == "apikey" {
				excluded = entry.ExcludedModels
			}
		}
		catalogRows, mappedOwners := oauthCatalogScope(a)
		models = appendOAuthProviderModelConfigs(models, provider, authKind, catalogRows, mappedOwners)
		models = applyExcludedModels(models, excluded)
	case "qwen":
		models = sdkmodelcatalog.StaticModelDefinitionsByChannel("qwen")
		models = applyExcludedModels(models, excluded)
	case "xai":
		// Live xAI discovery returns only the models the account is entitled to
		// today, so a newly released model id is unroutable until we ship a
		// catalog update. Honour the tenant's model library the same way
		// claude/codex do, letting operators add an id and use it immediately.
		models = s.fetchXAIRegistryModels(ctx, a, excluded)
		catalogRows, mappedOwners := oauthCatalogScope(a)
		models = appendOAuthProviderModelConfigs(models, provider, authKind, catalogRows, mappedOwners)
		models = applyExcludedModels(models, excluded)
	case "iflow":
		models = sdkmodelcatalog.StaticModelDefinitionsByChannel("iflow")
		models = applyExcludedModels(models, excluded)
	case "kimi":
		// Live discovery so a model Moonshot ships today is routable today; the
		// tenant model library still supplements it, because the coding gateway
		// lists what this account is entitled to rather than the full catalog.
		models = s.fetchKimiRegistryModels(ctx, a, excluded)
		catalogRows, mappedOwners := oauthCatalogScope(a)
		models = appendOAuthProviderModelConfigs(models, provider, authKind, catalogRows, mappedOwners)
		models = applyExcludedModels(models, excluded)
	default:
		if s.registerOpenAICompatModels(a, provider, compatProviderKey, compatDisplayName, compatDetected) {
			logModelRegistration(a, provider, authKind, "openai-compat", nil)
			return
		}
	}
	models = applyOAuthModelAlias(s.cfg, provider, authKind, models)
	// Applied after every filter: image models are entitlement-universal and never
	// reported by chat discovery, so chat-model curation must not remove them.
	models = serviceapp.WithRegisteredImageModels(provider, models)
	if len(models) > 0 {
		key := provider
		if key == "" {
			key = strings.ToLower(strings.TrimSpace(a.Provider))
		}
		logModelRegistration(a, provider, authKind, "catalog", models)
		GlobalModelRegistry().RegisterClient(a.ID, key, applyModelPrefixes(models, a.Prefix, s.cfg != nil && s.cfg.ForceModelPrefix))
		if provider == "antigravity" {
			s.backfillAntigravityModels(a, models)
		}
		return
	}

	logModelRegistration(a, provider, authKind, "none", nil)
	GlobalModelRegistry().UnregisterClient(a.ID)
}

func (s *Service) refreshRegisteredModels(ctx context.Context) {
	if s == nil || s.coreManager == nil {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	for _, auth := range s.coreManager.List() {
		s.registerModelsForAuth(ctx, auth)
	}
}
