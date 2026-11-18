// Package registry provides centralized model management for all AI service providers.
// It implements a dynamic model registry with reference counting to track active clients
// and automatically hide models when no clients are available or when quota is exceeded.
package registry

import (
	"context"
	"strings"
	"sync"
	"time"

	misc "github.com/router-for-me/CLIProxyAPI/v8/internal/misc"
	log "github.com/sirupsen/logrus"
)

// ModelInfo represents information about an available model
type ModelInfo struct {
	// ID is the unique identifier for the model
	ID string `json:"id"`
	// Object type for the model (typically "model")
	Object string `json:"object"`
	// Created timestamp when the model was created
	Created int64 `json:"created"`
	// OwnedBy indicates the organization that owns the model
	OwnedBy string `json:"owned_by"`
	// Type indicates the model type (e.g., "claude", "gemini", "openai")
	Type string `json:"type"`
	// DisplayName is the human-readable name for the model
	DisplayName string `json:"display_name,omitempty"`
	// UpstreamModelID is the provider-native model ID used after alias resolution.
	UpstreamModelID string `json:"upstream_model_id,omitempty"`
	// MetadataModelID is the catalog metadata/template id used by clients.
	MetadataModelID string `json:"metadata_model_id,omitempty"`
	// Name is used for Gemini-style model names
	Name string `json:"name,omitempty"`
	// Version is the model version
	Version string `json:"version,omitempty"`
	// Description provides detailed information about the model
	Description string `json:"description,omitempty"`
	// InputTokenLimit is the maximum input token limit
	InputTokenLimit int `json:"inputTokenLimit,omitempty"`
	// OutputTokenLimit is the maximum output token limit
	OutputTokenLimit int `json:"outputTokenLimit,omitempty"`
	// SupportedGenerationMethods lists supported generation methods
	SupportedGenerationMethods []string `json:"supportedGenerationMethods,omitempty"`
	// ContextLength is the context window size
	ContextLength    int `json:"context_length,omitempty"`
	MaxContextLength int `json:"max_context_length,omitempty"`
	// MaxCompletionTokens is the maximum completion tokens
	MaxCompletionTokens int `json:"max_completion_tokens,omitempty"`
	// SupportedParameters lists supported parameters
	SupportedParameters []string `json:"supported_parameters,omitempty"`
	// SupportedInputModalities describes explicit input attachment support.
	SupportedInputModalities  []string `json:"supported_input_modalities,omitempty"`
	SupportedOutputModalities []string `json:"supported_output_modalities,omitempty"`
	ExplicitInputModalities   bool     `json:"explicit_input_modalities,omitempty"`
	ExplicitThinking          bool     `json:"explicit_thinking,omitempty"`
	// SupportConfigurationUpdate reports internal support for configuration_update.
	SupportConfigurationUpdate bool                `json:"-"`
	SupportsWebSearch          bool                `json:"supports_web_search,omitempty"`
	NativeCapabilities         *NativeCapabilities `json:"native_capabilities,omitempty"`

	// Thinking holds provider-specific reasoning/thinking budget capabilities.
	// This is optional and currently used for Gemini thinking budget normalization.
	Thinking *ThinkingSupport `json:"thinking,omitempty"`

	// Config holds optional model-specific runtime overrides.
	Config *ModelConfig `json:"config,omitempty"`

	// ClaudeRequest lists the request fields a Claude catalog model rejects.
	// Only static catalog entries set it; executors look it up by upstream
	// name with LookupClaudeModelInfo, so copies made for the dynamic registry
	// do not need to carry it.
	ClaudeRequest *ClaudeRequestRules `json:"-"`

	// UserDefined indicates this model was defined through config file's models[]
	// array (e.g., openai-compatibility.*.models[], *-api-key.models[]).
	// UserDefined models have thinking configuration passed through without validation.
	UserDefined bool `json:"-"`
	// IsCompat enables compatibility handling for configured API-key models.
	IsCompat bool `json:"-"`
}

type NativeCapabilities struct {
	WebSearch  *bool `json:"web_search,omitempty"`
	ToolSearch *bool `json:"tool_search,omitempty"`
}

// ModelConfig holds optional runtime overrides for a model definition.
type ModelConfig struct {
	OverrideHeader map[string]string `json:"override_header,omitempty"`
}

