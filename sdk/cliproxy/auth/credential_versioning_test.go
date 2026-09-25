package auth

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// versionedTestStore models a versioned store: Save is a compare-and-set on the
// version stamped in the metadata, Get returns the newest document.
type versionedTestStore struct {
	mu        sync.Mutex
	docs      map[string]map[string]any
	indexes   map[string]string
	saves     int
	saveCtxs  []context.Context
	getCalls  int
	listCalls int
	// beforeSave runs once per Save before the version check, to simulate a
	// write committed by another node in between.
	beforeSave func(id string)
}

func newVersionedTestStore() *versionedTestStore {
	return &versionedTestStore{docs: make(map[string]map[string]any), indexes: make(map[string]string)}
}

func (s *versionedTestStore) put(id string, version int64, metadata map[string]any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	doc := make(map[string]any, len(metadata)+1)
	for k, v := range metadata {
		doc[k] = v
	}
	doc[CredentialVersionMetadataKey] = float64(version)
	s.docs[id] = doc
}

func (s *versionedTestStore) doc(id string) map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]any)
	for k, v := range s.docs[id] {
		out[k] = v
	}
	return out
}

func (s *versionedTestStore) List(context.Context) ([]*Auth, error) {
	s.mu.Lock()
	s.listCalls++
	s.mu.Unlock()
	return nil, nil
}

