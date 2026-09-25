package identity

import (
	"context"
	"database/sql"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeTenantStore stands in for the tenants table.
type fakeTenantStore struct {
	mu     sync.Mutex
	rows   map[string]Tenant
	err    error
	loads  atomic.Int32
	gate   chan struct{} // when set, loads wait for it
	loaded chan struct{} // when set, receives after each load starts
}

func newFakeTenantStore(rows ...Tenant) *fakeTenantStore {
	store := &fakeTenantStore{rows: map[string]Tenant{}}
	for _, row := range rows {
		store.rows[row.ID] = row
	}
	return store
}

func (f *fakeTenantStore) load(ctx context.Context, id string) (Tenant, error) {
	f.loads.Add(1)
	if f.loaded != nil {
		f.loaded <- struct{}{}
	}
	if f.gate != nil {
		<-f.gate
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return Tenant{}, f.err
	}
	row, ok := f.rows[id]
	if !ok {
		return Tenant{}, sql.ErrNoRows
	}
	return row, nil
}

func (f *fakeTenantStore) set(row Tenant) {
	f.mu.Lock()
	f.rows[row.ID] = row
	f.mu.Unlock()
}

func (f *fakeTenantStore) fail(err error) {
	f.mu.Lock()
	f.err = err
	f.mu.Unlock()
}

// age moves the cached row of id back by d.
func (c *tenantCache) age(id string, d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	entry := c.entries[id]
	entry.loadedAt = entry.loadedAt.Add(-d)
	c.entries[id] = entry
}

var errDatabaseDown = errors.New("dial tcp 10.0.0.1:5432: connect: connection refused")

func activeTenant(id string) Tenant {
	return Tenant{ID: id, Type: "standard", Status: "active"}
}

func TestTenantCacheServesFreshRowsWithoutReading(t *testing.T) {
	var cache tenantCache
	store := newFakeTenantStore(activeTenant("t1"))
	for i := 0; i < 5; i++ {
		tenant, err := cache.get(context.Background(), "t1", store.load)
		if err != nil || tenant.Status != "active" {
			t.Fatalf("get = %+v, %v", tenant, err)
		}
	}
	if got := store.loads.Load(); got != 1 {
		t.Fatalf("loads = %d, want one read within the TTL", got)
	}
	cache.age("t1", tenantCacheTTL)
	if _, err := cache.get(context.Background(), "t1", store.load); err != nil {
		t.Fatal(err)
	}
	if got := store.loads.Load(); got != 2 {
		t.Fatalf("loads = %d, want a re-read after the TTL", got)
	}
}

func TestTenantCacheServesStaleRowsWhileTheDatabaseIsDown(t *testing.T) {
	var cache tenantCache
	store := newFakeTenantStore(activeTenant("t1"))
	if _, err := cache.get(context.Background(), "t1", store.load); err != nil {
		t.Fatal(err)
	}
	cache.age("t1", tenantCacheTTL+time.Second)
	store.fail(errDatabaseDown)

	tenant, err := cache.get(context.Background(), "t1", store.load)
	if err != nil || tenant.ID != "t1" {
		t.Fatalf("get during outage = %+v, %v; want the cached row", tenant, err)
	}
	if got := cache.staleServed.Load(); got != 1 {
		t.Fatalf("stale served = %d, want 1", got)
	}
	// Once the outage is known, callers get the stale row without waiting on
	// the database; background refreshes are spaced by the retry interval.
	loads := store.loads.Load()
	for i := 0; i < 20; i++ {
		if _, err := cache.get(context.Background(), "t1", store.load); err != nil {
			t.Fatalf("get %d during outage: %v", i, err)
		}
	}
	if got := store.loads.Load() - loads; got > 1 {
		t.Fatalf("loads during the known outage = %d, want at most one background refresh", got)
	}

	// Rows older than the stale limit are not served.
	cache.age("t1", tenantCacheStaleMaxAge)
	if _, err := cache.get(context.Background(), "t1", store.load); err == nil {
		t.Fatal("get with a row past the stale limit succeeded, want the database error")
	}
	// A tenant never cached cannot be served either.
	if _, err := cache.get(context.Background(), "t2", store.load); err == nil {
		t.Fatal("get of an uncached tenant during the outage succeeded")
	}
}

func TestTenantCacheRecoversAfterTheOutage(t *testing.T) {
	var cache tenantCache
	store := newFakeTenantStore(activeTenant("t1"))
	if _, err := cache.get(context.Background(), "t1", store.load); err != nil {
		t.Fatal(err)
	}
	cache.age("t1", tenantCacheTTL+time.Second)
	store.fail(errDatabaseDown)
	if _, err := cache.get(context.Background(), "t1", store.load); err != nil {
		t.Fatal(err)
	}
	store.fail(nil)
	suspended := activeTenant("t1")
	suspended.Status = "suspended"
	store.set(suspended)
	deadline := time.Now().Add(3 * time.Second)
	for {
		tenant, err := cache.get(context.Background(), "t1", store.load)
		if err == nil && tenant.Status == "suspended" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("cache still serves %+v (err %v) after the database came back", tenant, err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestTenantCacheDropsDeletedTenants(t *testing.T) {
	var cache tenantCache
	store := newFakeTenantStore(activeTenant("t1"))
	if _, err := cache.get(context.Background(), "t1", store.load); err != nil {
		t.Fatal(err)
	}
	cache.age("t1", tenantCacheTTL+time.Second)
	store.mu.Lock()
	delete(store.rows, "t1")
	store.mu.Unlock()
	if _, err := cache.get(context.Background(), "t1", store.load); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("get of a deleted tenant = %v, want sql.ErrNoRows", err)
	}
	store.fail(errDatabaseDown)
	if _, err := cache.get(context.Background(), "t1", store.load); err == nil {
		t.Fatal("a deleted tenant must not come back from the cache during an outage")
	}
}

func TestTenantCacheInvalidateTakesEffectImmediately(t *testing.T) {
	var cache tenantCache
	store := newFakeTenantStore(activeTenant("t1"))
	if _, err := cache.get(context.Background(), "t1", store.load); err != nil {
		t.Fatal(err)
	}
	disabled := activeTenant("t1")
	disabled.Status = "disabled"
	store.set(disabled)
	cache.invalidate("t1")
	tenant, err := cache.get(context.Background(), "t1", store.load)
	if err != nil || tenant.Status != "disabled" {
		t.Fatalf("get after invalidate = %+v, %v; want the disabled row at once", tenant, err)
	}

	store.set(activeTenant("t1"))
	cache.invalidateAll()
	if tenant, _ = cache.get(context.Background(), "t1", store.load); tenant.Status != "active" {
		t.Fatalf("get after invalidateAll = %+v, want a fresh read", tenant)
	}
}

// A read that started before a tenant write must not put the old row back
// after the write invalidated it.
func TestTenantCacheInvalidationBeatsAnInFlightRead(t *testing.T) {
	var cache tenantCache
	store := newFakeTenantStore(activeTenant("t1"))
	store.gate = make(chan struct{})
	store.loaded = make(chan struct{}, 4)
	firstDone := make(chan Tenant, 1)
	go func() {
		tenant, _ := cache.get(context.Background(), "t1", store.load)
		firstDone <- tenant
	}()
	<-store.loaded // the stale read is in flight with the old row
	disabled := activeTenant("t1")
	disabled.Status = "disabled"
	cache.invalidate("t1")

	secondDone := make(chan Tenant, 1)
	go func() {
		// This load belongs to the new generation and reads the new row.
		tenant, _ := cache.get(context.Background(), "t1", func(ctx context.Context, id string) (Tenant, error) {
			return disabled, nil
		})
		secondDone <- tenant
	}()
	if tenant := <-secondDone; tenant.Status != "disabled" {
		t.Fatalf("read after the write = %+v, want disabled", tenant)
	}
	close(store.gate)
	<-firstDone
	if tenant, _ := cache.get(context.Background(), "t1", store.load); tenant.Status != "disabled" {
		t.Fatalf("cached row = %+v, want the pre-write read discarded", tenant)
	}
}

func TestTenantCacheEvaluatesExpiryOnEveryRead(t *testing.T) {
	var cache tenantCache
	expires := time.Now().Add(50 * time.Millisecond)
	row := activeTenant("t1")
	row.ExpiresAt = &expires
	store := newFakeTenantStore(row)
	tenant, err := cache.get(context.Background(), "t1", store.load)
	if err != nil || tenant.EffectiveStatus != "active" {
		t.Fatalf("get before expiry = %+v, %v", tenant, err)
	}
	time.Sleep(80 * time.Millisecond)
	tenant, err = cache.get(context.Background(), "t1", store.load)
	if err != nil {
		t.Fatal(err)
	}
	if tenant.EffectiveStatus != "expired" {
		t.Fatalf("effective status after expiry = %q, want expired from the cached row", tenant.EffectiveStatus)
	}
	if err := validateTenant(tenant, time.Now()); !errors.Is(err, ErrTenantExpired) {
		t.Fatalf("validateTenant = %v, want ErrTenantExpired", err)
	}
	// Callers get their own copy of the expiry.
	*tenant.ExpiresAt = time.Now().Add(time.Hour)
	if again, _ := cache.get(context.Background(), "t1", store.load); again.EffectiveStatus != "expired" {
		t.Fatal("a caller mutated the cached row")
	}
}

func TestTenantAccessChecksUseTheCache(t *testing.T) {
	service := &Service{}
	store := newFakeTenantStore(activeTenant("t1"))
	// Route the service's reads through the fake store.
	get := func() error {
		_, err := service.tenants.get(context.Background(), "t1", store.load)
		return err
	}
	for i := 0; i < 3; i++ {
		if err := get(); err != nil {
			t.Fatal(err)
		}
	}
	service.InvalidateTenant("t1")
	if err := get(); err != nil {
		t.Fatal(err)
	}
	if got := store.loads.Load(); got != 2 {
		t.Fatalf("loads = %d, want 2 (one per generation)", got)
	}
	if got := (*Service)(nil).TenantStaleServed(); got != 0 {
		t.Fatalf("nil service stale count = %d", got)
	}
}