// ThinkingSupport describes a model family's supported internal reasoning budget range.
// Values are interpreted in provider-native token units.
type ThinkingSupport struct {
	// Min is the minimum allowed thinking budget (inclusive).
	Min int `json:"min,omitempty"`
	// Max is the maximum allowed thinking budget (inclusive).
	Max int `json:"max,omitempty"`
	// ZeroAllowed indicates whether 0 is a valid value (to disable thinking).
	ZeroAllowed bool `json:"zero_allowed,omitempty"`
	// DynamicAllowed indicates whether -1 is a valid value (dynamic thinking budget).
	DynamicAllowed bool `json:"dynamic_allowed,omitempty"`
	// Levels defines discrete reasoning effort levels (e.g., "low", "medium", "high").
	// When set, the model uses level-based reasoning instead of token budgets.
	Levels []string `json:"levels,omitempty"`
	// Adaptive is set for Claude models that only take adaptive thinking with
	// output_config.effort (Opus 4.7 and later) and says how each one turns
	// thinking off. Nil for every other model.
	Adaptive    *AdaptiveThinking `json:"adaptive,omitempty"`
	DisableType string            `json:"disable_type,omitempty"`
}

// ModelRegistration tracks a model's availability
type ModelRegistration struct {
	// Info contains the model metadata
	Info *ModelInfo
	// InfoByProvider maps provider identifiers to specific ModelInfo to support differing capabilities.
	InfoByProvider map[string]*ModelInfo
	// Count is the number of active clients that can provide this model
	Count int
	// LastUpdated tracks when this registration was last modified
	LastUpdated time.Time
	// QuotaExceededClients tracks which clients have exceeded quota for this model
	QuotaExceededClients map[string]*time.Time
	// Providers tracks available clients grouped by provider identifier
	Providers map[string]int
	// SuspendedClients tracks temporarily disabled clients keyed by client ID
	SuspendedClients map[string]string
	// SuspendedClientDeadlines records optional expiry deadlines for suspensions.
	SuspendedClientDeadlines map[string]time.Time
}

// ModelRegistryHook provides optional callbacks for external integrations to track model list changes.
// Hook implementations must be non-blocking and resilient; calls are executed asynchronously and panics are recovered.
type ModelRegistryHook interface {
	OnModelsRegistered(ctx context.Context, provider, clientID string, models []*ModelInfo)
	OnModelsUnregistered(ctx context.Context, provider, clientID string)
}

// ModelRegistry manages the global registry of available models
type ModelRegistry struct {
	// models maps model ID to registration information
	models map[string]*ModelRegistration
	// clientModels maps client ID to the models it provides
	clientModels map[string][]string
	// clientModelInfos maps client ID to a map of model ID -> ModelInfo
	// This preserves the original model info provided by each client
	clientModelInfos map[string]map[string]*ModelInfo
	// clientProviders maps client ID to its provider identifier
	clientProviders map[string]string
	// clientEpochs tracks registration epochs for each client ID.
	clientEpochs map[string]uint64
	// clientGenerations tracks the latest projection generation for each client.
	clientGenerations map[string]uint64
	// mutex ensures thread-safe access to the registry
	mutex *sync.RWMutex
	// hook is an optional callback sink for model registration changes
	hook       ModelRegistryHook
	generation uint64
}

// Global model registry instance
var globalRegistry *ModelRegistry
var registryOnce sync.Once

// GetGlobalRegistry returns the global model registry instance
func GetGlobalRegistry() *ModelRegistry {
	registryOnce.Do(func() {
		globalRegistry = &ModelRegistry{
			models:            make(map[string]*ModelRegistration),
			clientModels:      make(map[string][]string),
			clientModelInfos:  make(map[string]map[string]*ModelInfo),
			clientProviders:   make(map[string]string),
			clientEpochs:      make(map[string]uint64),
			clientGenerations: make(map[string]uint64),
			mutex:             &sync.RWMutex{},
		}
	})
	return globalRegistry
}

// RegistrationEpoch returns a monotonic registry generation suitable for
// invalidating scheduler snapshots after model registration changes.
func (r *ModelRegistry) RegistrationEpoch() uint64 {
	if r == nil || r.mutex == nil {
		return 0
	}
	r.mutex.RLock()
	defer r.mutex.RUnlock()
	return r.generation
}

// LookupModelInfo searches dynamic registry (provider-specific > global) then static definitions.
func LookupModelInfo(modelID string, provider ...string) *ModelInfo {
	modelID = strings.TrimSpace(modelID)
	if modelID == "" {
		return nil
	}

	p := ""
	if len(provider) > 0 {
		p = strings.ToLower(strings.TrimSpace(provider[0]))
	}

	if info := GetGlobalRegistry().GetModelInfo(modelID, p); info != nil {
		return info
	}
	return LookupStaticModelInfo(modelID)
}

