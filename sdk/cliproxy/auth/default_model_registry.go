package auth

import (
	"sync"

	sdkmodelcatalog "github.com/router-for-me/CLIProxyAPI/v8/sdk/modelcatalog"
)

var (
	defaultModelRegistryProviderMu sync.RWMutex
	defaultModelRegistryProvider   func() ModelRegistry
)

func init() {
	SetDefaultModelRegistryProvider(func() ModelRegistry {
		return sdkmodelcatalog.GlobalRegistry()
	})
}

// SetDefaultModelRegistryProvider registers the runtime-owned default model registry accessor.
func SetDefaultModelRegistryProvider(provider func() ModelRegistry) {
	defaultModelRegistryProviderMu.Lock()
	defaultModelRegistryProvider = provider
	defaultModelRegistryProviderMu.Unlock()
}

// DefaultModelRegistry returns the shared default model registry when one is available.
func DefaultModelRegistry() ModelRegistry {
	defaultModelRegistryProviderMu.RLock()
	provider := defaultModelRegistryProvider
	defaultModelRegistryProviderMu.RUnlock()
	if provider == nil {
		return nil
	}
	return provider()
}

// AttachDefaultModelRegistry attaches the shared default model registry when the
// manager does not already have one.
func AttachDefaultModelRegistry(manager *Manager) {
	if manager == nil {
		return
	}
	manager.mu.RLock()
	existing := manager.modelRegistry
	manager.mu.RUnlock()
	if existing != nil {
		return
	}
	if registry := DefaultModelRegistry(); registry != nil {
		manager.SetModelRegistry(registry)
	}
}
