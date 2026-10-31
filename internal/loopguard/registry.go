package loopguard

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"
	"golang.org/x/sync/singleflight"
)

const (
	// cacheTTL is how long a tenant's settings are reused. The settings are read
	// by every inference request, so they are cached; a write made through this
	// process replaces the entry at once, one made by another node arrives
	// through Invalidate, and the TTL bounds the staleness of any write neither
	// reached (the other blue/green slot, while both are up).
	cacheTTL = 15 * time.Second
	// retryAfterFailure spaces the reads while the store fails, so a database
	// outage does not add a failing query to every request.
	retryAfterFailure = 2 * time.Second
	loadTimeout       = 3 * time.Second
	// updateAttempts is how often an update is recomputed on top of a
	// concurrent one before it gives up.
	updateAttempts = 4
	// systemTenantID mirrors identity.SystemTenantID; the empty tenant is the
	// system tenant, as everywhere tenant-scoped rows are stored.
	systemTenantID = "00000000-0000-0000-0000-000000000001"
)

// ErrNotPersistent is returned by writes when no store is attached: a setting
// that took effect but would be gone after the next restart is worse than one
// that was refused.
var ErrNotPersistent = errors.New("loop protection settings cannot be saved: no settings store is attached")

// ErrConflict is returned by Store.Save when the stored settings are no longer
// at the version they were read at.
var ErrConflict = errors.New("loop protection settings were changed by someone else while being saved")

// Stored is what a Store holds for a tenant. Version is 0 when there is no row.
type Stored struct {
	Settings Settings
	Version  int64
}

// Store loads and saves a tenant's settings. It is an interface so this
// package, which sits on the request path, never depends on the settings store.
type Store interface {
	Load(ctx context.Context, tenantID string) (Stored, error)
	// Save writes settings if the stored row is still at the version expected
	// (0: there is none) and returns ErrConflict otherwise.
	Save(ctx context.Context, tenantID string, settings Settings, expected int64) error
}

type entry struct {
	settings Settings
	// until is when the entry stops being served without a reload.
	until time.Time
}

// Registry caches the settings of the tenants that have sent a request and
// applies writes to them.
type Registry struct {
	mu      sync.RWMutex
	store   Store
	entries map[string]entry
	// gen changes on every invalidation and write. A read that started under an
	// older generation does not store its result, so it cannot put a row from
	// before a write back into the cache.
	gen   uint64
	loads singleflight.Group
	now   func() time.Time
}

// New returns a Registry with no store attached: every tenant has loop
// protection off, and writes are refused.
func New() *Registry {
	return &Registry{entries: make(map[string]entry), now: time.Now}
}

var defaultRegistry = New()

// Default returns the registry of this process.
func Default() *Registry { return defaultRegistry }

// SetStore attaches the store. Cached settings are dropped, so what it holds
// applies from the next request.
func (r *Registry) SetStore(store Store) {
	if r == nil {
		return
	}
	r.mu.Lock()
	r.store = store
	r.entries = make(map[string]entry)
	r.gen++
	r.mu.Unlock()
}

