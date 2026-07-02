package registry

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// Registry query contract:
// - Owner: model availability / provider capability read boundary.
// - Responsibility: compute public availability snapshots, provider views, and lookup helpers.
// - Non-goals: client registration reconciliation and quota/suspension mutation.

type ModelClientSource struct {
	ClientID        string `json:"client_id"`
	Provider        string `json:"provider"`
	ModelID         string `json:"model_id"`
	UpstreamModelID string `json:"upstream_model_id,omitempty"`
}

// GetAvailableModels returns all models that have at least one available client.
func (r *ModelRegistry) GetAvailableModels(handlerType string) []map[string]any {
	r.mutex.RLock()
	defer r.mutex.RUnlock()

	models := make([]map[string]any, 0)
	quotaExpiredDuration := 5 * time.Minute

	for _, registration := range r.models {
		availableClients := registration.Count
		now := time.Now()

		expiredClients := 0
		for _, quotaTime := range registration.QuotaExceededClients {
			if quotaTime != nil && now.Sub(*quotaTime) < quotaExpiredDuration {
				expiredClients++
			}
		}

		cooldownSuspended := 0
		otherSuspended := 0
		if registration.SuspendedClients != nil {
			for _, reason := range registration.SuspendedClients {
				if strings.EqualFold(reason, "quota") {
					cooldownSuspended++
					continue
				}
				otherSuspended++
			}
		}

		effectiveClients := availableClients - expiredClients - otherSuspended
		if effectiveClients < 0 {
			effectiveClients = 0
		}

		if effectiveClients > 0 || (availableClients > 0 && (expiredClients > 0 || cooldownSuspended > 0) && otherSuspended == 0) {
			model := r.convertModelToMap(registration.Info, handlerType)
			if model != nil {
				models = append(models, model)
			}
		}
	}

	return models
}

// GetAvailableModelsByProvider returns models available for the given provider identifier.
func (r *ModelRegistry) GetAvailableModelsByProvider(provider string) []*ModelInfo {
	provider = strings.ToLower(strings.TrimSpace(provider))
	if provider == "" {
		return nil
	}

	r.mutex.RLock()
	defer r.mutex.RUnlock()

	type providerModel struct {
		count int
		info  *ModelInfo
	}

	providerModels := make(map[string]*providerModel)
	for clientID, clientProvider := range r.clientProviders {
		if clientProvider != provider {
			continue
		}
		modelIDs := r.clientModels[clientID]
		if len(modelIDs) == 0 {
			continue
		}
		clientInfos := r.clientModelInfos[clientID]
		for _, modelID := range modelIDs {
			modelID = strings.TrimSpace(modelID)
			if modelID == "" {
				continue
			}
			entry := providerModels[modelID]
			if entry == nil {
				entry = &providerModel{}
				providerModels[modelID] = entry
			}
			entry.count++
			if entry.info == nil {
				if clientInfos != nil {
					if info := clientInfos[modelID]; info != nil {
						entry.info = info
					}
				}
				if entry.info == nil {
					if reg, ok := r.models[modelID]; ok && reg != nil && reg.Info != nil {
						entry.info = reg.Info
					}
				}
			}
		}
	}

	if len(providerModels) == 0 {
		return nil
	}

	quotaExpiredDuration := 5 * time.Minute
	now := time.Now()
	result := make([]*ModelInfo, 0, len(providerModels))

	for modelID, entry := range providerModels {
		if entry == nil || entry.count <= 0 {
			continue
		}
		registration, ok := r.models[modelID]

		expiredClients := 0
		cooldownSuspended := 0
		otherSuspended := 0
		if ok && registration != nil {
			if registration.QuotaExceededClients != nil {
				for clientID, quotaTime := range registration.QuotaExceededClients {
					if clientID == "" {
						continue
					}
					if p, okProvider := r.clientProviders[clientID]; !okProvider || p != provider {
						continue
					}
					if quotaTime != nil && now.Sub(*quotaTime) < quotaExpiredDuration {
						expiredClients++
					}
				}
			}
			if registration.SuspendedClients != nil {
				for clientID, reason := range registration.SuspendedClients {
					if clientID == "" {
						continue
					}
					if p, okProvider := r.clientProviders[clientID]; !okProvider || p != provider {
						continue
					}
					if strings.EqualFold(reason, "quota") {
						cooldownSuspended++
						continue
					}
					otherSuspended++
				}
			}
		}

		availableClients := entry.count
		effectiveClients := availableClients - expiredClients - otherSuspended
		if effectiveClients < 0 {
			effectiveClients = 0
		}

		if effectiveClients > 0 || (availableClients > 0 && (expiredClients > 0 || cooldownSuspended > 0) && otherSuspended == 0) {
			if entry.info != nil {
				result = append(result, entry.info)
				continue
			}
			if ok && registration != nil && registration.Info != nil {
				result = append(result, registration.Info)
			}
		}
	}

	return result
}

