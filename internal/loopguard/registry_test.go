package loopguard

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// memStore is a Store over a map, with the versioning of the real one.
type memStore struct {
	mu       sync.Mutex
	rows     map[string]Stored
	loads    atomic.Int64
	saves    atomic.Int64
	loadErr  error
	saveHook func(tenantID string) // runs inside Save, before the check
	// conflicts makes the next n Saves fail with ErrConflict after running
	// the hook, as if someone else had written between Load and Save.
	conflicts int
	loadGate  chan struct{}
}

func newMemStore() *memStore { return &memStore{rows: map[string]Stored{}} }

func (m *memStore) Load(_ context.Context, tenantID string) (Stored, error) {
	m.loads.Add(1)
	// The row is read before the gate and returned after it, as a database read
	// that is slow to answer returns the row as it was when it was read.
	m.mu.Lock()
	stored, err := m.rows[tenantID], m.loadErr
	m.mu.Unlock()
	if m.loadGate != nil {
		<-m.loadGate
	}
	if err != nil {
		return Stored{}, err
	}
	return stored, nil
}

func (m *memStore) Save(_ context.Context, tenantID string, settings Settings, expected int64) error {
	m.saves.Add(1)
	if m.saveHook != nil {
		m.saveHook(tenantID)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.conflicts > 0 {
		m.conflicts--
		return ErrConflict
	}
	if m.rows[tenantID].Version != expected {
		return ErrConflict
	}
	m.rows[tenantID] = Stored{Settings: settings, Version: expected + 1}
	return nil
}

func (m *memStore) put(tenantID string, settings Settings) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.rows[tenantID] = Stored{Settings: settings, Version: m.rows[tenantID].Version + 1}
}

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func newTestRegistry(store Store) (*Registry, *fakeClock) {
	clock := &fakeClock{t: time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)}
	r := New()
	r.now = clock.now
	if store != nil {
		r.SetStore(store)
	}
	return r, clock
}

func toolOn() Settings { return Settings{Scope: Scope{Tool: ToolRule{Enabled: Bool(true)}}} }

func TestRegistryWithoutAStoreIsOffAndReadOnly(t *testing.T) {
	r, _ := newTestRegistry(nil)
	if got := r.Effective("tenant-a", "key"); got.Any() {
		t.Fatalf("effective without a store = %+v", got)
	}
	if r.Persistent() {
		t.Fatal("persistent without a store")
	}
	if _, err := r.Update(context.Background(), "tenant-a", func(*Settings) error { return nil }); !errors.Is(err, ErrNotPersistent) {
		t.Fatalf("Update without a store = %v, want ErrNotPersistent", err)
	}
	var nilRegistry *Registry
	if nilRegistry.For("t").Tool.Enabled != nil || nilRegistry.Persistent() {
		t.Fatal("a nil registry is not off")
	}
	nilRegistry.Invalidate("t") // must not panic
}

func TestRegistryCachesForTheTTL(t *testing.T) {
	store := newMemStore()
	store.put("tenant-a", toolOn())
	r, clock := newTestRegistry(store)

	for i := 0; i < 5; i++ {
		if !r.Effective("tenant-a", "").Any() {
			t.Fatal("tenant-a should have the tool check on")
		}
	}
	if n := store.loads.Load(); n != 1 {
		t.Fatalf("5 requests read the store %d times", n)
	}

	// A change made behind this process's back shows after the TTL, no sooner.
	store.put("tenant-a", Settings{})
	clock.advance(cacheTTL - time.Second)
	if !r.Effective("tenant-a", "").Any() {
		t.Fatal("a stale entry was reloaded before its TTL")
	}
	clock.advance(2 * time.Second)
	if r.Effective("tenant-a", "").Any() {
		t.Fatal("the entry was not reloaded after its TTL")
	}
	if n := store.loads.Load(); n != 2 {
		t.Fatalf("store read %d times, want 2", n)
	}
}

func TestRegistryKeepsTenantsApart(t *testing.T) {
	store := newMemStore()
	store.put("tenant-a", toolOn())
	r, _ := newTestRegistry(store)
	if !r.Effective("tenant-a", "").Any() {
		t.Fatal("tenant-a lost its settings")
	}
	if r.Effective("tenant-b", "").Any() {
		t.Fatal("tenant-b picked up tenant-a's settings")
	}
	// The empty tenant is the system tenant, never a wildcard.
	store.put(systemTenantID, Settings{Scope: Scope{Reply: ReplyRule{Enabled: Bool(true)}}})
	if got := r.Effective("", ""); got.Reply == nil || got.Tool != nil {
		t.Fatalf("empty tenant = %+v, want the system tenant's reply check", got)
	}
}

