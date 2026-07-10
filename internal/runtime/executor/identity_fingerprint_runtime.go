package executor

import (
	"context"
	"fmt"
	"hash/fnv"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/identityfingerprint"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/usage"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

const (
	identityFingerprintReadCacheTTL  = 30 * time.Second
	identityFingerprintWriteCacheTTL = 5 * time.Minute
)

var (
	runtimeIdentityFingerprintStoreFuncMu sync.RWMutex
	runtimeGetIdentityFingerprint         = usage.GetIdentityFingerprint
	runtimeObserveIdentityFingerprint     = usage.ObserveIdentityFingerprint
)

func callRuntimeGetIdentityFingerprint(provider identityfingerprint.Provider, accountKey string) (*identityfingerprint.LearnedRecord, error) {
	runtimeIdentityFingerprintStoreFuncMu.RLock()
	fn := runtimeGetIdentityFingerprint
	runtimeIdentityFingerprintStoreFuncMu.RUnlock()
	return fn(provider, accountKey)
}

func callRuntimeObserveIdentityFingerprint(input identityfingerprint.LearnInput) (*identityfingerprint.LearnedRecord, identityfingerprint.MergeResult, error) {
	runtimeIdentityFingerprintStoreFuncMu.RLock()
	fn := runtimeObserveIdentityFingerprint
	runtimeIdentityFingerprintStoreFuncMu.RUnlock()
	return fn(input)
}

type identityFingerprintCacheEntry struct {
	record    *identityfingerprint.LearnedRecord
	signature string
	expiresAt time.Time
}

var runtimeIdentityFingerprintCache = struct {
	sync.Mutex
	records map[string]identityFingerprintCacheEntry
}{
	records: map[string]identityFingerprintCacheEntry{},
}

var runtimeIdentityFingerprintAsync = struct {
	sync.Mutex
	loads    map[string]struct{}
	persists map[string]struct{}
}{
	loads:    map[string]struct{}{},
	persists: map[string]struct{}{},
}

func init() {
	usage.RegisterIdentityFingerprintInvalidationHook(func(provider identityfingerprint.Provider, accountKey string) {
		invalidateCachedRuntimeIdentityFingerprint(provider, accountKey)
	})
}

func identityFingerprintHeadersFromContext(ctx context.Context) http.Header {
	ginCtx := ginContextFrom(ctx)
	if ginCtx == nil || ginCtx.Request == nil {
		return nil
	}
	return ginCtx.Request.Header
}

func identityFingerprintAccount(auth *cliproxyauth.Auth) (accountKey string, authSubjectID string) {
	identity := usage.ResolveAuthSubjectIdentity(auth)
	if identity != nil {
		return strings.TrimSpace(identity.ID), strings.TrimSpace(identity.ID)
	}
	if auth != nil {
		if id := strings.TrimSpace(auth.ID); id != "" {
			return id, ""
		}
		if idx := strings.TrimSpace(auth.EnsureIndex()); idx != "" {
			return idx, ""
		}
	}
	return "", ""
}

func observeRuntimeIdentityFingerprint(provider identityfingerprint.Provider, auth *cliproxyauth.Auth, ctx context.Context) *identityfingerprint.LearnedRecord {
	accountKey, authSubjectID := identityFingerprintAccount(auth)
	if accountKey == "" {
		return nil
	}
	headers := identityFingerprintHeadersFromContext(ctx)
	if len(headers) == 0 {
		if record := getCachedRuntimeIdentityFingerprint(provider, accountKey, "", ""); record != nil {
			return record
		}
		scheduleRuntimeIdentityFingerprintLoad(provider, accountKey)
		return nil
	}
	now := time.Now().UTC()
	obs, ok := identityfingerprint.ExtractObservation(identityfingerprint.LearnInput{
		Provider:      provider,
		AccountKey:    accountKey,
		AuthSubjectID: authSubjectID,
		Headers:       headers,
		ObservedAt:    now,
	})
	if !ok {
		return nil
	}
	profileKey := strings.TrimSpace(obs.ProfileKey)
	if profileKey == "" {
		profileKey = identityfingerprint.DefaultProfileKey(provider)
	}
	signature := runtimeIdentityFingerprintObservationSignature(obs)
	if record := getCachedRuntimeIdentityFingerprint(provider, accountKey, profileKey, signature); record != nil {
		return record
	}
	existing := getCachedRuntimeIdentityFingerprint(provider, accountKey, profileKey, "")
	result := identityfingerprint.MergeObservation(existing, obs)
	if result.Record == nil {
		return nil
	}
	setCachedRuntimeIdentityFingerprint(provider, accountKey, profileKey, signature, result.Record, identityFingerprintWriteCacheTTL)
	setCachedRuntimeIdentityFingerprint(provider, accountKey, "", "", result.Record, identityFingerprintReadCacheTTL)
	scheduleRuntimeIdentityFingerprintPersist(identityfingerprint.LearnInput{
		Provider:      provider,
		AccountKey:    accountKey,
		AuthSubjectID: authSubjectID,
		Headers:       headers.Clone(),
		ObservedAt:    now,
	}, profileKey, signature)
	return result.Record
}

func scheduleRuntimeIdentityFingerprintLoad(provider identityfingerprint.Provider, accountKey string) {
	accountKey = strings.TrimSpace(accountKey)
	if provider == "" || accountKey == "" {
		return
	}
	key := runtimeIdentityFingerprintCacheKey(provider, accountKey, "")
	runtimeIdentityFingerprintAsync.Lock()
	if _, ok := runtimeIdentityFingerprintAsync.loads[key]; ok {
		runtimeIdentityFingerprintAsync.Unlock()
		return
	}
	runtimeIdentityFingerprintAsync.loads[key] = struct{}{}
	runtimeIdentityFingerprintAsync.Unlock()

	go func() {
		defer func() {
			runtimeIdentityFingerprintAsync.Lock()
			delete(runtimeIdentityFingerprintAsync.loads, key)
			runtimeIdentityFingerprintAsync.Unlock()
		}()
		record, err := callRuntimeGetIdentityFingerprint(provider, accountKey)
		if err != nil {
			log.WithError(err).Warn("identity fingerprint: load learned record")
			return
		}
		if record == nil {
			return
		}
		setCachedRuntimeIdentityFingerprint(provider, accountKey, record.ProfileKey, "", record, identityFingerprintReadCacheTTL)
		setCachedRuntimeIdentityFingerprint(provider, accountKey, "", "", record, identityFingerprintReadCacheTTL)
	}()
}

func scheduleRuntimeIdentityFingerprintPersist(input identityfingerprint.LearnInput, profileKey, signature string) {
	provider := input.Provider
	accountKey := strings.TrimSpace(input.AccountKey)
	profileKey = strings.TrimSpace(profileKey)
	if provider == "" || accountKey == "" || profileKey == "" {
		return
	}
	key := runtimeIdentityFingerprintCacheKey(provider, accountKey, profileKey) + "\x00" + strings.TrimSpace(signature)
	runtimeIdentityFingerprintAsync.Lock()
	if _, ok := runtimeIdentityFingerprintAsync.persists[key]; ok {
		runtimeIdentityFingerprintAsync.Unlock()
		return
	}
	runtimeIdentityFingerprintAsync.persists[key] = struct{}{}
	runtimeIdentityFingerprintAsync.Unlock()

	go func() {
		defer func() {
			runtimeIdentityFingerprintAsync.Lock()
			delete(runtimeIdentityFingerprintAsync.persists, key)
			runtimeIdentityFingerprintAsync.Unlock()
		}()
		record, _, err := callRuntimeObserveIdentityFingerprint(input)
		if err != nil {
			log.WithError(err).Warn("identity fingerprint: observe learned record")
			return
		}
		if record == nil {
			return
		}
		setCachedRuntimeIdentityFingerprint(provider, accountKey, record.ProfileKey, signature, record, identityFingerprintWriteCacheTTL)
		setCachedRuntimeIdentityFingerprint(provider, accountKey, "", "", record, identityFingerprintReadCacheTTL)
	}()
}

func runtimeIdentityFingerprintCacheKey(provider identityfingerprint.Provider, accountKey, profileKey string) string {
	return string(provider) + "\x00" + strings.TrimSpace(accountKey) + "\x00" + strings.TrimSpace(profileKey)
}

func getCachedRuntimeIdentityFingerprint(provider identityfingerprint.Provider, accountKey, profileKey, signature string) *identityfingerprint.LearnedRecord {
	now := time.Now()
	key := runtimeIdentityFingerprintCacheKey(provider, accountKey, profileKey)
	runtimeIdentityFingerprintCache.Lock()
	entry, ok := runtimeIdentityFingerprintCache.records[key]
	if !ok || now.After(entry.expiresAt) || (signature != "" && entry.signature != signature) {
		if ok {
			delete(runtimeIdentityFingerprintCache.records, key)
		}
		runtimeIdentityFingerprintCache.Unlock()
		return nil
	}
	record := cloneRuntimeIdentityFingerprintRecord(entry.record)
	runtimeIdentityFingerprintCache.Unlock()
	return record
}

func setCachedRuntimeIdentityFingerprint(provider identityfingerprint.Provider, accountKey, profileKey, signature string, record *identityfingerprint.LearnedRecord, ttl time.Duration) {
	if record == nil || ttl <= 0 {
		return
	}
	key := runtimeIdentityFingerprintCacheKey(provider, accountKey, profileKey)
	entry := identityFingerprintCacheEntry{
		record:    cloneRuntimeIdentityFingerprintRecord(record),
		signature: signature,
		expiresAt: time.Now().Add(ttl),
	}
	runtimeIdentityFingerprintCache.Lock()
	runtimeIdentityFingerprintCache.records[key] = entry
	runtimeIdentityFingerprintCache.Unlock()
}

func listCachedRuntimeIdentityFingerprintProfiles(provider identityfingerprint.Provider, accountKey string) []identityfingerprint.LearnedRecord {
	accountKey = strings.TrimSpace(accountKey)
	if provider == "" || accountKey == "" {
		return nil
	}
	now := time.Now()
	prefix := string(provider) + "\x00" + accountKey + "\x00"
	seen := map[string]struct{}{}
	profiles := []identityfingerprint.LearnedRecord{}
	runtimeIdentityFingerprintCache.Lock()
	for key, entry := range runtimeIdentityFingerprintCache.records {
		if !strings.HasPrefix(key, prefix) {
			continue
		}
		profileKey := strings.TrimPrefix(key, prefix)
		if profileKey == "" || entry.record == nil {
			continue
		}
		if now.After(entry.expiresAt) {
			delete(runtimeIdentityFingerprintCache.records, key)
			continue
		}
		if _, ok := seen[profileKey]; ok {
			continue
		}
		seen[profileKey] = struct{}{}
		profiles = append(profiles, *cloneRuntimeIdentityFingerprintRecord(entry.record))
	}
	runtimeIdentityFingerprintCache.Unlock()
	sort.Slice(profiles, func(i, j int) bool {
		if !profiles[i].LastSeenAt.Equal(profiles[j].LastSeenAt) {
			return profiles[i].LastSeenAt.After(profiles[j].LastSeenAt)
		}
		return profiles[i].ProfileKey < profiles[j].ProfileKey
	})
	return profiles
}

func invalidateCachedRuntimeIdentityFingerprint(provider identityfingerprint.Provider, accountKey string) {
	prefix := string(provider) + "\x00" + strings.TrimSpace(accountKey) + "\x00"
	runtimeIdentityFingerprintCache.Lock()
	for key := range runtimeIdentityFingerprintCache.records {
		if strings.HasPrefix(key, prefix) {
			delete(runtimeIdentityFingerprintCache.records, key)
		}
	}
	runtimeIdentityFingerprintCache.Unlock()
}

func runtimeIdentityFingerprintObservationSignature(obs identityfingerprint.Observation) string {
	h := fnv.New64a()
	writeFingerprintHashPart(h, string(obs.Provider))
	writeFingerprintHashPart(h, obs.ProfileKey)
	writeFingerprintHashPart(h, obs.ClientProduct)
	writeFingerprintHashPart(h, obs.ClientVariant)
	writeFingerprintHashPart(h, obs.Version)
	for _, key := range sortedRuntimeFingerprintKeys(obs.Fields) {
		writeFingerprintHashPart(h, key)
		writeFingerprintHashPart(h, obs.Fields[key])
	}
	for _, key := range sortedRuntimeFingerprintKeys(obs.ObservedHeaders) {
		writeFingerprintHashPart(h, key)
		writeFingerprintHashPart(h, obs.ObservedHeaders[key])
	}
	return fmt.Sprintf("%016x", h.Sum64())
}

type fingerprintHash interface {
	Write([]byte) (int, error)
}

func writeFingerprintHashPart(h fingerprintHash, value string) {
	_, _ = h.Write([]byte(strings.TrimSpace(value)))
	_, _ = h.Write([]byte{0})
}

func sortedRuntimeFingerprintKeys(values map[string]string) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func cloneRuntimeIdentityFingerprintRecord(record *identityfingerprint.LearnedRecord) *identityfingerprint.LearnedRecord {
	if record == nil {
		return nil
	}
	out := *record
	out.Fields = cloneRuntimeStringMap(record.Fields)
	out.ObservedHeaders = cloneRuntimeStringMap(record.ObservedHeaders)
	return &out
}

func cloneRuntimeStringMap(in map[string]string) map[string]string {
	if in == nil {
		return nil
	}
	out := make(map[string]string, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}