func (s *versionedTestStore) Save(ctx context.Context, auth *Auth) (string, error) {
	if hook := s.beforeSave; hook != nil {
		hook(auth.ID)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.saves++
	s.saveCtxs = append(s.saveCtxs, ctx)
	current, ok := s.docs[auth.ID]
	if !ok {
		if !IsCredentialCreate(ctx) {
			return "", ErrCredentialGone
		}
	} else if MetadataCredentialVersion(current) != CredentialVersion(auth) {
		return "", fmt.Errorf("%w: %s", ErrCredentialConflict, auth.ID)
	}
	next := make(map[string]any, len(auth.Metadata)+1)
	for k, v := range auth.Metadata {
		next[k] = v
	}
	next[CredentialVersionMetadataKey] = float64(MetadataCredentialVersion(current) + 1)
	s.docs[auth.ID] = next
	return "", nil
}

func (s *versionedTestStore) Delete(context.Context, string) error { return nil }

func (s *versionedTestStore) Get(_ context.Context, id string) (*Auth, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.getCalls++
	doc, ok := s.docs[id]
	if !ok {
		return nil, nil
	}
	metadata := make(map[string]any, len(doc))
	for k, v := range doc {
		metadata[k] = v
	}
	auth := &Auth{ID: id, Provider: "test-provider", FileName: id, Metadata: metadata, Status: StatusActive}
	if prefix, ok := metadata["prefix"].(string); ok {
		auth.Prefix = prefix
	}
	RestorePersistedDisabled(auth)
	return auth, nil
}

func (s *versionedTestStore) ResolveAuthIndex(id string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.indexes[id]
}

func registerVersioned(t *testing.T, mgr *Manager, store *versionedTestStore, id string, version int64, metadata map[string]any) *Auth {
	t.Helper()
	store.put(id, version, metadata)
	latest, _ := store.Get(context.Background(), id)
	registered, err := mgr.Register(WithSkipPersist(context.Background()), latest)
	if err != nil {
		t.Fatalf("register %s: %v", id, err)
	}
	return registered
}

func TestMutateAuthWithFileStoreMutatesBaseAndUpdates(t *testing.T) {
	store := &trackingStore{}
	mgr := NewManager(store, nil, nil)
	if _, err := mgr.Register(WithSkipPersist(context.Background()), &Auth{ID: "a.json", Metadata: map[string]any{"type": "codex"}}); err != nil {
		t.Fatal(err)
	}
	base, _ := mgr.GetByID("a.json")
	var mutated *Auth
	updated, err := mgr.MutateAuth(context.Background(), base, func(a *Auth) (bool, error) {
		mutated = a
		a.Metadata["label"] = "renamed"
		return true, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if mutated != base {
		t.Fatal("single-node MutateAuth must apply the mutation to the caller's copy")
	}
	if store.saveCount.Load() != 1 || updated.Metadata["label"] != "renamed" {
		t.Fatalf("saves=%d label=%v, want one save of the mutated copy", store.saveCount.Load(), updated.Metadata["label"])
	}
	if _, err = mgr.MutateAuth(context.Background(), base, func(*Auth) (bool, error) { return false, nil }); err != nil || store.saveCount.Load() != 1 {
		t.Fatalf("unchanged mutation must not save, err=%v saves=%d", err, store.saveCount.Load())
	}
}

func TestMutateAuthAppliesToNewestCopyAndRetriesConflicts(t *testing.T) {
	store := newVersionedTestStore()
	mgr := NewManager(store, nil, nil)
	registerVersioned(t, mgr, store, "a.json", 1, map[string]any{"type": "claude", "access_token": "old-token"})
	base, _ := mgr.GetByID("a.json")

	// Another node rotates the token after this node read its copy.
	store.put("a.json", 2, map[string]any{"type": "claude", "access_token": "rotated-token"})
	// And commits once more in the middle of our first attempt.
	var injected atomic.Bool
	store.beforeSave = func(id string) {
		if injected.CompareAndSwap(false, true) {
			store.put(id, 3, map[string]any{"type": "claude", "access_token": "rotated-again", "prefix": "team"})
		}
	}

	attempts := 0
	updated, err := mgr.MutateAuth(context.Background(), base, func(a *Auth) (bool, error) {
		attempts++
		a.Metadata["label"] = "ops"
		return true, nil
	})
	if err != nil {
		t.Fatalf("MutateAuth: %v", err)
	}
	if attempts != 2 {
		t.Fatalf("attempts = %d, want the conflict to be retried once", attempts)
	}
	doc := store.doc("a.json")
	if doc["access_token"] != "rotated-again" || doc["label"] != "ops" || doc["prefix"] != "team" {
		t.Fatalf("persisted doc = %v, want the newest token and prefix kept plus the label", doc)
	}
	if updated.Prefix != "team" {
		t.Fatalf("in-memory prefix = %q, want it re-derived from the newest copy", updated.Prefix)
	}
}

func TestMutateAuthReportsDeletedCredential(t *testing.T) {
	store := newVersionedTestStore()
	mgr := NewManager(store, nil, nil)
	registerVersioned(t, mgr, store, "a.json", 1, map[string]any{"type": "claude"})
	base, _ := mgr.GetByID("a.json")
	store.mu.Lock()
	delete(store.docs, "a.json")
	store.mu.Unlock()
	called := false
	_, err := mgr.MutateAuth(context.Background(), base, func(*Auth) (bool, error) { called = true; return true, nil })
	if !errors.Is(err, ErrCredentialGone) || called {
		t.Fatalf("err=%v called=%v, want ErrCredentialGone before mutating", err, called)
	}
}

func TestUpdateRefreshedReappliesRefreshedKeysOnConflict(t *testing.T) {
	store := newVersionedTestStore()
	mgr := NewManager(store, nil, nil)
	registerVersioned(t, mgr, store, "a.json", 4, map[string]any{"type": "codex", "refresh_token": "rt-1", "access_token": "at-1"})
	current, _ := mgr.GetByID("a.json")
	before := SnapshotMetadata(current.Metadata)

	refreshed := current.Clone()
	refreshed.Metadata["refresh_token"] = "rt-2"
	refreshed.Metadata["access_token"] = "at-2"
	// A management edit lands while the refresh is in flight.
	store.put("a.json", 5, map[string]any{"type": "codex", "refresh_token": "rt-1", "access_token": "at-1", "label": "renamed"})

	if _, err := mgr.UpdateRefreshed(context.Background(), refreshed, before); err != nil {
		t.Fatalf("UpdateRefreshed: %v", err)
	}
	doc := store.doc("a.json")
	if doc["refresh_token"] != "rt-2" || doc["access_token"] != "at-2" || doc["label"] != "renamed" {
		t.Fatalf("persisted doc = %v, want rotated tokens on top of the concurrent edit", doc)
	}
}

func TestPinStoreIndexUsesResolverOnlyForVersionedStores(t *testing.T) {
	store := newVersionedTestStore()
	store.indexes["t1/a.json"] = "pinned-index"
	mgr := NewManager(store, nil, nil)
	registered, err := mgr.Register(WithSkipPersist(context.Background()), &Auth{ID: "t1/a.json", FileName: "a.json", Metadata: map[string]any{"type": "claude"}})
	if err != nil {
		t.Fatal(err)
	}
	if registered.Index != "pinned-index" {
		t.Fatalf("index = %q, want the store's pinned index", registered.Index)
	}
	virtual, _ := mgr.Register(WithSkipPersist(context.Background()), &Auth{ID: "t1/a.json::p", Attributes: map[string]string{"runtime_only": "true"}, Metadata: map[string]any{}})
	if virtual.Index == "pinned-index" || virtual.Index == "" {
		t.Fatalf("runtime-only auth index = %q, want its own derived index", virtual.Index)
	}

	fileMgr := NewManager(&trackingStore{}, nil, nil)
	plain, _ := fileMgr.Register(WithSkipPersist(context.Background()), &Auth{ID: "t1/a.json", FileName: "a.json", Metadata: map[string]any{"type": "claude"}})
	if plain.Index != (&Auth{FileName: "a.json"}).EnsureIndex() {
		t.Fatalf("file-store index = %q, want the unchanged FileName-derived index", plain.Index)
	}
}

func TestFileAuthIndexMatchesStartupLoadIndex(t *testing.T) {
	for _, id := range []string{"claude-a.json", "00000000-0000-0000-0000-000000000001/codex-b.json"} {
		loaded := &Auth{ID: id, FileName: id}
		if got, want := FileAuthIndex(id), loaded.EnsureIndex(); got != want {
			t.Fatalf("FileAuthIndex(%q) = %q, want %q", id, got, want)
		}
	}
}

func TestMarkResultMarksPersistAsResultRecording(t *testing.T) {
	store := newVersionedTestStore()
	mgr := NewManager(store, nil, nil)
	registerVersioned(t, mgr, store, "a.json", 1, map[string]any{"type": "codex"})
	mgr.MarkResult(context.Background(), Result{AuthID: "a.json", Provider: "codex", Success: true})
	store.mu.Lock()
	defer store.mu.Unlock()
	if len(store.saveCtxs) == 0 || !IsResultPersist(store.saveCtxs[len(store.saveCtxs)-1]) {
		t.Fatal("markResult must mark its save so a store can avoid blocking under the manager lock")
	}
}

type testRefreshCoordinator struct {
	mu       sync.Mutex
	acquired bool
	err      error
	latest   *Auth
	claims   int
	releases int
}

func (c *testRefreshCoordinator) ClaimRefresh(context.Context, string) (*Auth, bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.claims++
	if c.err != nil {
		return nil, false, c.err
	}
	if c.latest == nil {
		return nil, c.acquired, nil
	}
	return c.latest.Clone(), c.acquired, nil
}

func (c *testRefreshCoordinator) ReleaseRefresh(context.Context, string) {
	c.mu.Lock()
	c.releases++
	c.mu.Unlock()
}

const leaseTestProvider = "lease-test-provider"

func init() {
	RegisterRefreshLeadProvider(leaseTestProvider, func() *time.Duration {
		lead := time.Minute
		return &lead
	})
}

func newCoordinatedRefreshManager(t *testing.T, coordinator *testRefreshCoordinator, refreshed *atomic.Int32) (*Manager, *versionedTestStore) {
	t.Helper()
	store := newVersionedTestStore()
	mgr := NewManager(store, nil, nil)
	mgr.SetRefreshCoordinator(coordinator)
	mgr.RegisterExecutor(&stubExecutor{id: leaseTestProvider, onRefresh: func() { refreshed.Add(1) }})
	expired := time.Now().Add(-time.Minute).Format(time.RFC3339)
	store.put("a.json", 3, map[string]any{"type": leaseTestProvider, "refresh_token": "rt-1", "expired": expired})
	latest, _ := store.Get(context.Background(), "a.json")
	latest.Provider = leaseTestProvider
	if _, err := mgr.Register(WithSkipPersist(context.Background()), latest); err != nil {
		t.Fatal(err)
	}
	return mgr, store
}

func TestBackgroundRefreshSkipsWhenLeaseHeldElsewhereOrStoreDown(t *testing.T) {
	for name, coordinator := range map[string]*testRefreshCoordinator{
		"held elsewhere":    {acquired: false},
		"store unreachable": {err: errors.New("connection refused")},
	} {
		t.Run(name, func(t *testing.T) {
			var refreshed atomic.Int32
			mgr, _ := newCoordinatedRefreshManager(t, coordinator, &refreshed)
			mgr.refreshAuth(context.Background(), "a.json")
			if refreshed.Load() != 0 {
				t.Fatal("refresh must not spend the token without holding the lease")
			}
		})
	}
}

func TestBackgroundRefreshAdoptsNewerCopyInsteadOfRefreshing(t *testing.T) {
	fresh := time.Now().Add(time.Hour).Format(time.RFC3339)
	coordinator := &testRefreshCoordinator{acquired: true, latest: &Auth{
		ID: "a.json", Provider: leaseTestProvider,
		Metadata: map[string]any{"type": leaseTestProvider, "refresh_token": "rt-2", "expired": fresh, CredentialVersionMetadataKey: float64(4)},
	}}
	var refreshed atomic.Int32
	mgr, _ := newCoordinatedRefreshManager(t, coordinator, &refreshed)
	mgr.refreshAuth(context.Background(), "a.json")
	if refreshed.Load() != 0 {
		t.Fatal("a copy another node already refreshed must be adopted, not refreshed again")
	}
	current, _ := mgr.GetByID("a.json")
	if current.Metadata["refresh_token"] != "rt-2" || CredentialVersion(current) != 4 {
		t.Fatalf("in-memory copy = %v, want the adopted version 4", current.Metadata)
	}
	if coordinator.releases != 1 {
		t.Fatalf("releases = %d, want the lease released", coordinator.releases)
	}
}

func TestBackgroundRefreshPersistsUnderLease(t *testing.T) {
	coordinator := &testRefreshCoordinator{acquired: true}
	var refreshed atomic.Int32
	mgr, store := newCoordinatedRefreshManager(t, coordinator, &refreshed)
	mgr.refreshAuth(context.Background(), "a.json")
	if refreshed.Load() != 1 {
		t.Fatalf("refreshes = %d, want 1", refreshed.Load())
	}
	if MetadataCredentialVersion(store.doc("a.json")) != 4 {
		t.Fatalf("stored version = %v, want the refresh persisted as version 4", store.doc("a.json"))
	}
	if coordinator.releases != 1 {
		t.Fatalf("releases = %d, want 1", coordinator.releases)
	}
}

func TestBackgroundRefreshProceedsWhenNewestCopyStillExpired(t *testing.T) {
	expired := time.Now().Add(-time.Minute).Format(time.RFC3339)
	coordinator := &testRefreshCoordinator{acquired: true, latest: &Auth{
		ID: "a.json", Provider: leaseTestProvider,
		Metadata: map[string]any{"type": leaseTestProvider, "refresh_token": "rt-1", "expired": expired, CredentialVersionMetadataKey: float64(3)},
	}}
	var refreshed atomic.Int32
	mgr, _ := newCoordinatedRefreshManager(t, coordinator, &refreshed)
	mgr.refreshAuth(context.Background(), "a.json")
	if refreshed.Load() != 1 {
		t.Fatalf("refreshes = %d, want 1 when the newest copy still needs a refresh", refreshed.Load())
	}
}

func TestAcquireRefreshLeaseWaitsForHolder(t *testing.T) {
	mgr := NewManager(&trackingStore{}, nil, nil)
	lease, err := mgr.AcquireRefreshLease(context.Background(), "a.json", time.Second)
	if err != nil || lease == nil || lease.Latest != nil {
		t.Fatalf("single-node lease = %v, %v; want an empty lease", lease, err)
	}
	lease.Release()

	coordinator := &testRefreshCoordinator{acquired: false}
	mgr.SetRefreshCoordinator(coordinator)
	go func() {
		time.Sleep(300 * time.Millisecond)
		coordinator.mu.Lock()
		coordinator.acquired = true
		coordinator.mu.Unlock()
	}()
	lease, err = mgr.AcquireRefreshLease(context.Background(), "a.json", 5*time.Second)
	if err != nil {
		t.Fatalf("AcquireRefreshLease: %v", err)
	}
	lease.Release()
	lease.Release()
	if coordinator.claims < 2 || coordinator.releases != 1 {
		t.Fatalf("claims=%d releases=%d, want polling and one release", coordinator.claims, coordinator.releases)
	}
	coordinator.acquired = false
	if _, err = mgr.AcquireRefreshLease(context.Background(), "a.json", 0); !errors.Is(err, ErrRefreshInProgress) {
		t.Fatalf("err = %v, want ErrRefreshInProgress", err)
	}
}

func TestRecoverRotatedRefreshTokenReadsOneCredentialFromVersionedStore(t *testing.T) {
	store := newVersionedTestStore()
	mgr := NewManager(store, nil, nil)
	registerVersioned(t, mgr, store, "a.json", 1, map[string]any{"type": "codex", "refresh_token": "rt-1", "access_token": "at-1"})
	used, _ := mgr.GetByID("a.json")
	store.put("a.json", 2, map[string]any{"type": "codex", "refresh_token": "rt-2", "access_token": "at-2"})

	if !mgr.recoverRotatedRefreshToken(context.Background(), "a.json", used, time.Now(), errors.New("invalid_grant")) {
		t.Fatal("expected recovery from the rotated token another node persisted")
	}
	if store.listCalls != 0 || store.getCalls == 0 {
		t.Fatalf("list=%d get=%d, want a by-ID read instead of a full listing", store.listCalls, store.getCalls)
	}
	current, _ := mgr.GetByID("a.json")
	if current.Metadata["refresh_token"] != "rt-2" {
		t.Fatalf("refresh token = %v, want rt-2", current.Metadata["refresh_token"])
	}
}

func TestPreserveRuntimeMetadataCopiesOnlyRuntimeKeys(t *testing.T) {
	dst := &Auth{Metadata: map[string]any{"access_token": "new", ClaudeOAuthHealthMetadataKey: "file", persistedQuotaRuntimeMetadataKey: "stale"}}
	src := &Auth{Metadata: map[string]any{"access_token": "old", ClaudeOAuthHealthMetadataKey: "memory"}}
	PreserveRuntimeMetadata(dst, src)
	if dst.Metadata["access_token"] != "new" || dst.Metadata[ClaudeOAuthHealthMetadataKey] != "memory" {
		t.Fatalf("metadata = %v", dst.Metadata)
	}
	if _, ok := dst.Metadata[persistedQuotaRuntimeMetadataKey]; ok {
		t.Fatal("a runtime key the source no longer has must be dropped")
	}
}