// SetHook sets an optional hook for observing model registration changes.
func (r *ModelRegistry) SetHook(hook ModelRegistryHook) {
	if r == nil {
		return
	}
	r.mutex.Lock()
	defer r.mutex.Unlock()
	r.hook = hook
}

const defaultModelRegistryHookTimeout = 5 * time.Second

func (r *ModelRegistry) triggerModelsRegistered(provider, clientID string, models []*ModelInfo) {
	hook := r.hook
	if hook == nil {
		return
	}
	modelsCopy := cloneModelInfosUnique(models)
	// Registry hooks are intentionally detached from caller mutation paths.
	// Each invocation owns its own timeout-bounded context and self-cleans on return.
	go func() {
		defer func() {
			if recovered := recover(); recovered != nil {
				log.Errorf("model registry hook OnModelsRegistered panic: %v", recovered)
			}
		}()
		ctx, cancel := context.WithTimeout(context.Background(), defaultModelRegistryHookTimeout)
		defer cancel()
		hook.OnModelsRegistered(ctx, provider, clientID, modelsCopy)
	}()
}

func (r *ModelRegistry) triggerModelsUnregistered(provider, clientID string) {
	hook := r.hook
	if hook == nil {
		return
	}
	// Registry hooks are intentionally detached from caller mutation paths.
	// Each invocation owns its own timeout-bounded context and self-cleans on return.
	go func() {
		defer func() {
			if recovered := recover(); recovered != nil {
				log.Errorf("model registry hook OnModelsUnregistered panic: %v", recovered)
			}
		}()
		ctx, cancel := context.WithTimeout(context.Background(), defaultModelRegistryHookTimeout)
		defer cancel()
		hook.OnModelsUnregistered(ctx, provider, clientID)
	}()
}

