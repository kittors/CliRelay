package registry

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

//go:embed models/codex_client_models.json
var codexClientModelsJSON []byte

// OpenAIImageModelType is the public model type used by Codex clients for
// image-generation models.
const OpenAIImageModelType = "image"

// ModelsRefreshInterval is the shared cadence for refreshing live model
// catalogs and entitlement caches.
const ModelsRefreshInterval = 15 * time.Minute

// NativeCapabilityRoute describes one registered route to a public model.
type NativeCapabilityRoute struct {
	Provider           string
	NativeCapabilities *NativeCapabilities
}

// ResolveResponsesWebSearchCapability conservatively resolves native web search
// across every route that can serve a public model.
func ResolveResponsesWebSearchCapability(routes []NativeCapabilityRoute) *bool {
	if len(routes) == 0 {
		return nil
	}
	hasUnknown := false
	for _, route := range routes {
		if route.NativeCapabilities != nil && route.NativeCapabilities.WebSearch != nil && !*route.NativeCapabilities.WebSearch {
			return boolPointer(false)
		}
		support := responsesWebSearchProviderPathSupport(route.Provider)
		if support == nil {
			hasUnknown = true
			continue
		}
		if !*support {
			return boolPointer(false)
		}
		if route.NativeCapabilities == nil || route.NativeCapabilities.WebSearch == nil {
			hasUnknown = true
		}
	}
	if hasUnknown {
		return nil
	}
	return boolPointer(true)
}

func responsesWebSearchProviderPathSupport(provider string) *bool {
	switch provider = strings.ToLower(strings.TrimSpace(provider)); provider {
	case "codex", "xai", "claude":
		return boolPointer(true)
	case "openai", "openai-compatibility", "gemini", "aistudio", "vertex", "antigravity", "kimi", "interactions", "gemini-interactions":
		return boolPointer(false)
	default:
		if strings.HasPrefix(provider, "openai-compatible-") {
			return boolPointer(false)
		}
		return nil
	}
}

func boolPointer(value bool) *bool { return &value }

// GetResponsesWebSearchCapability resolves capability metadata across every
// registered client route for the exact public model ID.
func (r *ModelRegistry) GetResponsesWebSearchCapability(modelID string) *bool {
	modelID = strings.TrimSpace(modelID)
	if r == nil || modelID == "" {
		return nil
	}
	r.mutex.RLock()
	defer r.mutex.RUnlock()
	routes := make([]NativeCapabilityRoute, 0)
	for clientID, modelIDs := range r.clientModels {
		for _, registeredID := range modelIDs {
			if strings.TrimSpace(registeredID) != modelID {
				continue
			}
			var native *NativeCapabilities
			if info := r.clientModelInfos[clientID][registeredID]; info != nil {
				native = info.NativeCapabilities
			}
			routes = append(routes, NativeCapabilityRoute{Provider: r.clientProviders[clientID], NativeCapabilities: native})
		}
	}
	return ResolveResponsesWebSearchCapability(routes)
}

// GetCodexClientModelsSnapshot returns the optional Codex catalog snapshot and
// its revision. The v6 registry has no external snapshot store; an empty
// snapshot lets v8 callers fall back to built-in templates.
func GetCodexClientModelsSnapshot() ([]byte, uint64) {
	return append([]byte(nil), codexClientModelsJSON...), 1
}

// GetCodexClientModelsRevision returns the revision of the embedded catalog.
func GetCodexClientModelsRevision() uint64 { return 1 }

// GetGeneration returns a monotonically increasing registry generation.
func (r *ModelRegistry) GetGeneration() uint64 {
	if r == nil || r.mutex == nil {
		return 0
	}
	r.mutex.RLock()
	defer r.mutex.RUnlock()
	return r.generation
}

// ValidateCodexClientModelsJSON validates the shape used by the v8 catalog
// refresh command without requiring the full v8 catalog store.
func ValidateCodexClientModelsJSON(raw []byte) error {
	var payload struct {
		Models []json.RawMessage `json:"models"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return err
	}
	if payload.Models == nil {
		return fmt.Errorf("models field is missing")
	}
	return nil
}

// CodexClientModelUsesResponsesLite reports whether a Codex client catalog
// entry requests the Responses-Lite transport.
func CodexClientModelUsesResponsesLite(modelID string) bool {
	modelID = strings.ToLower(strings.TrimSpace(modelID))
	if modelID == "" {
		return false
	}
	var payload struct {
		Models []struct {
			Slug             string `json:"slug"`
			UseResponsesLite bool   `json:"use_responses_lite"`
		} `json:"models"`
	}
	if err := json.Unmarshal(codexClientModelsJSON, &payload); err != nil {
		return false
	}
	for _, model := range payload.Models {
		if strings.EqualFold(strings.TrimSpace(model.Slug), modelID) {
			return model.UseResponsesLite
		}
	}
	return false
}

// ModelOverrideHeaders returns model-specific outbound header overrides when
// available. Overrides live on the dynamic registry entry so executor behavior
// follows the same model registration snapshot used for routing.
func ModelOverrideHeaders(modelID string, _ ...string) map[string]string {
	modelID = strings.TrimSpace(modelID)
	if modelID == "" {
		return nil
	}
	reg := GetGlobalRegistry()
	if reg == nil {
		return nil
	}
	reg.mutex.RLock()
	defer reg.mutex.RUnlock()

	var info *ModelInfo
	if registration := reg.models[modelID]; registration != nil {
		info = registration.Info
	} else {
		for registeredID, registration := range reg.models {
			if strings.EqualFold(strings.TrimSpace(registeredID), modelID) && registration != nil {
				info = registration.Info
				break
			}
		}
	}
	if info == nil || info.Config == nil || len(info.Config.OverrideHeader) == 0 {
		return nil
	}
	out := make(map[string]string, len(info.Config.OverrideHeader))
	for key, value := range info.Config.OverrideHeader {
		out[key] = value
	}
	return out
}

// ClientModelProjection describes a desired client model availability state.
type ClientModelProjection struct {
	ModelID       string
	Suspended     bool
	SuspendReason string
	SuspendUntil  time.Time
	QuotaExceeded bool
}
