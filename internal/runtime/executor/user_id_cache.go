package executor

import (
	"crypto/sha256"
	"encoding/hex"
	"sync"
	"time"
)

type userIDCacheEntry struct {
	value  string
	expire time.Time
	// sharedAt is when the value was last agreed with the cluster; zero when
	// it never was (see cluster_shared_ids.go).
	sharedAt time.Time
}

var (
	userIDCache            = make(map[string]userIDCacheEntry)
	userIDCacheMu          sync.RWMutex
	userIDCacheCleanupOnce sync.Once
)

const (
	userIDTTL                = time.Hour
	userIDCacheCleanupPeriod = 15 * time.Minute
)

func startUserIDCacheCleanup() {
	go func() {
		ticker := time.NewTicker(userIDCacheCleanupPeriod)
		defer ticker.Stop()
		for range ticker.C {
			purgeExpiredUserIDs()
		}
	}()
}

func purgeExpiredUserIDs() {
	now := time.Now()
	userIDCacheMu.Lock()
	for key, entry := range userIDCache {
		if !entry.expire.After(now) {
			delete(userIDCache, key)
		}
	}
	userIDCacheMu.Unlock()
}

func userIDCacheKey(apiKey string) string {
	sum := sha256.Sum256([]byte(apiKey))
	return hex.EncodeToString(sum[:])
}

func cachedUserID(apiKey string) string {
	if apiKey == "" {
		return generateFakeUserID()
	}

	userIDCacheCleanupOnce.Do(startUserIDCacheCleanup)

	key := userIDCacheKey(apiKey)
	now := time.Now()

	userIDCacheMu.RLock()
	entry, ok := userIDCache[key]
	valid := ok && entry.value != "" && entry.expire.After(now) && isValidUserID(entry.value)
	userIDCacheMu.RUnlock()
	if valid {
		userIDCacheMu.Lock()
		entry = userIDCache[key]
		if entry.value != "" && entry.expire.After(now) && isValidUserID(entry.value) {
			entry.expire = now.Add(userIDTTL)
			refresh := userIDNeedsSharedRefresh(&entry, now)
			userIDCache[key] = entry
			userIDCacheMu.Unlock()
			if refresh {
				go refreshSharedUserID(key, entry.value)
			}
			return entry.value
		}
		userIDCacheMu.Unlock()
	}

	newID := generateFakeUserID()
	var sharedAt time.Time
	if shared, ok := mintSharedUserID(key, newID); ok {
		newID, sharedAt = shared, now
	}

	userIDCacheMu.Lock()
	entry, ok = userIDCache[key]
	if !ok || entry.value == "" || !entry.expire.After(now) || !isValidUserID(entry.value) {
		entry.value = newID
		entry.sharedAt = sharedAt
	}
	entry.expire = now.Add(userIDTTL)
	userIDCache[key] = entry
	userIDCacheMu.Unlock()
	return entry.value
}