// RegisterClient registers a client and its supported models
// Parameters:
//   - clientID: Unique identifier for the client
//   - clientProvider: Provider name (e.g., "gemini", "claude", "openai")
//   - models: List of models that this client can provide
func (r *ModelRegistry) RegisterClient(clientID, clientProvider string, models []*ModelInfo) {
	r.mutex.Lock()
	defer r.mutex.Unlock()
	r.generation++
	if r.clientEpochs == nil {
		r.clientEpochs = make(map[string]uint64)
	}
	r.clientEpochs[clientID]++

	provider := strings.ToLower(clientProvider)
	uniqueModelIDs := make([]string, 0, len(models))
	rawModelIDs := make([]string, 0, len(models))
	newModels := make(map[string]*ModelInfo, len(models))
	newCounts := make(map[string]int, len(models))
	for _, model := range models {
		if model == nil || model.ID == "" {
			continue
		}
		rawModelIDs = append(rawModelIDs, model.ID)
		newCounts[model.ID]++
		if _, exists := newModels[model.ID]; exists {
			continue
		}
		newModels[model.ID] = model
		uniqueModelIDs = append(uniqueModelIDs, model.ID)
	}

	if len(uniqueModelIDs) == 0 {
		// No models supplied; unregister existing client state if present.
		r.unregisterClientInternal(clientID)
		delete(r.clientModels, clientID)
		delete(r.clientModelInfos, clientID)
		delete(r.clientProviders, clientID)
		misc.LogCredentialSeparator()
		return
	}

	now := time.Now()

	oldModels, hadExisting := r.clientModels[clientID]
	oldProvider := r.clientProviders[clientID]
	providerChanged := oldProvider != provider
	if !hadExisting {
		// Pure addition path.
		for _, modelID := range rawModelIDs {
			model := newModels[modelID]
			r.addModelRegistration(modelID, provider, model, now)
		}
		r.clientModels[clientID] = append([]string(nil), rawModelIDs...)
		// Store client's own model infos
		clientInfos := make(map[string]*ModelInfo, len(newModels))
		for id, m := range newModels {
			clientInfos[id] = cloneModelInfo(m)
		}
		r.clientModelInfos[clientID] = clientInfos
		if provider != "" {
			r.clientProviders[clientID] = provider
		} else {
			delete(r.clientProviders, clientID)
		}
		r.triggerModelsRegistered(provider, clientID, models)
		log.Debugf("Registered client %s from provider %s with %d models", clientID, clientProvider, len(rawModelIDs))
		misc.LogCredentialSeparator()
		return
	}

	oldCounts := make(map[string]int, len(oldModels))
	for _, id := range oldModels {
		oldCounts[id]++
	}

	added := make([]string, 0)
	for _, id := range uniqueModelIDs {
		if oldCounts[id] == 0 {
			added = append(added, id)
		}
	}

	removed := make([]string, 0)
	for id := range oldCounts {
		if newCounts[id] == 0 {
			removed = append(removed, id)
		}
	}

	// Handle provider change for overlapping models before modifications.
	if providerChanged && oldProvider != "" {
		for id, newCount := range newCounts {
			if newCount == 0 {
				continue
			}
			oldCount := oldCounts[id]
			if oldCount == 0 {
				continue
			}
			toRemove := newCount
			if oldCount < toRemove {
				toRemove = oldCount
			}
			if reg, ok := r.models[id]; ok && reg.Providers != nil {
				if count, okProv := reg.Providers[oldProvider]; okProv {
					if count <= toRemove {
						delete(reg.Providers, oldProvider)
						if reg.InfoByProvider != nil {
							delete(reg.InfoByProvider, oldProvider)
						}
					} else {
						reg.Providers[oldProvider] = count - toRemove
					}
				}
			}
		}
	}

	// Apply removals first to keep counters accurate.
	for _, id := range removed {
		oldCount := oldCounts[id]
		for i := 0; i < oldCount; i++ {
			r.removeModelRegistration(clientID, id, oldProvider, now)
		}
	}

	for id, oldCount := range oldCounts {
		newCount := newCounts[id]
		if newCount == 0 || oldCount <= newCount {
			continue
		}
		overage := oldCount - newCount
		for i := 0; i < overage; i++ {
			r.removeModelRegistration(clientID, id, oldProvider, now)
		}
	}

	// Apply additions.
	for id, newCount := range newCounts {
		oldCount := oldCounts[id]
		if newCount <= oldCount {
			continue
		}
		model := newModels[id]
		diff := newCount - oldCount
		for i := 0; i < diff; i++ {
			r.addModelRegistration(id, provider, model, now)
		}
	}

	// Update metadata for models that remain associated with the client.
	addedSet := make(map[string]struct{}, len(added))
	for _, id := range added {
		addedSet[id] = struct{}{}
	}
	for _, id := range uniqueModelIDs {
		model := newModels[id]
		if reg, ok := r.models[id]; ok {
			reg.Info = cloneModelInfo(model)
			if provider != "" {
				if reg.InfoByProvider == nil {
					reg.InfoByProvider = make(map[string]*ModelInfo)
				}
				reg.InfoByProvider[provider] = cloneModelInfo(model)
			}
			reg.LastUpdated = now
			if providerChanged && provider != "" {
				if _, newlyAdded := addedSet[id]; newlyAdded {
					continue
				}
				overlapCount := newCounts[id]
				if oldCount := oldCounts[id]; oldCount < overlapCount {
					overlapCount = oldCount
				}
				if overlapCount <= 0 {
					continue
				}
				if reg.Providers == nil {
					reg.Providers = make(map[string]int)
				}
				reg.Providers[provider] += overlapCount
			}
		}
	}

	// Update client bookkeeping.
	if len(rawModelIDs) > 0 {
		r.clientModels[clientID] = append([]string(nil), rawModelIDs...)
	}
	// Update client's own model infos
	clientInfos := make(map[string]*ModelInfo, len(newModels))
	for id, m := range newModels {
		clientInfos[id] = cloneModelInfo(m)
	}
	r.clientModelInfos[clientID] = clientInfos
	if provider != "" {
		r.clientProviders[clientID] = provider
	} else {
		delete(r.clientProviders, clientID)
	}

	r.triggerModelsRegistered(provider, clientID, models)
	if len(added) == 0 && len(removed) == 0 && !providerChanged {
		// Only metadata (e.g., display name) changed; skip separator when no log output.
		return
	}

	log.Debugf("Reconciled client %s (provider %s) models: +%d, -%d", clientID, provider, len(added), len(removed))
	misc.LogCredentialSeparator()
}

// ReplaceClientModels conditionally replaces one client's models when its
// registration epoch still matches.
func (r *ModelRegistry) ReplaceClientModels(clientID, provider string, expectedEpoch uint64, models []*ModelInfo) (uint64, bool) {
	if r == nil || strings.TrimSpace(clientID) == "" {
		return 0, false
	}
	r.mutex.RLock()
	current := r.clientEpochs[clientID]
	r.mutex.RUnlock()
	if current != expectedEpoch {
		return current, false
	}
	r.RegisterClient(clientID, provider, models)
	return r.ClientRegistrationEpoch(clientID), true
}

func (r *ModelRegistry) RegisterClientIfEpochCurrent(clientID, provider string, expectedEpoch uint64, models []*ModelInfo, publish func() bool) bool {
	if r == nil {
		return false
	}
	r.mutex.RLock()
	current := r.clientEpochs[clientID]
	r.mutex.RUnlock()
	if current != expectedEpoch {
		return false
	}
	if publish != nil && !publish() {
		return false
	}
	r.RegisterClient(clientID, provider, models)
	return true
}