func TestRegistryInvalidateReloads(t *testing.T) {
	store := newMemStore()
	r, _ := newTestRegistry(store)
	if r.Effective("tenant-a", "").Any() {
		t.Fatal("no row should be everything off")
	}
	store.put("tenant-a", toolOn())
	if r.Effective("tenant-a", "").Any() {
		t.Fatal("served the new row before it was invalidated or expired")
	}
	r.Invalidate("tenant-a")
	if !r.Effective("tenant-a", "").Any() {
		t.Fatal("Invalidate did not drop the entry")
	}

	store.put("tenant-a", Settings{})
	r.InvalidateAll()
	if r.Effective("tenant-a", "").Any() {
		t.Fatal("InvalidateAll did not drop the entry")
	}
}

func TestRegistryReadFailureKeepsWhatWasRead(t *testing.T) {
	store := newMemStore()
	store.put("tenant-a", toolOn())
	r, clock := newTestRegistry(store)
	if !r.Effective("tenant-a", "").Any() {
		t.Fatal("setup")
	}

	store.loadErr = errors.New("database unreachable")
	clock.advance(cacheTTL + time.Second)
	if !r.Effective("tenant-a", "").Any() {
		t.Fatal("a failing store switched a tenant's protection off")
	}
	loads := store.loads.Load()
	// Failing reads are spaced out, not retried by every request.
	for i := 0; i < 10; i++ {
		r.Effective("tenant-a", "")
	}
	if n := store.loads.Load(); n != loads {
		t.Fatalf("%d store reads while the retry interval had not passed", n-loads)
	}
	clock.advance(retryAfterFailure + time.Second)
	store.loadErr = nil
	store.put("tenant-a", Settings{})
	if r.Effective("tenant-a", "").Any() {
		t.Fatal("the store recovered but the entry was not reloaded")
	}

	// A tenant never read before and a store that fails: off, and not cached as if read.
	store.loadErr = errors.New("database unreachable")
	if r.Effective("tenant-new", "").Any() {
		t.Fatal("an unreadable tenant must be off")
	}
	store.loadErr = nil
	store.put("tenant-new", toolOn())
	clock.advance(retryAfterFailure + time.Second)
	if !r.Effective("tenant-new", "").Any() {
		t.Fatal("a tenant that could not be read once was not read again")
	}
}

func TestRegistrySharesOneReadBetweenConcurrentRequests(t *testing.T) {
	store := newMemStore()
	store.put("tenant-a", toolOn())
	store.loadGate = make(chan struct{})
	r, _ := newTestRegistry(store)

	var wg sync.WaitGroup
	results := make(chan bool, 20)
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results <- r.Effective("tenant-a", "").Any()
		}()
	}
	// Let the goroutines pile up behind the one read.
	time.Sleep(50 * time.Millisecond)
	close(store.loadGate)
	wg.Wait()
	close(results)
	for on := range results {
		if !on {
			t.Fatal("a request did not see the stored settings")
		}
	}
	if n := store.loads.Load(); n != 1 {
		t.Fatalf("20 concurrent requests read the store %d times", n)
	}
}

// A read that was in flight when the settings were invalidated must not put what
// it read back: that is the row from before the change.
func TestRegistryDoesNotCacheAReadThatRacedAnInvalidation(t *testing.T) {
	store := newMemStore()
	store.put("tenant-a", Settings{})
	store.loadGate = make(chan struct{})
	r, _ := newTestRegistry(store)

	done := make(chan Effective)
	go func() { done <- r.Effective("tenant-a", "") }()
	time.Sleep(50 * time.Millisecond) // the read is waiting on the gate, holding the old row
	store.mu.Lock()
	store.rows["tenant-a"] = Stored{Settings: toolOn(), Version: 2}
	store.mu.Unlock()
	r.Invalidate("tenant-a")
	close(store.loadGate)
	<-done

	if !r.Effective("tenant-a", "").Any() {
		t.Fatal("the read that raced the invalidation put the old row back in the cache")
	}
}

