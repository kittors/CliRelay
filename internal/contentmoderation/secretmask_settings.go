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

// SecretMaskingSettings is a tenant's privacy masking (see secretmask.go): its
// default policy, what it enforces for its portal users, and whether they may
// keep a policy of their own. Nothing is masked until something is switched
// on.
//
// The row predates the policy: one written before has only Enabled, which is
// read as the policy "mask secrets" (policy). Enabled is still written, as
// the policy's Secrets, for a build that reads only it.
type SecretMaskingSettings struct {
	Enabled bool `json:"enabled"`
	// Key is the tenant's placeholder key (see privacymask.Masker): random
	// bytes in base64, made when masking is first set up. Switching masking off
	// keeps the key, so a conversation masked before gets the same placeholders,
	// and keeps its prompt cache, when masking comes back on. It is never sent to
	// a client. KeyEnc holds it instead, sealed, when the server has a data key
	// (secretmask_kek.go).
	Key    string `json:"key,omitempty"`
	KeyEnc string `json:"key_enc,omitempty"`
	// Policy is the tenant's default, nil for a row written before policies.
	Policy *MaskPolicy `json:"policy,omitempty"`
	// Enforced are the categories on for every portal user (EnforceSecrets,
	// EnforcePersonal).
	Enforced []string `json:"enforced,omitempty"`
	// PortalDefaultAllowed says whether portal users without an override of
	// their own may keep a policy; nil is yes.
	PortalDefaultAllowed *bool     `json:"portal_default_allowed,omitempty"`
	UpdatedAt            time.Time `json:"updated_at,omitzero"`
}

// policy is the tenant's default policy.
func (s SecretMaskingSettings) policy() MaskPolicy {
	if s.Policy != nil {
		return *s.Policy
	}
	p := MaskPolicy{Secrets: s.Enabled}
	_ = p.Normalize() // nothing to tidy: it only fills in the empty lists
	return p
}

// PolicyView is the tenant's default policy, tidied for a client.
func (s SecretMaskingSettings) PolicyView() MaskPolicy {
	p := s.policy()
	if err := p.Normalize(); err != nil {
		return s.policy() // a row written by hand: shown as it is
	}
	return p
}

// PortalAllowed says whether portal users without an override may keep a
// policy of their own.
func (s SecretMaskingSettings) PortalAllowed() bool {
	return s.PortalDefaultAllowed == nil || *s.PortalDefaultAllowed
}

// KeyProtected says the placeholder key is stored sealed.
func (s SecretMaskingSettings) KeyProtected() bool { return s.KeyEnc != "" && s.Key == "" }

// secretMaskingKeyBytes is the length of a placeholder key.
const secretMaskingKeyBytes = 32

// placeholderKey returns the decoded key, and false when the settings hold
// none that is usable: none made yet, or one sealed under a data key the
// server does not have.
func (s SecretMaskingSettings) placeholderKey(tenantID string) ([]byte, bool) {
	if s.Key == "" && s.KeyEnc != "" {
		key, err := openTenantKey(tenantID, s.KeyEnc)
		if err != nil || len(key) < secretMaskingKeyBytes {
			return nil, false
		}
		return key, true
	}
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

// SecretMaskingBulkStore is a SecretMaskingStore that can also read every
// tenant's settings at once (Preload).
type SecretMaskingBulkStore interface {
	LoadAll(ctx context.Context) (map[string]SecretMaskingSettings, error)
}

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
	// preloaded says every tenant's settings were read at once (Preload): a
	// tenant with no entry has no row, so masking is off for it, even while
	// the store cannot be read.
	preloaded bool
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
	r.preloaded = false
	r.mu.Unlock()
	maskScopes.Drop("")
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
// node. What masking remembers of the tenant's requests is forgotten with it:
// the next request brings it again.
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
	maskScopes.Drop(tenantID + "\x00")
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
	maskScopes.Drop("")
}

// For returns the settings of tenantID; see Lookup.
func (r *SecretMaskingRegistry) For(tenantID string) SecretMaskingSettings {
	settings, _ := r.Lookup(tenantID)
	return settings
}