func (r *ModelRegistry) UnregisterClientIfEpochCurrent(clientID string, expectedEpoch uint64, publish func() bool) bool {
	if r == nil {
		return false
	}
	r.mutex.RLock()
	current := r.clientEpochs[clientID]
	r.mutex.RUnlock()
	if current != expectedEpoch {
		return false
	}
	if publish != nil && !publish() {
		return false
	}
	r.UnregisterClient(clientID)
	return true
}

func (r *ModelRegistry) addModelRegistration(modelID, provider string, model *ModelInfo, now time.Time) {
	if model == nil || modelID == "" {
		return
	}
	if existing, exists := r.models[modelID]; exists {
		existing.Count++
		existing.LastUpdated = now
		existing.Info = cloneModelInfo(model)
		if existing.SuspendedClients == nil {
			existing.SuspendedClients = make(map[string]string)
		}
		if existing.SuspendedClientDeadlines == nil {
			existing.SuspendedClientDeadlines = make(map[string]time.Time)
		}
		if existing.InfoByProvider == nil {
			existing.InfoByProvider = make(map[string]*ModelInfo)
		}
		if provider != "" {
			if existing.Providers == nil {
				existing.Providers = make(map[string]int)
			}
			existing.Providers[provider]++
			existing.InfoByProvider[provider] = cloneModelInfo(model)
		}
		log.Debugf("Incremented count for model %s, now %d clients", modelID, existing.Count)
		return
	}

	registration := &ModelRegistration{
		Info:                     cloneModelInfo(model),
		InfoByProvider:           make(map[string]*ModelInfo),
		Count:                    1,
		LastUpdated:              now,
		QuotaExceededClients:     make(map[string]*time.Time),
		SuspendedClients:         make(map[string]string),
		SuspendedClientDeadlines: make(map[string]time.Time),
	}
	if provider != "" {
		registration.Providers = map[string]int{provider: 1}
		registration.InfoByProvider[provider] = cloneModelInfo(model)
	}
	r.models[modelID] = registration
	log.Debugf("Registered new model %s from provider %s", modelID, provider)
}

func (r *ModelRegistry) removeModelRegistration(clientID, modelID, provider string, now time.Time) {
	registration, exists := r.models[modelID]
	if !exists {
		return
	}
	registration.Count--
	registration.LastUpdated = now
	if registration.QuotaExceededClients != nil {
		delete(registration.QuotaExceededClients, clientID)
	}
	if registration.SuspendedClients != nil {
		delete(registration.SuspendedClients, clientID)
	}
	if registration.Count < 0 {
		registration.Count = 0
	}
	if provider != "" && registration.Providers != nil {
		if count, ok := registration.Providers[provider]; ok {
			if count <= 1 {
				delete(registration.Providers, provider)
				if registration.InfoByProvider != nil {
					delete(registration.InfoByProvider, provider)
				}
			} else {
				registration.Providers[provider] = count - 1
			}
		}
	}
	log.Debugf("Decremented count for model %s, now %d clients", modelID, registration.Count)
	if registration.Count <= 0 {
		delete(r.models, modelID)
		log.Debugf("Removed model %s as no clients remain", modelID)
	}
}

func cloneModelInfo(model *ModelInfo) *ModelInfo {
	if model == nil {
		return nil
	}
	copyModel := *model
	if len(model.SupportedGenerationMethods) > 0 {
		copyModel.SupportedGenerationMethods = append([]string(nil), model.SupportedGenerationMethods...)
	}
	if len(model.SupportedParameters) > 0 {
		copyModel.SupportedParameters = append([]string(nil), model.SupportedParameters...)
	}
	if len(model.SupportedInputModalities) > 0 {
		copyModel.SupportedInputModalities = append([]string(nil), model.SupportedInputModalities...)
	}
	if len(model.SupportedOutputModalities) > 0 {
		copyModel.SupportedOutputModalities = append([]string(nil), model.SupportedOutputModalities...)
	}
	return &copyModel
}

func cloneModelInfosUnique(models []*ModelInfo) []*ModelInfo {
	if len(models) == 0 {
		return nil
	}
	cloned := make([]*ModelInfo, 0, len(models))
	seen := make(map[string]struct{}, len(models))
	for _, model := range models {
		if model == nil || model.ID == "" {
			continue
		}
		if _, exists := seen[model.ID]; exists {
			continue
		}
		seen[model.ID] = struct{}{}
		cloned = append(cloned, cloneModelInfo(model))
	}
	return cloned
}

