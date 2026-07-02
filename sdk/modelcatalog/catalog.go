package modelcatalog

import (
	"context"
	"strings"
	"sync"

	log "github.com/sirupsen/logrus"
)

// ModelInfo represents SDK-visible model metadata.
type ModelInfo struct {
	ID                         string           `json:"id"`
	Object                     string           `json:"object"`
	Created                    int64            `json:"created"`
	OwnedBy                    string           `json:"owned_by"`
	Type                       string           `json:"type"`
	DisplayName                string           `json:"display_name,omitempty"`
	UpstreamModelID            string           `json:"upstream_model_id,omitempty"`
	Name                       string           `json:"name,omitempty"`
	Version                    string           `json:"version,omitempty"`
	Description                string           `json:"description,omitempty"`
	InputTokenLimit            int              `json:"inputTokenLimit,omitempty"`
	OutputTokenLimit           int              `json:"outputTokenLimit,omitempty"`
	SupportedGenerationMethods []string         `json:"supportedGenerationMethods,omitempty"`
	ContextLength              int              `json:"context_length,omitempty"`
	MaxCompletionTokens        int              `json:"max_completion_tokens,omitempty"`
	SupportedParameters        []string         `json:"supported_parameters,omitempty"`
	Thinking                   *ThinkingSupport `json:"thinking,omitempty"`
	UserDefined                bool             `json:"-"`
}

// ThinkingSupport describes a model family's supported reasoning budget range.
type ThinkingSupport struct {
	Min            int      `json:"min,omitempty"`
	Max            int      `json:"max,omitempty"`
	ZeroAllowed    bool     `json:"zero_allowed,omitempty"`
	DynamicAllowed bool     `json:"dynamic_allowed,omitempty"`
	Levels         []string `json:"levels,omitempty"`
}

// RegistryHook observes shared model registry changes.
type RegistryHook interface {
	OnModelsRegistered(ctx context.Context, provider, clientID string, models []*ModelInfo)
	OnModelsUnregistered(ctx context.Context, provider, clientID string)
}

// Registry describes the SDK-visible model registry surface.
type Registry interface {
	RegisterClient(clientID, clientProvider string, models []*ModelInfo)
	UnregisterClient(clientID string)
	SetModelQuotaExceeded(clientID, modelID string)
	SuspendClientModel(clientID, modelID string, reason string)
	ResumeClientModel(clientID, modelID string)
	ClearModelQuotaExceeded(clientID, modelID string)
	ClientSupportsModel(clientID, modelID string) bool
	GetAvailableModels(handlerType string) []map[string]any
	GetAvailableModelsByProvider(provider string) []*ModelInfo
	GetModelProviders(modelID string) []string
	GetFirstAvailableModel(handlerType string) (string, error)
	GetModelsForClient(clientID string) []*ModelInfo
	SetHook(RegistryHook)
}

// StaticCatalog exposes read-only static model definitions.
type StaticCatalog interface {
	StaticModelDefinitionsByChannel(channel string) []*ModelInfo
	LookupStaticModelInfo(modelID string) *ModelInfo
}

var (
	globalRegistryProviderMu sync.RWMutex
	globalRegistryProvider   func() Registry

	staticCatalogProviderMu sync.RWMutex
	staticCatalogProvider   func() StaticCatalog
)

// SetGlobalRegistryProvider registers the runtime-owned global model registry accessor.
func SetGlobalRegistryProvider(provider func() Registry) {
	globalRegistryProviderMu.Lock()
	globalRegistryProvider = provider
	globalRegistryProviderMu.Unlock()
}

// SetStaticCatalogProvider registers the runtime-owned static model catalog accessor.
func SetStaticCatalogProvider(provider func() StaticCatalog) {
	staticCatalogProviderMu.Lock()
	staticCatalogProvider = provider
	staticCatalogProviderMu.Unlock()
}

// GlobalRegistry returns the shared runtime model registry when one is available.
func GlobalRegistry() Registry {
	globalRegistryProviderMu.RLock()
	provider := globalRegistryProvider
	globalRegistryProviderMu.RUnlock()
	if provider == nil {
		return nil
	}
	return provider()
}

func globalStaticCatalog() StaticCatalog {
	staticCatalogProviderMu.RLock()
	provider := staticCatalogProvider
	staticCatalogProviderMu.RUnlock()
	if provider == nil {
		return nil
	}
	return provider()
}

// AvailableModels returns handler-visible model metadata from the shared registry.
func AvailableModels(handlerType string) []map[string]any {
	registryRef := GlobalRegistry()
	if registryRef == nil {
		return nil
	}
	return registryRef.GetAvailableModels(handlerType)
}

// GetProviderName determines all providers currently capable of serving a model.
func GetProviderName(modelName string) []string {
	modelName = strings.TrimSpace(modelName)
	if modelName == "" {
		return nil
	}
	registryRef := GlobalRegistry()
	if registryRef == nil {
		return nil
	}
	providers := registryRef.GetModelProviders(modelName)
	if len(providers) == 0 {
		return nil
	}
	out := make([]string, 0, len(providers))
	seen := make(map[string]struct{}, len(providers))
	for _, provider := range providers {
		trimmed := strings.TrimSpace(provider)
		if trimmed == "" {
			continue
		}
		if _, exists := seen[trimmed]; exists {
			continue
		}
		seen[trimmed] = struct{}{}
		out = append(out, trimmed)
	}
	return out
}

// ResolveAutoModel resolves the public "auto" model alias via the shared registry.
func ResolveAutoModel(modelName string) string {
	if strings.TrimSpace(modelName) != "auto" {
		return modelName
	}
	registryRef := GlobalRegistry()
	if registryRef == nil {
		return modelName
	}
	firstModel, err := registryRef.GetFirstAvailableModel("")
	if err != nil {
		log.Warnf("Failed to resolve 'auto' model: %v, falling back to original model name", err)
		return modelName
	}
	log.Infof("Resolved 'auto' model to: %s", firstModel)
	return firstModel
}

// LookupStaticModelInfo resolves SDK-visible static model metadata.
func LookupStaticModelInfo(modelID string) *ModelInfo {
	catalog := globalStaticCatalog()
	if catalog == nil {
		return nil
	}
	return catalog.LookupStaticModelInfo(modelID)
}

// StaticModelDefinitionsByChannel returns SDK-visible static model definitions for a provider/channel.
func StaticModelDefinitionsByChannel(channel string) []*ModelInfo {
	catalog := globalStaticCatalog()
	if catalog == nil {
		return nil
	}
	return catalog.StaticModelDefinitionsByChannel(channel)
}