// GetModelCount returns the number of available clients for a specific model.
func (r *ModelRegistry) GetModelCount(modelID string) int {
	r.mutex.RLock()
	defer r.mutex.RUnlock()

	if registration, exists := r.models[modelID]; exists {
		now := time.Now()
		quotaExpiredDuration := 5 * time.Minute

		expiredClients := 0
		for _, quotaTime := range registration.QuotaExceededClients {
			if quotaTime != nil && now.Sub(*quotaTime) < quotaExpiredDuration {
				expiredClients++
			}
		}
		suspendedClients := 0
		if registration.SuspendedClients != nil {
			suspendedClients = len(registration.SuspendedClients)
		}
		result := registration.Count - expiredClients - suspendedClients
		if result < 0 {
			return 0
		}
		return result
	}
	return 0
}

// GetModelProviders returns provider identifiers that currently supply the given model.
func (r *ModelRegistry) GetModelProviders(modelID string) []string {
	r.mutex.RLock()
	defer r.mutex.RUnlock()

	registration, exists := r.models[modelID]
	if !exists || registration == nil || len(registration.Providers) == 0 {
		return nil
	}

	type providerCount struct {
		name  string
		count int
	}
	providers := make([]providerCount, 0, len(registration.Providers))
	for name, count := range registration.Providers {
		if count <= 0 {
			continue
		}
		providers = append(providers, providerCount{name: name, count: count})
	}
	if len(providers) == 0 {
		return nil
	}

	sort.Slice(providers, func(i, j int) bool {
		if providers[i].count == providers[j].count {
			return providers[i].name < providers[j].name
		}
		return providers[i].count > providers[j].count
	})

	result := make([]string, 0, len(providers))
	for _, item := range providers {
		result = append(result, item.name)
	}
	return result
}

func (r *ModelRegistry) GetModelClientSources(modelID string) []ModelClientSource {
	modelID = strings.TrimSpace(modelID)
	if modelID == "" {
		return nil
	}
	target := strings.ToLower(modelID)

	r.mutex.RLock()
	defer r.mutex.RUnlock()

	out := make([]ModelClientSource, 0)
	seen := make(map[string]struct{})
	for clientID, modelIDs := range r.clientModels {
		if strings.TrimSpace(clientID) == "" {
			continue
		}
		provider := r.clientProviders[clientID]
		for _, registeredID := range modelIDs {
			registeredID = strings.TrimSpace(registeredID)
			if registeredID == "" || strings.ToLower(registeredID) != target {
				continue
			}
			upstreamModelID := ""
			if clientInfos := r.clientModelInfos[clientID]; clientInfos != nil {
				if info := clientInfos[registeredID]; info != nil {
					upstreamModelID = strings.TrimSpace(info.UpstreamModelID)
				}
			}
			key := clientID + "\x00" + provider + "\x00" + registeredID + "\x00" + upstreamModelID
			if _, exists := seen[key]; exists {
				continue
			}
			seen[key] = struct{}{}
			out = append(out, ModelClientSource{
				ClientID:        clientID,
				Provider:        provider,
				ModelID:         registeredID,
				UpstreamModelID: upstreamModelID,
			})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Provider == out[j].Provider {
			return out[i].ClientID < out[j].ClientID
		}
		return out[i].Provider < out[j].Provider
	})
	return out
}

// GetModelInfo returns ModelInfo, prioritizing provider-specific definition if available.
func (r *ModelRegistry) GetModelInfo(modelID, provider string) *ModelInfo {
	r.mutex.RLock()
	defer r.mutex.RUnlock()
	if reg, ok := r.models[modelID]; ok && reg != nil {
		if provider != "" && reg.InfoByProvider != nil {
			if reg.Providers != nil {
				if count, ok := reg.Providers[provider]; ok && count > 0 {
					if info, ok := reg.InfoByProvider[provider]; ok && info != nil {
						return info
					}
				}
			}
		}
		return reg.Info
	}
	return nil
}