// UnregisterClient removes a client and decrements counts for its models
// Parameters:
//   - clientID: Unique identifier for the client to remove
func (r *ModelRegistry) UnregisterClient(clientID string) {
	r.mutex.Lock()
	defer r.mutex.Unlock()
	r.generation++
	r.unregisterClientInternal(clientID)
}

// unregisterClientInternal performs the actual client unregistration (internal, no locking)
func (r *ModelRegistry) unregisterClientInternal(clientID string) {
	models, exists := r.clientModels[clientID]
	provider, hasProvider := r.clientProviders[clientID]
	if !exists {
		if hasProvider {
			delete(r.clientProviders, clientID)
		}
		return
	}

	now := time.Now()
	for _, modelID := range models {
		if registration, isExists := r.models[modelID]; isExists {
			registration.Count--
			registration.LastUpdated = now

			// Remove quota tracking for this client
			delete(registration.QuotaExceededClients, clientID)
			if registration.SuspendedClients != nil {
				delete(registration.SuspendedClients, clientID)
			}

			if hasProvider && registration.Providers != nil {
				if count, ok := registration.Providers[provider]; ok {
					if count <= 1 {
						delete(registration.Providers, provider)
						if registration.InfoByProvider != nil {
							delete(registration.InfoByProvider, provider)
						}
					} else {
						registration.Providers[provider] = count - 1
					}
				}
			}

			log.Debugf("Decremented count for model %s, now %d clients", modelID, registration.Count)

			// Remove model if no clients remain
			if registration.Count <= 0 {
				delete(r.models, modelID)
				log.Debugf("Removed model %s as no clients remain", modelID)
			}
		}
	}

	delete(r.clientModels, clientID)
	delete(r.clientModelInfos, clientID)
	if hasProvider {
		delete(r.clientProviders, clientID)
	}
	log.Debugf("Unregistered client %s", clientID)
	// Separator line after completing client unregistration (after the summary line)
	misc.LogCredentialSeparator()
	r.triggerModelsUnregistered(provider, clientID)
}

// SetModelQuotaExceeded marks a model as quota exceeded for a specific client
// Parameters:
//   - clientID: The client that exceeded quota
//   - modelID: The model that exceeded quota
func (r *ModelRegistry) SetModelQuotaExceeded(clientID, modelID string) {
	r.mutex.Lock()
	defer r.mutex.Unlock()

	if registration, exists := r.models[modelID]; exists {
		registration.QuotaExceededClients[clientID] = new(time.Now())
		log.Debugf("Marked model %s as quota exceeded for client %s", modelID, clientID)
	}
}

// ClearModelQuotaExceeded removes quota exceeded status for a model and client
// Parameters:
//   - clientID: The client to clear quota status for
//   - modelID: The model to clear quota status for
func (r *ModelRegistry) ClearModelQuotaExceeded(clientID, modelID string) {
	r.mutex.Lock()
	defer r.mutex.Unlock()

	if registration, exists := r.models[modelID]; exists {
		delete(registration.QuotaExceededClients, clientID)
		// log.Debugf("Cleared quota exceeded status for model %s and client %s", modelID, clientID)
	}
}

// SuspendClientModel marks a client's model as temporarily unavailable until explicitly resumed.
// Parameters:
//   - clientID: The client to suspend
//   - modelID: The model affected by the suspension
//   - reason: Optional description for observability
func (r *ModelRegistry) SuspendClientModel(clientID, modelID, reason string) {
	if clientID == "" || modelID == "" {
		return
	}
	r.mutex.Lock()
	defer r.mutex.Unlock()

	registration, exists := r.models[modelID]
	if !exists || registration == nil {
		return
	}
	if registration.SuspendedClients == nil {
		registration.SuspendedClients = make(map[string]string)
	}
	if _, already := registration.SuspendedClients[clientID]; already {
		return
	}
	registration.SuspendedClients[clientID] = reason
	registration.LastUpdated = time.Now()
	if reason != "" {
		log.Debugf("Suspended client %s for model %s: %s", clientID, modelID, reason)
	} else {
		log.Debugf("Suspended client %s for model %s", clientID, modelID)
	}
}

// ResumeClientModel clears a previous suspension so the client counts toward availability again.
// Parameters:
//   - clientID: The client to resume
//   - modelID: The model being resumed
func (r *ModelRegistry) ResumeClientModel(clientID, modelID string) {
	if clientID == "" || modelID == "" {
		return
	}
	r.mutex.Lock()
	defer r.mutex.Unlock()

	registration, exists := r.models[modelID]
	if !exists || registration == nil || registration.SuspendedClients == nil {
		return
	}
	if _, ok := registration.SuspendedClients[clientID]; !ok {
		return
	}
	delete(registration.SuspendedClients, clientID)
	registration.LastUpdated = time.Now()
	log.Debugf("Resumed client %s for model %s", clientID, modelID)
}

