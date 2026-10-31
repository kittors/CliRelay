package executor

import (
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/identityfingerprint"
	log "github.com/sirupsen/logrus"
)

// identityFingerprintColdLoadTimeout bounds how long a request waits for the
// stored record on the first lookup for an account. A slower store falls back to
// the background load, which still fills the cache when it finishes.
var identityFingerprintColdLoadTimeout = 500 * time.Millisecond

// runtimeIdentityFingerprintColdLoaded marks the (provider, account) pairs whose
// stored record has already been read in this process.
var runtimeIdentityFingerprintColdLoaded sync.Map

// identityFingerprintColdLoadProviders lists the providers whose stored record is
// read synchronously on a cold cache. Claude's identity is version-gated
// upstream, so serving the builtin template or a single fresh observation in
// place of the stored record changes which models the account may use.
var identityFingerprintColdLoadProviders = map[identityfingerprint.Provider]bool{
	identityfingerprint.ProviderClaude: true,
}

// loadRuntimeIdentityFingerprintOnColdStart reads the stored record the first
// time an account is looked up in this process, so a fresh process applies the
// same identity as a warm one instead of waiting for the background load. Later
// cache misses keep the asynchronous path. It returns nil when the provider is
// not covered, the account was already loaded, nothing is stored, or the store
// does not answer within identityFingerprintColdLoadTimeout.
func loadRuntimeIdentityFingerprintOnColdStart(provider identityfingerprint.Provider, accountKey string) *identityfingerprint.LearnedRecord {
	accountKey = strings.TrimSpace(accountKey)
	if !identityFingerprintColdLoadProviders[provider] || accountKey == "" {
		return nil
	}
	key := runtimeIdentityFingerprintCacheKey(provider, accountKey, "")
	if _, loaded := runtimeIdentityFingerprintColdLoaded.LoadOrStore(key, struct{}{}); loaded {
		return nil
	}

	done := make(chan *identityfingerprint.LearnedRecord, 1)
	go func() {
		record, err := callRuntimeGetIdentityFingerprint(provider, accountKey)
		if err != nil {
			log.WithError(err).Warn("identity fingerprint: load learned record")
			record = nil
		}
		if record != nil {
			setCachedRuntimeIdentityFingerprint(provider, accountKey, record.ProfileKey, "", record, identityFingerprintReadCacheTTL)
			setCachedRuntimeIdentityFingerprint(provider, accountKey, "", "", record, identityFingerprintReadCacheTTL)
		}
		done <- record
	}()

	timer := time.NewTimer(identityFingerprintColdLoadTimeout)
	defer timer.Stop()
	select {
	case record := <-done:
		return cloneRuntimeIdentityFingerprintRecord(record)
	case <-timer.C:
		return nil
	}
}