func (r *ModelRegistry) convertModelToMap(model *ModelInfo, handlerType string) map[string]any {
	if model == nil {
		return nil
	}

	switch handlerType {
	case "openai":
		result := map[string]any{
			"id":       model.ID,
			"object":   "model",
			"owned_by": model.OwnedBy,
		}
		if model.Created > 0 {
			result["created"] = model.Created
		}
		if model.Type != "" {
			result["type"] = model.Type
		}
		if model.DisplayName != "" {
			result["display_name"] = model.DisplayName
		}
		if model.Version != "" {
			result["version"] = model.Version
		}
		if model.Description != "" {
			result["description"] = model.Description
		}
		if model.ContextLength > 0 {
			result["context_length"] = model.ContextLength
		}
		if model.MaxCompletionTokens > 0 {
			result["max_completion_tokens"] = model.MaxCompletionTokens
		}
		if len(model.SupportedParameters) > 0 {
			result["supported_parameters"] = model.SupportedParameters
		}
		return result

	case "claude":
		result := map[string]any{
			"id":       model.ID,
			"object":   "model",
			"owned_by": model.OwnedBy,
		}
		if model.Created > 0 {
			result["created_at"] = model.Created
		}
		if model.Type != "" {
			result["type"] = "model"
		}
		if model.DisplayName != "" {
			result["display_name"] = model.DisplayName
		}
		return result

	case "gemini":
		result := map[string]any{}
		if model.Name != "" {
			result["name"] = model.Name
		} else {
			result["name"] = model.ID
		}
		if model.Version != "" {
			result["version"] = model.Version
		}
		if model.DisplayName != "" {
			result["displayName"] = model.DisplayName
		}
		if model.Description != "" {
			result["description"] = model.Description
		}
		if model.InputTokenLimit > 0 {
			result["inputTokenLimit"] = model.InputTokenLimit
		}
		if model.OutputTokenLimit > 0 {
			result["outputTokenLimit"] = model.OutputTokenLimit
		}
		if len(model.SupportedGenerationMethods) > 0 {
			result["supportedGenerationMethods"] = model.SupportedGenerationMethods
		}
		return result

	default:
		result := map[string]any{
			"id":     model.ID,
			"object": "model",
		}
		if model.OwnedBy != "" {
			result["owned_by"] = model.OwnedBy
		}
		if model.Type != "" {
			result["type"] = model.Type
		}
		if model.Created != 0 {
			result["created"] = model.Created
		}
		return result
	}
}

// GetFirstAvailableModel returns the first available model for the given handler type.
func (r *ModelRegistry) GetFirstAvailableModel(handlerType string) (string, error) {
	r.mutex.RLock()
	defer r.mutex.RUnlock()

	models := r.GetAvailableModels(handlerType)
	if len(models) == 0 {
		return "", fmt.Errorf("no models available for handler type: %s", handlerType)
	}

	sort.Slice(models, func(i, j int) bool {
		createdI, okI := models[i]["created"].(int64)
		createdJ, okJ := models[j]["created"].(int64)
		if !okI || !okJ {
			return false
		}
		return createdI > createdJ
	})

	for _, model := range models {
		if modelID, ok := model["id"].(string); ok {
			if count := r.GetModelCount(modelID); count > 0 {
				return modelID, nil
			}
		}
	}

	return "", fmt.Errorf("no available clients for any model in handler type: %s", handlerType)
}

// GetModelsForClient returns the models registered for a specific client.
func (r *ModelRegistry) GetModelsForClient(clientID string) []*ModelInfo {
	r.mutex.RLock()
	defer r.mutex.RUnlock()

	modelIDs, exists := r.clientModels[clientID]
	if !exists || len(modelIDs) == 0 {
		return nil
	}

	clientInfos := r.clientModelInfos[clientID]
	seen := make(map[string]struct{})
	result := make([]*ModelInfo, 0, len(modelIDs))
	for _, modelID := range modelIDs {
		if _, dup := seen[modelID]; dup {
			continue
		}
		seen[modelID] = struct{}{}

		if clientInfos != nil {
			if info, ok := clientInfos[modelID]; ok && info != nil {
				result = append(result, info)
				continue
			}
		}
		if reg, ok := r.models[modelID]; ok && reg.Info != nil {
			result = append(result, reg.Info)
		}
	}
	return result
}
