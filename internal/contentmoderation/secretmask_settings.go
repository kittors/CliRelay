package contentmoderation

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"strconv"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/configsync"
	log "github.com/sirupsen/logrus"
	"golang.org/x/sync/singleflight"
)

// SecretMaskingSettingKey is the runtime_settings row of a tenant holding its
// SecretMaskingSettings.
const SecretMaskingSettingKey = "secret-masking"

func init() {
	// The setting's receiver is this package, not a config field, so a write
	// is announced under its own domain and the other nodes drop just this
	// tenant's cached settings.
	configsync.RouteRuntimeSettingKey(SecretMaskingSettingKey, configsync.DomainSecretMasking)
}

// SecretMaskingSettings is a tenant's outbound secret masking (see
// secretmask_rules.go). It is off until the tenant switches it on.
type SecretMaskingSettings struct {
	Enabled bool `json:"enabled"`
	// Key is the tenant's placeholder key (see secretTable.placeholder): random
	// bytes in base64, made when masking is first switched on. Switching it off
	// keeps the key, so a conversation masked before gets the same placeholders,
	// and keeps its prompt cache, when masking comes back on. It is never sent to
	// a client.
	Key       string    `json:"key,omitempty"`
	UpdatedAt time.Time `json:"updated_at,omitzero"`
}

// secretMaskingKeyBytes is the length of a placeholder key.
const secretMaskingKeyBytes = 32

// placeholderKey returns the decoded key, and false when the settings hold
// none that is usable.
func (s SecretMaskingSettings) placeholderKey() ([]byte, bool) {
	key, err := base64.StdEncoding.DecodeString(s.Key)
	if err != nil || len(key) < secretMaskingKeyBytes {
		return nil, false
	}
	return key, true
}

// ErrSecretMaskingNotPersistent is returned by writes when no store is
// attached: a switch that would be lost on the next restart is worse than one
// that was refused.
var ErrSecretMaskingNotPersistent = errors.New("secret masking settings cannot be saved: no settings store is attached")

// ErrSecretMaskingConflict is returned by SecretMaskingStore.Save when the
// stored settings are no longer at the version they were read at.
var ErrSecretMaskingConflict = errors.New("secret masking settings were changed by someone else while being saved")

// SecretMaskingStore loads and saves a tenant's settings. It is an interface
// so this package, which sits on the request path, does not depend on the
// settings store that implements it.
type SecretMaskingStore interface {
	// Load returns the stored settings and their version, 0 when there is no
	// row.
	Load(ctx context.Context, tenantID string) (SecretMaskingSettings, int64, error)
	// Save writes settings if the stored row is still at the version expected
	// (0: there is none) and returns ErrSecretMaskingConflict otherwise.
	Save(ctx context.Context, tenantID string, settings SecretMaskingSettings, expected int64) error
}

const (
	// secretMaskingTTL is how long a tenant's settings are reused. A write made
	// through this process replaces the entry at once, one made by another node
	// arrives through Invalidate, and the TTL bounds the staleness of any write
	// neither reached (the other blue/green slot, while both are up).
	secretMaskingTTL = 15 * time.Second
	// secretMaskingRetryAfterFailure spaces the reads while the store fails, so
	// a database outage does not add a failing query to every request.
	secretMaskingRetryAfterFailure = 2 * time.Second
	secretMaskingLoadTimeout       = 3 * time.Second
	// secretMaskingRefreshWait bounds how long a request waits for its tenant's
	// settings to be read again. Every inference request reads them, those of
	// tenants that never switched masking on included, so a slow store must not
	// hold them: the request goes on with the settings read before, and the read
	// finishes in the background.
	secretMaskingRefreshWait = 75 * time.Millisecond
	// secretMaskingFirstReadWait is the wait while nothing has been read for the
	// tenant yet. Masking may be on, and going on without the settings would
	// send the tenant's secrets upstream, so the first request waits for a slow
	// store. A store that does not answer at all is taken as masking off for as
	// long as it fails: the word guard fails open the same way.
	secretMaskingFirstReadWait = time.Second
	// secretMaskingUpdateAttempts is how often a write is retried on top of a
	// concurrent one before it gives up.
	secretMaskingUpdateAttempts = 4
)

type secretMaskingEntry struct {
	settings SecretMaskingSettings
	// until is when the entry stops being served without a read.
	until time.Time
	// read says the settings came from the store at least once.
	read bool
}

// SecretMaskingRegistry caches the settings of the tenants that have sent a
// request and applies writes to them.
type SecretMaskingRegistry struct {
	mu      sync.RWMutex
	store   SecretMaskingStore
	entries map[string]secretMaskingEntry
	// gen changes on every invalidation and write. A read that started under an
	// older generation does not store its result, so it cannot put a row from
	// before a write back into the cache.
	gen   uint64
	loads singleflight.Group
	now   func() time.Time
}

// NewSecretMaskingRegistry returns a registry with no store attached: masking
// is off for every tenant, and writes are refused.
func NewSecretMaskingRegistry() *SecretMaskingRegistry {
	return &SecretMaskingRegistry{entries: make(map[string]secretMaskingEntry), now: time.Now}
}

var defaultSecretMasking = NewSecretMaskingRegistry()

// SecretMasking returns the registry of this process.
func SecretMasking() *SecretMaskingRegistry { return defaultSecretMasking }

// SetStore attaches the store. Cached settings are dropped, so what it holds
// applies from the next request.
func (r *SecretMaskingRegistry) SetStore(store SecretMaskingStore) {
	if r == nil {
		return
	}
	r.mu.Lock()
	r.store = store
	r.entries = make(map[string]secretMaskingEntry)
	r.gen++
	r.mu.Unlock()
}