// ClientSupportsModel reports whether the client registered support for modelID.
func (r *ModelRegistry) ClientSupportsModel(clientID, modelID string) bool {
	clientID = strings.TrimSpace(clientID)
	modelID = strings.TrimSpace(modelID)
	if clientID == "" || modelID == "" {
		return false
	}

	r.mutex.RLock()
	defer r.mutex.RUnlock()

	models, exists := r.clientModels[clientID]
	if !exists || len(models) == 0 {
		return false
	}

	for _, id := range models {
		if strings.EqualFold(strings.TrimSpace(id), modelID) {
			return true
		}
		if equivalentVideoModelID(id, modelID) {
			return true
		}
	}

	return false
}

func equivalentVideoModelID(registered, requested string) bool {
	registered = strings.ToLower(strings.TrimSpace(registered))
	requested = strings.ToLower(strings.TrimSpace(requested))
	for _, prefix := range []string{"xai/", "x-ai/", "grok/"} {
		registered = strings.TrimPrefix(registered, prefix)
		requested = strings.TrimPrefix(requested, prefix)
	}
	if (registered == "grok-imagine-video-1.5" && requested == "grok-imagine-video-1.5-preview") ||
		(registered == "grok-imagine-video-1.5-preview" && requested == "grok-imagine-video-1.5") {
		return true
	}
	return false
}

// IsModelSuspendedForClient reports whether a model is currently suspended for a client.
func (r *ModelRegistry) IsModelSuspendedForClient(clientID, modelID string) bool {
	clientID = strings.TrimSpace(clientID)
	modelID = strings.TrimSpace(modelID)
	if clientID == "" || modelID == "" {
		return false
	}
	r.mutex.RLock()
	defer r.mutex.RUnlock()
	registration, exists := r.models[modelID]
	if !exists || registration == nil {
		return false
	}
	_, _, active := clientSuspensionActive(registration, clientID, time.Now())
	return active
}

// IsModelQuotaExceededForClient reports whether a model is marked quota
// exceeded for a specific client.
func (r *ModelRegistry) IsModelQuotaExceededForClient(clientID, modelID string) bool {
	clientID = strings.TrimSpace(clientID)
	modelID = strings.TrimSpace(modelID)
	if clientID == "" || modelID == "" {
		return false
	}
	r.mutex.RLock()
	defer r.mutex.RUnlock()
	registration, exists := r.models[modelID]
	if !exists || registration == nil || registration.QuotaExceededClients == nil {
		return false
	}
	_, exceeded := registration.QuotaExceededClients[clientID]
	return exceeded
}

func clientSuspensionActive(registration *ModelRegistration, clientID string, now time.Time) (string, time.Time, bool) {
	if registration == nil || len(registration.SuspendedClients) == 0 {
		return "", time.Time{}, false
	}
	reason, ok := registration.SuspendedClients[clientID]
	if !ok {
		return "", time.Time{}, false
	}
	if deadline, hasDeadline := registration.SuspendedClientDeadlines[clientID]; hasDeadline && !deadline.IsZero() {
		if !now.Before(deadline) {
			return "", time.Time{}, false
		}
		return reason, deadline, true
	}
	return reason, time.Time{}, true
}

// GetModelsAndEpochForClient atomically returns the models registered for a
// client and the current registry generation.
func (r *ModelRegistry) GetModelsAndEpochForClient(clientID string) ([]*ModelInfo, uint64) {
	if r == nil {
		return nil, 0
	}
	r.mutex.RLock()
	defer r.mutex.RUnlock()
	modelIDs := r.clientModels[clientID]
	if len(modelIDs) == 0 {
		return nil, r.generation
	}
	clientInfos := r.clientModelInfos[clientID]
	result := make([]*ModelInfo, 0, len(modelIDs))
	seen := make(map[string]struct{}, len(modelIDs))
	for _, modelID := range modelIDs {
		if _, exists := seen[modelID]; exists {
			continue
		}
		seen[modelID] = struct{}{}
		if info := clientInfos[modelID]; info != nil {
			result = append(result, cloneModelInfo(info))
			continue
		}
		if registration := r.models[modelID]; registration != nil && registration.Info != nil {
			result = append(result, cloneModelInfo(registration.Info))
		}
	}
	return result, r.generation
}

