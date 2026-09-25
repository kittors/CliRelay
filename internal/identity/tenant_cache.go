package identity

import (
	"context"
	"database/sql"
	"errors"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	log "github.com/sirupsen/logrus"
	"golang.org/x/sync/singleflight"
)

// Tenant rows are read on every authenticated API request (TenantAccessDeadline)
// and again every two seconds for each in-flight request of a tenant that has
// an expiry. Reading the tenants table each time made all of those requests
// fail with 403 whenever the database was unreachable, so a 30-60 second
// primary failover became an outage for every tenant-scoped key.
//
// GetTenant now answers from a per-service cache. A row is reused for
// tenantCacheTTL; when a refresh fails with anything but "no such tenant", the
// row keeps being served for up to tenantCacheStaleMaxAge. Expiry is evaluated
// against the clock on every read, so a cached row never outlives its tenant's
// expires_at.
//
// Tenant writes made through this process invalidate their row before
// returning, so in single-node mode a suspended or disabled tenant is refused
// on its next request exactly as before. Writes made on another node arrive
// through InvalidateTenant (called by the cluster config-event subscriber) or,
// at the latest, when tenantCacheTTL runs out.
const (
	tenantCacheTTL         = 15 * time.Second
	tenantCacheStaleMaxAge = 10 * time.Minute
	// tenantLoadTimeout bounds one tenants read, so the first request after
	// the database disappears waits seconds, not a TCP timeout.
	tenantLoadTimeout = 5 * time.Second
	// tenantRefreshRetryInterval spaces background refreshes while the
	// database is failing; requests are answered from the stale rows meanwhile.
	tenantRefreshRetryInterval = time.Second
	tenantStaleLogInterval     = 30 * time.Second
)

type tenantLoader func(ctx context.Context, id string) (Tenant, error)

type tenantCacheEntry struct {
	tenant   Tenant
	loadedAt time.Time
}

type tenantCache struct {
	mu      sync.Mutex
	entries map[string]tenantCacheEntry
	// gen changes on every invalidation. A load that started under an older
	// generation neither stores its row nor lets later callers join it, so a
	// read racing a tenant write cannot put the pre-write row back.
	gen uint64
	// failing is set while refreshes fail with database errors. Callers with a
	// usable stale row then get it at once instead of each waiting on the
	// database; one background refresh per retry interval probes for recovery.
	failing     bool
	lastErr     error
	retryAfter  time.Time
	staleLogged map[string]time.Time

	loads       singleflight.Group
	staleServed atomic.Int64
}

// GetTenant returns the tenant row, from the cache when it is fresh enough;
// see the comment at the top of this file.
func (s *Service) GetTenant(ctx context.Context, id string) (Tenant, error) {
	return s.tenants.get(ctx, id, s.loadTenant)
}

// InvalidateTenant drops the cached row of tenant id so the next access
// check reads it from the database.
func (s *Service) InvalidateTenant(id string) {
	if s != nil {
		s.tenants.invalidate(id)
	}
}

// InvalidateAllTenants drops every cached tenant row.
func (s *Service) InvalidateAllTenants() {
	if s != nil {
		s.tenants.invalidateAll()
	}
}

// TenantStaleServed returns how many tenant checks were answered from a
// cached row because the database could not be read.
func (s *Service) TenantStaleServed() int64 {
	if s == nil {
		return 0
	}
	return s.tenants.staleServed.Load()
}

// InvalidateTenant drops the cached row of tenant id on the default service.
// Tenant writes made through this process already call it; in cluster mode
// the subscriber of cluster.TopicConfig events with domain "tenants" calls it
// for writes made on other nodes. Safe to call from any goroutine.
func InvalidateTenant(id string) {
	if s := Default(); s != nil {
		s.InvalidateTenant(id)
	}
}

// InvalidateAllTenants drops every cached tenant row on the default service.
// Call it on a cluster Resync event: notifications sent while this node was
// disconnected from the bus are lost, so any cached row may be stale.
func InvalidateAllTenants() {
	if s := Default(); s != nil {
		s.InvalidateAllTenants()
	}
}

// TenantStaleServed reports stale tenant answers of the default service.
func TenantStaleServed() int64 {
	return Default().TenantStaleServed()
}

func (c *tenantCache) get(ctx context.Context, id string, load tenantLoader) (Tenant, error) {
	now := time.Now()
	c.mu.Lock()
	entry, cached := c.entries[id]
	gen, failing := c.gen, c.failing
	c.mu.Unlock()

	age := now.Sub(entry.loadedAt)
	if cached && age < tenantCacheTTL {
		return tenantSnapshot(entry.tenant, now), nil
	}
	usable := cached && age <= tenantCacheStaleMaxAge
	if usable && failing {
		c.refreshInBackground(id, gen, load, now)
		c.noteStale(id, age, now)
		return tenantSnapshot(entry.tenant, now), nil
	}
	tenant, err := c.refresh(ctx, id, gen, load)
	if err == nil {
		return tenantSnapshot(tenant, time.Now()), nil
	}
	if !usable || errors.Is(err, sql.ErrNoRows) || ctx.Err() != nil {
		return Tenant{}, err
	}
	c.noteStale(id, age, now)
	return tenantSnapshot(entry.tenant, now), nil
}

func tenantFlightKey(id string, gen uint64) string {
	return id + "\x00" + strconv.FormatUint(gen, 10)
}

