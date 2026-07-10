package usage

import (
	"sync"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/identityfingerprint"
)

var identityFingerprintInvalidationHooks = struct {
	sync.RWMutex
	nextID int
	hooks  map[int]func(identityfingerprint.Provider, string)
}{
	hooks: map[int]func(identityfingerprint.Provider, string){},
}

func RegisterIdentityFingerprintInvalidationHook(hook func(identityfingerprint.Provider, string)) func() {
	if hook == nil {
		return func() {}
	}
	identityFingerprintInvalidationHooks.Lock()
	identityFingerprintInvalidationHooks.nextID++
	id := identityFingerprintInvalidationHooks.nextID
	identityFingerprintInvalidationHooks.hooks[id] = hook
	identityFingerprintInvalidationHooks.Unlock()
	return func() {
		identityFingerprintInvalidationHooks.Lock()
		delete(identityFingerprintInvalidationHooks.hooks, id)
		identityFingerprintInvalidationHooks.Unlock()
	}
}

func notifyIdentityFingerprintInvalidated(provider identityfingerprint.Provider, accountKey string) {
	identityFingerprintInvalidationHooks.RLock()
	hooks := make([]func(identityfingerprint.Provider, string), 0, len(identityFingerprintInvalidationHooks.hooks))
	for _, hook := range identityFingerprintInvalidationHooks.hooks {
		hooks = append(hooks, hook)
	}
	identityFingerprintInvalidationHooks.RUnlock()
	for _, hook := range hooks {
		hook(provider, accountKey)
	}
}