func TestRegistryUpdate(t *testing.T) {
	t.Run("saves what the mutation changed and serves it at once", func(t *testing.T) {
		store := newMemStore()
		r, _ := newTestRegistry(store)
		got, err := r.Update(context.Background(), "tenant-a", func(s *Settings) error {
			s.Tool = ToolRule{Enabled: Bool(true), Repeats: 6}
			s.Keys = map[string]Scope{"key-1": {Reply: ReplyRule{Enabled: Bool(true)}}}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		if got.UpdatedAt.IsZero() || got.Tool.Repeats != 6 {
			t.Fatalf("returned %+v", got)
		}
		stored := store.rows["tenant-a"]
		if stored.Version != 1 || stored.Settings.Tool.Repeats != 6 || stored.Settings.Keys["key-1"].Reply.Enabled == nil {
			t.Fatalf("stored %+v", stored)
		}
		effective := r.Effective("tenant-a", "key-1")
		if effective.Tool == nil || effective.Tool.Repeats != 6 || effective.Reply == nil {
			t.Fatalf("the write is not in the cache: %+v", effective)
		}
		if n := store.loads.Load(); n != 1 {
			t.Fatalf("serving the write read the store again (%d loads)", n)
		}
	})

	t.Run("a mutation that changes nothing writes nothing", func(t *testing.T) {
		store := newMemStore()
		r, _ := newTestRegistry(store)
		if _, err := r.Update(context.Background(), "tenant-a", func(s *Settings) error {
			s.Keys = map[string]Scope{"ghost": {}} // sanitized away
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		if store.saves.Load() != 0 || len(store.rows) != 0 {
			t.Fatalf("a no-op created a row: saves=%d rows=%v", store.saves.Load(), store.rows)
		}
		store.put("tenant-b", toolOn())
		saves := store.saves.Load()
		if _, err := r.Update(context.Background(), "tenant-b", func(s *Settings) error {
			s.Tool.Enabled = Bool(true)
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		if store.saves.Load() != saves || store.rows["tenant-b"].Version != 1 {
			t.Fatal("setting a value to what it already is bumped the version")
		}
	})

	t.Run("a refused mutation saves nothing", func(t *testing.T) {
		store := newMemStore()
		r, _ := newTestRegistry(store)
		refusal := errors.New("no")
		if _, err := r.Update(context.Background(), "tenant-a", func(*Settings) error { return refusal }); !errors.Is(err, refusal) {
			t.Fatalf("err = %v", err)
		}
		if store.saves.Load() != 0 {
			t.Fatal("saved a refused change")
		}
	})

	t.Run("a conflict recomputes the change on top of the concurrent one", func(t *testing.T) {
		store := newMemStore()
		store.put("tenant-a", Settings{})
		r, _ := newTestRegistry(store)
		var raced bool
		store.saveHook = func(tenantID string) {
			if raced {
				return
			}
			raced = true
			// Another admin sets a different key's override between our read and write.
			store.put(tenantID, Settings{Keys: map[string]Scope{"key-2": {Tool: ToolRule{Enabled: Bool(true)}}}})
		}
		var calls int
		got, err := r.Update(context.Background(), "tenant-a", func(s *Settings) error {
			calls++
			if s.Keys == nil {
				s.Keys = map[string]Scope{}
			}
			s.Keys["key-1"] = Scope{Reply: ReplyRule{Enabled: Bool(true)}}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		if calls != 2 {
			t.Fatalf("mutate ran %d times, want 2", calls)
		}
		if _, ok := got.Keys["key-1"]; !ok {
			t.Fatalf("lost our change: %+v", got.Keys)
		}
		if _, ok := got.Keys["key-2"]; !ok {
			t.Fatalf("overwrote the concurrent change: %+v", got.Keys)
		}
	})

	t.Run("gives up after repeated conflicts", func(t *testing.T) {
		store := newMemStore()
		store.conflicts = 100
		r, _ := newTestRegistry(store)
		_, err := r.Update(context.Background(), "tenant-a", func(s *Settings) error {
			s.Tool.Enabled = Bool(true)
			return nil
		})
		if !errors.Is(err, ErrConflict) {
			t.Fatalf("err = %v, want ErrConflict", err)
		}
		if n := store.saves.Load(); n != updateAttempts {
			t.Fatalf("tried %d times, want %d", n, updateAttempts)
		}
		if r.Effective("tenant-a", "").Any() {
			t.Fatal("a write that failed took effect")
		}
	})

	t.Run("an unreadable store is an error, not an empty row", func(t *testing.T) {
		store := newMemStore()
		store.loadErr = errors.New("database unreachable")
		r, _ := newTestRegistry(store)
		if _, err := r.Update(context.Background(), "tenant-a", func(s *Settings) error {
			s.Tool.Enabled = Bool(true)
			return nil
		}); err == nil {
			t.Fatal("Update on an unreadable row succeeded; it would have overwritten the row with defaults")
		}
		if store.saves.Load() != 0 {
			t.Fatal("saved over a row that could not be read")
		}
	})
}