// ClientRegistrationEpoch returns the current registration epoch for clientID.
func (r *ModelRegistry) ClientRegistrationEpoch(clientID string) uint64 {
	if r == nil {
		return 0
	}
	r.mutex.RLock()
	defer r.mutex.RUnlock()
	return r.clientEpochs[clientID]
}

// ApplyClientModelProjections atomically applies suspension and quota state for
// models owned by a registered client when the supplied epoch and generation
// are current.
func (r *ModelRegistry) ApplyClientModelProjections(clientID string, epoch, generation uint64, projections []ClientModelProjection) bool {
	if r == nil || strings.TrimSpace(clientID) == "" || len(projections) == 0 {
		return false
	}
	r.mutex.Lock()
	defer r.mutex.Unlock()
	if r.clientEpochs[clientID] != epoch || generation < r.clientGenerations[clientID] {
		return false
	}
	registered := make(map[string]struct{})
	for _, id := range r.clientModels[clientID] {
		registered[strings.TrimSpace(id)] = struct{}{}
	}
	if len(registered) == 0 {
		return false
	}
	valid := false
	for _, projection := range projections {
		id := strings.TrimSpace(projection.ModelID)
		if _, owned := registered[id]; owned && r.models[id] != nil {
			valid = true
			break
		}
	}
	if !valid {
		return false
	}
	if r.clientGenerations == nil {
		r.clientGenerations = make(map[string]uint64)
	}
	r.clientGenerations[clientID] = generation
	now := time.Now()
	for _, projection := range projections {
		id := strings.TrimSpace(projection.ModelID)
		if _, owned := registered[id]; !owned {
			continue
		}
		registration := r.models[id]
		if registration == nil {
			continue
		}
		if registration.SuspendedClients == nil {
			registration.SuspendedClients = make(map[string]string)
		}
		if registration.SuspendedClientDeadlines == nil {
			registration.SuspendedClientDeadlines = make(map[string]time.Time)
		}
		if projection.Suspended {
			registration.SuspendedClients[clientID] = projection.SuspendReason
			if projection.SuspendUntil.IsZero() {
				delete(registration.SuspendedClientDeadlines, clientID)
			} else {
				registration.SuspendedClientDeadlines[clientID] = projection.SuspendUntil
			}
		} else {
			delete(registration.SuspendedClients, clientID)
			delete(registration.SuspendedClientDeadlines, clientID)
		}
		if registration.QuotaExceededClients == nil {
			registration.QuotaExceededClients = make(map[string]*time.Time)
		}
		if projection.QuotaExceeded {
			timestamp := now
			registration.QuotaExceededClients[clientID] = &timestamp
		} else {
			delete(registration.QuotaExceededClients, clientID)
		}
		registration.LastUpdated = now
	}
	return true
}

// ApplyClientModelCapabilities mutates capability metadata for models owned by
// a client when the supplied registration epoch is current.
func (r *ModelRegistry) ApplyClientModelCapabilities(clientID string, expectedEpoch uint64, mutate func(modelID string, info *ModelInfo)) bool {
	if r == nil || strings.TrimSpace(clientID) == "" || mutate == nil {
		return false
	}
	r.mutex.Lock()
	defer r.mutex.Unlock()
	if r.clientEpochs[clientID] != expectedEpoch {
		return false
	}
	infos := r.clientModelInfos[clientID]
	if len(infos) == 0 {
		return false
	}
	provider := r.clientProviders[clientID]
	changed := false
	for id, info := range infos {
		if info == nil {
			continue
		}
		mutate(id, info)
		if registration := r.models[id]; registration != nil {
			registration.Info = cloneModelInfo(info)
			if provider != "" {
				if registration.InfoByProvider == nil {
					registration.InfoByProvider = make(map[string]*ModelInfo)
				}
				registration.InfoByProvider[provider] = cloneModelInfo(info)
			}
			registration.LastUpdated = time.Now()
		}
		changed = true
	}
	if changed {
		r.generation++
	}
	return changed
}

// CleanupExpiredQuotas removes expired quota tracking entries
func (r *ModelRegistry) CleanupExpiredQuotas() {
	r.mutex.Lock()
	defer r.mutex.Unlock()

	now := time.Now()
	quotaExpiredDuration := 5 * time.Minute

	for modelID, registration := range r.models {
		for clientID, quotaTime := range registration.QuotaExceededClients {
			if quotaTime != nil && now.Sub(*quotaTime) >= quotaExpiredDuration {
				delete(registration.QuotaExceededClients, clientID)
				log.Debugf("Cleaned up expired quota tracking for model %s, client %s", modelID, clientID)
			}
		}
	}
}