// refresh loads the row once for all concurrent callers of the same
// generation. The load is detached from the caller's context so one client
// going away does not fail the read for the others.
func (c *tenantCache) refresh(ctx context.Context, id string, gen uint64, load tenantLoader) (Tenant, error) {
	ch := c.loads.DoChan(tenantFlightKey(id, gen), func() (any, error) {
		return c.loadAndStore(id, gen, load)
	})
	select {
	case res := <-ch:
		if res.Err != nil {
			return Tenant{}, res.Err
		}
		return res.Val.(Tenant), nil
	case <-ctx.Done():
		return Tenant{}, ctx.Err()
	}
}

func (c *tenantCache) refreshInBackground(id string, gen uint64, load tenantLoader, now time.Time) {
	c.mu.Lock()
	if now.Before(c.retryAfter) {
		c.mu.Unlock()
		return
	}
	c.retryAfter = now.Add(tenantRefreshRetryInterval)
	c.mu.Unlock()
	c.loads.DoChan(tenantFlightKey(id, gen), func() (any, error) {
		return c.loadAndStore(id, gen, load)
	})
}

func (c *tenantCache) loadAndStore(id string, gen uint64, load tenantLoader) (Tenant, error) {
	ctx, cancel := context.WithTimeout(context.Background(), tenantLoadTimeout)
	defer cancel()
	tenant, err := load(ctx, id)
	now := time.Now()
	c.mu.Lock()
	defer c.mu.Unlock()
	switch {
	case err == nil:
		if c.failing {
			log.Infof("identity: tenant reads succeed again; cached tenant rows are refreshed from the database")
		}
		c.failing = false
		if c.gen == gen {
			if c.entries == nil {
				c.entries = make(map[string]tenantCacheEntry)
			}
			c.entries[id] = tenantCacheEntry{tenant: tenant, loadedAt: now}
		}
	case errors.Is(err, sql.ErrNoRows):
		c.failing = false
		delete(c.entries, id)
	default:
		c.failing = true
		c.lastErr = err
		c.retryAfter = now.Add(tenantRefreshRetryInterval)
	}
	return tenant, err
}

func (c *tenantCache) invalidate(id string) {
	c.mu.Lock()
	c.gen++
	delete(c.entries, id)
	c.mu.Unlock()
}

func (c *tenantCache) invalidateAll() {
	c.mu.Lock()
	c.gen++
	c.entries = nil
	c.mu.Unlock()
}

func (c *tenantCache) noteStale(id string, age time.Duration, now time.Time) {
	c.staleServed.Add(1)
	c.mu.Lock()
	if now.Sub(c.staleLogged[id]) < tenantStaleLogInterval {
		c.mu.Unlock()
		return
	}
	if c.staleLogged == nil {
		c.staleLogged = make(map[string]time.Time)
	}
	c.staleLogged[id] = now
	lastErr := c.lastErr
	c.mu.Unlock()
	log.Warnf("identity: tenant %s checked against its row cached %s ago because the database is unavailable: %v",
		id, age.Round(time.Second), lastErr)
}

// tenantSnapshot copies a cached row for one caller and evaluates expiry
// against now, so the cache never answers "active" past expires_at.
func tenantSnapshot(tenant Tenant, now time.Time) Tenant {
	if tenant.ExpiresAt != nil {
		expires := *tenant.ExpiresAt
		tenant.ExpiresAt = &expires
	}
	tenant.EffectiveStatus = effectiveTenantStatus(tenant, now)
	return tenant
}

func effectiveTenantStatus(tenant Tenant, now time.Time) string {
	if tenant.Status == "active" && tenant.Type != "system" && tenant.ExpiresAt != nil && !tenant.ExpiresAt.After(now) {
		return "expired"
	}
	return tenant.Status
}

// loadTenant reads one tenant row from the database.
func (s *Service) loadTenant(ctx context.Context, id string) (Tenant, error) {
	var tenant Tenant
	var expires sql.NullTime
	var accessTTL, refreshTTL sql.NullInt64
	err := s.db.QueryRowContext(ctx, `
		SELECT id, slug, name, type, status, expires_at, description,
		       COALESCE(access_token_ttl_seconds, 43200), COALESCE(refresh_token_ttl_seconds, 2592000),
		       created_at, updated_at, version
		  FROM tenants WHERE id = ?`, id).Scan(
		&tenant.ID, &tenant.Slug, &tenant.Name, &tenant.Type, &tenant.Status, &expires,
		&tenant.Description, &accessTTL, &refreshTTL, &tenant.CreatedAt, &tenant.UpdatedAt, &tenant.Version)
	if err != nil {
		// Fallback for DBs that have not yet applied TTL columns.
		err2 := s.db.QueryRowContext(ctx, `SELECT id, slug, name, type, status, expires_at, description, created_at, updated_at, version FROM tenants WHERE id = ?`, id).Scan(
			&tenant.ID, &tenant.Slug, &tenant.Name, &tenant.Type, &tenant.Status, &expires,
			&tenant.Description, &tenant.CreatedAt, &tenant.UpdatedAt, &tenant.Version)
		if err2 != nil {
			return tenant, err
		}
		tenant.AccessTokenTTLSeconds = 43200
		tenant.RefreshTokenTTLSeconds = 2592000
	} else {
		if accessTTL.Valid {
			tenant.AccessTokenTTLSeconds = int(accessTTL.Int64)
		} else {
			tenant.AccessTokenTTLSeconds = 43200
		}
		if refreshTTL.Valid {
			tenant.RefreshTokenTTLSeconds = int(refreshTTL.Int64)
		} else {
			tenant.RefreshTokenTTLSeconds = 2592000
		}
	}
	if expires.Valid {
		tenant.ExpiresAt = &expires.Time
	}
	tenant.EffectiveStatus = effectiveTenantStatus(tenant, time.Now())
	return tenant, nil
}
