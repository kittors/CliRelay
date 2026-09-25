package executor

import (
	"sync"
	"time"
)

type codexCache struct {
	ID     string
	Expire time.Time
	// Shared marks an id the cluster agreed on (cluster_shared_ids.go). Ids
	// minted outside a cluster, or while its store was unreachable, are not.
	Shared bool
}

// codexCacheMap stores prompt cache IDs keyed by model+user_id.
// Protected by codexCacheMu. Entries expire after 1 hour.
var (
	codexCacheMap = make(map[string]codexCache)
	codexCacheMu  sync.RWMutex
)

// codexCacheCleanupInterval controls how often expired entries are purged.
const codexCacheCleanupInterval = 15 * time.Minute

// codexCacheCleanupOnce ensures the background cleanup goroutine starts only once.
var codexCacheCleanupOnce sync.Once

// startCodexCacheCleanup launches a background goroutine that periodically
// removes expired entries from codexCacheMap to prevent memory leaks.
func startCodexCacheCleanup() {
	go func() {
		ticker := time.NewTicker(codexCacheCleanupInterval)
		defer ticker.Stop()
		for range ticker.C {
			purgeExpiredCodexCache()
		}
	}()
}

// purgeExpiredCodexCache removes entries that have expired.
func purgeExpiredCodexCache() {
	now := time.Now()
	codexCacheMu.Lock()
	defer codexCacheMu.Unlock()
	for key, cache := range codexCacheMap {
		if cache.Expire.Before(now) {
			delete(codexCacheMap, key)
		}
	}
}

// getCodexCache retrieves a cached entry, returning ok=false if not found or expired.
func getCodexCache(key string) (codexCache, bool) {
	codexCacheCleanupOnce.Do(startCodexCacheCleanup)
	codexCacheMu.RLock()
	cache, ok := codexCacheMap[key]
	codexCacheMu.RUnlock()
	if !ok || cache.Expire.Before(time.Now()) {
		return codexCache{}, false
	}
	return cache, true
}

// getOrCreateCodexCacheID returns the id bound to key, minting one under the
// write lock when there is none.
//
// A separate get-then-set lets two concurrent turns of the same conversation
// both miss and mint different ids, and the loser's turn then carries a cache
// key upstream has never seen. Deciding inside the lock is what makes the id
// per-conversation rather than per-request.
func getOrCreateCodexCacheID(key string, ttl time.Duration, newID func() string) string {
	if cache, ok := getCodexCache(key); ok {
		if cache.Shared {
			return cache.ID
		}
		return adoptSharedCodexID(key, cache)
	}
	if id, ok := mintSharedCodexID(key, ttl, newID); ok {
		return id
	}
	codexCacheMu.Lock()
	defer codexCacheMu.Unlock()
	if cache, ok := codexCacheMap[key]; ok && cache.Expire.After(time.Now()) {
		return cache.ID
	}
	id := newID()
	codexCacheMap[key] = codexCache{ID: id, Expire: time.Now().Add(ttl)}
	return id
}