// Persistent reports whether writes are saved.
func (r *SecretMaskingRegistry) Persistent() bool {
	if r == nil {
		return false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.store != nil
}

// Invalidate expires tenantID's cached settings, for a change made by another
// node.
func (r *SecretMaskingRegistry) Invalidate(tenantID string) {
	if r == nil {
		return
	}
	tenantID = configsync.NormalizeTenantID(tenantID)
	r.mu.Lock()
	if cached, ok := r.entries[tenantID]; ok {
		cached.until = time.Time{}
		r.entries[tenantID] = cached
	}
	r.gen++
	r.mu.Unlock()
}

// InvalidateAll expires every cached tenant, for a resync after missed events.
func (r *SecretMaskingRegistry) InvalidateAll() {
	if r == nil {
		return
	}
	r.mu.Lock()
	for tenantID, cached := range r.entries {
		cached.until = time.Time{}
		r.entries[tenantID] = cached
	}
	r.gen++
	r.mu.Unlock()
}

// For returns the settings of tenantID, from the cache unless the entry has
// expired; see secretMaskingRefreshWait and secretMaskingFirstReadWait for how
// long it waits for the store.
func (r *SecretMaskingRegistry) For(tenantID string) SecretMaskingSettings {
	if r == nil {
		return SecretMaskingSettings{}
	}
	tenantID = configsync.NormalizeTenantID(tenantID)
	r.mu.RLock()
	store, cached, gen := r.store, r.entries[tenantID], r.gen
	r.mu.RUnlock()
	if store == nil {
		return SecretMaskingSettings{}
	}
	if r.now().Before(cached.until) {
		return cached.settings
	}
	// Concurrent callers share one read. The generation is part of the key: a
	// read that started before an invalidation or a write returns the row as it
	// was then, so a request that came after must not join it.
	key := tenantID + "\x00" + strconv.FormatUint(gen, 10)
	loaded := r.loads.DoChan(key, func() (any, error) {
		return r.reload(tenantID, store, gen, cached), nil
	})
	waitFor := secretMaskingRefreshWait
	if !cached.read {
		waitFor = secretMaskingFirstReadWait
	}
	wait := time.NewTimer(waitFor)
	defer wait.Stop()
	select {
	case result := <-loaded:
		return result.Val.(SecretMaskingSettings)
	case <-wait.C:
		return cached.settings
	}
}

func (r *SecretMaskingRegistry) reload(tenantID string, store SecretMaskingStore, gen uint64, previous secretMaskingEntry) SecretMaskingSettings {
	ctx, cancel := context.WithTimeout(context.Background(), secretMaskingLoadTimeout)
	defer cancel()
	settings, _, err := store.Load(ctx, tenantID)
	now := r.now()
	next := secretMaskingEntry{settings: settings, until: now.Add(secretMaskingTTL), read: true}
	if err != nil {
		// Keep what was last read, if anything: a store that cannot be read for a
		// moment must not switch a tenant's masking off.
		log.WithError(err).WithField("tenant_id", tenantID).Warn("secret masking: stored settings are unreadable; using the last ones read")
		next = secretMaskingEntry{settings: previous.settings, until: now.Add(secretMaskingRetryAfterFailure), read: previous.read}
	}
	r.mu.Lock()
	if r.gen == gen {
		r.entries[tenantID] = next
	}
	r.mu.Unlock()
	return next.settings
}

// SetEnabled switches masking on or off for tenantID and returns the settings
// as saved. Switching it on the first time makes the tenant's key.
func (r *SecretMaskingRegistry) SetEnabled(ctx context.Context, tenantID string, enabled bool) (SecretMaskingSettings, error) {
	if r == nil {
		return SecretMaskingSettings{}, ErrSecretMaskingNotPersistent
	}
	tenantID = configsync.NormalizeTenantID(tenantID)
	r.mu.RLock()
	store := r.store
	r.mu.RUnlock()
	if store == nil {
		return SecretMaskingSettings{}, ErrSecretMaskingNotPersistent
	}
	for range secretMaskingUpdateAttempts {
		current, version, err := store.Load(ctx, tenantID)
		if err != nil {
			return SecretMaskingSettings{}, err
		}
		_, hasKey := current.placeholderKey()
		if current.Enabled == enabled && (hasKey || !enabled) {
			// Nothing to change: no new version, no announcement, and no row made
			// for a tenant that never had one.
			r.install(tenantID, current)
			return current, nil
		}
		next := current
		next.Enabled = enabled
		if enabled && !hasKey {
			key := make([]byte, secretMaskingKeyBytes)
			if _, err := rand.Read(key); err != nil {
				return SecretMaskingSettings{}, err
			}
			next.Key = base64.StdEncoding.EncodeToString(key)
		}
		next.UpdatedAt = r.now().UTC()
		err = store.Save(ctx, tenantID, next, version)
		if errors.Is(err, ErrSecretMaskingConflict) {
			continue
		}
		if err != nil {
			return SecretMaskingSettings{}, err
		}
		r.install(tenantID, next)
		return next, nil
	}
	return SecretMaskingSettings{}, ErrSecretMaskingConflict
}

// install makes settings the cached ones of tenantID.
func (r *SecretMaskingRegistry) install(tenantID string, settings SecretMaskingSettings) {
	r.mu.Lock()
	r.entries[tenantID] = secretMaskingEntry{settings: settings, until: r.now().Add(secretMaskingTTL), read: true}
	r.gen++
	r.mu.Unlock()
}