// Lookup returns the settings of tenantID, from the cache unless the entry has
// expired; see secretMaskingRefreshWait and secretMaskingFirstReadWait for how
// long it waits for the store. known is false when they are not known: never
// read, not preloaded, and the store did not answer now. Masking may be on for
// such a tenant, so its request must not be sent as it is.
func (r *SecretMaskingRegistry) Lookup(tenantID string) (settings SecretMaskingSettings, known bool) {
	if r == nil {
		return SecretMaskingSettings{}, true
	}
	tenantID = configsync.NormalizeTenantID(tenantID)
	r.mu.RLock()
	store, cached, gen, preloaded := r.store, r.entries[tenantID], r.gen, r.preloaded
	r.mu.RUnlock()
	if store == nil {
		return SecretMaskingSettings{}, true
	}
	if !cached.read && preloaded {
		// no row when every row was read: off, as far as anything here knows
		cached.read = true
	}
	if r.now().Before(cached.until) {
		return cached.settings, true
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
		entry := result.Val.(secretMaskingEntry)
		return entry.settings, entry.read
	case <-wait.C:
		return cached.settings, cached.read
	}
}

func (r *SecretMaskingRegistry) reload(tenantID string, store SecretMaskingStore, gen uint64, previous secretMaskingEntry) secretMaskingEntry {
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
	return next
}

// Preload reads every tenant's settings at once, when the store can. After it,
// a tenant whose settings cannot be read on its first request is known still:
// one with no row has masking off. Before it (or with a store that cannot),
// such a request is refused rather than sent unmasked (Lookup).
func (r *SecretMaskingRegistry) Preload(ctx context.Context) error {
	if r == nil {
		return nil
	}
	r.mu.RLock()
	store := r.store
	r.mu.RUnlock()
	bulk, ok := store.(SecretMaskingBulkStore)
	if !ok {
		return nil
	}
	all, err := bulk.LoadAll(ctx)
	if err != nil {
		return err
	}
	now := r.now()
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.store != store {
		return nil // another store came meanwhile; what was read is not its
	}
	for tenantID, settings := range all {
		tenantID = configsync.NormalizeTenantID(tenantID)
		if have, ok := r.entries[tenantID]; ok && have.read {
			continue // read or written since: newer than this
		}
		r.entries[tenantID] = secretMaskingEntry{settings: settings, until: now.Add(secretMaskingTTL), read: true}
	}
	r.preloaded = true
	return nil
}

// secretMaskingPreloadRetry spaces the tries of PreloadInBackground.
var secretMaskingPreloadRetry = 5 * time.Second

// PreloadInBackground runs Preload until it succeeds or the store changes. It
// returns at once.
func (r *SecretMaskingRegistry) PreloadInBackground() {
	if r == nil {
		return
	}
	r.mu.RLock()
	store := r.store
	r.mu.RUnlock()
	if _, ok := store.(SecretMaskingBulkStore); !ok {
		return
	}
	go func() {
		for attempt := 0; ; attempt++ {
			ctx, cancel := context.WithTimeout(context.Background(), 10*secretMaskingLoadTimeout)
			err := r.Preload(ctx)
			cancel()
			if err == nil {
				ctx, cancel := context.WithTimeout(context.Background(), 10*secretMaskingLoadTimeout)
				r.sealStoredKeys(ctx)
				cancel()
				return
			}
			if attempt%12 == 0 {
				log.WithError(err).Warn("secret masking: reading every tenant's settings failed; retrying")
			}
			time.Sleep(secretMaskingPreloadRetry)
			r.mu.RLock()
			same := r.store == store
			r.mu.RUnlock()
			if !same {
				return
			}
		}
	}()
}

// SetEnabled switches the masking of secrets on or off in tenantID's default
// policy, for the endpoint that predates policies, and returns the settings as
// saved.
func (r *SecretMaskingRegistry) SetEnabled(ctx context.Context, tenantID string, enabled bool) (SecretMaskingSettings, error) {
	return r.update(ctx, tenantID, enabled, func(next *SecretMaskingSettings) bool {
		if next.Policy == nil {
			// a row of before policies stays one: the switch is all it has
			changed := next.Enabled != enabled
			next.Enabled = enabled
			return changed
		}
		if next.Policy.Secrets == enabled {
			return false
		}
		p := *next.Policy
		p.Secrets = enabled
		next.Policy = &p
		return true
	})
}