// Persistent reports whether writes are saved.
func (r *Registry) Persistent() bool {
	if r == nil {
		return false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.store != nil
}

// Invalidate drops tenantID's cached settings, for a change made by another
// node: the next request reads them again.
func (r *Registry) Invalidate(tenantID string) {
	if r == nil {
		return
	}
	r.mu.Lock()
	delete(r.entries, normalizeTenant(tenantID))
	r.gen++
	r.mu.Unlock()
}

// InvalidateAll drops every cached tenant, for a resync after missed events.
func (r *Registry) InvalidateAll() {
	if r == nil {
		return
	}
	r.mu.Lock()
	r.entries = make(map[string]entry)
	r.gen++
	r.mu.Unlock()
}

// Effective resolves what loop protection does for a request of the API key
// with keyID in tenantID. It is on the path of every inference request: it
// answers from the cache and reads the store only when the entry has expired.
func (r *Registry) Effective(tenantID, keyID string) Effective {
	return r.For(tenantID).Effective(keyID)
}

// For returns the settings of tenantID: those it has stored, or none (everything
// off) when it has stored nothing or the store cannot be read and nothing was
// read before.
func (r *Registry) For(tenantID string) Settings {
	if r == nil {
		return Settings{}
	}
	tenantID = normalizeTenant(tenantID)
	r.mu.RLock()
	store, cached, gen := r.store, r.entries[tenantID], r.gen
	r.mu.RUnlock()
	if store == nil {
		return Settings{}
	}
	if now := r.now(); now.Before(cached.until) {
		return cached.settings
	}
	// Expired or absent. Concurrent callers share one read; those that have an
	// expired entry to fall back on still wait for it, which is one query per
	// tenant per TTL.
	v, _, _ := r.loads.Do(tenantID, func() (any, error) {
		return r.reload(tenantID, store, gen, cached), nil
	})
	return v.(Settings)
}

func (r *Registry) reload(tenantID string, store Store, gen uint64, previous entry) Settings {
	ctx, cancel := context.WithTimeout(context.Background(), loadTimeout)
	defer cancel()
	stored, err := store.Load(ctx, tenantID)
	now := r.now()
	next := entry{settings: stored.Settings.Sanitized(), until: now.Add(cacheTTL)}
	if err != nil {
		// Keep what was last read, if anything: a store that cannot be read for
		// a moment must not switch a tenant's protection off, or on.
		log.WithError(err).WithField("tenant", tenantID).Warn("loop guard: stored settings are unreadable; using the last ones read")
		next = entry{settings: previous.settings, until: now.Add(retryAfterFailure)}
	}
	r.mu.Lock()
	if r.gen == gen {
		r.entries[tenantID] = next
	}
	r.mu.Unlock()
	return next.settings
}

// Update applies mutate to the stored settings of tenantID and saves the
// result. It is computed on the row as stored, not on the cache, and saved only
// if the row is still at the version it was read at; on a conflict it is
// computed again, because a change to one key's override is valid on top of any
// concurrent change to another's. mutate may be called more than once. It
// returns the settings as saved.
func (r *Registry) Update(ctx context.Context, tenantID string, mutate func(*Settings) error) (Settings, error) {
	if r == nil {
		return Settings{}, ErrNotPersistent
	}
	tenantID = normalizeTenant(tenantID)
	r.mu.RLock()
	store := r.store
	r.mu.RUnlock()
	if store == nil {
		return Settings{}, ErrNotPersistent
	}
	for range updateAttempts {
		stored, err := store.Load(ctx, tenantID)
		if err != nil {
			return Settings{}, err
		}
		current := stored.Settings.Sanitized()
		next := current.Clone()
		if err := mutate(&next); err != nil {
			return Settings{}, err
		}
		next = next.Sanitized()
		if reflect.DeepEqual(next, current) {
			// Nothing changed: no new version, no announcement, and no row
			// created for a tenant that never had one.
			r.install(tenantID, current)
			return current, nil
		}
		next.UpdatedAt = r.now().UTC()
		err = store.Save(ctx, tenantID, next, stored.Version)
		if errors.Is(err, ErrConflict) {
			continue
		}
		if err != nil {
			return Settings{}, err
		}
		r.install(tenantID, next)
		return next, nil
	}
	return Settings{}, ErrConflict
}

// install makes settings the cached ones of tenantID.
func (r *Registry) install(tenantID string, settings Settings) {
	r.mu.Lock()
	r.entries[tenantID] = entry{settings: settings, until: r.now().Add(cacheTTL)}
	r.gen++
	r.mu.Unlock()
}

func normalizeTenant(tenantID string) string {
	if tenantID = strings.TrimSpace(tenantID); tenantID != "" {
		return tenantID
	}
	return systemTenantID
}