// SetPolicy saves tenantID's default policy, the categories it enforces and
// whether its portal users may keep a policy of their own. policy is
// normalized already.
func (r *SecretMaskingRegistry) SetPolicy(ctx context.Context, tenantID string, policy MaskPolicy, enforced []string, portalAllowed bool) (SecretMaskingSettings, error) {
	return r.update(ctx, tenantID, true, func(next *SecretMaskingSettings) bool {
		next.Policy = &policy
		next.Enforced = enforced
		next.PortalDefaultAllowed = &portalAllowed
		return true
	})
}

// EnsureKey makes tenantID's placeholder key if it has none: a portal user of
// the tenant is switching masking on for their own requests.
func (r *SecretMaskingRegistry) EnsureKey(ctx context.Context, tenantID string) error {
	_, err := r.update(ctx, tenantID, true, func(*SecretMaskingSettings) bool { return false })
	return err
}

// update applies change to tenantID's stored settings and saves them, on top
// of a concurrent write if there was one, and returns them as saved. needKey
// makes the placeholder key if there is none; a key stored unsealed is sealed
// when the server has a data key. Nothing is written when nothing changed: no
// new version, no announcement, no row for a tenant that never had one.
func (r *SecretMaskingRegistry) update(ctx context.Context, tenantID string, needKey bool, change func(*SecretMaskingSettings) bool) (SecretMaskingSettings, error) {
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
		next := current
		changed := change(&next)
		key, hasKey := next.placeholderKey(tenantID)
		if needKey && !hasKey {
			key = make([]byte, secretMaskingKeyBytes)
			if _, err := rand.Read(key); err != nil {
				return SecretMaskingSettings{}, err
			}
			next.Key, next.KeyEnc, hasKey, changed = base64.StdEncoding.EncodeToString(key), "", true, true
		}
		if hasKey && next.Key != "" && DataKeyConfigured() {
			sealed, err := sealTenantKey(tenantID, key)
			if err != nil {
				return SecretMaskingSettings{}, err
			}
			next.Key, next.KeyEnc, changed = "", sealed, true
		}
		if !changed {
			// refreshed, not changed: what masking remembers of the tenant's
			// requests stays, so their placeholders and prompt caches do too
			r.mu.Lock()
			r.entries[tenantID] = secretMaskingEntry{settings: current, until: r.now().Add(secretMaskingTTL), read: true}
			r.gen++
			r.mu.Unlock()
			return current, nil
		}
		next.Enabled = next.policy().Secrets
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

// sealStoredKeys seals the keys stored unsealed, once the server has a data
// key: rows written before it was set. Each is rewritten on its own, so a
// tenant whose row cannot be written keeps working with the key as it is.
func (r *SecretMaskingRegistry) sealStoredKeys(ctx context.Context) {
	if r == nil || !DataKeyConfigured() {
		return
	}
	r.mu.RLock()
	var plain []string
	for tenantID, entry := range r.entries {
		if entry.settings.Key != "" {
			plain = append(plain, tenantID)
		}
	}
	r.mu.RUnlock()
	for _, tenantID := range plain {
		if _, err := r.update(ctx, tenantID, false, func(*SecretMaskingSettings) bool { return false }); err != nil {
			log.WithError(err).WithField("tenant_id", tenantID).Warn("privacy masking: sealing the stored placeholder key failed; it stays as it is")
		}
	}
}

// install makes settings the cached ones of tenantID, and forgets what masking
// remembered of the tenant's requests: switched off, nothing of them is kept.
func (r *SecretMaskingRegistry) install(tenantID string, settings SecretMaskingSettings) {
	r.mu.Lock()
	r.entries[tenantID] = secretMaskingEntry{settings: settings, until: r.now().Add(secretMaskingTTL), read: true}
	r.gen++
	r.mu.Unlock()
	maskScopes.Drop(tenantID + "\x00")
}
